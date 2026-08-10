package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

type Candidate struct {
	Intent  domain.OrderIntent
	Account domain.Account
}

func (s *Store) CreateOrder(ctx context.Context, userID int64, intent domain.OrderIntent) (domain.OrderIntent, error) {
	if _, err := s.AccountForUser(ctx, userID, intent.AccountID); err != nil {
		return domain.OrderIntent{}, errors.New("account not found or not permitted")
	}
	if intent.Tag == "" {
		var err error
		intent.Tag, err = newOrderTag()
		if err != nil {
			return domain.OrderIntent{}, err
		}
	}
	now := time.Now()
	intent.OwnerUserID = userID
	intent.State = domain.OrderStateDraft
	intent.CreatedAt = now
	intent.UpdatedAt = now
	result, err := s.db.ExecContext(ctx, `
INSERT INTO order_intents(
 owner_user_id,account_id,execution_date,priority,tag,state,exchange,trading_symbol,transaction_type,
 product,order_type,quantity,disclosed_quantity,price,trigger_price,validity,validity_ttl,
 market_protection,market_protection_text,autoslice,created_at_ms,updated_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		intent.OwnerUserID, intent.AccountID, intent.ExecutionDate, intent.Priority, intent.Tag, intent.State,
		intent.Params.Exchange, intent.Params.TradingSymbol, intent.Params.TransactionType, intent.Params.Product,
		intent.Params.OrderType, intent.Params.Quantity, intent.Params.DisclosedQuantity, intent.Params.Price,
		intent.Params.TriggerPrice, intent.Params.Validity, intent.Params.ValidityTTL, 0, intent.Params.MarketProtection,
		intent.Params.Autoslice, millis(now), millis(now))
	if err != nil {
		return domain.OrderIntent{}, err
	}
	intent.ID, _ = result.LastInsertId()
	return intent, nil
}

func newOrderTag() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return "SO" + encoded[:18], nil
}

func (s *Store) ListOrders(ctx context.Context, userID int64, date string) ([]domain.OrderIntent, error) {
	query := `
SELECT o.id,o.owner_user_id,o.account_id,o.execution_date,o.priority,o.tag,o.state,
       o.exchange,o.trading_symbol,o.transaction_type,o.product,o.order_type,o.quantity,
       o.disclosed_quantity,o.price,o.trigger_price,o.validity,o.validity_ttl,o.market_protection_text,
       o.autoslice,o.live_confirmed,o.created_at_ms,o.updated_at_ms,o.armed_at_ms
FROM order_intents o
WHERE o.owner_user_id=?`
	args := []any{userID}
	if date != "" {
		query += " AND o.execution_date=?"
		args = append(args, date)
	}
	query += " ORDER BY o.execution_date DESC,o.priority,o.created_at_ms"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []domain.OrderIntent
	for rows.Next() {
		intent, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *Store) OrderForUser(ctx context.Context, userID, intentID int64) (domain.OrderIntent, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT o.id,o.owner_user_id,o.account_id,o.execution_date,o.priority,o.tag,o.state,
       o.exchange,o.trading_symbol,o.transaction_type,o.product,o.order_type,o.quantity,
       o.disclosed_quantity,o.price,o.trigger_price,o.validity,o.validity_ttl,o.market_protection_text,
       o.autoslice,o.live_confirmed,o.created_at_ms,o.updated_at_ms,o.armed_at_ms
FROM order_intents o
WHERE o.owner_user_id=? AND o.id=?`, userID, intentID)
	return scanIntent(row)
}

func scanIntent(row scanner) (domain.OrderIntent, error) {
	var intent domain.OrderIntent
	var autoslice, liveConfirmed int
	var created, updated, armed int64
	err := row.Scan(&intent.ID, &intent.OwnerUserID, &intent.AccountID, &intent.ExecutionDate, &intent.Priority,
		&intent.Tag, &intent.State, &intent.Params.Exchange, &intent.Params.TradingSymbol,
		&intent.Params.TransactionType, &intent.Params.Product, &intent.Params.OrderType, &intent.Params.Quantity,
		&intent.Params.DisclosedQuantity, &intent.Params.Price, &intent.Params.TriggerPrice, &intent.Params.Validity,
		&intent.Params.ValidityTTL, &intent.Params.MarketProtection, &autoslice, &liveConfirmed, &created, &updated, &armed)
	if err != nil {
		return domain.OrderIntent{}, err
	}
	intent.Params.Autoslice = autoslice != 0
	intent.LiveConfirmed = liveConfirmed != 0
	intent.CreatedAt = fromMillis(created)
	intent.UpdatedAt = fromMillis(updated)
	intent.ArmedAt = fromMillis(armed)
	return intent, nil
}

