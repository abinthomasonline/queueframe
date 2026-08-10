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

var (
	ErrNotFound                    = errors.New("not found")
	ErrConflict                    = errors.New("conflict")
	ErrQueueFrozen                 = errors.New("order queue is frozen")
	ErrQueueRevisionConflict       = errors.New("order queue changed; reload and try again")
	ErrAuthFlowExpired             = errors.New("Kite login state is expired or already used")
	ErrUserLoginBlackout           = errors.New("trader login is unavailable during the configured blackout")
	ErrRootAdminProtected          = errors.New("the root administrator cannot be deleted")
	ErrTraderDeleted               = errors.New("trader profile is deleted")
	ErrPolicyAttestationRequired   = errors.New("the hosting policy acknowledgement is required")
	ErrSignupInvitationUnavailable = errors.New("signup invitation is invalid, expired, revoked, consumed, or already in use")
	ErrInvalidAdminCredentials     = errors.New("invalid username or password")
)

type adminRowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) AuthenticateAdmin(ctx context.Context, username, password string) (domain.Admin, error) {
	return s.authenticateAdmin(ctx, s.db, username, password)
}

func (s *Store) authenticateAdmin(ctx context.Context, querier adminRowQuerier, username, password string) (domain.Admin, error) {
	var admin domain.Admin
	var passwordHash string
	var root, mustChange int
	var createdMS, updatedMS int64
	err := querier.QueryRowContext(ctx, `
SELECT id,username,password_hash,is_root,must_change_password,created_at_ms,updated_at_ms
FROM admins WHERE username=? COLLATE NOCASE`, strings.TrimSpace(username)).
		Scan(&admin.ID, &admin.Username, &passwordHash, &root, &mustChange, &createdMS, &updatedMS)
	if err != nil {
		_ = secure.VerifyPassword(s.dummyPasswordHash, password)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Admin{}, ErrInvalidAdminCredentials
		}
		return domain.Admin{}, err
	}
	if !secure.VerifyPassword(passwordHash, password) {
		return domain.Admin{}, ErrInvalidAdminCredentials
	}
	admin.Root = root != 0
	admin.MustChangePassword = mustChange != 0
	admin.CreatedAt = fromMillis(createdMS)
	admin.UpdatedAt = fromMillis(updatedMS)
	return admin, nil
}

// AuthenticateAdminAndCreateWebSession keeps verification and session
// insertion in one transaction. A concurrent password change therefore either
// revokes the new session or commits first and makes the old credential fail.
func (s *Store) AuthenticateAdminAndCreateWebSession(ctx context.Context, username, password string, ttl time.Duration, now time.Time) (domain.Admin, string, error) {
	if ttl <= 0 {
		return domain.Admin{}, "", errors.New("invalid administrator session")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Admin{}, "", err
	}
	defer tx.Rollback()
	admin, err := s.authenticateAdmin(ctx, tx, username, password)
	if err != nil {
		return domain.Admin{}, "", err
	}
	rawToken, tokenHash, err := secure.RandomToken(32)
	if err != nil {
		return domain.Admin{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO admin_web_sessions(token_hash,admin_id,expires_at_ms,created_at_ms)
VALUES(?,?,?,?)`, tokenHash, admin.ID, millis(now.Add(ttl)), millis(now)); err != nil {
		return domain.Admin{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return domain.Admin{}, "", err
	}
	return admin, rawToken, nil
}

func (s *Store) ListAdmins(ctx context.Context) ([]domain.Admin, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id,username,is_root,must_change_password,created_at_ms,updated_at_ms
FROM admins ORDER BY is_root DESC,username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var admins []domain.Admin
	for rows.Next() {
		var admin domain.Admin
		var root, mustChange int
		var createdMS, updatedMS int64
		if err := rows.Scan(&admin.ID, &admin.Username, &root, &mustChange, &createdMS, &updatedMS); err != nil {
			return nil, err
		}
		admin.Root = root != 0
		admin.MustChangePassword = mustChange != 0
		admin.CreatedAt = fromMillis(createdMS)
		admin.UpdatedAt = fromMillis(updatedMS)
		admins = append(admins, admin)
	}
	return admins, rows.Err()
}

func (s *Store) CreateAdminWebSession(ctx context.Context, adminID int64, ttl time.Duration, now time.Time) (string, error) {
	if adminID <= 0 || ttl <= 0 {
		return "", errors.New("invalid administrator session")
	}
	raw, hash, err := secure.RandomToken(32)
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO admin_web_sessions(token_hash,admin_id,expires_at_ms,created_at_ms)
SELECT ?,id,?,? FROM admins WHERE id=?`, hash, millis(now.Add(ttl)), millis(now), adminID)
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

func (s *Store) AdminByWebSession(ctx context.Context, rawToken string, now time.Time) (domain.Admin, error) {
	var admin domain.Admin
	var root, mustChange int
	var createdMS, updatedMS int64
	err := s.db.QueryRowContext(ctx, `
SELECT a.id,a.username,a.is_root,a.must_change_password,a.created_at_ms,a.updated_at_ms
FROM admin_web_sessions s JOIN admins a ON a.id=s.admin_id
WHERE s.token_hash=? AND s.expires_at_ms>?`, secure.TokenHash(rawToken), millis(now)).
		Scan(&admin.ID, &admin.Username, &root, &mustChange, &createdMS, &updatedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Admin{}, errors.New("session expired")
	}
	if err != nil {
		return domain.Admin{}, err
	}
	admin.Root = root != 0
	admin.MustChangePassword = mustChange != 0
	admin.CreatedAt = fromMillis(createdMS)
	admin.UpdatedAt = fromMillis(updatedMS)
	return admin, nil
}

func (s *Store) DeleteAdminWebSession(ctx context.Context, rawToken string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_web_sessions WHERE token_hash=?`, secure.TokenHash(rawToken))
	return err
}

func (s *Store) CountAdminSessions(ctx context.Context, adminID int64, now time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM admin_web_sessions WHERE admin_id=? AND expires_at_ms>?`, adminID, millis(now)).Scan(&count)
	return count, err
}

func (s *Store) ChangeAdminPassword(ctx context.Context, adminID int64, currentPassword, newPassword string, now time.Time) error {
	newHash, err := secure.HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentHash string
	var credentialVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT password_hash,updated_at_ms FROM admins WHERE id=?`, adminID).Scan(&currentHash, &credentialVersion); err != nil {
		return err
	}
	if !secure.VerifyPassword(currentHash, currentPassword) {
		return errors.New("current password is incorrect")
	}
	result, err := tx.ExecContext(ctx, `
UPDATE admins SET password_hash=?,must_change_password=0,updated_at_ms=?
WHERE id=? AND password_hash=? AND updated_at_ms=?`, newHash, millis(now), adminID, currentHash, credentialVersion)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_web_sessions WHERE admin_id=?`, adminID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAdmin is intentionally incapable of deleting the protected root.
// The database trigger provides a second line of defence against future call
// sites that bypass this method.
func (s *Store) DeleteAdmin(ctx context.Context, adminID int64) error {
	var root int
	if err := s.db.QueryRowContext(ctx, `SELECT is_root FROM admins WHERE id=?`, adminID).Scan(&root); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if root != 0 {
		return ErrRootAdminProtected
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM admins WHERE id=?`, adminID)
	return err
}
