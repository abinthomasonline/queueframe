package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/secure"
)

const kiteAuthFlowTTL = 5 * time.Minute

var indiaLocation = time.FixedZone("Asia/Kolkata", 5*60*60+30*60)

func normalizeClientID(clientID string) string {
	return strings.ToUpper(strings.TrimSpace(clientID))
}

func (s *Store) BeginTraderSignup(ctx context.Context, signup domain.TraderSignup, now time.Time) (domain.KiteAuthFlow, error) {
	settings, err := s.CurrentSettings(ctx, now)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	if UserLoginBlocked(settings, now) {
		return domain.KiteAuthFlow{}, ErrUserLoginBlackout
	}
	signup.ClientID = normalizeClientID(signup.ClientID)
	signup.APIKey = strings.TrimSpace(signup.APIKey)
	signup.APISecret = strings.TrimSpace(signup.APISecret)
	if signup.ClientID == "" || signup.APIKey == "" || signup.APISecret == "" {
		return domain.KiteAuthFlow{}, errors.New("client ID, API key, and API secret are required")
	}
	signup.InviteToken = strings.TrimSpace(signup.InviteToken)
	if !validSignupInvitationToken(signup.InviteToken) {
		return domain.KiteAuthFlow{}, ErrSignupInvitationUnavailable
	}
	attestation := signup.PolicyAttestation
	if attestation.Version != domain.HostingPolicyVersion ||
		attestation.SourceURL != domain.HostingPolicySourceURL ||
		attestation.Text != domain.HostingPolicyExactText {
		return domain.KiteAuthFlow{}, ErrPolicyAttestationRequired
	}
	// The server, rather than a browser-supplied timestamp, records the instant
	// at which the exact acknowledgement was submitted.
	attestation.AcknowledgedAt = now
	return s.insertInvitedSignupFlow(ctx, domain.KiteAuthFlow{
		Purpose:           domain.KiteAuthSignup,
		ClientID:          signup.ClientID,
		APIKey:            signup.APIKey,
		APISecret:         signup.APISecret,
		PolicyAttestation: attestation,
	}, signup.InviteToken, now)
}

func (s *Store) BeginTraderSignin(ctx context.Context, clientID string, now time.Time) (domain.KiteAuthFlow, error) {
	settings, err := s.CurrentSettings(ctx, now)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	if UserLoginBlocked(settings, now) {
		return domain.KiteAuthFlow{}, ErrUserLoginBlackout
	}
	trader, err := s.TraderByClientID(ctx, clientID)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	if trader.Deleted() {
		return domain.KiteAuthFlow{}, ErrTraderDeleted
	}
	return s.insertKiteAuthFlow(ctx, domain.KiteAuthFlow{
		Purpose:   domain.KiteAuthSignin,
		TraderID:  trader.ID,
		ClientID:  trader.ClientID,
		APIKey:    trader.APIKey,
		APISecret: trader.APISecret,
	}, now)
}

func (s *Store) insertKiteAuthFlow(ctx context.Context, flow domain.KiteAuthFlow, now time.Time) (domain.KiteAuthFlow, error) {
	rawState, stateHash, err := secure.RandomToken(32)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	clientID, err := s.vault.Encrypt(flow.ClientID)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	apiKey, err := s.vault.Encrypt(flow.APIKey)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	apiSecret, err := s.vault.Encrypt(flow.APISecret)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.ExpiresAt = now.Add(kiteAuthFlowTTL)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO kite_auth_flows(
 state_hash,purpose,trader_id,client_id_enc,api_key_enc,api_secret_enc,
 policy_version,policy_source_url,policy_exact_text,policy_acknowledged_at_ms,
 expires_at_ms,signup_invitation_id,created_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, stateHash, flow.Purpose, nullableID(flow.TraderID), clientID, apiKey, apiSecret,
		flow.PolicyAttestation.Version, flow.PolicyAttestation.SourceURL, flow.PolicyAttestation.Text,
		millis(flow.PolicyAttestation.AcknowledgedAt), millis(flow.ExpiresAt), flow.SignupInvitationID, millis(now))
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.ID, _ = result.LastInsertId()
	flow.State = rawState
	return flow, nil
}

