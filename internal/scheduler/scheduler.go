package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/config"
	"github.com/abinthomasonline/queueframe/internal/dispatch"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/markettime"
	ordercheck "github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/store"
)

type Scheduler struct {
	store                  *store.Store
	factory                broker.Factory
	catalog                *ordercheck.Catalog
	config                 config.Config
	location               *time.Location
	dispatcher             *dispatch.Dispatcher
	status                 atomic.Value
	monitorFor             time.Duration
	monitorEvery           time.Duration
	readinessCallTimeout   time.Duration
	readinessCallReserve   time.Duration
	readinessRetryMin      time.Duration
	readinessRetryMax      time.Duration
	persistenceRetryMin    time.Duration
	persistenceRetryMax    time.Duration
	postCutoffRecoveryFor  time.Duration
	settingsRefreshEvery   time.Duration
	persistDispatchResults func(context.Context, []domain.DispatchResult) error
}

type resultPersister func(context.Context, []domain.DispatchResult) error

func New(database *store.Store, factory broker.Factory, catalog *ordercheck.Catalog, cfg config.Config) (*Scheduler, error) {
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return nil, err
	}
	dispatcher, err := dispatch.New(dispatch.Config{
		LateCutoff: cfg.LateCutoff, BatchSize: 10, BatchInterval: 1050 * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	scheduler := &Scheduler{
		store: database, factory: factory, catalog: catalog, config: cfg, location: location, dispatcher: dispatcher,
		monitorFor: time.Minute, monitorEvery: time.Second,
		readinessCallTimeout: 8 * time.Second, readinessCallReserve: 8 * time.Second,
		readinessRetryMin: 250 * time.Millisecond, readinessRetryMax: 2 * time.Second,
		persistenceRetryMin: 100 * time.Millisecond, persistenceRetryMax: 2 * time.Second,
		postCutoffRecoveryFor:  time.Minute,
		settingsRefreshEvery:   5 * time.Second,
		persistDispatchResults: database.ApplyDispatchResults,
	}
	scheduler.status.Store("starting")
	return scheduler, nil
}

func (s *Scheduler) Status() string {
	value, _ := s.status.Load().(string)
	return value
}

func (s *Scheduler) setStatus(value string) {
	s.status.Store(value)
	slog.Info("scheduler", "status", value)
}

// runLegacy is retained only so existing v1 databases can be inspected and
// tested during the side-by-side migration. New scheduling uses the trader
// queue in queued.go.
func (s *Scheduler) runLegacy(ctx context.Context) error {
	now := time.Now().In(s.location)
	date := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.location)
	if err := s.recoverStaleDates(ctx, date.Format("2006-01-02"), now); err != nil {
		return err
	}
	if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		for date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			date = date.AddDate(0, 0, 1)
		}
	} else {
		todayTarget, err := markettime.Target(date.Format("2006-01-02"), s.config.DispatchClock, s.location)
		if err != nil {
			return err
		}
		if now.After(todayTarget.Add(s.config.LateCutoff)) {
			if err := s.recoverAfterCutoff(ctx, date.Format("2006-01-02"), todayTarget, "server was not ready before the late cutoff"); err != nil {
				return err
			}
			date = markettime.NextWeekday(date)
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		dateText := date.Format("2006-01-02")
		target, err := markettime.Target(dateText, s.config.DispatchClock, s.location)
		if err != nil {
			return err
		}
		prepareAt := target.Add(-s.config.PrepareBefore)
		s.setStatus(fmt.Sprintf("waiting for preparation at %s", prepareAt.Format("02 Jan 15:04:05 MST")))
		if !waitUntil(ctx, prepareAt) {
			return ctx.Err()
		}
		retryDelay := 250 * time.Millisecond
		for {
			err := s.runDay(ctx, dateText, target)
			if err == nil || errors.Is(err, context.Canceled) {
				break
			}
			slog.Error("daily dispatch failed; retrying within cutoff", "date", dateText, "error", err)
			s.setStatus("temporary scheduler error: " + err.Error())
			if time.Now().After(target.Add(s.config.LateCutoff)) {
				if recoveryErr := s.recoverAfterCutoff(ctx, dateText, target, "scheduler could not recover before the late cutoff"); recoveryErr != nil {
					slog.Error("post-cutoff recovery failed", "date", dateText, "error", recoveryErr)
				}
				break
			}
			select {
			case <-ctx.Done():
				break
			case <-time.After(retryDelay):
			}
			if retryDelay < 2*time.Second {
				retryDelay *= 2
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		date = markettime.NextWeekday(date)
	}
}

func (s *Scheduler) recoverStaleDates(ctx context.Context, beforeDate string, now time.Time) error {
	dates, err := s.store.StaleNonterminalDates(ctx, beforeDate)
	if err != nil {
		return fmt.Errorf("list stale scheduled work: %w", err)
	}
	for _, date := range dates {
		target, err := markettime.Target(date, s.config.DispatchClock, s.location)
		if err != nil {
			return fmt.Errorf("parse stale execution date %q: %w", date, err)
		}
		if err := s.store.RecoverPrepared(ctx, date, target, now); err != nil {
			return fmt.Errorf("recover stale prepared orders for %s: %w", date, err)
		}
		if err := s.store.MarkMissedPrepared(ctx, date, target, "server was not running through this past dispatch date"); err != nil {
			return fmt.Errorf("mark stale armed orders missed for %s: %w", date, err)
		}
		slog.Warn("finalized stale scheduled work without resubmission", "date", date)
	}
	return nil
}

func (s *Scheduler) recoverAfterCutoff(ctx context.Context, date string, target time.Time, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	recoveryCtx, cancel := s.postCutoffDrainContext(ctx)
	defer cancel()
	if err := s.store.RecoverPrepared(recoveryCtx, date, target, time.Now()); err != nil {
		return fmt.Errorf("recover prepared orders after cutoff: %w", err)
	}
	jobs, results, err := s.reconcilePersistedUnknown(recoveryCtx, date)
	reconcileErr := err
	if err := s.store.MarkMissedPrepared(recoveryCtx, date, target, reason); err != nil {
		return fmt.Errorf("mark unstarted orders missed: %w", err)
	}
	if reconcileErr != nil {
		return fmt.Errorf("reconcile unknown orders after cutoff: %w", reconcileErr)
	}
	if len(results) > 0 {
		s.monitorResults(recoveryCtx, target, jobs, results)
		s.setStatus(summarize(date, results))
	}
	return nil
}

// postCutoffDrainContext gives recovery one absolute budget. It deliberately
// survives a later shutdown cancellation so already-started reconciliation can
// drain, while retaining any earlier deadline supplied by the caller.
func (s *Scheduler) postCutoffDrainContext(parent context.Context) (context.Context, context.CancelFunc) {
	window := s.postCutoffRecoveryFor
	if window <= 0 {
		window = time.Minute
	}
	deadline := time.Now().Add(window)
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return context.WithDeadline(context.WithoutCancel(parent), deadline)
}

func (s *Scheduler) runDay(ctx context.Context, date string, target time.Time) error {
	now := time.Now()
	if err := s.store.FreezeDate(ctx, date); err != nil {
		return fmt.Errorf("freeze order batch: %w", err)
	}
	if err := s.store.RecoverPrepared(ctx, date, target, now); err != nil {
		return fmt.Errorf("recover prepared orders: %w", err)
	}
	recoveredJobs, recoveredResults, err := s.reconcilePersistedUnknown(ctx, date)
	if err != nil {
		return fmt.Errorf("reconcile persisted unknown orders: %w", err)
	}
	if time.Now().After(target.Add(s.config.LateCutoff)) {
		recoveryCtx, cancel := s.postCutoffDrainContext(ctx)
		defer cancel()
		if err := s.store.MarkMissedPrepared(recoveryCtx, date, target, "late dispatch cutoff passed before preparation completed"); err != nil {
			return err
		}
		s.finishRecovered(recoveryCtx, date, target, recoveredJobs, recoveredResults, "no dispatch before the late cutoff for "+date)
		return nil
	}

	s.setStatus("preparing " + date)
	if !s.config.DryRun {
		if err := s.store.RejectUnconfirmedLive(ctx, date, target); err != nil {
			return fmt.Errorf("reject orders without durable live confirmation: %w", err)
		}
	}
	candidates, err := s.store.ArmedCandidates(ctx, date)
	if err != nil {
		return fmt.Errorf("load armed orders: %w", err)
	}
	if len(candidates) == 0 {
		s.finishRecovered(ctx, date, target, recoveredJobs, recoveredResults, "no armed orders for "+date)
		return nil
	}

	clients, accountErrors := s.verifyAccounts(ctx, target, candidates)
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(clients) > 0 && !s.catalog.IsFresh(time.Now(), s.location) {
		if err := s.refreshCatalog(ctx, clients, target.Add(-s.config.WarmBefore)); err != nil {
			return fmt.Errorf("temporarily unable to refresh symbol master: %w", err)
		}
	}

	var valid []store.Candidate
	for _, candidate := range candidates {
		preflightErr := accountErrors[candidate.Account.ID]
		if preflightErr == nil {
			preflightErr = ordercheck.ValidateParams(candidate.Intent.Params)
		}
		if preflightErr == nil {
			preflightErr = s.catalog.Validate(candidate.Intent.Params.Exchange, candidate.Intent.Params.TradingSymbol)
		}
		if preflightErr != nil {
			if err := s.store.RejectPreflight(ctx, candidate.Intent.ID, "Preflight", preflightErr.Error(), target); err != nil {
				return err
			}
			continue
		}
		valid = append(valid, candidate)
	}
	if len(valid) == 0 {
		s.finishRecovered(ctx, date, target, recoveredJobs, recoveredResults, "all orders failed preflight for "+date)
		return nil
	}

	ids := make([]int64, 0, len(valid))
	for _, candidate := range valid {
		ids = append(ids, candidate.Intent.ID)
	}
	prepared, err := s.store.PrepareOrders(ctx, ids, target)
	if err != nil {
		return fmt.Errorf("persist prepared batch: %w", err)
	}
	jobs := make([]dispatch.PreparedJob, 0, len(valid))
	for _, candidate := range valid {
		if !prepared[candidate.Intent.ID] {
			continue
		}
		jobs = append(jobs, dispatch.PreparedJob{
			IntentID: candidate.Intent.ID, AccountID: candidate.Account.ID, Priority: candidate.Intent.Priority,
			Client: clients[candidate.Account.ID], Params: candidate.Intent.Params, Tag: candidate.Intent.Tag,
		})
	}
	if len(jobs) == 0 {
		s.finishRecovered(ctx, date, target, recoveredJobs, recoveredResults, "no orders remained prepared for "+date)
		return nil
	}

	// Everything needed by the order calls is now in RAM. From this point until
	// every result and reconciliation read completes, the store is untouched.
	s.setStatus(fmt.Sprintf("%d orders prepared in memory for %s", len(jobs), target.Format("15:04:05 MST")))
	if !waitUntil(ctx, target.Add(-s.config.WarmBefore)) {
		if time.Now().Before(target) {
			_ = s.store.RecoverPrepared(context.Background(), date, target, time.Now())
		}
		return ctx.Err()
	}

	activeJobs, _ := s.warmClients(ctx, target, jobs)
	if ctx.Err() != nil && time.Now().Before(target) {
		_ = s.store.RecoverPrepared(context.Background(), date, target, time.Now())
		return ctx.Err()
	}
	s.setStatus(fmt.Sprintf("dispatching %d in-memory orders", len(activeJobs)))
	resultChannel, err := s.dispatcher.RunBatch(ctx, target, activeJobs)
	if err != nil {
		_ = s.store.RecoverPrepared(context.Background(), date, target, time.Now())
		return err
	}
	results := make([]domain.DispatchResult, 0, len(activeJobs))
	for result := range resultChannel {
		results = append(results, result)
	}
	if ctx.Err() != nil && time.Now().Before(target) {
		_ = s.store.RecoverPrepared(context.Background(), date, target, time.Now())
		return ctx.Err()
	}

	s.reconcileUnknown(ctx, activeJobs, results)
	// First database access since preparation: durably flush the completed batch.
	if err := s.persistResultsWithRetry(results, s.persistenceDeadline(ctx)); err != nil {
		return fmt.Errorf("persist in-memory results: %w", err)
	}
	allJobs := append(recoveredJobs, activeJobs...)
	allResults := append(recoveredResults, results...)
	s.monitorResults(ctx, target, allJobs, allResults)
	s.setStatus(summarize(date, allResults))
	return nil
}

func (s *Scheduler) finishRecovered(ctx context.Context, date string, target time.Time, jobs []dispatch.PreparedJob, results []domain.DispatchResult, emptyStatus string) {
	if len(results) == 0 {
		s.setStatus(emptyStatus)
		return
	}
	s.monitorResults(ctx, target, jobs, results)
	s.setStatus(summarize(date, results))
}

// monitorResults starts only after the whole placement batch has finished and
// its first durable flush completed. It follows temporary OMS states until an
// exchange acknowledgement, OPEN, or a terminal rejection/cancellation is
// visible, without touching the latency-sensitive placement path.
func (s *Scheduler) monitorResults(ctx context.Context, target time.Time, jobs []dispatch.PreparedJob, results []domain.DispatchResult) {
	s.monitorResultsUsing(ctx, target, jobs, results, s.applyDispatchResults)
}

func (s *Scheduler) monitorResultsUsing(ctx context.Context, target time.Time, jobs []dispatch.PreparedJob, results []domain.DispatchResult, persist resultPersister) {
	jobsByIntent := make(map[int64]dispatch.PreparedJob, len(jobs))
	for _, job := range jobs {
		jobsByIntent[job.IntentID] = job
	}
	deadline := time.Now().Add(s.monitorFor)
	contextConstrained := false
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
		contextConstrained = true
	}
	if !time.Now().Before(deadline) {
		return
	}
	ticker := time.NewTicker(s.monitorEvery)
	defer ticker.Stop()
	dirty := false

monitoring:
	for time.Now().Before(deadline) {
		accounts := make(map[int64]broker.Client)
		for _, result := range results {
			if !needsMonitoring(result) {
				continue
			}
			if job, ok := jobsByIntent[result.IntentID]; ok {
				accounts[job.AccountID] = job.Client
			}
		}
		if len(accounts) == 0 && !dirty {
			return
		}
		if contextConstrained && !time.Now().Add(s.monitoringReserve()).Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			break monitoring
		case <-ticker.C:
		}
		if contextConstrained && !time.Now().Add(s.monitoringReserve()).Before(deadline) {
			break
		}
		books := make(map[int64]orderBook)
		if len(accounts) > 0 {
			books = fetchOrderBooks(ctx, accounts)
		}
		changed := false
		for index := range results {
			result := &results[index]
			if !needsMonitoring(*result) {
				continue
			}
			job, ok := jobsByIntent[result.IntentID]
			if !ok {
				continue
			}
			book := books[job.AccountID]
			if book.err != nil {
				continue
			}
			matches := matchingResultOrders(job, *result, book.orders)
			if len(matches) != 1 {
				if len(matches) > 1 && result.Outcome == domain.OrderStateUnknown {
					result.ErrorType = "DuplicateDetected"
					result.ErrorMessage += "; multiple matching orders found during monitoring"
					changed = true
				}
				continue
			}
			order := matches[0]
			if result.Outcome == domain.OrderStateUnknown {
				result.Outcome = domain.OrderStateAcknowledged
				result.ErrorMessage = appendResultDetail(result.ErrorMessage, "reconciled during OMS monitoring")
				changed = true
			}
			if result.RequestCount == 0 {
				// A unique Queueframe tag in Kite's orderbook proves that the
				// pre-crash placement request reached the broker.
				result.RequestCount = 1
				changed = true
			}
			if updateResultFromOrder(result, order) {
				changed = true
			}
			if appendOrderBookReceipt(result, order, book.observedAt) {
				changed = true
			}
		}
		dirty = dirty || changed
		if dirty {
			if err := applyDispatchResultsBeforeUsing(results, deadline, persist); err != nil {
				slog.Error("persist OMS monitoring update", "error", err)
			} else {
				dirty = false
			}
		}
	}
	if dirty {
		finalWindow := s.persistenceRetryMax
		if finalWindow <= 0 {
			finalWindow = 2 * time.Second
		}
		finalDeadline := time.Now().Add(finalWindow)
		if deadline.Before(finalDeadline) {
			finalDeadline = deadline
		}
		if err := s.persistResultsWithRetryUsing(results, finalDeadline, persist); err != nil {
			slog.Error("persist final OMS monitoring update", "error", err)
		}
	}
}

