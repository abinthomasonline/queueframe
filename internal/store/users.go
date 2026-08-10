package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/secure"
)

func (s *Store) Authenticate(ctx context.Context, username, password string) (domain.User, error) {
	var user domain.User
	var passwordHash string
	var admin, mustChange int
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id,username,password_hash,is_admin,must_change_password,created_at_ms FROM users WHERE username=?`, strings.TrimSpace(username)).
		Scan(&user.ID, &user.Username, &passwordHash, &admin, &mustChange, &created)
	if err != nil {
		_ = secure.VerifyPassword(s.dummyPasswordHash, password)
		return domain.User{}, errors.New("invalid username or password")
	}
	if !secure.VerifyPassword(passwordHash, password) {
		return domain.User{}, errors.New("invalid username or password")
	}
	user.Admin = admin != 0
	user.MustChangePassword = mustChange != 0
	user.CreatedAt = fromMillis(created)
	return user, nil
}

func (s *Store) CreateUser(ctx context.Context, username, password string, admin bool) (domain.User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return domain.User{}, errors.New("username is required")
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return domain.User{}, err
	}
	now := time.Now()
	result, err := s.db.ExecContext(ctx, `INSERT INTO users(username,password_hash,is_admin,must_change_password,created_at_ms) VALUES(?,?,?,1,?)`, username, hash, admin, millis(now))
	if err != nil {
		return domain.User{}, err
	}
	id, _ := result.LastInsertId()
	return domain.User{ID: id, Username: username, Admin: admin, MustChangePassword: true, CreatedAt: now}, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,is_admin,must_change_password,created_at_ms FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []domain.User
	for rows.Next() {
		var user domain.User
		var admin, mustChange int
		var created int64
		if err := rows.Scan(&user.ID, &user.Username, &admin, &mustChange, &created); err != nil {
			return nil, err
		}
		user.Admin = admin != 0
		user.MustChangePassword = mustChange != 0
		user.CreatedAt = fromMillis(created)
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) CreateWebSession(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	raw, hash, err := secure.RandomToken(32)
	if err != nil {
		return "", err
	}
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO web_sessions(token_hash,user_id,expires_at_ms,created_at_ms) VALUES(?,?,?,?)`, hash, userID, millis(now.Add(ttl)), millis(now))
	return raw, err
}

func (s *Store) UserBySession(ctx context.Context, rawToken string) (domain.User, error) {
	var user domain.User
	var admin, mustChange int
	var created int64
	err := s.db.QueryRowContext(ctx, `
SELECT u.id,u.username,u.is_admin,u.must_change_password,u.created_at_ms
FROM web_sessions s JOIN users u ON u.id=s.user_id
WHERE s.token_hash=? AND s.expires_at_ms>?`, secure.TokenHash(rawToken), millis(time.Now())).Scan(&user.ID, &user.Username, &admin, &mustChange, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, errors.New("session expired")
		}
		return domain.User{}, err
	}
	user.Admin = admin != 0
	user.MustChangePassword = mustChange != 0
	user.CreatedAt = fromMillis(created)
	return user, nil
}

func (s *Store) DeleteWebSession(ctx context.Context, rawToken string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash=?`, secure.TokenHash(rawToken))
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	now := millis(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM web_sessions WHERE expires_at_ms<=?`,
		`DELETE FROM trader_web_sessions WHERE expires_at_ms<=?`,
		`DELETE FROM admin_web_sessions WHERE expires_at_ms<=?`,
		`DELETE FROM kite_auth_flows WHERE expires_at_ms<=? OR completed_at_ms<>0`,
	} {
		if _, err := tx.ExecContext(ctx, statement, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ChangePassword(ctx context.Context, userID int64, currentPassword, newPassword string) error {
	var currentHash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&currentHash); err != nil {
		return err
	}
	if !secure.VerifyPassword(currentHash, currentPassword) {
		return errors.New("current password is incorrect")
	}
	newHash, err := secure.HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,must_change_password=0 WHERE id=?`, newHash, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM web_sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