// insertInvitedSignupFlow atomically reserves an available invitation and
// creates its five-minute Kite flow. The reservation is conditional and made
// under the same write transaction, so concurrent submissions of one bearer
// can produce at most one flow. Failed/abandoned flows release the reservation;
// an untouched expired reservation becomes eligible for a later retry.
func (s *Store) insertInvitedSignupFlow(ctx context.Context, flow domain.KiteAuthFlow, rawInvite string, now time.Time) (domain.KiteAuthFlow, error) {
	rawState, stateHash, err := secure.RandomToken(32)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	clientID, err := s.vault.Encrypt(flow.ClientID)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	apiKey, err := s.vault.Encrypt(flow.APIKey)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	apiSecret, err := s.vault.Encrypt(flow.APISecret)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.ExpiresAt = now.Add(kiteAuthFlowTTL)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	defer tx.Rollback()
	reservation, err := tx.ExecContext(ctx, `
UPDATE signup_invitations
SET reserved_at_ms=?,reserved_until_ms=?,reserved_flow_id=0
WHERE token_hash=? AND expires_at_ms>? AND consumed_at_ms=0 AND revoked_at_ms=0
  AND (reserved_until_ms=0 OR reserved_until_ms<=?)`, millis(now), millis(flow.ExpiresAt),
		secure.TokenHash(rawInvite), millis(now), millis(now))
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	changed, err := reservation.RowsAffected()
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	if changed != 1 {
		return domain.KiteAuthFlow{}, ErrSignupInvitationUnavailable
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM signup_invitations WHERE token_hash=?`, secure.TokenHash(rawInvite)).Scan(&flow.SignupInvitationID); err != nil {
		return domain.KiteAuthFlow{}, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM traders WHERE deleted_at_ms=0 AND
 (client_id=? COLLATE NOCASE OR api_key=?))`, flow.ClientID, flow.APIKey).Scan(&exists); err != nil {
		return domain.KiteAuthFlow{}, err
	}
	if exists != 0 {
		return domain.KiteAuthFlow{}, fmt.Errorf("%w: client ID or API key is already registered", ErrConflict)
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO kite_auth_flows(
 state_hash,purpose,trader_id,client_id_enc,api_key_enc,api_secret_enc,
 policy_version,policy_source_url,policy_exact_text,policy_acknowledged_at_ms,
 expires_at_ms,signup_invitation_id,created_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, stateHash, flow.Purpose, nil, clientID, apiKey, apiSecret,
		flow.PolicyAttestation.Version, flow.PolicyAttestation.SourceURL, flow.PolicyAttestation.Text,
		millis(flow.PolicyAttestation.AcknowledgedAt), millis(flow.ExpiresAt), flow.SignupInvitationID, millis(now))
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.ID, _ = result.LastInsertId()
	updated, err := tx.ExecContext(ctx, `
UPDATE signup_invitations SET reserved_flow_id=?
WHERE id=? AND reserved_flow_id=0 AND reserved_until_ms=?`, flow.ID, flow.SignupInvitationID, millis(flow.ExpiresAt))
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	changed, err = updated.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return domain.KiteAuthFlow{}, err
		}
		return domain.KiteAuthFlow{}, ErrSignupInvitationUnavailable
	}
	if err := tx.Commit(); err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.State = rawState
	return flow, nil
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ConsumeKiteAuthFlow marks state used before revealing any credentials. The
// UPDATE guard makes two simultaneous callbacks safe: exactly one succeeds.
func (s *Store) ConsumeKiteAuthFlow(ctx context.Context, rawState string, now time.Time) (domain.KiteAuthFlow, error) {
	if strings.TrimSpace(rawState) == "" {
		return domain.KiteAuthFlow{}, ErrAuthFlowExpired
	}
	settings, err := s.CurrentSettings(ctx, now)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	if UserLoginBlocked(settings, now) {
		return domain.KiteAuthFlow{}, ErrUserLoginBlackout
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	defer tx.Rollback()
	var flow domain.KiteAuthFlow
	var traderID sql.NullInt64
	var clientIDEnc, apiKeyEnc, apiSecretEnc string
	var ackMS, expiresMS, consumedMS, completedMS int64
	err = tx.QueryRowContext(ctx, `
SELECT id,purpose,trader_id,client_id_enc,api_key_enc,api_secret_enc,
       policy_version,policy_source_url,policy_exact_text,policy_acknowledged_at_ms,
       expires_at_ms,consumed_at_ms,completed_at_ms,signup_invitation_id
FROM kite_auth_flows WHERE state_hash=?`, secure.TokenHash(rawState)).Scan(
		&flow.ID, &flow.Purpose, &traderID, &clientIDEnc, &apiKeyEnc, &apiSecretEnc,
		&flow.PolicyAttestation.Version, &flow.PolicyAttestation.SourceURL, &flow.PolicyAttestation.Text,
		&ackMS, &expiresMS, &consumedMS, &completedMS, &flow.SignupInvitationID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.KiteAuthFlow{}, ErrAuthFlowExpired
	}
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	if consumedMS != 0 || completedMS != 0 || expiresMS <= millis(now) {
		return domain.KiteAuthFlow{}, ErrAuthFlowExpired
	}
	result, err := tx.ExecContext(ctx, `
UPDATE kite_auth_flows SET consumed_at_ms=?
WHERE id=? AND consumed_at_ms=0 AND completed_at_ms=0 AND expires_at_ms>?`, millis(now), flow.ID, millis(now))
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return domain.KiteAuthFlow{}, err
		}
		return domain.KiteAuthFlow{}, ErrAuthFlowExpired
	}
	if err := tx.Commit(); err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.TraderID = traderID.Int64
	flow.ExpiresAt = fromMillis(expiresMS)
	flow.ConsumedAt = now
	flow.PolicyAttestation.AcknowledgedAt = fromMillis(ackMS)
	flow.ClientID, err = s.vault.Decrypt(clientIDEnc)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.APIKey, err = s.vault.Decrypt(apiKeyEnc)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	flow.APISecret, err = s.vault.Decrypt(apiSecretEnc)
	if err != nil {
		return domain.KiteAuthFlow{}, err
	}
	return flow, nil
}

