package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/secure"
)

const (
	MinSignupInvitationTTL = time.Hour
	MaxSignupInvitationTTL = 30 * 24 * time.Hour
)

// CreateSignupInvitation returns the high-entropy bearer exactly once. Only
// its SHA-256 hash is stored, so a database reader cannot reconstruct a link.
func (s *Store) CreateSignupInvitation(ctx context.Context, adminID int64, ttl time.Duration, now time.Time) (domain.SignupInvitation, string, error) {
	if adminID <= 0 {
		return domain.SignupInvitation{}, "", errors.New("invalid administrator")
	}
	if ttl < MinSignupInvitationTTL || ttl > MaxSignupInvitationTTL {
		return domain.SignupInvitation{}, "", errors.New("invitation lifetime must be between 1 hour and 30 days")
	}
	raw, hash, err := secure.RandomToken(32)
	if err != nil {
		return domain.SignupInvitation{}, "", err
	}
	invitation := domain.SignupInvitation{
		CreatedByAdminID: adminID,
		ExpiresAt:        now.Add(ttl),
		CreatedAt:        now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SignupInvitation{}, "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO signup_invitations(token_hash,created_by_admin_id,expires_at_ms,created_at_ms)
SELECT ?,id,?,? FROM admins WHERE id=?`, hash, millis(invitation.ExpiresAt), millis(now), adminID)
	if err != nil {
		return domain.SignupInvitation{}, "", err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.SignupInvitation{}, "", err
	}
	if changed != 1 {
		return domain.SignupInvitation{}, "", ErrNotFound
	}
	invitation.ID, _ = result.LastInsertId()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO operator_audit_events(actor_type,actor_id,event_type,subject_type,subject_id,detail,created_at_ms)
VALUES('ADMIN',?,'SIGNUP_INVITATION_CREATED','SIGNUP_INVITATION',?,'administrator created a single-use signup invitation',?)`,
		adminID, invitation.ID, millis(now)); err != nil {
		return domain.SignupInvitation{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return domain.SignupInvitation{}, "", err
	}
	return invitation, raw, nil
}

// AvailableSignupInvitation verifies a bearer without reserving it. It is
// used only to exchange a link for the short-lived HttpOnly browser cookie;
// BeginTraderSignup performs the authoritative atomic reservation.
func (s *Store) AvailableSignupInvitation(ctx context.Context, rawToken string, now time.Time) (domain.SignupInvitation, error) {
	rawToken = strings.TrimSpace(rawToken)
	if !validSignupInvitationToken(rawToken) {
		return domain.SignupInvitation{}, ErrSignupInvitationUnavailable
	}
	row := s.db.QueryRowContext(ctx, `
SELECT i.id,i.created_by_admin_id,COALESCE(a.username,'Deleted administrator'),i.expires_at_ms,i.reserved_at_ms,
       i.reserved_until_ms,i.consumed_at_ms,COALESCE(i.consumed_by_trader_id,0),
       COALESCE(t.display_client_id,''),i.revoked_at_ms,COALESCE(i.revoked_by_admin_id,0),
       COALESCE(ra.username,''),i.created_at_ms
FROM signup_invitations i
LEFT JOIN admins a ON a.id=i.created_by_admin_id
LEFT JOIN admins ra ON ra.id=i.revoked_by_admin_id
LEFT JOIN traders t ON t.id=i.consumed_by_trader_id
WHERE i.token_hash=? AND i.expires_at_ms>? AND i.consumed_at_ms=0 AND i.revoked_at_ms=0
  AND (i.reserved_until_ms=0 OR i.reserved_until_ms<=?)`, secure.TokenHash(rawToken), millis(now), millis(now))
	return scanSignupInvitation(row)
}

func validSignupInvitationToken(rawToken string) bool {
	if len(rawToken) != 43 { // Raw URL encoding of exactly 32 random bytes.
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rawToken)
	return err == nil && len(decoded) == 32
}

func (s *Store) ListSignupInvitations(ctx context.Context) ([]domain.SignupInvitation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT i.id,i.created_by_admin_id,COALESCE(a.username,'Deleted administrator'),i.expires_at_ms,i.reserved_at_ms,
       i.reserved_until_ms,i.consumed_at_ms,COALESCE(i.consumed_by_trader_id,0),
       COALESCE(t.display_client_id,''),i.revoked_at_ms,COALESCE(i.revoked_by_admin_id,0),
       COALESCE(ra.username,''),i.created_at_ms
FROM signup_invitations i
LEFT JOIN admins a ON a.id=i.created_by_admin_id
LEFT JOIN admins ra ON ra.id=i.revoked_by_admin_id
LEFT JOIN traders t ON t.id=i.consumed_by_trader_id
ORDER BY i.created_at_ms DESC,i.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var invitations []domain.SignupInvitation
	for rows.Next() {
		invitation, err := scanSignupInvitation(rows)
		if err != nil {
			return nil, err
		}
		invitations = append(invitations, invitation)
	}
	return invitations, rows.Err()
}

func scanSignupInvitation(row scanner) (domain.SignupInvitation, error) {
	var invitation domain.SignupInvitation
	var expiresMS, reservedMS, reservedUntilMS, consumedMS, revokedMS, createdMS int64
	if err := row.Scan(&invitation.ID, &invitation.CreatedByAdminID, &invitation.CreatedByUsername,
		&expiresMS, &reservedMS, &reservedUntilMS, &consumedMS, &invitation.ConsumedByTraderID,
		&invitation.ConsumedByClientID, &revokedMS, &invitation.RevokedByAdminID,
		&invitation.RevokedByUsername, &createdMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.SignupInvitation{}, ErrSignupInvitationUnavailable
		}
		return domain.SignupInvitation{}, err
	}
	invitation.ExpiresAt = fromMillis(expiresMS)
	invitation.ReservedAt = fromMillis(reservedMS)
	invitation.ReservedUntil = fromMillis(reservedUntilMS)
	invitation.ConsumedAt = fromMillis(consumedMS)
	invitation.RevokedAt = fromMillis(revokedMS)
	invitation.CreatedAt = fromMillis(createdMS)
	return invitation, nil
}

// RevokeSignupInvitation invalidates an unused invitation, including a
// currently reserved flow. That flow may finish talking to Kite, but its
// completion transaction cannot create a trader after revocation.
func (s *Store) RevokeSignupInvitation(ctx context.Context, invitationID, adminID int64, now time.Time) error {
	if invitationID <= 0 || adminID <= 0 {
		return ErrSignupInvitationUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE signup_invitations
SET revoked_at_ms=?,revoked_by_admin_id=?
WHERE id=? AND consumed_at_ms=0 AND revoked_at_ms=0
  AND EXISTS(SELECT 1 FROM admins WHERE id=?)`, millis(now), adminID, invitationID, adminID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrSignupInvitationUnavailable
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO operator_audit_events(actor_type,actor_id,event_type,subject_type,subject_id,detail,created_at_ms)
VALUES('ADMIN',?,'SIGNUP_INVITATION_REVOKED','SIGNUP_INVITATION',?,'administrator revoked an unused signup invitation',?)`,
		adminID, invitationID, millis(now)); err != nil {
		return err
	}
	return tx.Commit()
}
