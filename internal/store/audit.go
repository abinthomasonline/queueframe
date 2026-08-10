package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// WriteAudit records a secret-free operator action. Audit failures are
// returned to the caller so the web layer can surface them in logs without
// ever placing API secrets, tokens, or passwords in this table.
func (s *Store) WriteAudit(ctx context.Context, userID int64, eventType, subjectType string, subjectID int64, detail string) error {
	eventType = strings.TrimSpace(eventType)
	subjectType = strings.TrimSpace(subjectType)
	if userID <= 0 || eventType == "" || subjectType == "" || subjectID <= 0 {
		return errors.New("invalid audit event")
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events(user_id,event_type,subject_type,subject_id,detail,created_at_ms) VALUES(?,?,?,?,?,?)`,
		userID, eventType, subjectType, subjectID, detail, millis(time.Now()))
	return err
}

// WriteOperatorAudit records v2 administrator, trader, and system actions
// without coupling their independent identity tables. Detail must remain
// secret-free; it is length bounded as defence in depth.
func (s *Store) WriteOperatorAudit(ctx context.Context, actorType string, actorID int64, eventType, subjectType string, subjectID int64, detail string, now time.Time) error {
	actorType = strings.ToUpper(strings.TrimSpace(actorType))
	eventType = strings.TrimSpace(eventType)
	subjectType = strings.TrimSpace(subjectType)
	if actorType != "ADMIN" && actorType != "TRADER" && actorType != "SYSTEM" {
		return errors.New("invalid audit actor type")
	}
	if actorType != "SYSTEM" && actorID <= 0 {
		return errors.New("invalid audit actor")
	}
	if eventType == "" || subjectType == "" || subjectID < 0 {
		return errors.New("invalid audit event")
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO operator_audit_events(actor_type,actor_id,event_type,subject_type,subject_id,detail,created_at_ms)
VALUES(?,?,?,?,?,?,?)`, actorType, actorID, eventType, subjectType, subjectID, detail, millis(now))
	return err
}