func (s *Store) SetOrderArmed(ctx context.Context, userID, intentID int64, armed bool, armDeadline time.Time) error {
	return s.SetOrderArmedMode(ctx, userID, intentID, armed, armDeadline, false)
}

// SetOrderArmedMode atomically binds an arm action to the deployment mode the
// user confirmed. A later live process must not send an order armed only for a
// dry run.
func (s *Store) SetOrderArmedMode(ctx context.Context, userID, intentID int64, armed bool, armDeadline time.Time, liveConfirmed bool) error {
	from, to := domain.OrderStateDraft, domain.OrderStateArmed
	var armedAt int64
	if !armed {
		from, to = domain.OrderStateArmed, domain.OrderStateDraft
		liveConfirmed = false
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, date string
	var accountID int64
	err = tx.QueryRowContext(ctx, `
SELECT o.state,o.execution_date,o.account_id FROM order_intents o
WHERE o.id=? AND o.owner_user_id=?`, intentID, userID).Scan(&state, &date, &accountID)
	if err != nil {
		return err
	}
	if state != from {
		return fmt.Errorf("order must be %s", strings.ToLower(from))
	}
	now := time.Now()
	if armed {
		armedAt = millis(now)
	}
	if armed {
		if armDeadline.IsZero() || !now.Before(armDeadline) {
			return errors.New("the preparation window for this date has started")
		}
		var frozen int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM schedule_freezes WHERE execution_date=?`, date).Scan(&frozen)
		if err == nil {
			return errors.New("the scheduler has frozen this date's batch")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var armedCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM order_intents WHERE account_id=? AND execution_date=? AND state IN (?,?)`, accountID, date, domain.OrderStateArmed, domain.OrderStatePrepared).Scan(&armedCount); err != nil {
			return err
		}
		if armedCount >= 10 {
			return errors.New("this account already has 10 armed orders for that date")
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE order_intents SET state=?,armed_at_ms=?,live_confirmed=?,updated_at_ms=? WHERE id=? AND state=?`, to, armedAt, liveConfirmed, millis(now), intentID, from)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return fmt.Errorf("order must be %s", strings.ToLower(from))
	}
	return tx.Commit()
}

// FreezeDate serializes with SetOrderArmed. Once it commits, the scheduler's
// following snapshot cannot miss an order that was successfully armed.
func (s *Store) FreezeDate(ctx context.Context, date string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_freezes(execution_date,frozen_at_ms) VALUES(?,?) ON CONFLICT(execution_date) DO NOTHING`, date, millis(time.Now()))
	return err
}

func (s *Store) DeleteDraftOrder(ctx context.Context, userID, intentID int64) error {
	if _, err := s.OrderForUser(ctx, userID, intentID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM order_intents WHERE id=? AND state=?`, intentID, domain.OrderStateDraft)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return errors.New("only draft orders can be deleted")
	}
	return nil
}

func (s *Store) ArmedCandidates(ctx context.Context, date string) ([]Candidate, error) {
	return s.candidatesInState(ctx, date, domain.OrderStateArmed)
}

// UnknownCandidates loads the immutable order payload and account binding for
// attempts whose placement result was not durably recorded. The scheduler uses
// these only for orderbook reconciliation; they must never be submitted again.
func (s *Store) UnknownCandidates(ctx context.Context, date string) ([]Candidate, error) {
	return s.candidatesInState(ctx, date, domain.OrderStateUnknown)
}

// StaleNonterminalDates returns past dates that still contain work a previous
// process never finalized. Kite's order book is trading-day scoped, so the
// scheduler marks these visibly UNKNOWN/MISSED rather than resubmitting them.
func (s *Store) StaleNonterminalDates(ctx context.Context, beforeDate string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT execution_date FROM order_intents
WHERE execution_date<? AND state IN (?,?) ORDER BY execution_date`,
		beforeDate, domain.OrderStateArmed, domain.OrderStatePrepared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dates []string
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			return nil, err
		}
		dates = append(dates, date)
	}
	return dates, rows.Err()
}