// CompleteKiteAuth persists a session only if the Kite response matches both
// the expected client ID and API key. A successful verification creates a
// daily activation only inside that weekday's configured activation window.
func (s *Store) CompleteKiteAuth(ctx context.Context, flowID int64, session broker.Session, expiresAt time.Time) (domain.Trader, error) {
	return s.CompleteKiteAuthAt(ctx, flowID, session, expiresAt, time.Now())
}

// CompleteKiteAuthAt is the deterministic form used by the scheduler/store
// boundary tests. Production callers should normally use CompleteKiteAuth.
func (s *Store) CompleteKiteAuthAt(ctx context.Context, flowID int64, session broker.Session, expiresAt, now time.Time) (domain.Trader, error) {
	settings, err := s.CurrentSettings(ctx, now)
	if err != nil {
		return domain.Trader{}, err
	}
	if UserLoginBlocked(settings, now) {
		return domain.Trader{}, ErrUserLoginBlackout
	}
	loginAt := session.LoginAt
	if loginAt.IsZero() {
		loginAt = now
	}
	if loginAt.After(now.Add(time.Minute)) {
		return domain.Trader{}, errors.New("Kite returned a login time in the future")
	}
	if session.AccessToken == "" || !expiresAt.After(loginAt) {
		return domain.Trader{}, errors.New("Kite returned an invalid session")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Trader{}, err
	}
	defer tx.Rollback()
	var purpose string
	var existingTraderID sql.NullInt64
	var signupInvitationID int64
	var clientIDEnc, apiKeyEnc, apiSecretEnc string
	var policyVersion, policySource, policyText string
	var ackMS, expiresMS, consumedMS, completedMS int64
	err = tx.QueryRowContext(ctx, `
SELECT purpose,trader_id,client_id_enc,api_key_enc,api_secret_enc,
       policy_version,policy_source_url,policy_exact_text,policy_acknowledged_at_ms,
       expires_at_ms,consumed_at_ms,completed_at_ms,signup_invitation_id
FROM kite_auth_flows WHERE id=?`, flowID).Scan(&purpose, &existingTraderID, &clientIDEnc, &apiKeyEnc,
		&apiSecretEnc, &policyVersion, &policySource, &policyText, &ackMS, &expiresMS, &consumedMS, &completedMS,
		&signupInvitationID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Trader{}, ErrAuthFlowExpired
	}
	if err != nil {
		return domain.Trader{}, err
	}
	if consumedMS == 0 || completedMS != 0 || expiresMS <= millis(now) {
		return domain.Trader{}, ErrAuthFlowExpired
	}
	clientID, err := s.vault.Decrypt(clientIDEnc)
	if err != nil {
		return domain.Trader{}, err
	}
	apiKey, err := s.vault.Decrypt(apiKeyEnc)
	if err != nil {
		return domain.Trader{}, err
	}
	apiSecret, err := s.vault.Decrypt(apiSecretEnc)
	if err != nil {
		return domain.Trader{}, err
	}
	if normalizeClientID(session.UserID) != normalizeClientID(clientID) || session.APIKey != apiKey {
		return domain.Trader{}, errors.New("Kite login identity does not match this signup")
	}
	accessTokenEnc, err := s.vault.Encrypt(session.AccessToken)
	if err != nil {
		return domain.Trader{}, err
	}
	var traderID int64
	switch purpose {
	case domain.KiteAuthSignup:
		if signupInvitationID <= 0 {
			return domain.Trader{}, ErrSignupInvitationUnavailable
		}
		if policyVersion != domain.HostingPolicyVersion || policySource != domain.HostingPolicySourceURL ||
			policyText != domain.HostingPolicyExactText || ackMS == 0 {
			return domain.Trader{}, ErrPolicyAttestationRequired
		}
		apiSecretStored, err := s.vault.Encrypt(apiSecret)
		if err != nil {
			return domain.Trader{}, err
		}
		result, err := tx.ExecContext(ctx, `
INSERT INTO traders(
 client_id,display_client_id,api_key,api_secret_enc,access_token_enc,verified_at_ms,
 session_login_at_ms,session_expires_at_ms,created_at_ms,updated_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?)`, normalizeClientID(clientID), normalizeClientID(clientID), apiKey, apiSecretStored,
			accessTokenEnc, millis(now), millis(loginAt), millis(expiresAt), millis(now), millis(now))
		if err != nil {
			return domain.Trader{}, fmt.Errorf("%w: client ID or API key is already registered", ErrConflict)
		}
		traderID, _ = result.LastInsertId()
		if _, err := tx.ExecContext(ctx, `
INSERT INTO trader_policy_attestations(
 trader_id,policy_version,source_url,exact_text,acknowledged_at_ms,created_at_ms)
VALUES(?,?,?,?,?,?)`, traderID, policyVersion, policySource, policyText, ackMS, millis(now)); err != nil {
			return domain.Trader{}, err
		}
		// Consumption is in the same transaction as trader creation. Revoking
		// the invitation during Kite verification therefore prevents the new
		// profile, while a successful commit makes the bearer permanently used.
		invitationResult, err := tx.ExecContext(ctx, `
UPDATE signup_invitations
SET consumed_at_ms=?,consumed_by_trader_id=?,reserved_at_ms=0,reserved_until_ms=0,reserved_flow_id=0
WHERE id=? AND consumed_at_ms=0 AND revoked_at_ms=0 AND reserved_flow_id=? AND reserved_until_ms>?`,
			millis(now), traderID, signupInvitationID, flowID, millis(now))
		if err != nil {
			return domain.Trader{}, err
		}
		changed, err := invitationResult.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return domain.Trader{}, err
			}
			return domain.Trader{}, ErrSignupInvitationUnavailable
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO operator_audit_events(actor_type,actor_id,event_type,subject_type,subject_id,detail,created_at_ms)
VALUES('TRADER',?,'SIGNUP_INVITATION_USED','SIGNUP_INVITATION',?,'verified trader consumed a single-use signup invitation',?)`,
			traderID, signupInvitationID, millis(now)); err != nil {
			return domain.Trader{}, err
		}
	case domain.KiteAuthSignin:
		if !existingTraderID.Valid {
			return domain.Trader{}, ErrAuthFlowExpired
		}
		traderID = existingTraderID.Int64
		result, err := tx.ExecContext(ctx, `
UPDATE traders SET access_token_enc=?,verified_at_ms=?,session_login_at_ms=?,
 session_expires_at_ms=?,updated_at_ms=?
WHERE id=? AND deleted_at_ms=0 AND client_id=? COLLATE NOCASE AND api_key=?`, accessTokenEnc,
			millis(now), millis(loginAt), millis(expiresAt), millis(now), traderID, normalizeClientID(clientID), apiKey)
		if err != nil {
			return domain.Trader{}, err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return domain.Trader{}, err
			}
			return domain.Trader{}, errors.New("Kite login identity does not match this trader")
		}
	default:
		return domain.Trader{}, errors.New("invalid Kite login purpose")
	}
	activationDate, err := activationDateForLogin(settings, loginAt, now)
	if err != nil {
		return domain.Trader{}, err
	}
	if activationDate != "" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO trader_daily_activations(
 trader_id,execution_date,login_at_ms,expires_at_ms,access_token_enc,created_at_ms,updated_at_ms)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(trader_id,execution_date) DO UPDATE SET
 login_at_ms=excluded.login_at_ms,expires_at_ms=excluded.expires_at_ms,
 access_token_enc=excluded.access_token_enc,updated_at_ms=excluded.updated_at_ms`, traderID, activationDate,
			millis(loginAt), millis(expiresAt), accessTokenEnc, millis(now), millis(now)); err != nil {
			return domain.Trader{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE traders SET activated_for_date=? WHERE id=?`, activationDate, traderID); err != nil {
			return domain.Trader{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE kite_auth_flows SET completed_at_ms=?,trader_id=?,client_id_enc='',api_key_enc='',api_secret_enc=''
WHERE id=? AND consumed_at_ms<>0 AND completed_at_ms=0`, millis(now), traderID, flowID)
	if err != nil {
		return domain.Trader{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return domain.Trader{}, err
		}
		return domain.Trader{}, ErrAuthFlowExpired
	}
	if err := tx.Commit(); err != nil {
		return domain.Trader{}, err
	}
	return s.TraderByID(ctx, traderID)
}

