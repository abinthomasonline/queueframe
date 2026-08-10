package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/config"
	"github.com/abinthomasonline/queueframe/internal/dispatch"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/markettime"
	"github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/secure"
	"github.com/abinthomasonline/queueframe/internal/store"
)

type fakeFactory struct{ client *schedulerClient }

func (f *fakeFactory) LoginURL(string, string) string { return "" }
func (f *fakeFactory) GenerateSession(context.Context, string, string, string) (broker.Session, error) {
	return broker.Session{}, nil
}
func (f *fakeFactory) Client(string, string) broker.Client { return f.client }

type schedulerClient struct {
	mu                 sync.Mutex
	placeStatus        string
	orders             []broker.Order
	orderResponses     [][]broker.Order
	ordersErr          error
	profileResults     []error
	profileDefaultErr  error
	profileCalls       int
	instrumentFailures int
	instrumentErr      error
	instrumentCalls    int
	placeCalls         int
	placeStartedAt     []time.Time
	placeStarted       chan time.Time
	ordersCalls        int
	ordersDelayOnce    time.Duration
	ordersBlockOnce    <-chan struct{}
}

func (c *schedulerClient) Profile(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.profileCalls++
	err := c.profileDefaultErr
	if len(c.profileResults) > 0 {
		err = c.profileResults[0]
		c.profileResults = c.profileResults[1:]
	}
	if err != nil {
		return "", err
	}
	return "AB1234", nil
}
func (c *schedulerClient) Instruments(_ context.Context, exchange string) ([]domain.Instrument, error) {
	c.mu.Lock()
	c.instrumentCalls++
	if c.instrumentFailures > 0 {
		c.instrumentFailures--
		err := c.instrumentErr
		c.mu.Unlock()
		if err == nil {
			err = errors.New("temporary instrument download failure")
		}
		return nil, err
	}
	c.mu.Unlock()
	return []domain.Instrument{{Exchange: exchange, TradingSymbol: "ACME", InstrumentType: "EQ", Segment: exchange}}, nil
}
func (c *schedulerClient) PlaceRegularOrder(_ context.Context, params domain.OrderParams, tag string) (broker.Order, error) {
	startedAt := time.Now()
	c.mu.Lock()
	c.placeCalls++
	c.placeStartedAt = append(c.placeStartedAt, startedAt)
	status := c.placeStatus
	started := c.placeStarted
	c.mu.Unlock()
	if started != nil {
		started <- startedAt
	}
	return broker.Order{OrderID: "ORDER1", Status: status, Tag: tag, Params: params}, nil
}
func (c *schedulerClient) Orders(context.Context) ([]broker.Order, error) {
	c.mu.Lock()
	c.ordersCalls++
	delay := c.ordersDelayOnce
	c.ordersDelayOnce = 0
	block := c.ordersBlockOnce
	c.ordersBlockOnce = nil
	orders := c.orders
	if len(c.orderResponses) > 0 {
		orders = c.orderResponses[0]
		c.orderResponses = c.orderResponses[1:]
	}
	orders = append([]broker.Order(nil), orders...)
	err := c.ordersErr
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return orders, err
}
func (*schedulerClient) InitiateHoldingsAuthorization(context.Context) (string, error) {
	return "", nil
}