func (s *Store) candidatesInState(ctx context.Context, date, state string) ([]Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT o.id,o.owner_user_id,o.account_id,o.execution_date,o.priority,o.tag,o.state,
       o.exchange,o.trading_symbol,o.transaction_type,o.product,o.order_type,o.quantity,
       o.disclosed_quantity,o.price,o.trigger_price,o.validity,o.validity_ttl,o.market_protection_text,
       o.autoslice,o.live_confirmed,o.created_at_ms,o.updated_at_ms,o.armed_at_ms,
       a.id,a.owner_user_id,a.name,a.client_id,a.api_key,a.api_secret_enc,a.access_token_enc,
       a.verified,a.session_login_at_ms,a.session_expires_at_ms,a.created_at_ms,a.updated_at_ms
FROM order_intents o JOIN broker_accounts a ON a.id=o.account_id
WHERE o.execution_date=? AND o.state=?
ORDER BY a.id,o.priority,o.created_at_ms`, date, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []Candidate
	for rows.Next() {
		var candidate Candidate
		var autoslice, liveConfirmed, verified int
		var orderCreated, orderUpdated, armedAt, loginAt, expiresAt, accountCreated, accountUpdated int64
		var secret, token string
		err := rows.Scan(
			&candidate.Intent.ID, &candidate.Intent.OwnerUserID, &candidate.Intent.AccountID,
			&candidate.Intent.ExecutionDate, &candidate.Intent.Priority, &candidate.Intent.Tag, &candidate.Intent.State,
			&candidate.Intent.Params.Exchange, &candidate.Intent.Params.TradingSymbol,
			&candidate.Intent.Params.TransactionType, &candidate.Intent.Params.Product, &candidate.Intent.Params.OrderType,
			&candidate.Intent.Params.Quantity, &candidate.Intent.Params.DisclosedQuantity, &candidate.Intent.Params.Price,
			&candidate.Intent.Params.TriggerPrice, &candidate.Intent.Params.Validity, &candidate.Intent.Params.ValidityTTL,
			&candidate.Intent.Params.MarketProtection, &autoslice, &liveConfirmed, &orderCreated, &orderUpdated, &armedAt,
			&candidate.Account.ID, &candidate.Account.OwnerUserID, &candidate.Account.Name, &candidate.Account.ClientID,
			&candidate.Account.APIKey, &secret, &token, &verified, &loginAt, &expiresAt, &accountCreated, &accountUpdated)
		if err != nil {
			return nil, err
		}
		candidate.Intent.Params.Autoslice = autoslice != 0
		candidate.Intent.LiveConfirmed = liveConfirmed != 0
		candidate.Intent.CreatedAt, candidate.Intent.UpdatedAt, candidate.Intent.ArmedAt = fromMillis(orderCreated), fromMillis(orderUpdated), fromMillis(armedAt)
		candidate.Account.APISecret, err = s.vault.Decrypt(secret)
		if err != nil {
			return nil, err
		}
		candidate.Account.AccessToken, err = s.vault.Decrypt(token)
		if err != nil {
			return nil, err
		}
		candidate.Account.Verified = verified != 0
		candidate.Account.SessionLoginAt, candidate.Account.SessionExpiresAt = fromMillis(loginAt), fromMillis(expiresAt)
		candidate.Account.CreatedAt, candidate.Account.UpdatedAt = fromMillis(accountCreated), fromMillis(accountUpdated)
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// PrepareOrders makes the attempt durable before the dispatcher loads the jobs
// into its hot-path memory. It returns the IDs successfully claimed from ARMED.
func (s *Store) PrepareOrders(ctx context.Context, ids []int64, target time.Time) (map[int64]bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	prepared := make(map[int64]bool, len(ids))
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `UPDATE order_intents SET state=?,updated_at_ms=? WHERE id=? AND state=?`, domain.OrderStatePrepared, millis(time.Now()), id, domain.OrderStateArmed)
		if err != nil {
			return nil, err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dispatch_attempts(intent_id,attempt,target_at_ms) VALUES(?,1,?)`, id, millis(target)); err != nil {
			return nil, err
		}
		prepared[id] = true
	}
	return prepared, tx.Commit()
}