func activationDateForLogin(settings domain.RuntimeSettings, loginAt, completedAt time.Time) (string, error) {
	localLogin := loginAt.In(indiaLocation)
	localCompletion := completedAt.In(indiaLocation)
	if localCompletion.Weekday() == time.Saturday || localCompletion.Weekday() == time.Sunday {
		return "", nil
	}
	executionDate := localCompletion.Format("2006-01-02")
	if localLogin.Format("2006-01-02") != executionDate {
		return "", nil
	}
	openAt, closeAt, err := DailyActivationWindow(settings, executionDate)
	if err != nil {
		return "", err
	}
	// Both Kite's reported login time and Queueframe's completed verification
	// must be inside the window. A flow begun just before either boundary cannot
	// activate orders when its callback finishes outside the safe interval.
	if localLogin.Before(openAt) || !localLogin.Before(closeAt) ||
		localCompletion.Before(openAt) || !localCompletion.Before(closeAt) {
		return "", nil
	}
	return executionDate, nil
}

// TraderDailyActivation returns credential-free metadata for one date. The
// boolean is false when the trader has no activation row for that date.
func (s *Store) TraderDailyActivation(ctx context.Context, traderID int64, executionDate string) (domain.DailyActivation, bool, error) {
	var activation domain.DailyActivation
	var loginMS, expiresMS int64
	var tokenPresent bool
	err := s.db.QueryRowContext(ctx, `
SELECT trader_id,execution_date,login_at_ms,expires_at_ms,(access_token_enc<>'')
FROM trader_daily_activations WHERE trader_id=? AND execution_date=?`, traderID, executionDate).
		Scan(&activation.TraderID, &activation.ExecutionDate, &loginMS, &expiresMS, &tokenPresent)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DailyActivation{}, false, nil
	}
	if err != nil {
		return domain.DailyActivation{}, false, err
	}
	activation.LoginAt = fromMillis(loginMS)
	activation.ExpiresAt = fromMillis(expiresMS)
	activation.TokenPresent = tokenPresent
	return activation, true, nil
}