func (s *Scheduler) monitoringReserve() time.Duration {
	callReserve := s.readinessCallReserve
	if callReserve <= 0 {
		callReserve = 8 * time.Second
	}
	persistReserve := s.persistenceRetryMax
	if persistReserve <= 0 {
		persistReserve = 2 * time.Second
	}
	return callReserve + persistReserve
}

func updateResultFromOrder(result *domain.DispatchResult, order broker.Order) bool {
	changed := false
	if result.OrderID != order.OrderID {
		result.OrderID = order.OrderID
		changed = true
	}
	if result.ExchangeOrderID != order.ExchangeOrderID {
		result.ExchangeOrderID = order.ExchangeOrderID
		changed = true
	}
	if result.BrokerStatus != order.Status {
		result.BrokerStatus = order.Status
		changed = true
	}
	switch strings.ToUpper(strings.TrimSpace(order.Status)) {
	case "COMPLETE":
		if result.Outcome != domain.OrderStateComplete {
			result.Outcome = domain.OrderStateComplete
			changed = true
		}
	case "REJECTED":
		if result.Outcome != domain.OrderStateRejected {
			result.Outcome = domain.OrderStateRejected
			changed = true
		}
	case "CANCELLED":
		if result.Outcome != domain.OrderStateCancelled {
			result.Outcome = domain.OrderStateCancelled
			changed = true
		}
	}
	if !order.OrderTimestamp.IsZero() && !result.OrderTimestamp.Equal(order.OrderTimestamp) {
		result.OrderTimestamp = order.OrderTimestamp
		changed = true
	}
	if !order.ExchangeTimestamp.IsZero() && !result.ExchangeTimestamp.Equal(order.ExchangeTimestamp) {
		result.ExchangeTimestamp = order.ExchangeTimestamp
		changed = true
	}
	if !order.ExchangeUpdateTimestamp.IsZero() && !result.ExchangeUpdateTimestamp.Equal(order.ExchangeUpdateTimestamp) {
		result.ExchangeUpdateTimestamp = order.ExchangeUpdateTimestamp
		changed = true
	}
	statusMessage := strings.TrimSpace(order.StatusMessage)
	if statusMessage != "" {
		detail := "Kite order status: " + statusMessage
		if !strings.Contains(result.ErrorMessage, detail) {
			result.ErrorMessage = appendResultDetail(result.ErrorMessage, detail)
			changed = true
		}
	}
	return changed
}