func (s *Store) RejectPreflight(ctx context.Context, intentID int64, errorType, message string, target time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE order_intents SET state=?,updated_at_ms=? WHERE id=? AND state=?`, domain.OrderStateRejected, millis(time.Now()), intentID, domain.OrderStateArmed)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed == 1 {
		_, err = tx.ExecContext(ctx, `INSERT INTO dispatch_attempts(intent_id,attempt,target_at_ms,outcome,error_type,error_message) VALUES(?,1,?,'REJECTED',?,?)`, intentID, millis(target), errorType, message)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RejectUnconfirmedLive fails closed when a process is switched from dry-run
// to live after an order was armed. Confirmation is stored in the same
// transaction as the arm action, so an audit-write failure cannot bypass it.
func (s *Store) RejectUnconfirmedLive(ctx context.Context, date string, target time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM order_intents WHERE execution_date=? AND state=? AND live_confirmed=0`, date, domain.OrderStateArmed)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `UPDATE order_intents SET state=?,updated_at_ms=? WHERE id=? AND state=? AND live_confirmed=0`, domain.OrderStateRejected, millis(time.Now()), id, domain.OrderStateArmed)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("unconfirmed live intent %d changed during rejection", id)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dispatch_attempts(intent_id,attempt,target_at_ms,outcome,error_type,error_message) VALUES(?,1,?,'REJECTED','LiveConfirmation','order was armed in dry-run mode and was not confirmed for live placement')`, id, millis(target)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ApplyDispatchResult(ctx context.Context, result domain.DispatchResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyDispatchResult(ctx, tx, result); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyDispatchResults flushes a complete in-memory batch only after every
// order call has finished. No SQLite activity is needed while the hot path is
// running.
func (s *Store) ApplyDispatchResults(ctx context.Context, results []domain.DispatchResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, result := range results {
		if err := applyDispatchResult(ctx, tx, result); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func applyDispatchResult(ctx context.Context, tx *sql.Tx, result domain.DispatchResult) error {
	state := result.Outcome
	if state == "RATE_LIMITED" {
		state = domain.OrderStateRejected
	}
	var startedAtNS, finishedAtNS int64
	if !result.StartedAt.IsZero() {
		startedAtNS = result.StartedAt.UnixNano()
	}
	if !result.FinishedAt.IsZero() {
		finishedAtNS = result.FinishedAt.UnixNano()
	}
	attemptUpdate, err := tx.ExecContext(ctx, `
UPDATE dispatch_attempts SET request_count=?,started_at_ns=?,finished_at_ns=?,order_timestamp_ms=?,exchange_timestamp_ms=?,exchange_update_timestamp_ms=?,outcome=?,order_id=?,exchange_order_id=?,broker_status=?,child_orders=?,error_type=?,error_message=?
WHERE intent_id=? AND attempt=?`, result.RequestCount, startedAtNS, finishedAtNS, millis(result.OrderTimestamp), millis(result.ExchangeTimestamp), millis(result.ExchangeUpdateTimestamp), result.Outcome,
		result.OrderID, result.ExchangeOrderID, result.BrokerStatus, result.ChildOrders, result.ErrorType, result.ErrorMessage, result.IntentID, result.Attempt)
	if err != nil {
		return err
	}
	changed, err := attemptUpdate.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("dispatch attempt %d for intent %d was not found", result.Attempt, result.IntentID)
	}
	intentUpdate, err := tx.ExecContext(ctx, `
