package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

func istTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, indiaLocation)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func issueSignupInvite(t *testing.T, database *Store, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	admins, err := database.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(admins) == 0 {
		if err := database.BootstrapAdmin(ctx, "invite-test-admin", "a-long-invite-test-password"); err != nil {
			t.Fatal(err)
		}
		admins, err = database.ListAdmins(ctx)
		if err != nil || len(admins) == 0 {
			t.Fatalf("test administrator unavailable: %#v, %v", admins, err)
		}
	}
	_, rawToken, err := database.CreateSignupInvitation(ctx, admins[0].ID, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	return rawToken
}

func onboardTrader(t *testing.T, database *Store, clientID, apiKey, apiSecret string, loginAt time.Time) domain.Trader {
	t.Helper()
	ctx := context.Background()
	flow, err := database.BeginTraderSignup(ctx, domain.TraderSignup{
		ClientID:    clientID,
		APIKey:      apiKey,
		APISecret:   apiSecret,
		InviteToken: issueSignupInvite(t, database, loginAt),
		PolicyAttestation: domain.PolicyAttestation{
			Version:   domain.HostingPolicyVersion,
			SourceURL: domain.HostingPolicySourceURL,
			Text:      domain.HostingPolicyExactText,
		},
	}, loginAt)
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := database.ConsumeKiteAuthFlow(ctx, flow.State, loginAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	trader, err := database.CompleteKiteAuthAt(ctx, consumed.ID, broker.Session{
		UserID: clientID, APIKey: apiKey, AccessToken: "token-" + clientID, LoginAt: loginAt,
	}, loginAt.Add(23*time.Hour), loginAt.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return trader
}

func TestV2RootAdminIsSeparateAndProtected(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	if err := database.BootstrapAdmin(ctx, "root", "a-long-root-password"); err != nil {
		t.Fatal(err)
	}
	admin, err := database.AuthenticateAdmin(ctx, "ROOT", "a-long-root-password")
	if err != nil || !admin.Root {
		t.Fatalf("admin = %#v, err = %v", admin, err)
	}
	if _, err := database.BeginTraderSignin(ctx, "root", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("local admin unexpectedly resolved as trader: %v", err)
	}
	now := time.Now()
	token, err := database.CreateAdminWebSession(ctx, admin.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := database.AdminByWebSession(ctx, token, now.Add(time.Minute)); err != nil || got.ID != admin.ID {
		t.Fatalf("session admin = %#v, err = %v", got, err)
	}
	if err := database.DeleteAdmin(ctx, admin.ID); !errors.Is(err, ErrRootAdminProtected) {
		t.Fatalf("DeleteAdmin(root) = %v", err)
	}
	if _, err := database.db.ExecContext(ctx, `DELETE FROM admins WHERE id=?`, admin.ID); err == nil || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Fatalf("database allowed root deletion: %v", err)
	}
}

func TestV2SignupFlowIsEncryptedSingleUseAndAttested(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	input := domain.TraderSignup{
		ClientID: "ab1234", APIKey: "public-key", APISecret: "very-secret-value",
		InviteToken: issueSignupInvite(t, database, now),
		PolicyAttestation: domain.PolicyAttestation{
			Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText,
		},
	}
	flow, err := database.BeginTraderSignup(ctx, input, now)
	if err != nil {
		t.Fatal(err)
	}
	if flow.State == "" || !flow.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("flow = %#v", flow)
	}
	var stateHash, clientEnc, keyEnc, secretEnc string
	if err := database.db.QueryRowContext(ctx, `
SELECT state_hash,client_id_enc,api_key_enc,api_secret_enc FROM kite_auth_flows WHERE id=?`, flow.ID).
		Scan(&stateHash, &clientEnc, &keyEnc, &secretEnc); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"state": stateHash, "client": clientEnc, "key": keyEnc, "secret": secretEnc} {
		if strings.Contains(value, flow.State) || strings.Contains(value, input.ClientID) || strings.Contains(value, input.APIKey) || strings.Contains(value, input.APISecret) {
			t.Fatalf("%s was stored in plaintext: %q", name, value)
		}
	}
	consumed, err := database.ConsumeKiteAuthFlow(ctx, flow.State, now.Add(time.Second))
	if err != nil || consumed.ClientID != "AB1234" || consumed.APISecret != input.APISecret {
		t.Fatalf("consumed = %#v, err = %v", consumed, err)
	}
	if _, err := database.ConsumeKiteAuthFlow(ctx, flow.State, now.Add(2*time.Second)); !errors.Is(err, ErrAuthFlowExpired) {
		t.Fatalf("second consume = %v", err)
	}
	trader, err := database.CompleteKiteAuthAt(ctx, consumed.ID, broker.Session{
		UserID: "AB1234", APIKey: input.APIKey, AccessToken: "access-token", LoginAt: now,
	}, now.Add(23*time.Hour), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if trader.ClientID != "AB1234" || trader.APISecret != input.APISecret || trader.AccessToken != "access-token" ||
		trader.ActivatedForDate != "2026-08-10" || trader.PolicyAttestation.Text != domain.HostingPolicyExactText {
		t.Fatalf("trader = %#v", trader)
	}
	var storedSecret, storedToken string
	if err := database.db.QueryRowContext(ctx, `SELECT api_secret_enc,access_token_enc FROM traders WHERE id=?`, trader.ID).Scan(&storedSecret, &storedToken); err != nil {
		t.Fatal(err)
	}
	if storedSecret == input.APISecret || storedToken == "access-token" {
		t.Fatal("trader credentials were not encrypted")
	}
}

func TestV2AbandonedAuthFlowErasesCredentialsImmediately(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	flow, err := database.BeginTraderSignup(ctx, domain.TraderSignup{
		ClientID: "AB1234", APIKey: "public-key", APISecret: "very-secret-value",
		InviteToken: issueSignupInvite(t, database, now),
		PolicyAttestation: domain.PolicyAttestation{
			Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText,
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := database.ConsumeKiteAuthFlow(ctx, flow.State, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AbandonKiteAuthFlow(ctx, consumed.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var clientEnc, keyEnc, secretEnc string
	var completed int64
	if err := database.db.QueryRowContext(ctx, `
SELECT client_id_enc,api_key_enc,api_secret_enc,completed_at_ms
FROM kite_auth_flows WHERE id=?`, consumed.ID).Scan(&clientEnc, &keyEnc, &secretEnc, &completed); err != nil {
		t.Fatal(err)
	}
	if clientEnc != "" || keyEnc != "" || secretEnc != "" || completed == 0 {
		t.Fatalf("abandoned auth flow retained credentials: client=%q key=%q secret=%q completed=%d", clientEnc, keyEnc, secretEnc, completed)
	}
	if _, err := database.ConsumeKiteAuthFlow(ctx, flow.State, now.Add(3*time.Second)); !errors.Is(err, ErrAuthFlowExpired) {
		t.Fatalf("abandoned state was reusable: %v", err)
	}
}

func TestV2SignupRequiresExactCurrentPolicyAndFlowExpires(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	inviteToken := issueSignupInvite(t, database, now)
	_, err := database.BeginTraderSignup(ctx, domain.TraderSignup{
		ClientID: "AB1234", APIKey: "key", APISecret: "secret",
		InviteToken:       inviteToken,
		PolicyAttestation: domain.PolicyAttestation{Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: "almost the policy"},
	}, now)
	if !errors.Is(err, ErrPolicyAttestationRequired) {
		t.Fatalf("policy error = %v", err)
	}
	valid := domain.TraderSignup{
		ClientID: "AB1234", APIKey: "key", APISecret: "secret",
		InviteToken:       inviteToken,
		PolicyAttestation: domain.PolicyAttestation{Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText},
	}
	flow, err := database.BeginTraderSignup(ctx, valid, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConsumeKiteAuthFlow(ctx, flow.State, now.Add(5*time.Minute)); !errors.Is(err, ErrAuthFlowExpired) {
		t.Fatalf("boundary expiry = %v", err)
	}
}

func TestV2TraderAuthAndCallbackAreBlockedDuringBlackout(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	input := domain.TraderSignup{
		ClientID: "AB1234", APIKey: "key", APISecret: "secret",
		InviteToken:       issueSignupInvite(t, database, istTime(t, "2026-08-10 08:44:58")),
		PolicyAttestation: domain.PolicyAttestation{Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText},
	}
	if _, err := database.BeginTraderSignup(ctx, input, istTime(t, "2026-08-10 08:45:00")); !errors.Is(err, ErrUserLoginBlackout) {
		t.Fatalf("signup at blackout boundary = %v", err)
	}
	flow, err := database.BeginTraderSignup(ctx, input, istTime(t, "2026-08-10 08:44:58"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConsumeKiteAuthFlow(ctx, flow.State, istTime(t, "2026-08-10 08:45:00")); !errors.Is(err, ErrUserLoginBlackout) {
		t.Fatalf("callback at blackout boundary = %v", err)
	}
	// Rejection does not consume state, but the five-minute lifetime means it
	// cannot be replayed when the default blackout ends.
	if _, err := database.ConsumeKiteAuthFlow(ctx, flow.State, istTime(t, "2026-08-10 09:30:00")); !errors.Is(err, ErrAuthFlowExpired) {
		t.Fatalf("blackout-spanning flow = %v", err)
	}
}

func TestV2SigninActivatesOnlyAfterSixIST(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	beforeSix := istTime(t, "2026-08-10 05:59:59")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", beforeSix)
	if trader.ActivatedForDate != "" {
		t.Fatalf("pre-06:00 login activated %q", trader.ActivatedForDate)
	}
	flow, err := database.BeginTraderSignin(ctx, "ab1234", istTime(t, "2026-08-10 06:01:00"))
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := database.ConsumeKiteAuthFlow(ctx, flow.State, istTime(t, "2026-08-10 06:01:01"))
	if err != nil {
		t.Fatal(err)
	}
	trader, err = database.CompleteKiteAuthAt(ctx, consumed.ID, broker.Session{
		UserID: "AB1234", APIKey: "key", AccessToken: "new-token", LoginAt: istTime(t, "2026-08-10 06:01:00"),
	}, istTime(t, "2026-08-11 06:00:00"), istTime(t, "2026-08-10 06:01:02"))
	if err != nil {
		t.Fatal(err)
	}
	if !trader.ActiveFor("2026-08-10", istTime(t, "2026-08-10 09:00:00")) {
		t.Fatalf("post-06:00 login was not active: %#v", trader)
	}
}

func TestV2DailyActivationPersistsOnlyInsideWeekdayWindow(t *testing.T) {
	tests := []struct {
		name      string
		loginAt   string
		clientID  string
		wantFound bool
	}{
		{name: "before six", loginAt: "2026-08-10 05:59:00", clientID: "PRE600", wantFound: false},
		{name: "at six", loginAt: "2026-08-10 06:00:00", clientID: "AT600", wantFound: true},
		{name: "before blackout", loginAt: "2026-08-10 08:44:57", clientID: "BEFORE", wantFound: true},
		{name: "after blackout and dispatch", loginAt: "2026-08-10 09:30:00", clientID: "AFTER", wantFound: false},
		{name: "weekend", loginAt: "2026-08-09 07:00:00", clientID: "SUNDAY", wantFound: false},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := testStore(t)
			loginAt := istTime(t, test.loginAt)
			trader := onboardTrader(t, database, test.clientID, fmt.Sprintf("key-%d", index), "secret", loginAt)
			activation, found, err := database.TraderDailyActivation(context.Background(), trader.ID, loginAt.In(indiaLocation).Format("2006-01-02"))
			if err != nil {
				t.Fatal(err)
			}
			if found != test.wantFound {
				t.Fatalf("activation = %#v, found=%t", activation, found)
			}
			if found && (!activation.TokenPresent || activation.LoginAt.UnixMilli() != loginAt.UnixMilli()) {
				t.Fatalf("activation metadata = %#v", activation)
			}
		})
	}
}

func TestV2CredentialFreeActivationStatusesDriveAdminViews(t *testing.T) {
	database := testStore(t)
	loginAt := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "admin-status-key", "secret", loginAt)
	listed, err := database.ListTraders(context.Background(), false)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed traders = %#v, %v", listed, err)
	}
	if listed[0].AccessToken != "" {
		t.Fatal("credential-free trader list unexpectedly exposed an access token")
	}
	statuses, err := database.DailyActivationStatuses(context.Background(), "2026-08-10", istTime(t, "2026-08-10 09:00:00"))
	if err != nil || !statuses[trader.ID] {
		t.Fatalf("activation statuses = %#v, %v", statuses, err)
	}
	weekend, err := database.DailyActivationStatuses(context.Background(), "2026-08-09", istTime(t, "2026-08-09 09:00:00"))
	if err != nil || weekend[trader.ID] {
		t.Fatalf("weekend activation statuses = %#v, %v", weekend, err)
	}
}

func TestV2DailyActivationWindowSupportsMidnightCrossingBlackout(t *testing.T) {
	settings := domain.RuntimeSettings{
		DispatchClock: "23:50:00", UserBlackoutStart: "23:40:00", UserBlackoutEnd: "00:02:00",
		FreezeLead: 5 * time.Minute, BatchInterval: 1050 * time.Millisecond, MaxOrdersPerTrader: 30,
	}
	openAt, closeAt, err := DailyActivationWindow(settings, "2026-08-10")
	if err != nil {
		t.Fatal(err)
	}
	if want := istTime(t, "2026-08-10 06:00:00"); !openAt.Equal(want) {
		t.Fatalf("openAt = %v, want %v", openAt, want)
	}
	if want := istTime(t, "2026-08-10 23:40:00"); !closeAt.Equal(want) {
		t.Fatalf("closeAt = %v, want %v", closeAt, want)
	}
}

func TestV2QueueDenseReorderFixedFieldsAndRevision(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	date := "2026-08-11"
	makeOrder := func(symbol, kind, price string) domain.QueuedOrder {
		return domain.QueuedOrder{ExecutionDate: date, Params: domain.OrderParams{
			Exchange: "nse", TradingSymbol: symbol, TransactionType: "sell", Product: "cnc",
			OrderType: kind, Quantity: 5, Price: price, Validity: "TTL", MarketProtection: "99", Autoslice: true,
		}}
	}
	first, revision, err := database.CreateQueuedOrder(ctx, trader.ID, makeOrder("AAA", "limit", "10.50"), now)
	if err != nil {
		t.Fatal(err)
	}
	second, revision, err := database.CreateQueuedOrder(ctx, trader.ID, makeOrder("BBB", "market", "123"), now)
	if err != nil {
		t.Fatal(err)
	}
	third, revision, err := database.CreateQueuedOrder(ctx, trader.ID, makeOrder("CCC", "limit", "30"), now)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 3 || first.Position != 1 || second.Position != 2 || third.Position != 3 {
		t.Fatalf("positions/revision: %d, %d/%d/%d", revision, first.Position, second.Position, third.Position)
	}
	if first.Params.Validity != "DAY" || first.Params.MarketProtection != "0" || first.Params.Autoslice ||
		second.Params.Validity != "DAY" || second.Params.MarketProtection != "-1" || second.Params.Price != "" {
		t.Fatalf("fixed payload fields: first=%#v second=%#v", first.Params, second.Params)
	}
	newRevision, err := database.ReorderQueuedOrders(ctx, trader.ID, date, revision, []int64{third.ID, first.ID, second.ID}, now.Add(time.Minute))
	if err != nil || newRevision != 4 {
		t.Fatalf("reorder revision=%d err=%v", newRevision, err)
	}
	if _, err := database.ReorderQueuedOrders(ctx, trader.ID, date, revision, []int64{first.ID, second.ID, third.ID}, now.Add(time.Minute)); !errors.Is(err, ErrQueueRevisionConflict) {
		t.Fatalf("stale reorder = %v", err)
	}
	newRevision, err = database.DeleteQueuedOrder(ctx, trader.ID, first.ID, newRevision, now.Add(2*time.Minute))
	if err != nil || newRevision != 5 {
		t.Fatalf("delete revision=%d err=%v", newRevision, err)
	}
	queue, err := database.ListTraderOrders(ctx, trader.ID, date)
	if err != nil || len(queue.Orders) != 2 || queue.Orders[0].ID != third.ID || queue.Orders[0].Position != 1 || queue.Orders[1].ID != second.ID || queue.Orders[1].Position != 2 {
		t.Fatalf("queue = %#v, err=%v", queue, err)
	}
}

func TestV2UpdateCanAtomicallyMoveDateAndRedensifySource(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	create := func(date, symbol string) (domain.QueuedOrder, int64) {
		order, revision, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{ExecutionDate: date, Params: domain.OrderParams{
			Exchange: "NSE", TradingSymbol: symbol, TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Price: "10", Quantity: 1,
		}}, now)
		if err != nil {
			t.Fatal(err)
		}
		return order, revision
	}
	first, _ := create("2026-08-11", "AAA")
	second, sourceRevision := create("2026-08-11", "BBB")
	_, _ = create("2026-08-12", "CCC")
	moved, destinationRevision, err := database.UpdateQueuedOrder(ctx, trader.ID, first.ID, first.ExecutionDate, sourceRevision, domain.QueuedOrder{
		ExecutionDate: "2026-08-12",
		Params:        domain.OrderParams{Exchange: "BSE", TradingSymbol: "MOVED", TransactionType: "BUY", Product: "MIS", OrderType: "MARKET", Quantity: 2},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if moved.ExecutionDate != "2026-08-12" || moved.Position != 2 || moved.Params.Validity != "DAY" || moved.Params.MarketProtection != "-1" || destinationRevision != 2 {
		t.Fatalf("moved = %#v, revision=%d", moved, destinationRevision)
	}
	if _, _, err := database.UpdateQueuedOrder(ctx, trader.ID, first.ID, "2026-08-11", sourceRevision, domain.QueuedOrder{
		ExecutionDate: "2026-08-11", Params: moved.Params,
	}, now.Add(2*time.Minute)); !errors.Is(err, ErrQueueRevisionConflict) {
		t.Fatalf("stale cross-date edit with equal destination revision = %v", err)
	}
	source, err := database.ListTraderOrders(ctx, trader.ID, "2026-08-11")
	if err != nil || len(source.Orders) != 1 || source.Orders[0].ID != second.ID || source.Orders[0].Position != 1 || source.Revision != sourceRevision+1 {
		t.Fatalf("source = %#v, err=%v", source, err)
	}
	byID, err := database.QueuedOrderByID(ctx, first.ID)
	if err != nil || byID.TraderID != trader.ID || byID.ExecutionDate != "2026-08-12" {
		t.Fatalf("QueuedOrderByID = %#v, err=%v", byID, err)
	}
}

func TestV2DeleteTraderIsBlockedAtPreparationCutoffWithoutSchedulerFreezeRow(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	loginAt := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", loginAt)
	order, _, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{
		ExecutionDate: "2026-08-10",
		Params:        domain.OrderParams{Exchange: "NSE", TradingSymbol: "AAA", TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Price: "10", Quantity: 1},
	}, loginAt)
	if err != nil {
		t.Fatal(err)
	}
	// The default 09:00 target freezes at 08:55. This guards the gap where the
	// wall-clock cutoff passed but the scheduler has not inserted its freeze row.
	err = database.DeleteTrader(ctx, trader.ID, istTime(t, "2026-08-10 08:55:00"))
	if err == nil || !strings.Contains(err.Error(), "preparation cutoff") {
		t.Fatalf("DeleteTrader at cutoff = %v", err)
	}
	if got, err := database.TraderByID(ctx, trader.ID); err != nil || got.Deleted() {
		t.Fatalf("trader after rejected deletion = %#v, %v", got, err)
	}
	if got, err := database.QueuedOrder(ctx, trader.ID, order.ID); err != nil || got.State != domain.OrderStateScheduled {
		t.Fatalf("order after rejected deletion = %#v, %v", got, err)
	}
}

func TestV2PrepareUsesExactTargetsAndIndependentInactiveRecords(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	active := onboardTrader(t, database, "AB1234", "key-1", "secret-1", now)
	inactive := onboardTrader(t, database, "CD5678", "key-2", "secret-2", istTime(t, "2026-08-10 05:00:00"))
	date := "2026-08-10"
	newOrder := func(traderID int64, symbol string) domain.QueuedOrder {
		order, _, err := database.CreateQueuedOrder(ctx, traderID, domain.QueuedOrder{ExecutionDate: date, Params: domain.OrderParams{
			Exchange: "NSE", TradingSymbol: symbol, TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 1,
		}}, now)
		if err != nil {
			t.Fatal(err)
		}
		return order
	}
	activeOrder := newOrder(active.ID, "AAA")
	inactiveOrder := newOrder(inactive.ID, "BBB")
	if err := database.FreezeQueuedDate(ctx, date, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	candidates, err := database.QueuedCandidates(ctx, date)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	target := istTime(t, "2026-08-10 09:00:00")
	if prepared, err := database.PrepareQueuedOrders(ctx, date, map[int64]time.Time{activeOrder.ID: target}, now.Add(2*time.Minute)); err != nil || !prepared[activeOrder.ID] {
		t.Fatalf("prepared=%v err=%v", prepared, err)
	}
	if missed, err := database.MarkInactiveQueuedOrders(ctx, date, map[int64]time.Time{inactiveOrder.ID: target}, now.Add(2*time.Minute)); err != nil || !missed[inactiveOrder.ID] {
		t.Fatalf("inactive=%v err=%v", missed, err)
	}
	records, err := database.ListAllDispatches(ctx, date)
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	for _, record := range records {
		if record.Result.RequestCount != 0 || !record.Result.TargetAt.Equal(target) {
			t.Fatalf("record = %#v", record)
		}
	}
	if records[1].Result.Outcome != domain.OrderStateMissedInactive && records[0].Result.Outcome != domain.OrderStateMissedInactive {
		t.Fatalf("missing inactive outcome: %#v", records)
	}
}

func TestV2RecoveryNeverResubmitsPassedTarget(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	date := "2026-08-10"
	create := func(symbol string) domain.QueuedOrder {
		order, _, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{ExecutionDate: date, Params: domain.OrderParams{
			Exchange: "NSE", TradingSymbol: symbol, TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 1,
		}}, now)
		if err != nil {
			t.Fatal(err)
		}
		return order
	}
	passed, future := create("AAA"), create("BBB")
	restartAt := istTime(t, "2026-08-10 09:00:00")
	if _, err := database.PrepareQueuedOrders(ctx, date, map[int64]time.Time{
		passed.ID: restartAt.Add(-time.Millisecond), future.ID: restartAt.Add(time.Second),
	}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := database.RecoverPreparedQueuedOrders(ctx, date, restartAt); err != nil {
		t.Fatal(err)
	}
	unknown, err := database.UnknownQueuedCandidates(ctx, date)
	if err != nil || len(unknown) != 1 || unknown[0].Order.ID != passed.ID {
		t.Fatalf("unknown=%#v err=%v", unknown, err)
	}
	unknownResults, err := database.UnknownQueuedResults(ctx, date)
	if err != nil || len(unknownResults) != 1 || unknownResults[0].RequestCount != 0 {
		t.Fatalf("unknown results=%#v err=%v", unknownResults, err)
	}
	eligible, err := database.QueuedCandidates(ctx, date)
	if err != nil || len(eligible) != 1 || eligible[0].Order.ID != future.ID {
		t.Fatalf("eligible=%#v err=%v", eligible, err)
	}
}

func TestV2DispatchRequestCountCannotExceedOne(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	order, _, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{ExecutionDate: "2026-08-10", Params: domain.OrderParams{
		Exchange: "NSE", TradingSymbol: "AAA", TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 1,
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := istTime(t, "2026-08-10 09:00:00")
	if _, err := database.PrepareQueuedOrders(ctx, order.ExecutionDate, map[int64]time.Time{order.ID: target}, now); err != nil {
		t.Fatal(err)
	}
	err = database.ApplyQueuedDispatchResults(ctx, []domain.DispatchResult{{
		IntentID: order.ID, AccountID: trader.ID, Attempt: 1, RequestCount: 2, Outcome: domain.OrderStateAcknowledged,
	}})
	if err == nil {
		t.Fatal("request_count=2 was accepted")
	}
	if err := database.ApplyQueuedDispatchResults(ctx, []domain.DispatchResult{{
		IntentID: order.ID, AccountID: trader.ID, Attempt: 1, RequestCount: 1,
		TargetAt: target, StartedAt: target, FinishedAt: target.Add(time.Millisecond),
		Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER-1",
	}}); err != nil {
		t.Fatal(err)
	}
	records, err := database.ListTraderDispatches(ctx, trader.ID, order.ExecutionDate)
	if err != nil || len(records) != 1 || records[0].Result.RequestCount != 1 || records[0].Result.OrderID != "ORDER-1" {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	if err := database.ApplyQueuedDispatchResults(ctx, []domain.DispatchResult{{
		IntentID: order.ID, AccountID: trader.ID, Attempt: 1, RequestCount: 1,
		TargetAt: target, StartedAt: target, FinishedAt: target.Add(time.Millisecond),
		Outcome: domain.OrderStateComplete, BrokerStatus: "COMPLETE", OrderID: "ORDER-1",
	}}); err != nil {
		t.Fatal(err)
	}
	updated, err := database.QueuedOrder(ctx, trader.ID, order.ID)
	if err != nil || updated.State != domain.OrderStateComplete {
		t.Fatalf("terminal order = %#v, %v", updated, err)
	}
}

func TestV2DispatchReceiptsPersistInOrderAndDeduplicateRetries(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	order, _, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{
		ExecutionDate: "2026-08-10",
		Params: domain.OrderParams{
			Exchange: "NSE", TradingSymbol: "AAA", TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Quantity: 1, Price: "10",
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := istTime(t, "2026-08-10 09:00:00")
	if _, err := database.PrepareQueuedOrders(ctx, order.ExecutionDate, map[int64]time.Time{order.ID: target}, now); err != nil {
		t.Fatal(err)
	}
	responseAt := target.Add(117*time.Millisecond + 321*time.Microsecond)
	omsAt := target.Add(5 * time.Second)
	exchangeAt := target.Add(5 * time.Second)
	result := domain.DispatchResult{
		IntentID: order.ID, AccountID: trader.ID, Attempt: 1, RequestCount: 1,
		TargetAt: target, StartedAt: target.Add(158 * time.Microsecond), FinishedAt: responseAt,
		Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER-1", ExchangeOrderID: "EXCHANGE-1", BrokerStatus: "OPEN",
		OrderTimestamp: omsAt, ExchangeTimestamp: exchangeAt,
		Receipts: []domain.DispatchReceipt{
			{Source: domain.DispatchReceiptKiteResponse, ObservedAt: responseAt, OrderID: "ORDER-1"},
			{Source: domain.DispatchReceiptOrderBook, ObservedAt: target.Add(time.Second), OrderID: "ORDER-1", BrokerStatus: "OPEN PENDING", OrderTimestamp: target},
			{Source: domain.DispatchReceiptOrderBook, ObservedAt: target.Add(6 * time.Second), OrderID: "ORDER-1", ExchangeOrderID: "EXCHANGE-1", BrokerStatus: "OPEN", OrderTimestamp: omsAt, ExchangeTimestamp: exchangeAt},
		},
	}
	// Persistence retries carry the same in-memory receipt IDs/times. The
	// append-only table must remain idempotent.
	if err := database.ApplyQueuedDispatchResults(ctx, []domain.DispatchResult{result}); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyQueuedDispatchResults(ctx, []domain.DispatchResult{result}); err != nil {
		t.Fatal(err)
	}
	records, err := database.ListTraderDispatches(ctx, trader.ID, order.ExecutionDate)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || len(records[0].Receipts) != 3 {
		t.Fatalf("records = %#v", records)
	}
	receipts := records[0].Receipts
	if receipts[0].Source != domain.DispatchReceiptKiteResponse || !receipts[0].ObservedAt.Equal(responseAt) {
		t.Fatalf("Kite response receipt = %#v", receipts[0])
	}
	if receipts[1].BrokerStatus != "OPEN PENDING" || !receipts[1].OrderTimestamp.Equal(target) || !receipts[1].ExchangeTimestamp.IsZero() {
		t.Fatalf("pending receipt = %#v", receipts[1])
	}
	if receipts[2].BrokerStatus != "OPEN" || receipts[2].ExchangeOrderID != "EXCHANGE-1" || !receipts[2].ExchangeTimestamp.Equal(exchangeAt) {
		t.Fatalf("exchange receipt = %#v", receipts[2])
	}
	if len(records[0].Result.Receipts) != len(receipts) {
		t.Fatalf("result receipt projection = %#v", records[0].Result.Receipts)
	}
}

func TestV2MigrationBackfillsOneCurrentReceiptForExistingDispatch(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	order, _, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{
		ExecutionDate: "2026-08-10",
		Params: domain.OrderParams{
			Exchange: "NSE", TradingSymbol: "AAA", TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 1,
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := istTime(t, "2026-08-10 09:00:00")
	if _, err := database.PrepareQueuedOrders(ctx, order.ExecutionDate, map[int64]time.Time{order.ID: target}, now); err != nil {
		t.Fatal(err)
	}
	exchangeAt := target.Add(5 * time.Second)
	if err := database.ApplyQueuedDispatchResults(ctx, []domain.DispatchResult{{
		IntentID: order.ID, AccountID: trader.ID, RequestCount: 1,
		TargetAt: target, StartedAt: target, FinishedAt: target.Add(120 * time.Millisecond),
		Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER-1", ExchangeOrderID: "EXCHANGE-1",
		BrokerStatus: "OPEN", OrderTimestamp: exchangeAt, ExchangeTimestamp: exchangeAt,
	}}); err != nil {
		t.Fatal(err)
	}
	records, err := database.ListTraderDispatches(ctx, trader.ID, order.ExecutionDate)
	if err != nil || len(records) != 1 || len(records[0].Receipts) != 0 {
		t.Fatalf("pre-migration records = %#v, %v", records, err)
	}
	if err := database.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Running migration repeatedly must not manufacture history.
	if err := database.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	records, err = database.ListTraderDispatches(ctx, trader.ID, order.ExecutionDate)
	if err != nil || len(records) != 1 || len(records[0].Receipts) != 1 {
		t.Fatalf("post-migration records = %#v, %v", records, err)
	}
	receipt := records[0].Receipts[0]
	if receipt.Source != domain.DispatchReceiptCurrentSnapshot || receipt.BrokerStatus != "OPEN" || receipt.ExchangeOrderID != "EXCHANGE-1" || !receipt.ExchangeTimestamp.Equal(exchangeAt) {
		t.Fatalf("backfilled receipt = %#v", receipt)
	}
}

func TestV2DeleteTraderAfterPlacementKeepsRedactedDispatchHistory(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	order, _, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{ExecutionDate: "2026-08-10", Params: domain.OrderParams{
		Exchange: "NSE", TradingSymbol: "AAA", TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 1,
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := istTime(t, "2026-08-10 09:00:00")
	if _, err := database.PrepareQueuedOrders(ctx, order.ExecutionDate, map[int64]time.Time{order.ID: target}, now); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyQueuedDispatchResults(ctx, []domain.DispatchResult{{
		IntentID: order.ID, AccountID: trader.ID, RequestCount: 1, TargetAt: target,
		StartedAt: target, FinishedAt: target.Add(time.Millisecond), Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER-1",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteTrader(ctx, trader.ID, target.Add(time.Minute)); err != nil {
		t.Fatalf("DeleteTrader after placement = %v", err)
	}
	deleted, err := database.TraderByID(ctx, trader.ID)
	if err != nil || !deleted.Deleted() || deleted.APISecret != "" || deleted.AccessToken != "" {
		t.Fatalf("deleted trader = %#v, %v", deleted, err)
	}
	records, err := database.ListAllDispatches(ctx, order.ExecutionDate)
	if err != nil || len(records) != 1 || records[0].ClientID == "AB1234" || records[0].Result.OrderID != "ORDER-1" {
		t.Fatalf("redacted dispatches = %#v, %v", records, err)
	}
}

func TestV2DeleteTraderCryptoErasesAndAllowsReonboarding(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	now := istTime(t, "2026-08-10 07:00:00")
	trader := onboardTrader(t, database, "AB1234", "key", "secret", now)
	if _, _, err := database.CreateQueuedOrder(ctx, trader.ID, domain.QueuedOrder{ExecutionDate: "2026-08-11", Params: domain.OrderParams{
		Exchange: "NSE", TradingSymbol: "AAA", TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 1,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteTrader(ctx, trader.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	deleted, err := database.TraderByID(ctx, trader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Deleted() || deleted.APISecret != "" || deleted.AccessToken != "" || !strings.Contains(deleted.ClientID, "DELETED-") {
		t.Fatalf("deleted trader = %#v", deleted)
	}
	if _, err := database.BeginTraderSignup(ctx, domain.TraderSignup{
		ClientID: "AB1234", APIKey: "key", APISecret: "new-secret",
		InviteToken:       issueSignupInvite(t, database, now.Add(2*time.Minute)),
		PolicyAttestation: domain.PolicyAttestation{Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText},
	}, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("re-onboarding was blocked: %v", err)
	}
}

func TestV2VersionedSettingsAndBlackout(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	if err := database.BootstrapAdmin(ctx, "root", "a-long-root-password"); err != nil {
		t.Fatal(err)
	}
	admin, _ := database.AuthenticateAdmin(ctx, "root", "a-long-root-password")
	defaults, err := database.SettingsForDate(ctx, "2026-08-10")
	if err != nil || defaults.DispatchClock != "09:00:00" || defaults.BatchInterval != 1050*time.Millisecond {
		t.Fatalf("defaults=%#v err=%v", defaults, err)
	}
	next, err := database.ScheduleSettings(ctx, domain.RuntimeSettings{
		DispatchClock: "09:15:00", UserBlackoutStart: "08:30:00", UserBlackoutEnd: "09:45:00",
		FreezeLead: 10 * time.Minute, BatchInterval: 1100 * time.Millisecond, MaxOrdersPerTrader: 100,
	}, "2026-08-11", admin.ID, istTime(t, "2026-08-10 07:00:00"))
	if err != nil || next.Version != 2 {
		t.Fatalf("next=%#v err=%v", next, err)
	}
	before, _ := database.SettingsForDate(ctx, "2026-08-10")
	after, _ := database.SettingsForDate(ctx, "2026-08-11")
	if before.DispatchClock != "09:00:00" || after.DispatchClock != "09:15:00" {
		t.Fatalf("before=%#v after=%#v", before, after)
	}
	if !UserLoginBlocked(after, istTime(t, "2026-08-11 08:30:00")) || UserLoginBlocked(after, istTime(t, "2026-08-11 09:45:00")) {
		t.Fatal("blackout half-open boundary is wrong")
	}
}

func TestV2RuntimeSettingsRequireBlackoutAcrossPreparationAndAllBatches(t *testing.T) {
	settings := domain.RuntimeSettings{
		DispatchClock: "09:00:00", UserBlackoutStart: "08:55:00", UserBlackoutEnd: "09:00:02",
		FreezeLead: 5 * time.Minute, BatchInterval: 1050 * time.Millisecond, MaxOrdersPerTrader: 30,
	}
	if err := ValidateRuntimeSettings(settings); err == nil || !strings.Contains(err.Error(), "full queue-preparation") {
		t.Fatalf("short blackout was accepted: %v", err)
	}
	settings.UserBlackoutStart = "08:54:59"
	settings.UserBlackoutEnd = "09:00:03"
	if err := ValidateRuntimeSettings(settings); err != nil {
		t.Fatalf("covering blackout was rejected: %v", err)
	}
	settings.DispatchClock = "23:50:00"
	settings.UserBlackoutStart = "23:40:00"
	settings.UserBlackoutEnd = "00:02:00"
	if err := ValidateRuntimeSettings(settings); err != nil {
		t.Fatalf("midnight-crossing blackout was rejected: %v", err)
	}
	settings.DispatchClock = "05:00:00"
	settings.UserBlackoutStart = "04:00:00"
	settings.UserBlackoutEnd = "06:00:00"
	if err := ValidateRuntimeSettings(settings); err == nil || !strings.Contains(err.Error(), "after 06:00") {
		t.Fatalf("schedule without same-day activation window was accepted: %v", err)
	}
}
