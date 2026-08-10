package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/abinthomasonline/queueframe/internal/secure"
)

type Store struct {
	db                *sql.DB
	vault             *secure.Vault
	dummyPasswordHash string
}

func Open(path string, vault *secure.Vault) (*Store, error) {
	directory := filepath.Dir(path)
	_, statErr := os.Stat(directory)
	directoryExisted := statErr == nil
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if !directoryExisted {
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("protect database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("protect database file: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	dummyPasswordHash, err := secure.HashPassword("dummy-password-used-only-for-timing")
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, vault: vault, dummyPasswordHash: dummyPasswordHash}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL COLLATE NOCASE UNIQUE,
  password_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  must_change_password INTEGER NOT NULL DEFAULT 0,
  created_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS web_sessions (
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at_ms INTEGER NOT NULL,
  created_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS broker_accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_user_id INTEGER NOT NULL REFERENCES users(id),
  name TEXT NOT NULL,
  client_id TEXT NOT NULL COLLATE NOCASE UNIQUE,
  api_key TEXT NOT NULL UNIQUE,
  api_secret_enc TEXT NOT NULL,
  access_token_enc TEXT NOT NULL DEFAULT '',
  verified INTEGER NOT NULL DEFAULT 0,
  session_login_at_ms INTEGER NOT NULL DEFAULT 0,
  session_expires_at_ms INTEGER NOT NULL DEFAULT 0,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS account_memberships (
  account_id INTEGER NOT NULL REFERENCES broker_accounts(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  can_trade INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(account_id, user_id)
);
CREATE TABLE IF NOT EXISTS login_nonces (
  state_hash TEXT PRIMARY KEY,
  account_id INTEGER NOT NULL REFERENCES broker_accounts(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at_ms INTEGER NOT NULL,
  consumed_at_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS order_intents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_user_id INTEGER NOT NULL REFERENCES users(id),
  account_id INTEGER NOT NULL REFERENCES broker_accounts(id),
  execution_date TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 100,
  tag TEXT NOT NULL UNIQUE,
  state TEXT NOT NULL,
  exchange TEXT NOT NULL,
  trading_symbol TEXT NOT NULL,
  transaction_type TEXT NOT NULL,
  product TEXT NOT NULL,
  order_type TEXT NOT NULL,
  quantity INTEGER NOT NULL,
  disclosed_quantity INTEGER NOT NULL DEFAULT 0,
  price TEXT NOT NULL DEFAULT '',
  trigger_price TEXT NOT NULL DEFAULT '',
  validity TEXT NOT NULL,
  validity_ttl INTEGER NOT NULL DEFAULT 0,
  -- Kept for databases created before fractional market protection support.
  market_protection INTEGER NOT NULL DEFAULT 0,
  market_protection_text TEXT NOT NULL DEFAULT '__legacy__',
  autoslice INTEGER NOT NULL DEFAULT 0,
  live_confirmed INTEGER NOT NULL DEFAULT 0,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL,
  armed_at_ms INTEGER NOT NULL DEFAULT 0,
  UNIQUE(account_id, execution_date, tag)
);
CREATE INDEX IF NOT EXISTS idx_order_date_state ON order_intents(execution_date, state);
CREATE TABLE IF NOT EXISTS schedule_freezes (
  execution_date TEXT PRIMARY KEY,
  frozen_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS dispatch_attempts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  intent_id INTEGER NOT NULL REFERENCES order_intents(id),
  attempt INTEGER NOT NULL,
  request_count INTEGER NOT NULL DEFAULT 0,
  target_at_ms INTEGER NOT NULL,
  started_at_ns INTEGER NOT NULL DEFAULT 0,
  finished_at_ns INTEGER NOT NULL DEFAULT 0,
  order_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  exchange_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  exchange_update_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT 'PREPARED',
  order_id TEXT NOT NULL DEFAULT '',
  exchange_order_id TEXT NOT NULL DEFAULT '',
  broker_status TEXT NOT NULL DEFAULT '',
  child_orders TEXT NOT NULL DEFAULT '',
  error_type TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  UNIQUE(intent_id, attempt)
);
CREATE TABLE IF NOT EXISTS audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER,
  event_type TEXT NOT NULL,
  subject_type TEXT NOT NULL,
  subject_id INTEGER NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  created_at_ms INTEGER NOT NULL
);

-- V2 keeps local administrators and Kite-authenticated traders as distinct
-- security principals. Legacy tables remain in place so an upgrade can never
-- reinterpret or resubmit old work.
CREATE TABLE IF NOT EXISTS admins (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL COLLATE NOCASE UNIQUE,
  password_hash TEXT NOT NULL,
  is_root INTEGER NOT NULL DEFAULT 0 CHECK(is_root IN (0,1)),
  must_change_password INTEGER NOT NULL DEFAULT 0 CHECK(must_change_password IN (0,1)),
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_single_root_admin ON admins(is_root) WHERE is_root=1;
CREATE TRIGGER IF NOT EXISTS protect_root_admin_delete
BEFORE DELETE ON admins WHEN OLD.is_root=1
BEGIN SELECT RAISE(ABORT, 'the root administrator cannot be deleted'); END;
CREATE TRIGGER IF NOT EXISTS protect_root_admin_demotion
BEFORE UPDATE OF is_root ON admins WHEN OLD.is_root=1 AND NEW.is_root<>1
BEGIN SELECT RAISE(ABORT, 'the root administrator cannot be demoted'); END;
CREATE TABLE IF NOT EXISTS admin_web_sessions (
  token_hash TEXT PRIMARY KEY,
  admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
  expires_at_ms INTEGER NOT NULL,
  created_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS traders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  client_id TEXT NOT NULL COLLATE NOCASE,
  display_client_id TEXT NOT NULL,
  api_key TEXT NOT NULL,
  api_secret_enc TEXT NOT NULL,
  access_token_enc TEXT NOT NULL DEFAULT '',
  verified_at_ms INTEGER NOT NULL DEFAULT 0,
  session_login_at_ms INTEGER NOT NULL DEFAULT 0,
  session_expires_at_ms INTEGER NOT NULL DEFAULT 0,
  activated_for_date TEXT NOT NULL DEFAULT '',
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL,
  deleted_at_ms INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_active_trader_client_id
  ON traders(client_id COLLATE NOCASE) WHERE deleted_at_ms=0;
CREATE UNIQUE INDEX IF NOT EXISTS idx_active_trader_api_key
  ON traders(api_key) WHERE deleted_at_ms=0;
CREATE TABLE IF NOT EXISTS trader_policy_attestations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  trader_id INTEGER NOT NULL REFERENCES traders(id),
  policy_version TEXT NOT NULL,
  source_url TEXT NOT NULL,
  exact_text TEXT NOT NULL,
  acknowledged_at_ms INTEGER NOT NULL,
  created_at_ms INTEGER NOT NULL,
  UNIQUE(trader_id,policy_version,exact_text)
);
CREATE TABLE IF NOT EXISTS trader_daily_activations (
  trader_id INTEGER NOT NULL REFERENCES traders(id),
  execution_date TEXT NOT NULL,
  login_at_ms INTEGER NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  access_token_enc TEXT NOT NULL,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY(trader_id,execution_date)
);
CREATE TABLE IF NOT EXISTS trader_web_sessions (
  token_hash TEXT PRIMARY KEY,
  trader_id INTEGER NOT NULL REFERENCES traders(id),
  expires_at_ms INTEGER NOT NULL,
  created_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kite_auth_flows (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  state_hash TEXT NOT NULL UNIQUE,
  purpose TEXT NOT NULL CHECK(purpose IN ('SIGNUP','SIGNIN')),
  trader_id INTEGER REFERENCES traders(id),
  client_id_enc TEXT NOT NULL,
  api_key_enc TEXT NOT NULL,
  api_secret_enc TEXT NOT NULL,
  policy_version TEXT NOT NULL DEFAULT '',
  policy_source_url TEXT NOT NULL DEFAULT '',
  policy_exact_text TEXT NOT NULL DEFAULT '',
  policy_acknowledged_at_ms INTEGER NOT NULL DEFAULT 0,
  expires_at_ms INTEGER NOT NULL,
  consumed_at_ms INTEGER NOT NULL DEFAULT 0,
  completed_at_ms INTEGER NOT NULL DEFAULT 0,
  signup_invitation_id INTEGER NOT NULL DEFAULT 0,
  created_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kite_auth_flows_expiry ON kite_auth_flows(expires_at_ms);

-- Invitation tokens are bearer credentials. Only their SHA-256 hashes are
-- durable. A reservation lasts exactly as long as its Kite flow; failed and
-- expired flows may be retried, while successful consumption is permanent.
CREATE TABLE IF NOT EXISTS signup_invitations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash TEXT NOT NULL UNIQUE,
  created_by_admin_id INTEGER NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  reserved_at_ms INTEGER NOT NULL DEFAULT 0,
  reserved_until_ms INTEGER NOT NULL DEFAULT 0,
  reserved_flow_id INTEGER NOT NULL DEFAULT 0,
  consumed_at_ms INTEGER NOT NULL DEFAULT 0,
  consumed_by_trader_id INTEGER REFERENCES traders(id),
  revoked_at_ms INTEGER NOT NULL DEFAULT 0,
  revoked_by_admin_id INTEGER,
  created_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_signup_invitations_lifecycle
  ON signup_invitations(consumed_at_ms,revoked_at_ms,expires_at_ms);

CREATE TABLE IF NOT EXISTS runtime_settings_versions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  version INTEGER NOT NULL UNIQUE,
  effective_date TEXT NOT NULL,
  dispatch_clock TEXT NOT NULL,
  user_blackout_start TEXT NOT NULL,
  user_blackout_end TEXT NOT NULL,
  freeze_lead_ms INTEGER NOT NULL,
  batch_interval_ms INTEGER NOT NULL,
  max_orders_per_trader INTEGER NOT NULL,
  created_by_admin_id INTEGER REFERENCES admins(id),
  created_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runtime_settings_effective
  ON runtime_settings_versions(effective_date DESC,version DESC);

CREATE TABLE IF NOT EXISTS queued_date_freezes (
  execution_date TEXT PRIMARY KEY,
  frozen_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS trader_queue_versions (
  trader_id INTEGER NOT NULL REFERENCES traders(id),
  execution_date TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY(trader_id,execution_date)
);
CREATE TABLE IF NOT EXISTS queued_orders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  trader_id INTEGER NOT NULL REFERENCES traders(id),
  execution_date TEXT NOT NULL,
  position INTEGER NOT NULL,
  tag TEXT NOT NULL UNIQUE,
  state TEXT NOT NULL,
  exchange TEXT NOT NULL,
  trading_symbol TEXT NOT NULL,
  transaction_type TEXT NOT NULL,
  product TEXT NOT NULL,
  order_type TEXT NOT NULL,
  quantity INTEGER NOT NULL,
  disclosed_quantity INTEGER NOT NULL DEFAULT 0,
  price TEXT NOT NULL DEFAULT '',
  trigger_price TEXT NOT NULL DEFAULT '',
  validity TEXT NOT NULL DEFAULT 'DAY' CHECK(validity='DAY'),
  market_protection_text TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL,
  deleted_at_ms INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_queued_order_position
  ON queued_orders(trader_id,execution_date,position) WHERE deleted_at_ms=0;
CREATE INDEX IF NOT EXISTS idx_queued_order_date_state
  ON queued_orders(execution_date,state,position) WHERE deleted_at_ms=0;
CREATE TABLE IF NOT EXISTS queued_dispatch_records (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  queued_order_id INTEGER NOT NULL UNIQUE REFERENCES queued_orders(id),
  trader_id INTEGER NOT NULL REFERENCES traders(id),
  client_id_redacted TEXT NOT NULL,
  execution_date TEXT NOT NULL,
  queue_position INTEGER NOT NULL,
  batch_number INTEGER NOT NULL,
  payload_json TEXT NOT NULL,
  request_count INTEGER NOT NULL DEFAULT 0 CHECK(request_count BETWEEN 0 AND 1),
  target_at_ms INTEGER NOT NULL,
  started_at_ns INTEGER NOT NULL DEFAULT 0,
  finished_at_ns INTEGER NOT NULL DEFAULT 0,
  order_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  exchange_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  exchange_update_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT 'PREPARED',
  order_id TEXT NOT NULL DEFAULT '',
  exchange_order_id TEXT NOT NULL DEFAULT '',
  broker_status TEXT NOT NULL DEFAULT '',
  child_orders TEXT NOT NULL DEFAULT '',
  error_type TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  created_at_ms INTEGER NOT NULL,
  updated_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_queued_dispatch_date
  ON queued_dispatch_records(execution_date,trader_id,queue_position);
CREATE TABLE IF NOT EXISTS queued_dispatch_receipts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  dispatch_record_id INTEGER NOT NULL REFERENCES queued_dispatch_records(id),
  source TEXT NOT NULL CHECK(source IN ('KITE_RESPONSE','ORDERBOOK','CURRENT_SNAPSHOT')),
  observed_at_ns INTEGER NOT NULL,
  order_id TEXT NOT NULL DEFAULT '',
  exchange_order_id TEXT NOT NULL DEFAULT '',
  broker_status TEXT NOT NULL DEFAULT '',
  status_message TEXT NOT NULL DEFAULT '',
  order_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  exchange_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  exchange_update_timestamp_ms INTEGER NOT NULL DEFAULT 0,
  UNIQUE(dispatch_record_id,source,observed_at_ns)
);
CREATE INDEX IF NOT EXISTS idx_queued_dispatch_receipt_record
  ON queued_dispatch_receipts(dispatch_record_id,observed_at_ns,id);
CREATE TABLE IF NOT EXISTS operator_audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  actor_type TEXT NOT NULL CHECK(actor_type IN ('ADMIN','TRADER','SYSTEM')),
  actor_id INTEGER NOT NULL DEFAULT 0,
  event_type TEXT NOT NULL,
  subject_type TEXT NOT NULL,
  subject_id INTEGER NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT '',
  created_at_ms INTEGER NOT NULL
);`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin database migration: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	columns := []struct {
		table, name, definition string
	}{
		{"dispatch_attempts", "request_count", "INTEGER NOT NULL DEFAULT 0"},
		{"dispatch_attempts", "child_orders", "TEXT NOT NULL DEFAULT ''"},
		{"dispatch_attempts", "order_timestamp_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"dispatch_attempts", "exchange_timestamp_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"dispatch_attempts", "exchange_update_timestamp_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"users", "must_change_password", "INTEGER NOT NULL DEFAULT 0"},
		{"order_intents", "market_protection_text", "TEXT NOT NULL DEFAULT '__legacy__'"},
		{"order_intents", "live_confirmed", "INTEGER NOT NULL DEFAULT 0"},
		{"kite_auth_flows", "signup_invitation_id", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if err := ensureColumn(ctx, tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	// Older records predate the receipt stream. Preserve their most recent
	// broker snapshot once so an upgrade immediately exposes the timestamps
	// already known without pretending to reconstruct missing transitions.
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO queued_dispatch_receipts(
 dispatch_record_id,source,observed_at_ns,order_id,exchange_order_id,broker_status,
 status_message,order_timestamp_ms,exchange_timestamp_ms,exchange_update_timestamp_ms)
SELECT d.id,'CURRENT_SNAPSHOT',d.updated_at_ms*1000000,d.order_id,d.exchange_order_id,d.broker_status,
       '',d.order_timestamp_ms,d.exchange_timestamp_ms,d.exchange_update_timestamp_ms
FROM queued_dispatch_records d
WHERE d.request_count=1
  AND (d.order_id<>'' OR d.exchange_order_id<>'' OR d.broker_status<>''
       OR d.order_timestamp_ms<>0 OR d.exchange_timestamp_ms<>0 OR d.exchange_update_timestamp_ms<>0)
  AND NOT EXISTS(SELECT 1 FROM queued_dispatch_receipts r WHERE r.dispatch_record_id=d.id)`); err != nil {
		return fmt.Errorf("backfill queued dispatch receipts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE order_intents
SET market_protection_text=CASE WHEN UPPER(order_type)='LIMIT' THEN '0' ELSE CAST(market_protection AS TEXT) END
WHERE market_protection_text='__legacy__'`); err != nil {
		return fmt.Errorf("migrate market protection values: %w", err)
	}
	// Preserve the existing local administrator during a side-by-side upgrade.
	// Trader accounts and old orders intentionally stay in the legacy tables;
	// silently reinterpreting them could arm or resubmit work under new rules.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO admins(username,password_hash,is_root,must_change_password,created_at_ms,updated_at_ms)
SELECT username,password_hash,1,must_change_password,created_at_ms,created_at_ms
FROM users WHERE is_admin=1 AND NOT EXISTS(SELECT 1 FROM admins)
ORDER BY id LIMIT 1`); err != nil {
		return fmt.Errorf("migrate root administrator: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runtime_settings_versions(
 version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
 freeze_lead_ms,batch_interval_ms,max_orders_per_trader,created_at_ms)
SELECT 1,'1970-01-01','09:00:00','08:45:00','09:30:00',300000,1050,100,?
WHERE NOT EXISTS(SELECT 1 FROM runtime_settings_versions)`, millis(time.Now())); err != nil {
		return fmt.Errorf("seed runtime settings: %w", err)
	}
	return tx.Commit()
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("inspect %s schema: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	if found {
		return nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) BootstrapAdmin(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("administrator username is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var adminCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&adminCount); err != nil {
		return err
	}
	if adminCount > 0 {
		return tx.Commit()
	}
	var hash string
	var createdMS int64
	err = tx.QueryRowContext(ctx, `
SELECT password_hash,created_at_ms FROM users
WHERE username=? COLLATE NOCASE AND is_admin=1`, username).Scan(&hash, &createdMS)
	if errors.Is(err, sql.ErrNoRows) {
		if password == "" {
			return errors.New("administrator password is required on first start")
		}
		hash, err = secure.HashPassword(password)
		if err != nil {
			return err
		}
		createdMS = millis(time.Now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(username,password_hash,is_admin,created_at_ms) VALUES(?,?,1,?)`, username, hash, createdMS); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO admins(username,password_hash,is_root,must_change_password,created_at_ms,updated_at_ms)
VALUES(?,?,1,0,?,?)`, username, hash, createdMS, millis(time.Now()))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}