// DailyActivationStatuses resolves every trader's activation for one target
// without decrypting or returning any credential material. It is intended for
// administrator summaries and list views.
func (s *Store) DailyActivationStatuses(ctx context.Context, executionDate string, target time.Time) (map[int64]bool, error) {
	date, err := time.ParseInLocation("2006-01-02", executionDate, indiaLocation)
	if err != nil {
		return nil, errors.New("execution date must use YYYY-MM-DD")
	}
	if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		return map[int64]bool{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT trader_id,login_at_ms,expires_at_ms,(access_token_enc<>'')
FROM trader_daily_activations WHERE execution_date=?`, executionDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	statuses := make(map[int64]bool)
	for rows.Next() {
		var activation domain.DailyActivation
		var loginMS, expiresMS int64
		if err := rows.Scan(&activation.TraderID, &loginMS, &expiresMS, &activation.TokenPresent); err != nil {
			return nil, err
		}
		activation.ExecutionDate = executionDate
		activation.LoginAt = fromMillis(loginMS)
		activation.ExpiresAt = fromMillis(expiresMS)
		statuses[activation.TraderID] = activation.ActiveFor(executionDate, target)
	}
	return statuses, rows.Err()
}

func (s *Store) TraderByID(ctx context.Context, traderID int64) (domain.Trader, error) {
	return s.traderFromRow(ctx, s.db.QueryRowContext(ctx, `
SELECT id,client_id,display_client_id,api_key,api_secret_enc,access_token_enc,verified_at_ms,
       session_login_at_ms,session_expires_at_ms,activated_for_date,created_at_ms,updated_at_ms,deleted_at_ms
FROM traders WHERE id=?`, traderID))
}

func (s *Store) TraderByClientID(ctx context.Context, clientID string) (domain.Trader, error) {
	return s.traderFromRow(ctx, s.db.QueryRowContext(ctx, `
SELECT id,client_id,display_client_id,api_key,api_secret_enc,access_token_enc,verified_at_ms,
       session_login_at_ms,session_expires_at_ms,activated_for_date,created_at_ms,updated_at_ms,deleted_at_ms
FROM traders WHERE client_id=? COLLATE NOCASE AND deleted_at_ms=0`, normalizeClientID(clientID)))
}

func (s *Store) traderFromRow(ctx context.Context, row scanner) (domain.Trader, error) {
	var trader domain.Trader
	var apiSecretEnc, accessTokenEnc string
	var verifiedMS, loginMS, expiresMS, createdMS, updatedMS, deletedMS int64
	if err := row.Scan(&trader.ID, &trader.ClientID, &trader.DisplayClientID, &trader.APIKey,
		&apiSecretEnc, &accessTokenEnc, &verifiedMS, &loginMS, &expiresMS, &trader.ActivatedForDate,
		&createdMS, &updatedMS, &deletedMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Trader{}, ErrNotFound
		}
		return domain.Trader{}, err
	}
	var err error
	trader.APISecret, err = s.vault.Decrypt(apiSecretEnc)
	if err != nil {
		return domain.Trader{}, err
	}
	trader.AccessToken, err = s.vault.Decrypt(accessTokenEnc)
	if err != nil {
		return domain.Trader{}, err
	}
	trader.VerifiedAt = fromMillis(verifiedMS)
	trader.SessionLoginAt = fromMillis(loginMS)
	trader.SessionExpiresAt = fromMillis(expiresMS)
	trader.CreatedAt = fromMillis(createdMS)
	trader.UpdatedAt = fromMillis(updatedMS)
	trader.DeletedAt = fromMillis(deletedMS)
	var ackMS int64
	err = s.db.QueryRowContext(ctx, `
SELECT policy_version,source_url,exact_text,acknowledged_at_ms
FROM trader_policy_attestations WHERE trader_id=? ORDER BY acknowledged_at_ms DESC,id DESC LIMIT 1`, trader.ID).
		Scan(&trader.PolicyAttestation.Version, &trader.PolicyAttestation.SourceURL,
			&trader.PolicyAttestation.Text, &ackMS)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.Trader{}, err
	}
	trader.PolicyAttestation.AcknowledgedAt = fromMillis(ackMS)
	return trader, nil
}

func (s *Store) ListTraders(ctx context.Context, includeDeleted bool) ([]domain.Trader, error) {
	query := `
SELECT id,client_id,display_client_id,api_key,api_secret_enc,access_token_enc,verified_at_ms,
       session_login_at_ms,session_expires_at_ms,activated_for_date,created_at_ms,updated_at_ms,deleted_at_ms
FROM traders`
	if !includeDeleted {
		query += ` WHERE deleted_at_ms=0`
	}
	query += ` ORDER BY display_client_id,id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var traders []domain.Trader
	for rows.Next() {
		var trader domain.Trader
		var secretEnc, tokenEnc string
		var verifiedMS, loginMS, expiresMS, createdMS, updatedMS, deletedMS int64
		if err := rows.Scan(&trader.ID, &trader.ClientID, &trader.DisplayClientID, &trader.APIKey,
			&secretEnc, &tokenEnc, &verifiedMS, &loginMS, &expiresMS, &trader.ActivatedForDate,
			&createdMS, &updatedMS, &deletedMS); err != nil {
			return nil, err
		}
		trader.VerifiedAt, trader.SessionLoginAt, trader.SessionExpiresAt = fromMillis(verifiedMS), fromMillis(loginMS), fromMillis(expiresMS)
		trader.CreatedAt, trader.UpdatedAt, trader.DeletedAt = fromMillis(createdMS), fromMillis(updatedMS), fromMillis(deletedMS)
		// Lists never expose credentials. Scheduler-specific candidate loading is
		// the only bulk API that decrypts them.
		traders = append(traders, trader)
	}
	return traders, rows.Err()
}

func (s *Store) CreateTraderWebSession(ctx context.Context, traderID int64, ttl time.Duration, now time.Time) (string, error) {
	if traderID <= 0 || ttl <= 0 {
		return "", errors.New("invalid trader session")
	}
	raw, hash, err := secure.RandomToken(32)
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO trader_web_sessions(token_hash,trader_id,expires_at_ms,created_at_ms)
SELECT ?,id,?,? FROM traders WHERE id=? AND deleted_at_ms=0`, hash, millis(now.Add(ttl)), millis(now), traderID)
	if err != nil {
		return "", err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return "", err
		}
		return "", ErrNotFound
	}
	return raw, nil
}

func (s *Store) TraderByWebSession(ctx context.Context, rawToken string, now time.Time) (domain.Trader, error) {
	var traderID int64
	err := s.db.QueryRowContext(ctx, `
SELECT s.trader_id FROM trader_web_sessions s JOIN traders t ON t.id=s.trader_id
WHERE s.token_hash=? AND s.expires_at_ms>? AND t.deleted_at_ms=0`, secure.TokenHash(rawToken), millis(now)).Scan(&traderID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Trader{}, errors.New("session expired")
	}
	if err != nil {
		return domain.Trader{}, err
	}
	return s.TraderByID(ctx, traderID)
}

func (s *Store) DeleteTraderWebSession(ctx context.Context, rawToken string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM trader_web_sessions WHERE token_hash=?`, secure.TokenHash(rawToken))
	return err
}

