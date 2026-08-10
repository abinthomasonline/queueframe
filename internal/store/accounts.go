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
)

func (s *Store) CreateAccount(ctx context.Context, ownerID int64, name, clientID, apiKey, apiSecret string) (domain.Account, error) {
	name = strings.TrimSpace(name)
	clientID = strings.ToUpper(strings.TrimSpace(clientID))
	apiKey = strings.TrimSpace(apiKey)
	if name == "" || clientID == "" || apiKey == "" || apiSecret == "" {
		return domain.Account{}, errors.New("name, client ID, API key, and API secret are required")
	}
	secret, err := s.vault.Encrypt(apiSecret)
	if err != nil {
		return domain.Account{}, err
	}
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Account{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO broker_accounts(owner_user_id,name,client_id,api_key,api_secret_enc,created_at_ms,updated_at_ms)
VALUES(?,?,?,?,?,?,?)`, ownerID, name, clientID, apiKey, secret, millis(now), millis(now))
	if err != nil {
		return domain.Account{}, fmt.Errorf("create account: %w", err)
	}
	id, _ := result.LastInsertId()
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_memberships(account_id,user_id,can_trade) VALUES(?,?,1)`, id, ownerID); err != nil {
		return domain.Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Account{}, err
	}
	return domain.Account{ID: id, OwnerUserID: ownerID, Name: name, ClientID: clientID, APIKey: apiKey, APISecret: apiSecret, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) ListAccounts(ctx context.Context, userID int64) ([]domain.Account, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id,a.owner_user_id,a.name,a.client_id,a.api_key,a.api_secret_enc,a.access_token_enc,
       a.verified,a.session_login_at_ms,a.session_expires_at_ms,a.created_at_ms,a.updated_at_ms
FROM broker_accounts a JOIN account_memberships m ON m.account_id=a.id
WHERE m.user_id=? ORDER BY a.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []domain.Account
	for rows.Next() {
		account, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *Store) AccountForUser(ctx context.Context, userID, accountID int64) (domain.Account, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT a.id,a.owner_user_id,a.name,a.client_id,a.api_key,a.api_secret_enc,a.access_token_enc,
       a.verified,a.session_login_at_ms,a.session_expires_at_ms,a.created_at_ms,a.updated_at_ms
FROM broker_accounts a JOIN account_memberships m ON m.account_id=a.id
WHERE m.user_id=? AND m.account_id=? AND m.can_trade=1`, userID, accountID)
	return s.scanAccount(row)
}

func (s *Store) Account(ctx context.Context, accountID int64) (domain.Account, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id,owner_user_id,name,client_id,api_key,api_secret_enc,access_token_enc,
       verified,session_login_at_ms,session_expires_at_ms,created_at_ms,updated_at_ms
FROM broker_accounts WHERE id=?`, accountID)
	return s.scanAccount(row)
}

type scanner interface{ Scan(...any) error }

func (s *Store) scanAccount(row scanner) (domain.Account, error) {
	var account domain.Account
	var secret, token string
	var verified int
	var loginAt, expiresAt, createdAt, updatedAt int64
	if err := row.Scan(&account.ID, &account.OwnerUserID, &account.Name, &account.ClientID, &account.APIKey,
		&secret, &token, &verified, &loginAt, &expiresAt, &createdAt, &updatedAt); err != nil {
		return domain.Account{}, err
	}
	var err error
	account.APISecret, err = s.vault.Decrypt(secret)
	if err != nil {
		return domain.Account{}, err
	}
	account.AccessToken, err = s.vault.Decrypt(token)
	if err != nil {
		return domain.Account{}, err
	}
	account.Verified = verified != 0
	account.SessionLoginAt = fromMillis(loginAt)
	account.SessionExpiresAt = fromMillis(expiresAt)
	account.CreatedAt = fromMillis(createdAt)
	account.UpdatedAt = fromMillis(updatedAt)
	return account, nil
}

func (s *Store) CreateLoginNonce(ctx context.Context, userID, accountID int64, stateHash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO login_nonces(state_hash,account_id,user_id,expires_at_ms) VALUES(?,?,?,?)`, stateHash, accountID, userID, millis(expires))
	return err
}

func (s *Store) ConsumeLoginNonce(ctx context.Context, stateHash string) (userID, accountID int64, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var expires, consumed int64
	err = tx.QueryRowContext(ctx, `SELECT user_id,account_id,expires_at_ms,consumed_at_ms FROM login_nonces WHERE state_hash=?`, stateHash).
		Scan(&userID, &accountID, &expires, &consumed)
	if err != nil {
		return 0, 0, err
	}
	if consumed != 0 || expires <= millis(time.Now()) {
		return 0, 0, errors.New("login state is expired or already used")
	}
	result, err := tx.ExecContext(ctx, `UPDATE login_nonces SET consumed_at_ms=? WHERE state_hash=? AND consumed_at_ms=0`, millis(time.Now()), stateHash)
	if err != nil {
		return 0, 0, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return 0, 0, errors.New("login state was already used")
	}
	return userID, accountID, tx.Commit()
}

func (s *Store) SaveKiteSession(ctx context.Context, accountID int64, session broker.Session, expiresAt time.Time) error {
	token, err := s.vault.Encrypt(session.AccessToken)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE broker_accounts SET access_token_enc=?,verified=1,session_login_at_ms=?,session_expires_at_ms=?,updated_at_ms=?
WHERE id=? AND client_id=? COLLATE NOCASE AND api_key=?`, token, millis(session.LoginAt), millis(expiresAt), millis(time.Now()), accountID, session.UserID, session.APIKey)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return errors.New("Kite login identity does not match this account")
	}
	return nil
}

func (s *Store) InvalidateKiteSession(ctx context.Context, accountID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE broker_accounts SET access_token_enc='',session_expires_at_ms=0,updated_at_ms=? WHERE id=?`, millis(time.Now()), accountID)
	return err
}
