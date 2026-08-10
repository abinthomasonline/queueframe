package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/secure"
)

func invitedSignupInput(clientID, apiKey, inviteToken string) domain.TraderSignup {
	return domain.TraderSignup{
		ClientID: clientID, APIKey: apiKey, APISecret: "secret-" + clientID,
		InviteToken: inviteToken,
		PolicyAttestation: domain.PolicyAttestation{
			Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText,
		},
	}
}

func TestSignupInvitationMigrationAddsFlowAssociation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-invitation.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
CREATE TABLE kite_auth_flows (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  state_hash TEXT NOT NULL UNIQUE,
  purpose TEXT NOT NULL CHECK(purpose IN ('SIGNUP','SIGNIN')),
  trader_id INTEGER,
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
  created_at_ms INTEGER NOT NULL
);`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	vault, err := secure.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := Open(path, vault)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.db.Query(`PRAGMA table_info(kite_auth_flows)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found = found || name == "signup_invitation_id"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("migration did not add kite_auth_flows.signup_invitation_id")
	}
}

func TestSignupInvitationStoresOnlyHashAndSafeRetryConsumesOnce(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	if err := database.BootstrapAdmin(ctx, "root", "a-long-root-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := database.AuthenticateAdmin(ctx, "root", "a-long-root-password")
	if err != nil {
		t.Fatal(err)
	}
	now := istTime(t, "2026-08-10 07:00:00")
	invitation, rawToken, err := database.CreateSignupInvitation(ctx, admin.ID, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if invitation.ID <= 0 || len(rawToken) != 43 {
		t.Fatalf("invitation=%#v token length=%d", invitation, len(rawToken))
	}
	var storedHash string
	if err := database.db.QueryRowContext(ctx, `SELECT token_hash FROM signup_invitations WHERE id=?`, invitation.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == rawToken || storedHash != secure.TokenHash(rawToken) || strings.Contains(storedHash, rawToken) {
		t.Fatalf("stored invitation token material = %q", storedHash)
	}
	var createdAudits int
	var auditDetails string
	if err := database.db.QueryRowContext(ctx, `
SELECT COUNT(*),COALESCE(GROUP_CONCAT(detail),'') FROM operator_audit_events
WHERE event_type='SIGNUP_INVITATION_CREATED' AND subject_id=?`, invitation.ID).Scan(&createdAudits, &auditDetails); err != nil {
		t.Fatal(err)
	}
	if createdAudits != 1 || strings.Contains(auditDetails, rawToken) {
		t.Fatalf("creation audit count=%d details=%q", createdAudits, auditDetails)
	}
	listed, err := database.ListSignupInvitations(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != invitation.ID {
		t.Fatalf("listed invitations=%#v err=%v", listed, err)
	}

	first, err := database.BeginTraderSignup(ctx, invitedSignupInput("AB1234", "key-one", rawToken), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTraderSignup(ctx, invitedSignupInput("CD5678", "key-two", rawToken), now.Add(time.Second)); !errors.Is(err, ErrSignupInvitationUnavailable) {
		t.Fatalf("second flow while reserved = %v", err)
	}
	consumedFirst, err := database.ConsumeKiteAuthFlow(ctx, first.State, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AbandonKiteAuthFlow(ctx, consumedFirst.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AvailableSignupInvitation(ctx, rawToken, now.Add(3*time.Second)); err != nil {
		t.Fatalf("abandoned flow did not release invitation: %v", err)
	}

	second, err := database.BeginTraderSignup(ctx, invitedSignupInput("CD5678", "key-two", rawToken), now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	consumedSecond, err := database.ConsumeKiteAuthFlow(ctx, second.State, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	trader, err := database.CompleteKiteAuthAt(ctx, consumedSecond.ID, broker.Session{
		UserID: "CD5678", APIKey: "key-two", AccessToken: "access-token", LoginAt: now.Add(4 * time.Second),
	}, now.Add(23*time.Hour), now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AvailableSignupInvitation(ctx, rawToken, now.Add(6*time.Second)); !errors.Is(err, ErrSignupInvitationUnavailable) {
		t.Fatalf("consumed invitation remained available: %v", err)
	}
	listed, err = database.ListSignupInvitations(ctx)
	if err != nil || listed[0].ConsumedByTraderID != trader.ID || listed[0].ConsumedAt.IsZero() {
		t.Fatalf("consumed invitation metadata=%#v err=%v", listed, err)
	}
	var usedAudits int
	if err := database.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM operator_audit_events
WHERE event_type='SIGNUP_INVITATION_USED' AND subject_id=? AND actor_id=?`, invitation.ID, trader.ID).Scan(&usedAudits); err != nil {
		t.Fatal(err)
	}
	if usedAudits != 1 {
		t.Fatalf("successful invitation use audits=%d", usedAudits)
	}
}

func TestSignupInvitationReservationIsAtomicAcrossConcurrentStarts(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	if err := database.BootstrapAdmin(ctx, "root", "a-long-root-password"); err != nil {
		t.Fatal(err)
	}
	admin, _ := database.AuthenticateAdmin(ctx, "root", "a-long-root-password")
	now := istTime(t, "2026-08-10 07:00:00")
	_, rawToken, err := database.CreateSignupInvitation(ctx, admin.ID, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	for index, input := range []domain.TraderSignup{
		invitedSignupInput("AB1234", "key-one", rawToken),
		invitedSignupInput("CD5678", "key-two", rawToken),
	} {
		wait.Add(1)
		go func(index int, input domain.TraderSignup) {
			defer wait.Done()
			<-start
			_, err := database.BeginTraderSignup(ctx, input, now.Add(time.Duration(index)*time.Millisecond))
			errorsSeen <- err
		}(index, input)
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	var successes, unavailable int
	for err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSignupInvitationUnavailable):
			unavailable++
		default:
			t.Fatalf("unexpected concurrent start error: %v", err)
		}
	}
	if successes != 1 || unavailable != 1 {
		t.Fatalf("concurrent starts: successes=%d unavailable=%d", successes, unavailable)
	}
}

func TestRevokedOrExpiredInvitationCannotCreateTrader(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	if err := database.BootstrapAdmin(ctx, "root", "a-long-root-password"); err != nil {
		t.Fatal(err)
	}
	admin, _ := database.AuthenticateAdmin(ctx, "root", "a-long-root-password")
	now := istTime(t, "2026-08-10 07:00:00")
	invitation, rawToken, err := database.CreateSignupInvitation(ctx, admin.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := database.BeginTraderSignup(ctx, invitedSignupInput("AB1234", "key-one", rawToken), now)
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := database.ConsumeKiteAuthFlow(ctx, flow.State, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeSignupInvitation(ctx, invitation.ID, admin.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeSignupInvitation(ctx, invitation.ID, admin.ID, now.Add(2500*time.Millisecond)); !errors.Is(err, ErrSignupInvitationUnavailable) {
		t.Fatalf("second revocation = %v", err)
	}
	if _, err := database.CompleteKiteAuthAt(ctx, consumed.ID, broker.Session{
		UserID: "AB1234", APIKey: "key-one", AccessToken: "access-token", LoginAt: now.Add(time.Second),
	}, now.Add(23*time.Hour), now.Add(3*time.Second)); !errors.Is(err, ErrSignupInvitationUnavailable) {
		t.Fatalf("completion after revocation = %v", err)
	}
	var traders int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traders`).Scan(&traders); err != nil || traders != 0 {
		t.Fatalf("traders after revoked completion=%d err=%v", traders, err)
	}
	var revokedAudits, failedUseAudits int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_audit_events WHERE event_type='SIGNUP_INVITATION_REVOKED' AND subject_id=?`, invitation.ID).Scan(&revokedAudits); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_audit_events WHERE event_type='SIGNUP_INVITATION_USED' AND subject_id=?`, invitation.ID).Scan(&failedUseAudits); err != nil {
		t.Fatal(err)
	}
	if revokedAudits != 1 || failedUseAudits != 0 {
		t.Fatalf("revoked audits=%d failed-use audits=%d", revokedAudits, failedUseAudits)
	}

	_, expiredToken, err := database.CreateSignupInvitation(ctx, admin.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginTraderSignup(ctx, invitedSignupInput("CD5678", "key-two", expiredToken), now.Add(time.Hour)); !errors.Is(err, ErrSignupInvitationUnavailable) {
		t.Fatalf("signup at invitation expiry boundary = %v", err)
	}
	if _, err := database.BeginTraderSignup(ctx, invitedSignupInput("EF9012", "key-three", ""), now); !errors.Is(err, ErrSignupInvitationUnavailable) {
		t.Fatalf("signup without invitation = %v", err)
	}
}