func (s *Store) DeleteTrader(ctx context.Context, traderID int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var displayID string
	var deletedMS int64
	if err := tx.QueryRowContext(ctx, `SELECT display_client_id,deleted_at_ms FROM traders WHERE id=?`, traderID).Scan(&displayID, &deletedMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if deletedMS != 0 {
		return ErrTraderDeleted
	}
	var unsafe int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(
 SELECT 1 FROM queued_orders q
 LEFT JOIN queued_date_freezes f ON f.execution_date=q.execution_date
 WHERE q.trader_id=? AND q.deleted_at_ms=0 AND
 (q.state IN ('PREPARED','DISPATCHING') OR
  (q.state='SCHEDULED' AND f.execution_date IS NOT NULL))
)`, traderID).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe != 0 {
		return errors.New("trader has a frozen or nonterminal dispatch")
	}
	dateRows, err := tx.QueryContext(ctx, `
SELECT DISTINCT execution_date FROM queued_orders
WHERE trader_id=? AND deleted_at_ms=0 AND state=?`, traderID, domain.OrderStateScheduled)
	if err != nil {
		return err
	}
	var scheduledDates []string
	for dateRows.Next() {
		var date string
		if err := dateRows.Scan(&date); err != nil {
			dateRows.Close()
			return err
		}
		scheduledDates = append(scheduledDates, date)
	}
	if err := dateRows.Err(); err != nil {
		dateRows.Close()
		return err
	}
	if err := dateRows.Close(); err != nil {
		return err
	}
	for _, executionDate := range scheduledDates {
		settings, err := settingsForDateQuery(tx.QueryRowContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
 freeze_lead_ms,batch_interval_ms,max_orders_per_trader,COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions WHERE effective_date<=?
ORDER BY effective_date DESC,version DESC LIMIT 1`, executionDate))
		if err != nil {
			return err
		}
		if err := ensureQueueMutable(ctx, tx, executionDate, settings, now); err != nil {
			return errors.New("trader has an order queue at or past its preparation cutoff")
		}
	}
	redacted := redactClientID(displayID)
	if _, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET state=?,position=-id,deleted_at_ms=?,updated_at_ms=?
