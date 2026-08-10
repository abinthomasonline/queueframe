// Package dispatch contains the latency-sensitive, memory-only order path.
// It deliberately has no persistence or filesystem dependency.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
)

const (
	defaultBatchSize     = 10
	defaultBatchInterval = 1050 * time.Millisecond
)

type Config struct {
	// LateCutoff bounds timer/scheduler jitter for a schedule admitted before
	// its target. A schedule first submitted after an order's planned target is
	// never admitted, irrespective of this allowance.
	LateCutoff time.Duration
	BatchSize  int
	// BatchInterval is the absolute spacing between account-local batches.
	BatchInterval time.Duration
	Now           func() time.Time
	After         func(time.Duration) <-chan time.Time
}

type Dispatcher struct {
	lateCutoff   time.Duration
	batchSize    int
	batchSpacing time.Duration
	now          func() time.Time
	after        func(time.Duration) <-chan time.Time
}

type PreparedJob struct {
	IntentID  int64
	AccountID int64
	// Position is the one-based position in this trader's queue. Priority is
	// retained as a migration fallback while existing queues are upgraded.
	Position int
	Priority int
	Client   broker.Client
	Params   domain.OrderParams
	Tag      string
}

type plannedBatch struct {
	targetAt time.Time
	jobs     []PreparedJob
}