UPDATE order_intents SET state=?,updated_at_ms=?
WHERE id=? AND (state IN (?,?,?) OR state=?)`, state, millis(time.Now()), result.IntentID,
		domain.OrderStatePrepared, domain.OrderStateUnknown, domain.OrderStateAcknowledged, state)
	if err != nil {
		return err
	}
	changed, err = intentUpdate.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("intent %d was not found while applying dispatch result", result.IntentID)
	}
	return nil
}

// RecoverPrepared handles a previous process that stopped after durable
// preparation. Before the target, it can safely return unsent work to ARMED;
// at or after target, it preserves the possibility that a POST succeeded and
// marks the outcome UNKNOWN instead of resending.
func (s *Store) RecoverPrepared(ctx context.Context, date string, target, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if now.Before(target) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM dispatch_attempts WHERE intent_id IN (SELECT id FROM order_intents WHERE execution_date=? AND state=?) AND outcome='PREPARED' AND started_at_ns=0`, date, domain.OrderStatePrepared); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE order_intents SET state=?,updated_at_ms=? WHERE execution_date=? AND state=?`, domain.OrderStateArmed, millis(now), date, domain.OrderStatePrepared); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE dispatch_attempts SET outcome='UNKNOWN',error_type='Restart',error_message='process stopped before in-memory results were persisted' WHERE intent_id IN (SELECT id FROM order_intents WHERE execution_date=? AND state=?) AND outcome='PREPARED'`, date, domain.OrderStatePrepared); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE order_intents SET state=?,updated_at_ms=? WHERE execution_date=? AND state=?`, domain.OrderStateUnknown, millis(now), date, domain.OrderStatePrepared); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) MarkMissedPrepared(ctx context.Context, date string, target time.Time, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM order_intents WHERE execution_date=? AND state IN (?,?)`, date, domain.OrderStateArmed, domain.OrderStatePrepared)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE order_intents SET state=?,updated_at_ms=? WHERE id=?`, domain.OrderStateMissed, millis(time.Now()), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dispatch_attempts(intent_id,attempt,target_at_ms,outcome,error_type,error_message) VALUES(?,1,?,'MISSED','Deadline',?)`, id, millis(target), reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AttemptResults(ctx context.Context, userID int64, date string) ([]domain.DispatchResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.intent_id,o.account_id,o.tag,d.attempt,d.request_count,d.target_at_ms,d.started_at_ns,d.finished_at_ns,
       d.order_timestamp_ms,d.exchange_timestamp_ms,d.exchange_update_timestamp_ms,d.outcome,
       d.order_id,d.exchange_order_id,d.broker_status,d.child_orders,d.error_type,d.error_message
FROM dispatch_attempts d JOIN order_intents o ON o.id=d.intent_id
WHERE o.owner_user_id=? AND o.execution_date=? ORDER BY o.priority,d.intent_id,d.attempt`, userID, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDispatchResults(rows)
}

// UnknownAttemptResults is scheduler-scoped: unlike AttemptResults it is not
// a user-facing authorization query. It pairs with UnknownCandidates so a
// restarted process can query Kite's orderbook without ever resubmitting the
// original request.
func (s *Store) UnknownAttemptResults(ctx context.Context, date string) ([]domain.DispatchResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.intent_id,o.account_id,o.tag,d.attempt,d.request_count,d.target_at_ms,d.started_at_ns,d.finished_at_ns,
       d.order_timestamp_ms,d.exchange_timestamp_ms,d.exchange_update_timestamp_ms,d.outcome,
       d.order_id,d.exchange_order_id,d.broker_status,d.child_orders,d.error_type,d.error_message
FROM dispatch_attempts d JOIN order_intents o ON o.id=d.intent_id
WHERE o.execution_date=? AND o.state=? AND d.outcome=?
ORDER BY o.priority,d.intent_id,d.attempt`, date, domain.OrderStateUnknown, domain.OrderStateUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDispatchResults(rows)
}

func scanDispatchResults(rows *sql.Rows) ([]domain.DispatchResult, error) {
	var results []domain.DispatchResult
	for rows.Next() {
		var result domain.DispatchResult
		var targetMS, startedNS, finishedNS, orderTimestampMS, exchangeTimestampMS, exchangeUpdateTimestampMS int64
		if err := rows.Scan(&result.IntentID, &result.AccountID, &result.Tag, &result.Attempt, &result.RequestCount, &targetMS, &startedNS,
			&finishedNS, &orderTimestampMS, &exchangeTimestampMS, &exchangeUpdateTimestampMS, &result.Outcome,
			&result.OrderID, &result.ExchangeOrderID, &result.BrokerStatus, &result.ChildOrders,
			&result.ErrorType, &result.ErrorMessage); err != nil {
			return nil, err
		}
		result.TargetAt = fromMillis(targetMS)
		if startedNS != 0 {
			result.StartedAt = time.Unix(0, startedNS)
		}
		if finishedNS != 0 {
			result.FinishedAt = time.Unix(0, finishedNS)
		}
		result.OrderTimestamp = fromMillis(orderTimestampMS)
		result.ExchangeTimestamp = fromMillis(exchangeTimestampMS)
		result.ExchangeUpdateTimestamp = fromMillis(exchangeUpdateTimestampMS)
		results = append(results, result)
	}
	return results, rows.Err()
}
