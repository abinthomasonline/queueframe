package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/secure"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	vault, err := secure.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "test.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOpenBackfillsLegacyMarketProtection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
CREATE TABLE order_intents (
  id INTEGER PRIMARY KEY,
  execution_date TEXT NOT NULL,
  state TEXT NOT NULL,
  order_type TEXT NOT NULL,
  market_protection INTEGER NOT NULL DEFAULT -1
);
INSERT INTO order_intents(id,execution_date,state,order_type,market_protection) VALUES
  (1,'2026-08-10','DRAFT','LIMIT',-1),
  (2,'2026-08-10','DRAFT','MARKET',-1),
  (3,'2026-08-10','DRAFT','LIMIT',0),
  (4,'2026-08-10','DRAFT','MARKET',100);`); err != nil {
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

	rows, err := database.db.Query(`SELECT market_protection_text FROM order_intents ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"0", "-1", "0", "100"}
	if len(got) != len(want) {
		t.Fatalf("backfilled values = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("backfilled values = %#v, want %#v", got, want)
		}
	}
}

func TestAccountOrderAndDispatchLifecycle(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, err := store.Authenticate(ctx, "admin", "a-long-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.CreateAccount(ctx, user.ID, "Primary", "AB1234", "api-key", "api-secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.SaveKiteSession(ctx, account.ID, broker.Session{UserID: "AB1234", APIKey: "api-key", AccessToken: "access-token", LoginAt: now}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	account, err = store.AccountForUser(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.APISecret != "api-secret" || account.AccessToken != "access-token" || !account.Verified {
		t.Fatalf("unexpected decrypted account: %#v", account)
	}

	intent, err := store.CreateOrder(ctx, user.ID, domain.OrderIntent{
		AccountID: account.ID, ExecutionDate: now.Format("2006-01-02"), Priority: 1,
		Params: domain.OrderParams{Exchange: "NSE", TradingSymbol: "ACME", TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 5, Validity: "DAY", MarketProtection: "0.30000000000000004"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(intent.Tag) != 20 {
		t.Fatalf("tag length = %d", len(intent.Tag))
	}
	if err := store.SetOrderArmed(ctx, user.ID, intent.ID, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.ArmedCandidates(ctx, intent.ExecutionDate)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %d, %v", len(candidates), err)
	}
	if got := candidates[0].Intent.Params.MarketProtection; got != "0.30000000000000004" {
		t.Fatalf("market protection = %q, want exact stored decimal", got)
	}
	target := now.Add(time.Minute)
	prepared, err := store.PrepareOrders(ctx, []int64{intent.ID}, target)
	if err != nil || !prepared[intent.ID] {
		t.Fatalf("prepare: %#v, %v", prepared, err)
	}
	orderTimestamp := time.UnixMilli(now.UnixMilli())
	exchangeTimestamp := orderTimestamp.Add(time.Second)
	exchangeUpdateTimestamp := exchangeTimestamp.Add(time.Second)
	result := domain.DispatchResult{
		IntentID: intent.ID, AccountID: account.ID, Tag: intent.Tag, Attempt: 1, RequestCount: 1,
		TargetAt: target, StartedAt: now, FinishedAt: now.Add(time.Millisecond), Outcome: domain.OrderStateAcknowledged, OrderID: "order-1",
		OrderTimestamp: orderTimestamp, ExchangeTimestamp: exchangeTimestamp, ExchangeUpdateTimestamp: exchangeUpdateTimestamp,
	}
	if err := store.ApplyDispatchResults(ctx, []domain.DispatchResult{result}); err != nil {
		t.Fatal(err)
	}
	results, err := store.AttemptResults(ctx, user.ID, intent.ExecutionDate)
	if err != nil || len(results) != 1 || results[0].OrderID != "order-1" || results[0].RequestCount != 1 ||
		!results[0].OrderTimestamp.Equal(orderTimestamp) || !results[0].ExchangeTimestamp.Equal(exchangeTimestamp) ||
		!results[0].ExchangeUpdateTimestamp.Equal(exchangeUpdateTimestamp) {
		t.Fatalf("results = %#v, %v", results, err)
	}
}

func TestRecoverPreparedBeforeTargetReturnsToArmed(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, _ := store.Authenticate(ctx, "admin", "a-long-admin-password")
	account, _ := store.CreateAccount(ctx, user.ID, "Primary", "AB1234", "key", "secret")
	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	intent, _ := store.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	if err := store.SetOrderArmed(ctx, user.ID, intent.ID, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	target := time.Now().Add(time.Hour)
	if _, err := store.PrepareOrders(ctx, []int64{intent.ID}, target); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverPrepared(ctx, date, target, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := store.OrderForUser(ctx, user.ID, intent.ID)
	if err != nil || got.State != domain.OrderStateArmed {
		t.Fatalf("state = %s, %v", got.State, err)
	}
}

func TestFrozenDateRejectsLateArm(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, _ := store.Authenticate(ctx, "admin", "a-long-admin-password")
	account, _ := store.CreateAccount(ctx, user.ID, "Primary", "AB1234", "key", "secret")
	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	intent, _ := store.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	if err := store.FreezeDate(ctx, date); err != nil {
		t.Fatal(err)
	}
	if err := store.SetOrderArmed(ctx, user.ID, intent.ID, true, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("frozen date accepted a new armed order")
	}
}

func TestLivePreparationRejectsDryRunOnlyArm(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, _ := store.Authenticate(ctx, "admin", "a-long-admin-password")
	account, _ := store.CreateAccount(ctx, user.ID, "Primary", "AB1234", "key", "secret")
	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	dryOnly, _ := store.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	live, _ := store.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	deadline := time.Now().Add(time.Hour)
	if err := store.SetOrderArmedMode(ctx, user.ID, dryOnly.ID, true, deadline, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetOrderArmedMode(ctx, user.ID, live.ID, true, deadline, true); err != nil {
		t.Fatal(err)
	}
	if err := store.RejectUnconfirmedLive(ctx, date, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	gotDry, _ := store.OrderForUser(ctx, user.ID, dryOnly.ID)
	gotLive, _ := store.OrderForUser(ctx, user.ID, live.ID)
	if gotDry.State != domain.OrderStateRejected || gotDry.LiveConfirmed {
		t.Fatalf("dry-only order = %#v", gotDry)
	}
	if gotLive.State != domain.OrderStateArmed || !gotLive.LiveConfirmed {
		t.Fatalf("live-confirmed order = %#v", gotLive)
	}
	results, err := store.AttemptResults(ctx, user.ID, date)
	if err != nil || len(results) != 1 || results[0].IntentID != dryOnly.ID || results[0].ErrorType != "LiveConfirmation" {
		t.Fatalf("results = %#v, err = %v", results, err)
	}
}

func TestWriteAuditStoresSecretFreeAction(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, err := store.Authenticate(ctx, "admin", "a-long-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAudit(ctx, user.ID, "ORDER_ARMED", "ORDER", 42, "mode=dry-run"); err != nil {
		t.Fatal(err)
	}
	var eventType, detail string
	if err := store.db.QueryRowContext(ctx, `SELECT event_type,detail FROM audit_events WHERE user_id=? AND subject_id=?`, user.ID, 42).Scan(&eventType, &detail); err != nil {
		t.Fatal(err)
	}
	if eventType != "ORDER_ARMED" || detail != "mode=dry-run" {
		t.Fatalf("event = %q, detail = %q", eventType, detail)
	}
}

func TestApplyDispatchResultsIsIdempotentForMixedTerminalBatch(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, _ := store.Authenticate(ctx, "admin", "a-long-admin-password")
	account, _ := store.CreateAccount(ctx, user.ID, "Primary", "AB1234", "key", "secret")
	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	first, _ := store.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	second, _ := store.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	for _, intent := range []domain.OrderIntent{first, second} {
		if err := store.SetOrderArmed(ctx, user.ID, intent.ID, true, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	target := time.Now().Add(time.Minute)
	if _, err := store.PrepareOrders(ctx, []int64{first.ID, second.ID}, target); err != nil {
		t.Fatal(err)
	}
	results := []domain.DispatchResult{
		{IntentID: first.ID, AccountID: account.ID, Tag: first.Tag, Attempt: 1, TargetAt: target, Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER1"},
		{IntentID: second.ID, AccountID: account.ID, Tag: second.Tag, Attempt: 1, TargetAt: target, Outcome: domain.OrderStateRejected, ErrorType: "InputException"},
	}
	if err := store.ApplyDispatchResults(ctx, results); err != nil {
		t.Fatal(err)
	}
	results[0].ExchangeOrderID = "EXCHANGE1"
	results[0].ExchangeTimestamp = time.UnixMilli(time.Now().UnixMilli())
	if err := store.ApplyDispatchResults(ctx, results); err != nil {
		t.Fatalf("idempotent mixed-batch update failed: %v", err)
	}
}