func TestRunDayPreparesDispatchesAndFlushes(t *testing.T) {
	ctx := context.Background()
	vault, _ := secure.NewVault(make([]byte, 32))
	database, err := store.Open(filepath.Join(t.TempDir(), "scheduler.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, _ := database.Authenticate(ctx, "admin", "a-long-admin-password")
	account, _ := database.CreateAccount(ctx, user.ID, "Primary", "AB1234", "key", "secret")
	now := time.Now()
	if err := database.SaveKiteSession(ctx, account.ID, broker.Session{UserID: "AB1234", APIKey: "key", AccessToken: "token", LoginAt: now}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	date := now.Format("2006-01-02")
	intent, err := database.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Priority: 1, Params: domain.OrderParams{Exchange: "NSE", TradingSymbol: "ACME", TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Quantity: 1, Price: "10", Validity: "DAY", MarketProtection: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetOrderArmedMode(ctx, user.ID, intent.ID, true, time.Now().Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	orderTimestamp := time.UnixMilli(now.UnixMilli())
	exchangeTimestamp := orderTimestamp.Add(time.Second)
	factory := &fakeFactory{client: &schedulerClient{orders: []broker.Order{{
		OrderID: "ORDER1", ExchangeOrderID: "EXCHANGE1", Status: "OPEN",
		OrderTimestamp: orderTimestamp, ExchangeTimestamp: exchangeTimestamp,
	}}}}
	scheduler, err := New(database, factory, orders.NewCatalog(), config.Config{LateCutoff: time.Second, WarmBefore: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.monitorEvery = 5 * time.Millisecond
	scheduler.monitorFor = 100 * time.Millisecond
	// Leave headroom for instrument/store work under the full race suite; the
	// assertion concerns ordering and persistence, not an artificial 80ms SLA.
	target := time.Now().Add(500 * time.Millisecond)
	if err := scheduler.runDay(ctx, date, target); err != nil {
		t.Fatal(err)
	}
	results, err := database.AttemptResults(ctx, user.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != domain.OrderStateAcknowledged || results[0].OrderID != "ORDER1" || results[0].ExchangeOrderID != "EXCHANGE1" || results[0].BrokerStatus != "OPEN" ||
		!results[0].OrderTimestamp.Equal(orderTimestamp) || !results[0].ExchangeTimestamp.Equal(exchangeTimestamp) {
		t.Fatalf("results = %#v", results)
	}
}

func TestVerifyAccountsRetriesTransientProfileFailure(t *testing.T) {
	client := &schedulerClient{profileResults: []error{errors.New("temporary profile failure"), nil}}
	scheduler := &Scheduler{
		factory: &fakeFactory{client: client}, config: config.Config{WarmBefore: 10 * time.Millisecond},
		readinessCallTimeout: 10 * time.Millisecond, readinessRetryMin: time.Millisecond, readinessRetryMax: time.Millisecond,
	}
	target := time.Now().Add(80 * time.Millisecond)
	candidate := store.Candidate{Account: domain.Account{
		ID: 1, ClientID: "AB1234", APIKey: "key", AccessToken: "token", Verified: true,
		SessionExpiresAt: target.Add(time.Hour),
	}}

	clients, accountErrors := scheduler.verifyAccounts(context.Background(), target, []store.Candidate{candidate})
	if accountErrors[1] != nil || clients[1] != client {
		t.Fatalf("clients = %#v, errors = %#v", clients, accountErrors)
	}
	client.mu.Lock()
	profileCalls := client.profileCalls
	client.mu.Unlock()
	if profileCalls != 2 {
		t.Fatalf("profile calls = %d, want 2", profileCalls)
	}
}

func TestRefreshCatalogRetriesTransientDownloadFailure(t *testing.T) {
	client := &schedulerClient{instrumentFailures: 1}
	scheduler := &Scheduler{
		catalog: orders.NewCatalog(), readinessCallTimeout: 10 * time.Millisecond,
		readinessRetryMin: time.Millisecond, readinessRetryMax: time.Millisecond,
	}
	if err := scheduler.refreshCatalog(context.Background(), map[int64]broker.Client{1: client}, time.Now().Add(50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	instrumentCalls := client.instrumentCalls
	client.mu.Unlock()
	if instrumentCalls < 4 {
		t.Fatalf("instrument calls = %d, want a retried NSE/BSE download", instrumentCalls)
	}
}

func TestWarmFailureDoesNotSuppressPreparedJob(t *testing.T) {
	client := &schedulerClient{profileDefaultErr: errors.New("temporary warm-up failure")}
	scheduler := &Scheduler{
		readinessCallTimeout: 5 * time.Millisecond, readinessRetryMin: time.Millisecond, readinessRetryMax: 2 * time.Millisecond,
	}
	job := dispatch.PreparedJob{IntentID: 1, AccountID: 1, Client: client, Tag: "tag"}
	active, rejected := scheduler.warmClients(context.Background(), time.Now().Add(15*time.Millisecond), []dispatch.PreparedJob{job})
	if len(active) != 1 || active[0].IntentID != job.IntentID || len(rejected) != 0 {
		t.Fatalf("active = %#v, rejected = %#v", active, rejected)
	}
}

func TestRunDayReconcilesRestartUnknownWithoutResubmitting(t *testing.T) {
	ctx := context.Background()
	vault, _ := secure.NewVault(make([]byte, 32))
	database, err := store.Open(filepath.Join(t.TempDir(), "restart.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, _ := database.Authenticate(ctx, "admin", "a-long-admin-password")
	account, _ := database.CreateAccount(ctx, user.ID, "Primary", "AB1234", "key", "secret")
	now := time.Now()
	if err := database.SaveKiteSession(ctx, account.ID, broker.Session{UserID: "AB1234", APIKey: "key", AccessToken: "token", LoginAt: now}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	date := now.Format("2006-01-02")
	params := domain.OrderParams{Exchange: "NSE", TradingSymbol: "ACME", TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Quantity: 1, Price: "10", Validity: "DAY", MarketProtection: "0"}
	intent, err := database.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Priority: 1, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetOrderArmed(ctx, user.ID, intent.ID, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	target := time.Now().Add(-50 * time.Millisecond)
	if _, err := database.PrepareOrders(ctx, []int64{intent.ID}, target); err != nil {
		t.Fatal(err)
	}
	client := &schedulerClient{orders: []broker.Order{{
		OrderID: "RECOVERED1", ExchangeOrderID: "EXCHANGE1", Status: "REJECTED", StatusMessage: "insufficient holdings",
		Tag: intent.Tag, Params: params,
	}}}
	scheduler, err := New(database, &fakeFactory{client: client}, orders.NewCatalog(), config.Config{LateCutoff: time.Second, WarmBefore: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.monitorEvery = time.Millisecond
	scheduler.monitorFor = 30 * time.Millisecond
	if err := scheduler.runDay(ctx, date, target); err != nil {
		t.Fatal(err)
	}
	results, err := database.AttemptResults(ctx, user.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != domain.OrderStateRejected || results[0].RequestCount != 1 || results[0].OrderID != "RECOVERED1" || results[0].BrokerStatus != "REJECTED" {
		t.Fatalf("results = %#v", results)
	}
	if !strings.Contains(results[0].ErrorMessage, "insufficient holdings") {
		t.Fatalf("status message was not persisted: %#v", results[0])
	}
	client.mu.Lock()
	placeCalls := client.placeCalls
	client.mu.Unlock()
	if placeCalls != 0 {
		t.Fatalf("restart recovery resubmitted %d order(s)", placeCalls)
	}
}

func TestRecoverStaleDatesNeverResubmitsPastWork(t *testing.T) {
	ctx := context.Background()
	vault, _ := secure.NewVault(make([]byte, 32))
	database, err := store.Open(filepath.Join(t.TempDir(), "stale.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.BootstrapAdmin(ctx, "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	user, _ := database.Authenticate(ctx, "admin", "a-long-admin-password")
	account, _ := database.CreateAccount(ctx, user.ID, "Primary", "AB1234", "key", "secret")
	location, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(location)
	date := now.AddDate(0, 0, -1).Format("2006-01-02")
	armed, _ := database.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	prepared, _ := database.CreateOrder(ctx, user.ID, domain.OrderIntent{AccountID: account.ID, ExecutionDate: date, Params: domain.OrderParams{}})
	for _, intent := range []domain.OrderIntent{armed, prepared} {
		if err := database.SetOrderArmed(ctx, user.ID, intent.ID, true, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	target, _ := markettime.Target(date, "09:00:00", location)
	if _, err := database.PrepareOrders(ctx, []int64{prepared.ID}, target); err != nil {
		t.Fatal(err)
	}
	client := &schedulerClient{}
	scheduler, err := New(database, &fakeFactory{client: client}, orders.NewCatalog(), config.Config{DispatchClock: "09:00:00", LateCutoff: time.Second, WarmBefore: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.recoverStaleDates(ctx, now.Format("2006-01-02"), now); err != nil {
		t.Fatal(err)
	}
	gotArmed, _ := database.OrderForUser(ctx, user.ID, armed.ID)
	gotPrepared, _ := database.OrderForUser(ctx, user.ID, prepared.ID)
	if gotArmed.State != domain.OrderStateMissed || gotPrepared.State != domain.OrderStateUnknown {
		t.Fatalf("states = (%s, %s), want (MISSED, UNKNOWN)", gotArmed.State, gotPrepared.State)
	}
	client.mu.Lock()
	placeCalls := client.placeCalls
	client.mu.Unlock()
	if placeCalls != 0 {
		t.Fatalf("stale recovery resubmitted %d order(s)", placeCalls)
	}
}

func TestMonitorRetriesDirtyPersistenceAfterTerminalUpdate(t *testing.T) {
	client := &schedulerClient{orders: []broker.Order{{
		OrderID: "ORDER1", ExchangeOrderID: "EXCHANGE1", Status: "REJECTED", StatusMessage: "RMS: blocked",
	}}}
	var mu sync.Mutex
	persistCalls := 0
	var persisted domain.DispatchResult
	scheduler := &Scheduler{
		monitorEvery: time.Millisecond, monitorFor: 30 * time.Millisecond,
		persistenceRetryMin: time.Millisecond, persistenceRetryMax: 2 * time.Millisecond,
		persistDispatchResults: func(_ context.Context, results []domain.DispatchResult) error {
			mu.Lock()
			defer mu.Unlock()
			persistCalls++
			if persistCalls == 1 {
				return errors.New("temporary SQLite failure")
			}
			persisted = results[0]
			return nil
		},
	}
	job := dispatch.PreparedJob{IntentID: 1, AccountID: 1, Client: client, Tag: "tag"}
	results := []domain.DispatchResult{{IntentID: 1, AccountID: 1, Tag: "tag", Attempt: 1, Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER1"}}
	scheduler.monitorResults(context.Background(), time.Now(), []dispatch.PreparedJob{job}, results)

	mu.Lock()
	calls := persistCalls
	saved := persisted
	mu.Unlock()
	if calls < 2 {
		t.Fatalf("persistence calls = %d, want a dirty retry", calls)
	}
	if saved.Outcome != domain.OrderStateRejected || saved.BrokerStatus != "REJECTED" || saved.ExchangeOrderID != "EXCHANGE1" || !strings.Contains(saved.ErrorMessage, "RMS: blocked") {
		t.Fatalf("persisted result = %#v", saved)
	}
}

func TestMonitorPersistsDistinctPostPlacementReceipts(t *testing.T) {
	target := time.Now().Add(-time.Second)
	responseAt := target.Add(120 * time.Millisecond)
	omsAt := target
	exchangeAt := target.Add(5 * time.Second)
	client := &schedulerClient{orderResponses: [][]broker.Order{
		{{OrderID: "ORDER1", Status: "OPEN PENDING", OrderTimestamp: omsAt}},
		{{OrderID: "ORDER1", ExchangeOrderID: "EXCHANGE1", Status: "OPEN", OrderTimestamp: exchangeAt, ExchangeTimestamp: exchangeAt}},
	}}
	var persisted domain.DispatchResult
	scheduler := &Scheduler{
		monitorEvery: time.Millisecond, monitorFor: 100 * time.Millisecond,
		persistenceRetryMin: time.Millisecond, persistenceRetryMax: 2 * time.Millisecond,
		persistDispatchResults: func(_ context.Context, results []domain.DispatchResult) error {
			persisted = results[0]
			persisted.Receipts = append([]domain.DispatchReceipt(nil), results[0].Receipts...)
			return nil
		},
	}
	results := []domain.DispatchResult{{
		IntentID: 1, AccountID: 1, Tag: "tag", Attempt: 1, RequestCount: 1,
		TargetAt: target, StartedAt: target, FinishedAt: responseAt,
		Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER1",
	}}
	// This happens only after the placement result channel has closed in the
	// queued scheduler.
	appendKiteResponseReceipts(results)
	job := dispatch.PreparedJob{IntentID: 1, AccountID: 1, Client: client, Tag: "tag"}
	scheduler.monitorResults(context.Background(), target, []dispatch.PreparedJob{job}, results)

	if len(persisted.Receipts) != 3 {
		t.Fatalf("receipts = %#v", persisted.Receipts)
	}
	if persisted.Receipts[0].Source != domain.DispatchReceiptKiteResponse || !persisted.Receipts[0].ObservedAt.Equal(responseAt) {
		t.Fatalf("placement receipt = %#v", persisted.Receipts[0])
	}
	if persisted.Receipts[1].Source != domain.DispatchReceiptOrderBook || persisted.Receipts[1].BrokerStatus != "OPEN PENDING" || !persisted.Receipts[1].ExchangeTimestamp.IsZero() {
		t.Fatalf("pending receipt = %#v", persisted.Receipts[1])
	}
	if persisted.Receipts[2].BrokerStatus != "OPEN" || persisted.Receipts[2].ExchangeOrderID != "EXCHANGE1" || !persisted.Receipts[2].ExchangeTimestamp.Equal(exchangeAt) {
		t.Fatalf("exchange receipt = %#v", persisted.Receipts[2])
	}
	if persisted.Receipts[1].ObservedAt.After(persisted.Receipts[2].ObservedAt) {
		t.Fatalf("receipt observation order = %#v", persisted.Receipts)
	}
	client.mu.Lock()
	ordersCalls := client.ordersCalls
	client.mu.Unlock()
	if ordersCalls != 2 {
		t.Fatalf("order-book GETs = %d, want the existing two monitoring reads", ordersCalls)
	}
}

func TestUpdateResultFromOrderMapsTerminalBrokerOutcome(t *testing.T) {
	for _, test := range []struct {
		status string
		want   string
	}{
		{status: "COMPLETE", want: domain.OrderStateComplete},
		{status: "REJECTED", want: domain.OrderStateRejected},
		{status: "CANCELLED", want: domain.OrderStateCancelled},
	} {
		t.Run(test.status, func(t *testing.T) {
			result := domain.DispatchResult{Outcome: domain.OrderStateAcknowledged}
			if !updateResultFromOrder(&result, broker.Order{Status: test.status}) {
				t.Fatal("terminal result was not reported as changed")
			}
			if result.Outcome != test.want || result.BrokerStatus != test.status {
				t.Fatalf("result = %#v, want outcome %s", result, test.want)
			}
		})
	}
}

func TestPersistenceAttemptUsesRemainingDeadline(t *testing.T) {
	var observedDeadline time.Time
	scheduler := &Scheduler{
		persistenceRetryMin: time.Millisecond,
		persistenceRetryMax: 2 * time.Millisecond,
		persistDispatchResults: func(ctx context.Context, _ []domain.DispatchResult) error {
			var ok bool
			observedDeadline, ok = ctx.Deadline()
			if !ok {
				t.Fatal("persistence context has no deadline")
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	deadline := time.Now().Add(20 * time.Millisecond)
	started := time.Now()
	err := scheduler.persistResultsWithRetry([]domain.DispatchResult{{IntentID: 1}}, deadline)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("persist error = %v, want deadline exceeded", err)
	}
	if observedDeadline.After(deadline) {
		t.Fatalf("attempt deadline %s exceeds persistence deadline %s", observedDeadline, deadline)
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("persistence exceeded bounded deadline: %s", elapsed)
	}
}

func TestPostCutoffDrainContextSurvivesCancellationButIsBounded(t *testing.T) {
	scheduler := &Scheduler{postCutoffRecoveryFor: 100 * time.Millisecond}
	parent, cancelParent := context.WithCancel(context.Background())
	drainCtx, cancelDrain := scheduler.postCutoffDrainContext(parent)
	defer cancelDrain()
	cancelParent()

	select {
	case <-drainCtx.Done():
		t.Fatal("drain context was canceled with its parent")
	case <-time.After(10 * time.Millisecond):
	}
	select {
	case <-drainCtx.Done():
		if !errors.Is(drainCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("drain context error = %v", drainCtx.Err())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("drain context exceeded its fixed budget")
	}
}

func TestMonitorSkipsOrderBookWhenRecoveryBudgetCannotFitCall(t *testing.T) {
	client := &schedulerClient{}
	scheduler := &Scheduler{
		monitorEvery:         time.Millisecond,
		monitorFor:           time.Minute,
		readinessCallReserve: 20 * time.Millisecond,
		persistenceRetryMax:  2 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	job := dispatch.PreparedJob{IntentID: 1, AccountID: 1, Client: client, Tag: "tag"}
	results := []domain.DispatchResult{{IntentID: 1, AccountID: 1, Tag: "tag", Attempt: 1, Outcome: domain.OrderStateUnknown}}

	scheduler.monitorResults(ctx, time.Now(), []dispatch.PreparedJob{job}, results)
	client.mu.Lock()
	ordersCalls := client.ordersCalls
	client.mu.Unlock()
	if ordersCalls != 0 {
		t.Fatalf("orderbook calls = %d, want 0 with insufficient recovery budget", ordersCalls)
	}
}