func appendResultDetail(existing, detail string) string {
	existing = strings.TrimSpace(existing)
	detail = strings.TrimSpace(detail)
	if existing == "" {
		return detail
	}
	if detail == "" {
		return existing
	}
	return existing + "; " + detail
}

func needsMonitoring(result domain.DispatchResult) bool {
	if result.Outcome != domain.OrderStateAcknowledged && result.Outcome != domain.OrderStateUnknown {
		return false
	}
	if result.ErrorType == "DuplicateDetected" {
		return false
	}
	status := strings.ToUpper(strings.TrimSpace(result.BrokerStatus))
	switch status {
	case "DRY_RUN", "OPEN", "COMPLETE", "REJECTED", "CANCELLED":
		return false
	}
	return true
}

type orderBook struct {
	orders     []broker.Order
	err        error
	observedAt time.Time
}

func fetchOrderBooks(ctx context.Context, accounts map[int64]broker.Client) map[int64]orderBook {
	type observed struct {
		accountID int64
		orderBook
	}
	channel := make(chan observed, len(accounts))
	for accountID, client := range accounts {
		accountID, client := accountID, client
		go func() {
			orders, err := client.Orders(ctx)
			channel <- observed{accountID: accountID, orderBook: orderBook{orders: orders, err: err, observedAt: time.Now()}}
		}()
	}
	books := make(map[int64]orderBook, len(accounts))
	for range accounts {
		item := <-channel
		books[item.accountID] = item.orderBook
	}
	return books
}

