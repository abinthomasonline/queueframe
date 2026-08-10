package scheduler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/config"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/secure"
	"github.com/abinthomasonline/queueframe/internal/store"
)

func TestWaitForQueuedFreezeRefreshesSettingsAndLocksFinalVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	database := openQueuedSchedulerStore(t)
	admin, err := database.AuthenticateAdmin(ctx, "admin", "a-long-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	location, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(location)
	if now.Hour() < 6 || (now.Hour() == 6 && now.Minute() == 0 && now.Second() < 2) {
		t.Skip("fixed post-06:00 activation rule prevents a valid near-future wall-clock schedule at this hour")
	}
	if now.Hour() == 23 && now.Minute() == 59 {
		t.Skip("test needs several seconds before the local date changes")
	}
	date := now.Format("2006-01-02")
	settings, err := database.SettingsForDate(ctx, date)
	if err != nil {
		t.Fatal(err)
	}
	settings.DispatchClock = now.Add(2 * time.Second).Format("15:04:05")
	settings.FreezeLead = 250 * time.Millisecond
	settings.UserBlackoutStart = now.Format("15:04:05")
	settings.UserBlackoutEnd = now.Add(30 * time.Second).Format("15:04:05")
	initial, err := database.ScheduleSettings(ctx, settings, date, admin.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	daily, err := New(database, &fakeFactory{client: &schedulerClient{}}, orders.NewCatalog(), config.Config{LateCutoff: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	daily.settingsRefreshEvery = 10 * time.Millisecond
	type observed struct {
		settings domain.RuntimeSettings
		target   time.Time
		err      error
	}
	resultChannel := make(chan observed, 1)
	go func() {
		locked, target, err := daily.waitForQueuedFreeze(ctx, date)
		resultChannel <- observed{settings: locked, target: target, err: err}
	}()

	time.Sleep(100 * time.Millisecond)
	settings.DispatchClock = now.Add(3 * time.Second).Format("15:04:05")
	updated, err := database.ScheduleSettings(ctx, settings, date, admin.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version <= initial.Version {
		t.Fatalf("updated version = %d, initial = %d", updated.Version, initial.Version)
	}

	var result observed
	select {
	case result = <-resultChannel:
	case <-ctx.Done():
		t.Fatal("scheduler did not reach the refreshed freeze boundary")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	wantTarget, err := store.SettingsDispatchAt(updated, date)
	if err != nil {
		t.Fatal(err)
	}
	if result.settings.Version != updated.Version || !result.target.Equal(wantTarget) {
		t.Fatalf("locked settings = v%d at %s, want v%d at %s", result.settings.Version, result.target, updated.Version, wantTarget)
	}
	if _, err := database.ScheduleSettings(ctx, settings, date, admin.ID, time.Now()); !errors.Is(err, store.ErrQueueFrozen) {
		t.Fatalf("post-freeze settings update error = %v, want ErrQueueFrozen", err)
	}
}

func TestPlannedQueuedTargetsUsePositionAndTraderLocalBatches(t *testing.T) {
	base := time.Unix(500, 123)
	settings := domain.RuntimeSettings{BatchInterval: 1050 * time.Millisecond, MaxOrdersPerTrader: 100}
	candidates := []domain.QueueCandidate{
		{Order: domain.QueuedOrder{ID: 11, TraderID: 1, Position: 11}, Trader: domain.Trader{ID: 1}},
		{Order: domain.QueuedOrder{ID: 1, TraderID: 1, Position: 1}, Trader: domain.Trader{ID: 1}},
		{Order: domain.QueuedOrder{ID: 2, TraderID: 2, Position: 1}, Trader: domain.Trader{ID: 2}},
	}
	targets, err := plannedQueuedTargets(base, settings, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !targets[1].Equal(base) || !targets[2].Equal(base) || !targets[11].Equal(base.Add(1050*time.Millisecond)) {
		t.Fatalf("targets = %#v", targets)
	}

	candidates = append(candidates, domain.QueueCandidate{
		Order: domain.QueuedOrder{ID: 12, TraderID: 1, Position: 11}, Trader: domain.Trader{ID: 1},
	})
	if _, err := plannedQueuedTargets(base, settings, candidates); err == nil {
		t.Fatal("duplicate trader queue position was accepted")
	}
}

func TestRunQueuedDayDispatchesOnceAndPersistsExactTimes(t *testing.T) {
	ctx := context.Background()
	database := openQueuedSchedulerStore(t)
	localNow := todayAt(t, 7, 0)
	trader := onboardQueuedTrader(t, database, localNow)
	date := localNow.Format("2006-01-02")
	order := createQueuedSchedulerOrder(t, database, trader.ID, date, 1, localNow)

	client := &schedulerClient{placeStatus: "DRY_RUN"}
	daily, err := New(database, &fakeFactory{client: client}, orders.NewCatalog(), config.Config{
		LateCutoff: time.Second, WarmBefore: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// This is also the durability retry budget; leave enough room for SQLite
	// when the full race suite is running packages concurrently.
	daily.monitorFor = 2 * time.Second
	daily.monitorEvery = time.Millisecond
	settings, err := database.SettingsForDate(ctx, date)
	if err != nil {
		t.Fatal(err)
	}
	target := time.Now().Add(500 * time.Millisecond)
	if err := daily.runQueuedDay(ctx, date, target, settings); err != nil {
		t.Fatal(err)
	}
	records, err := database.ListTraderDispatches(ctx, trader.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	result := records[0].Result
	if records[0].QueuedOrderID != order.ID || result.Outcome != domain.OrderStateAcknowledged || result.RequestCount != 1 {
		t.Fatalf("record = %#v", records[0])
	}
	if !result.TargetAt.Equal(time.UnixMilli(target.UnixMilli())) {
		t.Fatalf("persisted target = %s, want %s", result.TargetAt, time.UnixMilli(target.UnixMilli()))
	}
	if result.StartedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) || result.StartedAt.Before(target) {
		t.Fatalf("timestamps = target %s start %s finish %s", result.TargetAt, result.StartedAt, result.FinishedAt)
	}
	client.mu.Lock()
	placeCalls := client.placeCalls
	client.mu.Unlock()
	if placeCalls != 1 {
		t.Fatalf("placement calls = %d, want exactly 1", placeCalls)
	}
}

func TestRunQueuedDayRecordsInactiveWithoutRequest(t *testing.T) {
	ctx := context.Background()
	database := openQueuedSchedulerStore(t)
	localNow := todayAt(t, 5, 0) // Before the fixed 06:00 daily activation boundary.
	trader := onboardQueuedTrader(t, database, localNow)
	date := localNow.Format("2006-01-02")
	createQueuedSchedulerOrder(t, database, trader.ID, date, 1, localNow)

	client := &schedulerClient{}
	daily, err := New(database, &fakeFactory{client: client}, orders.NewCatalog(), config.Config{
		LateCutoff: time.Second, WarmBefore: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := database.SettingsForDate(ctx, date)
	target := time.Now().Add(50 * time.Millisecond)
	if err := daily.runQueuedDay(ctx, date, target, settings); err != nil {
		t.Fatal(err)
	}
	records, err := database.ListTraderDispatches(ctx, trader.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Result.Outcome != domain.OrderStateMissedInactive || records[0].Result.RequestCount != 0 || !records[0].Result.StartedAt.IsZero() {
		t.Fatalf("inactive records = %#v", records)
	}
	client.mu.Lock()
	placeCalls := client.placeCalls
	client.mu.Unlock()
	if placeCalls != 0 {
		t.Fatalf("inactive trader made %d placement call(s)", placeCalls)
	}
}

func TestRunQueuedDayRestartNeverResubmitsPassedBatchButRunsFutureBatch(t *testing.T) {
	ctx := context.Background()
	database := openQueuedSchedulerStore(t)
	localNow := todayAt(t, 7, 0)
	trader := onboardQueuedTrader(t, database, localNow)
	date := localNow.Format("2006-01-02")
	var queued []domain.QueuedOrder
	for position := 1; position <= 11; position++ {
		queued = append(queued, createQueuedSchedulerOrder(t, database, trader.ID, date, position, localNow))
	}
	settings, _ := database.SettingsForDate(ctx, date)
	base := time.Now().Add(-100 * time.Millisecond)
	// Leave a real two-second runway for batch two. Batch one remains in the
	// past, while race instrumentation or a busy host cannot consume the whole
	// future window during SQLite setup.
	settings.BatchInterval = 2100 * time.Millisecond
	candidates, err := database.QueuedCandidates(ctx, date)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := plannedQueuedTargets(base, settings, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.PrepareQueuedOrders(ctx, date, planned, base.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	// Hold the restart order-book read indefinitely. It must be deferred until
	// the future placement starts; a sleep-based delay makes that ordering test
	// depend on host CPU scheduling.
	releaseReconciliation := make(chan struct{})
	placementStarted := make(chan time.Time, 11)
	client := &schedulerClient{
		placeStatus: "DRY_RUN", placeStarted: placementStarted,
		ordersBlockOnce: releaseReconciliation,
	}
	daily, err := New(database, &fakeFactory{client: client}, orders.NewCatalog(), config.Config{
		LateCutoff: time.Second, WarmBefore: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Race instrumentation can make the first SQLite persistence noticeably
	// slower; keep a realistic bounded reconciliation budget.
	daily.monitorFor = 500 * time.Millisecond
	daily.monitorEvery = 10 * time.Millisecond
	runResult := make(chan error, 1)
	go func() {
		runResult <- daily.runQueuedDay(ctx, date, base, settings)
	}()
	wantStart := base.Add(settings.BatchInterval)
	startDeadline := time.NewTimer(time.Until(wantStart.Add(2 * time.Second)))
	var observedStart time.Time
	select {
	case observedStart = <-placementStarted:
		startDeadline.Stop()
	case <-startDeadline.C:
		close(releaseReconciliation)
		<-runResult
		t.Fatal("future placement did not start while restart reconciliation was blocked")
	}
	close(releaseReconciliation)
	if err := <-runResult; err != nil {
		t.Fatal(err)
	}
	if observedStart.Before(wantStart) || observedStart.After(wantStart.Add(time.Second)) {
		t.Fatalf("future placement started at %s, want near %s despite blocked reconciliation", observedStart, wantStart)
	}
	records, err := database.ListTraderDispatches(ctx, trader.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 11 {
		t.Fatalf("record count = %d, want 11", len(records))
	}
	for index, record := range records {
		if index < 10 {
			if record.Result.Outcome != domain.OrderStateUnknown || record.Result.RequestCount != 0 {
				t.Fatalf("passed record %d = %#v", index+1, record)
			}
			continue
		}
		wantTarget := base.Add(settings.BatchInterval)
		if record.QueuedOrderID != queued[10].ID || record.Result.Outcome != domain.OrderStateAcknowledged || record.Result.RequestCount != 1 || !record.Result.TargetAt.Equal(time.UnixMilli(wantTarget.UnixMilli())) {
			t.Fatalf("future record = %#v, want target %s", record, wantTarget)
		}
	}
	client.mu.Lock()
	placeCalls := client.placeCalls
	placeStartedAt := append([]time.Time(nil), client.placeStartedAt...)
	client.mu.Unlock()
	if placeCalls != 1 {
		t.Fatalf("restart made %d placement calls, want only the one future order", placeCalls)
	}
	if len(placeStartedAt) != 1 || !placeStartedAt[0].Equal(observedStart) {
		t.Fatalf("future placement started at %v, want near %v despite slow reconciliation", placeStartedAt, wantStart)
	}
}

func TestRunQueuedDayDeletedTraderUnknownDoesNotResubmitOrWedge(t *testing.T) {
	ctx := context.Background()
	database := openQueuedSchedulerStore(t)
	localNow := todayAt(t, 7, 0)
	trader := onboardQueuedTrader(t, database, localNow)
	date := localNow.Format("2006-01-02")
	order := createQueuedSchedulerOrder(t, database, trader.ID, date, 1, localNow)
	settings, err := database.SettingsForDate(ctx, date)
	if err != nil {
		t.Fatal(err)
	}
	target := time.Now().Add(-time.Second)
	if _, err := database.PrepareQueuedOrders(ctx, date, map[int64]time.Time{order.ID: target}, localNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := database.RecoverPreparedQueuedOrders(ctx, date, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteTrader(ctx, trader.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	client := &schedulerClient{}
	daily, err := New(database, &fakeFactory{client: client}, orders.NewCatalog(), config.Config{
		LateCutoff: time.Second, WarmBefore: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The same window also bounds the durable recovery write. Leave enough
	// headroom for SQLite under the race detector; there are no broker jobs in
	// this scenario, so the test still returns immediately after persistence.
	daily.monitorFor = 500 * time.Millisecond
	daily.monitorEvery = time.Millisecond
	if err := daily.runQueuedDay(ctx, date, target, settings); err != nil {
		t.Fatalf("same-day restart after trader deletion: %v", err)
	}

	client.mu.Lock()
	placeCalls := client.placeCalls
	client.mu.Unlock()
	if placeCalls != 0 {
		t.Fatalf("deleted trader UNKNOWN made %d placement call(s), want zero", placeCalls)
	}
	results, err := database.UnknownQueuedResults(ctx, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].IntentID != order.ID ||
		!strings.Contains(results[0].ErrorMessage, "local trader profile was deleted") {
		t.Fatalf("durable deleted-trader UNKNOWN = %#v", results)
	}
}

func openQueuedSchedulerStore(t *testing.T) *store.Store {
	t.Helper()
	vault, err := secure.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "queued-scheduler.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.BootstrapAdmin(context.Background(), "admin", "a-long-admin-password"); err != nil {
		t.Fatal(err)
	}
	return database
}

func todayAt(t *testing.T, hour, minute int) time.Time {
	t.Helper()
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(location)
	setup := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, location)
	if !setup.Before(now) {
		setup = setup.AddDate(0, 0, -1)
	}
	// Keep the fixture on a trading weekday while remaining before wall-clock
	// execution, including tests that happen to cross midnight or a weekend.
	for setup.Weekday() == time.Saturday || setup.Weekday() == time.Sunday {
		setup = setup.AddDate(0, 0, -1)
	}
	return setup
}

func onboardQueuedTrader(t *testing.T, database *store.Store, loginAt time.Time) domain.Trader {
	t.Helper()
	ctx := context.Background()
	admins, err := database.ListAdmins(ctx)
	if err != nil || len(admins) != 1 {
		t.Fatalf("test administrator = %#v, %v", admins, err)
	}
	_, inviteToken, err := database.CreateSignupInvitation(ctx, admins[0].ID, 24*time.Hour, loginAt)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := database.BeginTraderSignup(ctx, domain.TraderSignup{
		ClientID: "AB1234", APIKey: "key", APISecret: "secret",
		InviteToken: inviteToken,
		PolicyAttestation: domain.PolicyAttestation{
			Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText,
		},
	}, loginAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConsumeKiteAuthFlow(ctx, flow.State, loginAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	trader, err := database.CompleteKiteAuthAt(ctx, flow.ID, broker.Session{
		UserID: "AB1234", APIKey: "key", AccessToken: "token", LoginAt: loginAt,
	}, loginAt.Add(7*24*time.Hour), loginAt.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return trader
}

func createQueuedSchedulerOrder(t *testing.T, database *store.Store, traderID int64, date string, position int, now time.Time) domain.QueuedOrder {
	t.Helper()
	order, _, err := database.CreateQueuedOrder(context.Background(), traderID, domain.QueuedOrder{
		ExecutionDate: date,
		Params: domain.OrderParams{
			Exchange: "NSE", TradingSymbol: fmt.Sprintf("ACME%d", position), TransactionType: "SELL",
			Product: "CNC", OrderType: "LIMIT", Quantity: 1, Price: "10", Validity: "DAY",
		},
	}, now.Add(time.Duration(position)*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if order.Position != position {
		t.Fatalf("created position = %d, want %d", order.Position, position)
	}
	return order
}