func New(config Config) (*Dispatcher, error) {
	if config.LateCutoff <= 0 {
		return nil, errors.New("late cutoff must be positive")
	}
	if config.BatchSize == 0 {
		config.BatchSize = defaultBatchSize
	}
	if config.BatchSize < 1 || config.BatchSize > defaultBatchSize {
		return nil, errors.New("batch size must be between 1 and 10")
	}
	if config.BatchInterval == 0 {
		config.BatchInterval = defaultBatchInterval
	}
	if config.BatchInterval < time.Second {
		return nil, errors.New("batch interval must be at least one second")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.After == nil {
		config.After = time.After
	}
	return &Dispatcher{
		lateCutoff: config.LateCutoff, batchSize: config.BatchSize,
		batchSpacing: config.BatchInterval, now: config.Now, after: config.After,
	}, nil
}

// PlannedTargets returns the exact request-start target for each job. Batches
// are local to an account: positions 1-10 target baseAt, 11-20 target
// baseAt+BatchInterval, and so on. The returned map can be persisted before
// the hot path begins.
func (d *Dispatcher) PlannedTargets(baseAt time.Time, jobs []PreparedJob) (map[int64]time.Time, error) {
	groups, err := groupJobs(jobs)
	if err != nil {
		return nil, err
	}
	targets := make(map[int64]time.Time, len(jobs))
	for _, accountJobs := range groups {
		for _, batch := range d.planAccount(baseAt, accountJobs) {
			for _, job := range batch.jobs {
				targets[job.IntentID] = batch.targetAt
			}
		}
	}
	return targets, nil
}

// RunBatch validates and copies already prepared jobs, then returns a buffered
// result channel immediately. Each account-local batch has an independent
// absolute timer, so a slow request in one batch cannot delay any later batch.
// Every admitted job calls PlaceRegularOrder at most once.
func (d *Dispatcher) RunBatch(ctx context.Context, baseAt time.Time, jobs []PreparedJob) (<-chan domain.DispatchResult, error) {
	groups, err := groupJobs(jobs)
	if err != nil {
		return nil, err
	}
	results := make(chan domain.DispatchResult, len(jobs))
	if len(jobs) == 0 {
		close(results)
		return results, nil
	}

	// Admission time distinguishes ordinary timer jitter from restart recovery.
	// A planned target already in the past at admission is never resubmitted.
	admittedAt := d.now()
	var batches sync.WaitGroup
	for _, accountJobs := range groups {
		for _, planned := range d.planAccount(baseAt, accountJobs) {
			batch := planned.jobs
			plannedAt := planned.targetAt
			batches.Add(1)
			go func() {
				defer batches.Done()
				d.runAbsoluteBatch(ctx, admittedAt, plannedAt, batch, results)
			}()
		}
	}
	go func() {
		batches.Wait()
		close(results)
	}()
	return results, nil
}

func (d *Dispatcher) planAccount(baseAt time.Time, jobs []PreparedJob) []plannedBatch {
	byIndex := make(map[int][]PreparedJob)
	var indexes []int
	for sortedIndex, job := range jobs {
		batchIndex := sortedIndex / d.batchSize
		if job.Position > 0 {
			batchIndex = (job.Position - 1) / d.batchSize
		}
		if _, exists := byIndex[batchIndex]; !exists {
			indexes = append(indexes, batchIndex)
		}
		byIndex[batchIndex] = append(byIndex[batchIndex], job)
	}
	sort.Ints(indexes)
	planned := make([]plannedBatch, 0, len(indexes))
	for _, batchIndex := range indexes {
		planned = append(planned, plannedBatch{
			targetAt: baseAt.Add(time.Duration(batchIndex) * d.batchSpacing),
			jobs:     append([]PreparedJob(nil), byIndex[batchIndex]...),
		})
	}
	return planned
}

func groupJobs(jobs []PreparedJob) (map[int64][]PreparedJob, error) {
	seen := make(map[int64]bool, len(jobs))
	groups := make(map[int64][]PreparedJob)
	for _, job := range jobs {
		if job.IntentID <= 0 || job.AccountID <= 0 || job.Client == nil || job.Tag == "" {
			return nil, fmt.Errorf("invalid prepared job for intent %d", job.IntentID)
		}
		if seen[job.IntentID] {
			return nil, fmt.Errorf("duplicate intent ID %d", job.IntentID)
		}
		seen[job.IntentID] = true
		groups[job.AccountID] = append(groups[job.AccountID], job)
	}
	for accountID := range groups {
		positions := make(map[int]bool, len(groups[accountID]))
		for _, job := range groups[accountID] {
			if job.Position <= 0 {
				continue
			}
			if positions[job.Position] {
				return nil, fmt.Errorf("duplicate queue position %d for account %d", job.Position, accountID)
			}
			positions[job.Position] = true
		}
		sort.SliceStable(groups[accountID], func(i, j int) bool {
			left, right := queuePosition(groups[accountID][i]), queuePosition(groups[accountID][j])
			if left == right {
				return groups[accountID][i].IntentID < groups[accountID][j].IntentID
			}
			return left < right
		})
	}
	return groups, nil
}

func queuePosition(job PreparedJob) int {
	if job.Position > 0 {
		return job.Position
	}
	return job.Priority
}

func (d *Dispatcher) runAbsoluteBatch(ctx context.Context, admittedAt, plannedAt time.Time, jobs []PreparedJob, results chan<- domain.DispatchResult) {
	// Pre-spawn every worker before waiting on the clock. At the target, the
	// coordinator only closes a channel; goroutine allocation is never part of
	// the latency-sensitive release.
	release := make(chan struct{})
	abort := make(chan struct{})
	var ready sync.WaitGroup
	var workers sync.WaitGroup
	ready.Add(len(jobs))
	workers.Add(len(jobs))
	for _, job := range jobs {
		job := job
		go func() {
			defer workers.Done()
			ready.Done()
			select {
			case <-release:
				results <- d.execute(ctx, plannedAt, job)
			case <-abort:
			}
		}()
	}
	ready.Wait()

	if admittedAt.After(plannedAt) {
		close(abort)
		workers.Wait()
		d.emitUnstarted(results, jobs, plannedAt, "PassedTarget", "planned dispatch time had already passed when the schedule was admitted")
		return
	}
	if !d.waitUntil(ctx, plannedAt) {
		close(abort)
		workers.Wait()
		d.emitUnstarted(results, jobs, plannedAt, "Cancelled", "dispatcher stopped before planned dispatch time")
		return
	}
	if d.now().After(plannedAt.Add(d.lateCutoff)) {
		close(abort)
		workers.Wait()
		d.emitUnstarted(results, jobs, plannedAt, "Deadline", "late dispatch cutoff passed")
		return
	}

	// The batch starts concurrently; response order and latency cannot affect
	// sibling starts.
	close(release)
	workers.Wait()
}

func (d *Dispatcher) execute(ctx context.Context, plannedAt time.Time, job PreparedJob) (result domain.DispatchResult) {
	result = domain.DispatchResult{
		IntentID: job.IntentID, AccountID: job.AccountID, Tag: job.Tag,
		Attempt: 1, RequestCount: 1, TargetAt: plannedAt, StartedAt: d.now(),
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.FinishedAt = d.now()
			result.Outcome = domain.OrderStateUnknown
			result.ErrorType = "Panic"
			result.ErrorMessage = fmt.Sprintf("isolated broker client panic: %v", recovered)
		}
	}()
	order, err := job.Client.PlaceRegularOrder(ctx, job.Params, job.Tag)
	result.FinishedAt = d.now()
	if err == nil {
		result.Outcome = domain.OrderStateAcknowledged
		result.OrderID = order.OrderID
		result.ExchangeOrderID = order.ExchangeOrderID
		result.BrokerStatus = order.Status
		result.OrderTimestamp = order.OrderTimestamp
		result.ExchangeTimestamp = order.ExchangeTimestamp
		result.ExchangeUpdateTimestamp = order.ExchangeUpdateTimestamp
		if len(order.Children) > 0 {
			encoded, _ := json.Marshal(order.Children)
			result.ChildOrders = string(encoded)
		}
		return result
	}

	var placeError *broker.PlaceError
	if !errors.As(err, &placeError) {
		result.Outcome = domain.OrderStateUnknown
		result.ErrorType = "UnknownError"
		result.ErrorMessage = err.Error()
		return result
	}
	result.ErrorType = placeError.Type
	result.ErrorMessage = placeError.Message
	if placeError.Ambiguous {
		result.Outcome = domain.OrderStateUnknown
	} else {
		result.Outcome = domain.OrderStateRejected
	}
	return result
}

func (d *Dispatcher) emitUnstarted(results chan<- domain.DispatchResult, jobs []PreparedJob, plannedAt time.Time, errorType, message string) {
	now := d.now()
	for _, job := range jobs {
		results <- missed(job, plannedAt, now, errorType, message)
	}
}

func missed(job PreparedJob, plannedAt, now time.Time, errorType, message string) domain.DispatchResult {
	return domain.DispatchResult{
		IntentID: job.IntentID, AccountID: job.AccountID, Tag: job.Tag,
		Attempt: 1, RequestCount: 0, TargetAt: plannedAt, FinishedAt: now,
		Outcome: domain.OrderStateMissed, ErrorType: errorType, ErrorMessage: message,
	}
}

func (d *Dispatcher) waitUntil(ctx context.Context, target time.Time) bool {
	delay := target.Sub(d.now())
	if delay <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-d.after(delay):
		return true
	}
}