func appendKiteResponseReceipts(results []domain.DispatchResult) {
	for index := range results {
		result := &results[index]
		if result.RequestCount != 1 || result.OrderID == "" || result.FinishedAt.IsZero() {
			continue
		}
		result.Receipts = append(result.Receipts, domain.DispatchReceipt{
			Source:                  domain.DispatchReceiptKiteResponse,
			ObservedAt:              result.FinishedAt,
			OrderID:                 result.OrderID,
			ExchangeOrderID:         result.ExchangeOrderID,
			BrokerStatus:            result.BrokerStatus,
			OrderTimestamp:          result.OrderTimestamp,
			ExchangeTimestamp:       result.ExchangeTimestamp,
			ExchangeUpdateTimestamp: result.ExchangeUpdateTimestamp,
		})
	}
}

// appendOrderBookReceipt retains only changed broker snapshots. ObservedAt is
// captured after the existing GET returns; this function performs no network,
// logging, or persistence work.
func appendOrderBookReceipt(result *domain.DispatchResult, order broker.Order, observedAt time.Time) bool {
	for index := len(result.Receipts) - 1; index >= 0; index-- {
		previous := result.Receipts[index]
		if previous.Source != domain.DispatchReceiptOrderBook {
			continue
		}
		if previous.OrderID == order.OrderID &&
			previous.ExchangeOrderID == order.ExchangeOrderID &&
			previous.BrokerStatus == order.Status &&
			previous.StatusMessage == order.StatusMessage &&
			previous.OrderTimestamp.Equal(order.OrderTimestamp) &&
			previous.ExchangeTimestamp.Equal(order.ExchangeTimestamp) &&
			previous.ExchangeUpdateTimestamp.Equal(order.ExchangeUpdateTimestamp) {
			return false
		}
		break
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	result.Receipts = append(result.Receipts, domain.DispatchReceipt{
		Source:                  domain.DispatchReceiptOrderBook,
		ObservedAt:              observedAt,
		OrderID:                 order.OrderID,
		ExchangeOrderID:         order.ExchangeOrderID,
		BrokerStatus:            order.Status,
		StatusMessage:           order.StatusMessage,
		OrderTimestamp:          order.OrderTimestamp,
		ExchangeTimestamp:       order.ExchangeTimestamp,
		ExchangeUpdateTimestamp: order.ExchangeUpdateTimestamp,
	})
	return true
}

func matchingResultOrders(job dispatch.PreparedJob, result domain.DispatchResult, orders []broker.Order) []broker.Order {
	if result.OrderID != "" {
		var matches []broker.Order
		for _, order := range orders {
			if order.OrderID == result.OrderID {
				matches = append(matches, order)
			}
		}
		return matches
	}
	return dispatch.MatchOrders(job, job.AccountID, orders)
}

func (s *Scheduler) verifyAccounts(ctx context.Context, target time.Time, candidates []store.Candidate) (map[int64]broker.Client, map[int64]error) {
	accounts := make(map[int64]domain.Account)
	for _, candidate := range candidates {
		accounts[candidate.Account.ID] = candidate.Account
	}
	type result struct {
		id     int64
		client broker.Client
		err    error
	}
	results := make(chan result, len(accounts))
	for id, account := range accounts {
		id, account := id, account
		go func() {
			if !account.SessionReady(target) {
				results <- result{id: id, err: errors.New("daily Kite session is missing or expires before dispatch")}
				return
			}
			client := s.factory.Client(account.APIKey, account.AccessToken)
			identityChanged := false
			err := s.retryReadiness(ctx, target.Add(-s.config.WarmBefore), func(profileCtx context.Context) error {
				userID, profileErr := client.Profile(profileCtx)
				if profileErr != nil {
					return profileErr
				}
				identityChanged = !strings.EqualFold(userID, account.ClientID)
				return nil
			})
			if identityChanged {
				results <- result{id: id, err: errors.New("Kite profile identity changed")}
				return
			}
			if err != nil {
				if ctx.Err() != nil {
					results <- result{id: id, err: ctx.Err()}
					return
				}
				// The daily token was identity-bound when it was saved. A temporary
				// profile outage must not suppress the actual order request; Kite's
				// placement response remains authoritative at dispatch time.
				slog.Warn("Kite profile readiness check exhausted; retaining bound session", "account_id", id, "error", err)
			}
			results <- result{id: id, client: client}
		}()
	}
	clients := make(map[int64]broker.Client)
	errorsByAccount := make(map[int64]error)
	for range accounts {
		result := <-results
		if result.err != nil {
			errorsByAccount[result.id] = result.err
			continue
		}
		clients[result.id] = result.client
	}
	return clients, errorsByAccount
}

func (s *Scheduler) refreshCatalog(ctx context.Context, clients map[int64]broker.Client, deadline time.Time) error {
	ids := make([]int64, 0, len(clients))
	for id := range clients {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return s.retryReadiness(ctx, deadline, func(refreshCtx context.Context) error {
		var lastErr error
		for _, id := range ids {
			if err := s.catalog.Refresh(refreshCtx, clients[id]); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		if lastErr == nil {
			lastErr = errors.New("no verified account is available to load symbols")
		}
		return lastErr
	})
}

func (s *Scheduler) warmClients(ctx context.Context, target time.Time, jobs []dispatch.PreparedJob) ([]dispatch.PreparedJob, []domain.DispatchResult) {
	if time.Now().Add(s.readinessCallReserve).After(target) {
		slog.Warn("skipping Kite warm-up because its bounded call window has passed")
		return jobs, nil
	}
	clients := make(map[int64]broker.Client)
	for _, job := range jobs {
		clients[job.AccountID] = job.Client
	}
	type result struct {
		accountID int64
		err       error
	}
	channel := make(chan result, len(clients))
	for accountID, client := range clients {
		accountID, client := accountID, client
		go func() {
			err := s.retryReadiness(ctx, target, func(warmCtx context.Context) error {
				_, err := client.Profile(warmCtx)
				return err
			})
			channel <- result{accountID: accountID, err: err}
		}()
	}
	for range clients {
		result := <-channel
		if result.err != nil {
			slog.Warn("Kite warm-up failed; placement will still be attempted", "account_id", result.accountID, "error", result.err)
		}
	}
	return jobs, nil
}

func (s *Scheduler) retryReadiness(ctx context.Context, deadline time.Time, operation func(context.Context) error) error {
	delay := s.readinessRetryMin
	if delay <= 0 {
		delay = 250 * time.Millisecond
	}
	maxDelay := s.readinessRetryMax
	if maxDelay < delay {
		maxDelay = delay
	}
	reserve := s.readinessCallReserve
	if reserve < 0 {
		reserve = 0
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		callCtx := ctx
		cancel := func() {}
		if s.readinessCallTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, s.readinessCallTimeout)
		}
		lastErr = operation(callCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		latestRetryStart := deadline.Add(-reserve)
		if time.Now().Add(delay).After(latestRetryStart) {
			return lastErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (s *Scheduler) reconcileUnknown(ctx context.Context, jobs []dispatch.PreparedJob, results []domain.DispatchResult) {
	jobsByIntent := make(map[int64]dispatch.PreparedJob, len(jobs))
	unknownAccounts := make(map[int64]broker.Client)
	for _, job := range jobs {
		jobsByIntent[job.IntentID] = job
	}
	for _, result := range results {
		if result.Outcome == domain.OrderStateUnknown {
			if job, ok := jobsByIntent[result.IntentID]; ok {
				unknownAccounts[job.AccountID] = job.Client
			}
		}
	}
	if len(unknownAccounts) == 0 {
		return
	}
	type observed struct {
		accountID  int64
		orders     []broker.Order
		err        error
		observedAt time.Time
	}
	channel := make(chan observed, len(unknownAccounts))
	for accountID, client := range unknownAccounts {
		accountID, client := accountID, client
		go func() {
			orders, err := client.Orders(ctx)
			channel <- observed{accountID: accountID, orders: orders, err: err, observedAt: time.Now()}
		}()
	}
	books := make(map[int64]observed)
	for range unknownAccounts {
		result := <-channel
		books[result.accountID] = result
	}
	for index := range results {
		result := &results[index]
		if result.Outcome != domain.OrderStateUnknown {
			continue
		}
		job, ok := jobsByIntent[result.IntentID]
		if !ok {
			continue
		}
		book := books[job.AccountID]
		if book.err != nil {
			result.ErrorMessage = appendResultDetail(result.ErrorMessage, "orderbook reconciliation failed: "+book.err.Error())
			continue
		}
		matches := matchingResultOrders(job, *result, book.orders)
		switch len(matches) {
		case 1:
			result.Outcome = domain.OrderStateAcknowledged
			if result.RequestCount == 0 {
				result.RequestCount = 1
			}
			updateResultFromOrder(result, matches[0])
			appendOrderBookReceipt(result, matches[0], book.observedAt)
			result.ErrorMessage = appendResultDetail(result.ErrorMessage, "reconciled from Kite orderbook")
		case 0:
			result.ErrorMessage = appendResultDetail(result.ErrorMessage, "no matching order visible during reconciliation")
		default:
			result.ErrorType = "DuplicateDetected"
			result.ErrorMessage = appendResultDetail(result.ErrorMessage, "multiple matching orders found; manual review required")
		}
	}
}

// reconcilePersistedUnknown reconstructs only orderbook lookup jobs. UNKNOWN
// attempts are never handed to the dispatcher, so restart recovery cannot
// duplicate a request whose HTTP response was lost before the SQLite flush.
func (s *Scheduler) reconcilePersistedUnknown(ctx context.Context, date string) ([]dispatch.PreparedJob, []domain.DispatchResult, error) {
	candidates, err := s.store.UnknownCandidates(ctx, date)
	if err != nil {
		return nil, nil, err
	}
	results, err := s.store.UnknownAttemptResults(ctx, date)
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 0 && len(results) == 0 {
		return nil, nil, nil
	}

	resultsByIntent := make(map[int64]int, len(results))
	for index, result := range results {
		if _, exists := resultsByIntent[result.IntentID]; exists {
			return nil, nil, fmt.Errorf("multiple unknown attempts for intent %d", result.IntentID)
		}
		resultsByIntent[result.IntentID] = index
	}

	jobs := make([]dispatch.PreparedJob, 0, len(candidates))
	seen := make(map[int64]bool, len(candidates))
	now := time.Now()
	for _, candidate := range candidates {
		index, ok := resultsByIntent[candidate.Intent.ID]
		if !ok {
			return nil, nil, fmt.Errorf("unknown intent %d has no unknown dispatch attempt", candidate.Intent.ID)
		}
		result := &results[index]
		if result.AccountID != candidate.Account.ID {
			return nil, nil, fmt.Errorf("unknown intent %d account binding changed", candidate.Intent.ID)
		}
		seen[candidate.Intent.ID] = true
		if !candidate.Account.SessionReady(now) {
			detail := "restart reconciliation unavailable: daily Kite session is not ready"
			if !strings.Contains(result.ErrorMessage, detail) {
				result.ErrorMessage = appendResultDetail(result.ErrorMessage, detail)
			}
			continue
		}
		jobs = append(jobs, dispatch.PreparedJob{
			IntentID: candidate.Intent.ID, AccountID: candidate.Account.ID, Priority: candidate.Intent.Priority,
			Client: s.factory.Client(candidate.Account.APIKey, candidate.Account.AccessToken),
			Params: candidate.Intent.Params, Tag: candidate.Intent.Tag,
		})
	}
	for _, result := range results {
		if !seen[result.IntentID] {
			return nil, nil, fmt.Errorf("unknown attempt for intent %d has no unknown order", result.IntentID)
		}
	}

	s.reconcileUnknown(ctx, jobs, results)
	if err := s.persistResultsWithRetry(results, s.persistenceDeadline(ctx)); err != nil {
		return nil, nil, fmt.Errorf("persist restart reconciliation: %w", err)
	}
	return jobs, results, nil
}

func (s *Scheduler) persistenceDeadline(ctx context.Context) time.Time {
	window := s.monitorFor
	if window <= 0 {
		window = time.Minute
	}
	deadline := time.Now().Add(window)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return deadline
}

func (s *Scheduler) applyDispatchResults(ctx context.Context, results []domain.DispatchResult) error {
	if len(results) == 0 {
		return nil
	}
	if s.persistDispatchResults != nil {
		return s.persistDispatchResults(ctx, results)
	}
	return s.store.ApplyDispatchResults(ctx, results)
}

func (s *Scheduler) persistResultsWithRetry(results []domain.DispatchResult, deadline time.Time) error {
	return s.persistResultsWithRetryUsing(results, deadline, s.applyDispatchResults)
}

func (s *Scheduler) persistResultsWithRetryUsing(results []domain.DispatchResult, deadline time.Time, persist resultPersister) error {
	delay := s.persistenceRetryMin
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	maxDelay := s.persistenceRetryMax
	if maxDelay < delay {
		maxDelay = delay
	}
	var lastErr error
	for {
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return context.DeadlineExceeded
		}
		lastErr = applyDispatchResultsBeforeUsing(results, deadline, persist)
		if lastErr == nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return lastErr
		}
		wait := delay
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		<-timer.C
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (s *Scheduler) applyDispatchResultsBefore(results []domain.DispatchResult, deadline time.Time) error {
	return applyDispatchResultsBeforeUsing(results, deadline, s.applyDispatchResults)
}

func applyDispatchResultsBeforeUsing(results []domain.DispatchResult, deadline time.Time, persist resultPersister) error {
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	if persist == nil {
		return errors.New("dispatch result persister is not configured")
	}
	attemptCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return persist(attemptCtx, results)
}

func summarize(date string, results []domain.DispatchResult) string {
	counts := make(map[string]int)
	for _, result := range results {
		counts[result.Outcome]++
	}
	return fmt.Sprintf("%s finished: %d accepted/complete, %d rejected/cancelled, %d unknown, %d missed", date,
		counts[domain.OrderStateAcknowledged]+counts[domain.OrderStateComplete],
		counts[domain.OrderStateRejected]+counts[domain.OrderStateCancelled],
		counts[domain.OrderStateUnknown], counts[domain.OrderStateMissed]+counts[domain.OrderStateMissedInactive])
}

func waitUntil(ctx context.Context, target time.Time) bool {
	delay := time.Until(target)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