WHERE trader_id=? AND deleted_at_ms=0 AND state=?`, domain.OrderStateDeleted, millis(now), millis(now), traderID, domain.OrderStateScheduled); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trader_web_sessions WHERE trader_id=?`, traderID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kite_auth_flows WHERE trader_id=?`, traderID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trader_daily_activations WHERE trader_id=?`, traderID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE traders SET client_id=?,display_client_id=?,api_key=?,api_secret_enc='',access_token_enc='',
 session_login_at_ms=0,session_expires_at_ms=0,activated_for_date='',updated_at_ms=?,deleted_at_ms=?
WHERE id=? AND deleted_at_ms=0`, fmt.Sprintf("DELETED-%d", traderID), redacted,
		fmt.Sprintf("DELETED-%d", traderID), millis(now), millis(now), traderID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	return tx.Commit()
}

func redactClientID(clientID string) string {
	runes := []rune(strings.TrimSpace(clientID))
	if len(runes) <= 2 {
		return "••"
	}
	if len(runes) <= 4 {
		return string(runes[0]) + "••" + string(runes[len(runes)-1])
	}
	return string(runes[:2]) + strings.Repeat("•", len(runes)-4) + string(runes[len(runes)-2:])
}

func (s *Store) PurgeExpiredKiteAuthFlows(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kite_auth_flows WHERE expires_at_ms<=? OR completed_at_ms<>0`, millis(now))
	return err
}

// AbandonKiteAuthFlow cryptographically erases a consumed flow when broker or
// profile validation fails. A signup reservation is released only when this
// exact flow still owns it, allowing the same family member to retry without
// ever permitting two concurrent signup flows.
func (s *Store) AbandonKiteAuthFlow(ctx context.Context, flowID int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var invitationID int64
	if err := tx.QueryRowContext(ctx, `SELECT signup_invitation_id FROM kite_auth_flows WHERE id=?`, flowID).Scan(&invitationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAuthFlowExpired
		}
		return err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE kite_auth_flows
SET client_id_enc='',api_key_enc='',api_secret_enc='',completed_at_ms=?
WHERE id=? AND completed_at_ms=0`, millis(now), flowID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrAuthFlowExpired
	}
	if invitationID > 0 {
		if _, err := tx.ExecContext(ctx, `
UPDATE signup_invitations
SET reserved_at_ms=0,reserved_until_ms=0,reserved_flow_id=0
WHERE id=? AND reserved_flow_id=? AND consumed_at_ms=0 AND revoked_at_ms=0`, invitationID, flowID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
