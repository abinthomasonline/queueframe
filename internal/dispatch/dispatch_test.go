package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

type fakeClient struct {
	mu        sync.Mutex
	started   []string
	startedAt map[string]time.Time
	responses map[string][]error
	orders    []broker.Order
	block     map[string]<-chan struct{}
	panics    map[string]any
	startCh   chan string
}

// immediateTestDispatcher keeps behavior-only tests independent of host CPU
// scheduling. Admission happens against a fixed pre-target clock and the
// injected timer releases the already-prepared workers immediately.
func immediateTestDispatcher(t *testing.T) (*Dispatcher, time.Time) {
	t.Helper()
	now := time.Unix(1_700_000_000, 123)
	dispatcher, err := New(Config{
		LateCutoff: time.Second,
		Now:        func() time.Time { return now },
		After: func(time.Duration) <-chan time.Time {
			fired := make(chan time.Time, 1)
			fired <- now
			return fired
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, now.Add(time.Second)
}

func (f *fakeClient) Profile(context.Context) (string, error) { return "AB1234", nil }
func (f *fakeClient) Instruments(context.Context, string) ([]domain.Instrument, error) {
	return nil, nil
}
func (f *fakeClient) InitiateHoldingsAuthorization(context.Context) (string, error) {
	return "", nil
}
func (f *fakeClient) Orders(context.Context) ([]broker.Order, error) { return f.orders, nil }
func (f *fakeClient) PlaceRegularOrder(_ context.Context, params domain.OrderParams, tag string) (broker.Order, error) {
	f.mu.Lock()
	f.started = append(f.started, tag)
	if f.startedAt == nil {
		f.startedAt = make(map[string]time.Time)
	}
	f.startedAt[tag] = time.Now()
	var err error
	if responses := f.responses[tag]; len(responses) > 0 {
		err = responses[0]
		f.responses[tag] = responses[1:]
	}
	block := f.block[tag]
	panicValue := f.panics[tag]
	startCh := f.startCh
	f.mu.Unlock()
	if startCh != nil {
		startCh <- tag
	}
	if block != nil {
		<-block
	}
	if panicValue != nil {
		panic(panicValue)
	}
	if err != nil {
		return broker.Order{}, err
	}
	return broker.Order{OrderID: "OID-" + tag, Tag: tag, Params: params}, nil
}

func TestPlannedTargetsAreAccountLocalAndExact(t *testing.T) {
	dispatcher, err := New(Config{LateCutoff: time.Second, BatchSize: 10, BatchInterval: 1050 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{}
	base := time.Unix(123, 456)
	jobs := make([]PreparedJob, 0, 23)
	for position := 1; position <= 21; position++ {
		jobs = append(jobs, PreparedJob{IntentID: int64(position), AccountID: 1, Position: position, Client: client, Tag: fmt.Sprintf("A%d", position)})
	}
	jobs = append(jobs,
		PreparedJob{IntentID: 22, AccountID: 2, Position: 2, Client: client, Tag: "B2"},
		PreparedJob{IntentID: 23, AccountID: 2, Position: 1, Client: client, Tag: "B1"},
	)
	targets, err := dispatcher.PlannedTargets(base, jobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id   int64
		want time.Time
	}{{1, base}, {10, base}, {11, base.Add(1050 * time.Millisecond)}, {20, base.Add(1050 * time.Millisecond)}, {21, base.Add(2100 * time.Millisecond)}, {22, base}, {23, base}} {
		if got := targets[test.id]; !got.Equal(test.want) {
			t.Fatalf("target[%d] = %s, want %s", test.id, got, test.want)
		}
	}
	resumed, err := dispatcher.PlannedTargets(base, []PreparedJob{{IntentID: 99, AccountID: 1, Position: 11, Client: client, Tag: "RESUMED"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := base.Add(1050 * time.Millisecond); !resumed[99].Equal(want) {
		t.Fatalf("resumed position 11 target = %s, want %s", resumed[99], want)
	}
}

func TestBatchWorkersStartConcurrently(t *testing.T) {
	release := make(chan struct{})
	client := &fakeClient{startCh: make(chan string, 10), block: make(map[string]<-chan struct{})}
	jobs := make([]PreparedJob, 0, 10)
	for position := 1; position <= 10; position++ {
		tag := fmt.Sprintf("J%d", position)
		client.block[tag] = release
		jobs = append(jobs, PreparedJob{IntentID: int64(position), AccountID: 1, Position: position, Client: client, Tag: tag})
	}
	const maximumStartSkew = 2 * time.Second
	dispatcher, _ := New(Config{LateCutoff: 5 * time.Second})
	// Keep this as a real timer test, with enough admission runway for race
	// instrumentation and a busy test host. The skew assertion below still
	// verifies that one release starts the whole batch together.
	target := time.Now().Add(250 * time.Millisecond)
	results, err := dispatcher.RunBatch(context.Background(), target, jobs)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Until(target.Add(5 * time.Second)))
	defer deadline.Stop()
	for range jobs {
		select {
		case <-client.startCh:
		case <-deadline.C:
			close(release)
			t.Fatal("not all ten workers reached PlaceRegularOrder before a sibling completed")
		}
	}
	client.mu.Lock()
	starts := make([]time.Time, 0, len(client.startedAt))
	for _, startedAt := range client.startedAt {
		starts = append(starts, startedAt)
	}
	client.mu.Unlock()
	if len(starts) != len(jobs) {
		close(release)
		t.Fatalf("recorded starts = %d, want %d", len(starts), len(jobs))
	}
	earliest, latest := starts[0], starts[0]
	for _, startedAt := range starts[1:] {
		if startedAt.Before(earliest) {
			earliest = startedAt
		}
		if startedAt.After(latest) {
			latest = startedAt
		}
	}
	if earliest.Before(target) {
		close(release)
		t.Fatalf("worker started at %s before target %s", earliest, target)
	}
	if skew := latest.Sub(earliest); skew > maximumStartSkew {
		close(release)
		t.Fatalf("batch worker start skew = %s, want <= %s", skew, maximumStartSkew)
	}
	close(release)
	for result := range results {
		if result.RequestCount != 1 || result.Outcome != domain.OrderStateAcknowledged || !result.TargetAt.Equal(target) {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestLaterAbsoluteBatchDoesNotWaitForEarlierResponses(t *testing.T) {
	releaseFirst := make(chan struct{})
	client := &fakeClient{startCh: make(chan string, 11), block: make(map[string]<-chan struct{})}
	jobs := make([]PreparedJob, 0, 11)
	for position := 1; position <= 11; position++ {
		tag := fmt.Sprintf("J%d", position)
		if position <= 10 {
			client.block[tag] = releaseFirst
		}
		jobs = append(jobs, PreparedJob{IntentID: int64(position), AccountID: 1, Position: position, Client: client, Tag: tag})
	}
	now := time.Unix(1_700_000_000, 456)
	target := now.Add(time.Second)
	firstTimer := make(chan time.Time, 1)
	secondTimer := make(chan time.Time, 1)
	dispatcher, _ := New(Config{
		LateCutoff: time.Second, BatchInterval: time.Second,
		Now: func() time.Time { return now },
		After: func(delay time.Duration) <-chan time.Time {
			if delay == time.Second {
				return firstTimer
			}
			return secondTimer
		},
	})
	results, err := dispatcher.RunBatch(context.Background(), target, jobs)
	if err != nil {
		t.Fatal(err)
	}
	firstTimer <- target
	seen := make(map[string]bool)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(seen) < 10 {
		select {
		case tag := <-client.startCh:
			if tag == "J11" {
				close(releaseFirst)
				t.Fatal("second batch started before its independent absolute timer")
			}
			seen[tag] = true
		case <-deadline.C:
			close(releaseFirst)
			t.Fatal("first batch workers did not start")
		}
	}
	secondTimer <- target.Add(time.Second)
	for !seen["J11"] {
		select {
		case tag := <-client.startCh:
			seen[tag] = true
		case <-deadline.C:
			close(releaseFirst)
			t.Fatal("second batch waited for blocked first-batch responses")
		}
	}
	close(releaseFirst)
	var secondResult domain.DispatchResult
	for result := range results {
		if result.Tag == "J11" {
			secondResult = result
		}
	}
	want := target.Add(time.Second)
	if secondResult.RequestCount != 1 || secondResult.Outcome != domain.OrderStateAcknowledged || !secondResult.TargetAt.Equal(want) {
		t.Fatalf("second batch result = %#v, want target %s", secondResult, want)
	}
}

func TestPlacementErrorsAreNeverRetried(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		outcome string
	}{
		{"rate limit", &broker.PlaceError{Code: 429, Type: "RateLimitException", Message: "slow down"}, domain.OrderStateRejected},
		{"server error", &broker.PlaceError{Code: 503, Type: "NetworkException", Message: "upstream unavailable", Ambiguous: true}, domain.OrderStateUnknown},
		{"timeout", context.DeadlineExceeded, domain.OrderStateUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeClient{responses: map[string][]error{"TAG": {test.err, nil}}}
			dispatcher, target := immediateTestDispatcher(t)
			results, err := dispatcher.RunBatch(context.Background(), target, []PreparedJob{{IntentID: 1, AccountID: 1, Position: 1, Client: client, Tag: "TAG"}})
			if err != nil {
				t.Fatal(err)
			}
			result := <-results
			client.mu.Lock()
			calls := len(client.started)
			client.mu.Unlock()
			if calls != 1 || result.RequestCount != 1 || result.Outcome != test.outcome {
				t.Fatalf("calls=%d result=%#v", calls, result)
			}
		})
	}
}

func TestPanickingOrderIsIsolatedFromSibling(t *testing.T) {
	client := &fakeClient{panics: map[string]any{"BAD": "sdk bug"}}
	dispatcher, target := immediateTestDispatcher(t)
	jobs := []PreparedJob{
		{IntentID: 1, AccountID: 1, Position: 1, Client: client, Tag: "BAD"},
		{IntentID: 2, AccountID: 1, Position: 2, Client: client, Tag: "GOOD"},
	}
	channel, err := dispatcher.RunBatch(context.Background(), target, jobs)
	if err != nil {
		t.Fatal(err)
	}
	results := make(map[int64]domain.DispatchResult)
	for result := range channel {
		results[result.IntentID] = result
	}
	if results[1].Outcome != domain.OrderStateUnknown || results[1].ErrorType != "Panic" || results[1].RequestCount != 1 {
		t.Fatalf("panicking result = %#v", results[1])
	}
	if results[2].Outcome != domain.OrderStateAcknowledged || results[2].RequestCount != 1 {
		t.Fatalf("sibling result = %#v", results[2])
	}
}

func TestPassedBatchIsMissedWhileFutureBatchStillRuns(t *testing.T) {
	client := &fakeClient{startCh: make(chan string, 1)}
	now := time.Unix(1_700_000_000, 789)
	base := now.Add(-100 * time.Millisecond)
	futureTimer := make(chan time.Time, 1)
	dispatcher, _ := New(Config{
		LateCutoff: time.Second, BatchSize: 1, BatchInterval: time.Second,
		Now:   func() time.Time { return now },
		After: func(time.Duration) <-chan time.Time { return futureTimer },
	})
	jobs := []PreparedJob{
		{IntentID: 1, AccountID: 1, Position: 1, Client: client, Tag: "PAST"},
		{IntentID: 2, AccountID: 1, Position: 2, Client: client, Tag: "FUTURE"},
	}
	results, err := dispatcher.RunBatch(context.Background(), base, jobs)
	if err != nil {
		t.Fatal(err)
	}
	futureTimer <- base.Add(time.Second)
	var got []domain.DispatchResult
	for result := range results {
		got = append(got, result)
	}
	if len(got) != 2 {
		t.Fatalf("results = %#v", got)
	}
	byIntent := map[int64]domain.DispatchResult{got[0].IntentID: got[0], got[1].IntentID: got[1]}
	if byIntent[1].Outcome != domain.OrderStateMissed || byIntent[1].RequestCount != 0 {
		t.Fatalf("past result = %#v", byIntent[1])
	}
	if byIntent[2].Outcome != domain.OrderStateAcknowledged || byIntent[2].RequestCount != 1 || !byIntent[2].TargetAt.Equal(base.Add(time.Second)) {
		t.Fatalf("future result = %#v", byIntent[2])
	}
}

func TestNewRejectsUnsafeTimingConfiguration(t *testing.T) {
	for _, cfg := range []Config{
		{LateCutoff: 0},
		{LateCutoff: time.Second, BatchSize: 11},
		{LateCutoff: time.Second, BatchInterval: 999 * time.Millisecond},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New accepted %#v", cfg)
		}
	}
}

func TestReconcileNormalizesDecimals(t *testing.T) {
	params := domain.OrderParams{Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Quantity: 3, Price: "12.50", Validity: "DAY", MarketProtection: "0"}
	client := &fakeClient{orders: []broker.Order{{OrderID: "OID", Tag: "TAG", Params: domain.OrderParams{Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Quantity: 3, Price: "12.5", TriggerPrice: "0", Validity: "DAY", MarketProtection: ""}}}}
	order, err := Reconcile(context.Background(), PreparedJob{IntentID: 1, AccountID: 1, Client: client, Tag: "TAG", Params: params})
	if err != nil || order.OrderID != "OID" {
		t.Fatalf("order = %#v, err = %v", order, err)
	}
	if _, err := Reconcile(context.Background(), PreparedJob{IntentID: 1, AccountID: 1, Client: client, Tag: "OTHER", Params: params}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v", err)
	}

	marketParams := params
	marketParams.OrderType = "MARKET"
	marketParams.Price = ""
	marketParams.MarketProtection = "-1"
	client.orders = []broker.Order{{OrderID: "MARKET-OID", Tag: "MARKET-TAG", Params: domain.OrderParams{Exchange: "NSE", TradingSymbol: "ABC", TransactionType: "SELL", Product: "CNC", OrderType: "MARKET", Quantity: 3, Price: "0", TriggerPrice: "0", Validity: "DAY", MarketProtection: "-1"}}}
	order, err = Reconcile(context.Background(), PreparedJob{IntentID: 2, AccountID: 1, Client: client, Tag: "MARKET-TAG", Params: marketParams})
	if err != nil || order.OrderID != "MARKET-OID" {
		t.Fatalf("automatic market protection reconciliation = (%#v, %v)", order, err)
	}
}
