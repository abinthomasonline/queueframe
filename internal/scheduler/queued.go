package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/dispatch"
	"github.com/abinthomasonline/queueframe/internal/domain"
	ordercheck "github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/store"
)

const queuedBatchSize = 10

// Run drives the v2 trader-owned queues. Runtime settings are resolved for
// each execution date so an administrator's versioned changes never alter an
// already frozen date.
func (s *Scheduler) Run(ctx context.Context) error {
	now := time.Now().In(s.location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.location)
	if err := s.recoverStaleQueuedDates(ctx, today.Format("2006-01-02"), now); err != nil {
		return err
	}
	date := today
	if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		for date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			date = date.AddDate(0, 0, 1)
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		dateText := date.Format("2006-01-02")
		settings, target, err := s.waitForQueuedFreeze(ctx, dateText)
		if err != nil {
			return err
		}

		cutoff := latestQueuedTarget(target, settings).Add(s.config.LateCutoff)
		retryDelay := 250 * time.Millisecond
		for {
			err = s.runQueuedDay(ctx, dateText, target, settings)
			if err == nil || errors.Is(err, context.Canceled) {
				break
			}
			slog.Error("daily queued dispatch failed; retrying or recovering queued date", "date", dateText, "error", err)
			s.setStatus("temporary scheduler error: " + err.Error())
			if !time.Now().Before(cutoff) {
				// Keep retrying only the durable recovery path. runQueuedDay and
				// the dispatcher independently refuse every passed target, so this
				// can finalize MISSED/UNKNOWN records but can never place late.
				s.status.Store("recovering durable outcomes after the final dispatch target")
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				break
			case <-timer.C:
			}
			if retryDelay < 2*time.Second {
				retryDelay *= 2
				if retryDelay > 2*time.Second {
					retryDelay = 2 * time.Second
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		date = nextWeekday(date)
	}
}

// waitForQueuedFreeze keeps resolving settings while a date is mutable. At
// the effective boundary it freezes first and reads once more. SQLite's write
// serialization then guarantees that an admin settings transaction either
// committed before the freeze and is observed by the final read, or sees the
// freeze and is rejected. The caller receives one locked schedule and invokes
// runQueuedDay exactly once.
func (s *Scheduler) waitForQueuedFreeze(ctx context.Context, date string) (domain.RuntimeSettings, time.Time, error) {
	refreshEvery := s.settingsRefreshEvery
	if refreshEvery <= 0 {
		refreshEvery = 5 * time.Second
	}
	var observedVersion int64 = -1
	for {
		if err := ctx.Err(); err != nil {
			return domain.RuntimeSettings{}, time.Time{}, err
		}
		settings, err := s.store.SettingsForDate(ctx, date)
		if err != nil {
			return domain.RuntimeSettings{}, time.Time{}, fmt.Errorf("load runtime settings for %s: %w", date, err)
		}
		target, err := store.SettingsDispatchAt(settings, date)
		if err != nil {
			return domain.RuntimeSettings{}, time.Time{}, fmt.Errorf("resolve dispatch target for %s: %w", date, err)
		}
		freezeAt := target.Add(-settings.FreezeLead)
		if settings.Version != observedVersion {
			s.setStatus(fmt.Sprintf("waiting for queue freeze at %s (settings v%d)", freezeAt.Format("02 Jan 15:04:05 MST"), settings.Version))
			observedVersion = settings.Version
		}
		now := time.Now()
		if !now.Before(freezeAt) {
			if err := s.store.FreezeQueuedDate(ctx, date, now); err != nil {
				return domain.RuntimeSettings{}, time.Time{}, fmt.Errorf("freeze queued date %s: %w", date, err)
			}
			locked, err := s.store.SettingsForDate(ctx, date)
			if err != nil {
				return domain.RuntimeSettings{}, time.Time{}, fmt.Errorf("load frozen runtime settings for %s: %w", date, err)
			}
			lockedTarget, err := store.SettingsDispatchAt(locked, date)
			if err != nil {
				return domain.RuntimeSettings{}, time.Time{}, fmt.Errorf("resolve frozen dispatch target for %s: %w", date, err)
			}
			// A settings transaction may have committed just before our freeze
			// and moved the boundary later. The date is now immutable, so a
			// single precise wait is sufficient and cannot cache stale data.
			lockedFreezeAt := lockedTarget.Add(-locked.FreezeLead)
			if !waitUntil(ctx, lockedFreezeAt) {
				return domain.RuntimeSettings{}, time.Time{}, ctx.Err()
			}
			return locked, lockedTarget, nil
		}

		wait := refreshEvery
		remaining := time.Until(freezeAt)
		if remaining <= time.Minute && wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return domain.RuntimeSettings{}, time.Time{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func nextWeekday(date time.Time) time.Time {
	date = date.AddDate(0, 0, 1)
	for date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		date = date.AddDate(0, 0, 1)
	}
	return date
}

func latestQueuedTarget(base time.Time, settings domain.RuntimeSettings) time.Time {
	if settings.MaxOrdersPerTrader <= 0 {
		return base
	}
	batchesAfterFirst := (settings.MaxOrdersPerTrader - 1) / queuedBatchSize
	return base.Add(time.Duration(batchesAfterFirst) * settings.BatchInterval)
}

func (s *Scheduler) queuedDispatcher(settings domain.RuntimeSettings) (*dispatch.Dispatcher, error) {
	return dispatch.New(dispatch.Config{
		LateCutoff: s.config.LateCutoff, BatchSize: queuedBatchSize, BatchInterval: settings.BatchInterval,
	})
}

func plannedQueuedTargets(base time.Time, settings domain.RuntimeSettings, candidates []domain.QueueCandidate) (map[int64]time.Time, error) {
	positions := make(map[int64]map[int]bool)
	counts := make(map[int64]int)
	targets := make(map[int64]time.Time, len(candidates))
	for _, candidate := range candidates {
		order := candidate.Order
		if order.ID <= 0 || order.TraderID <= 0 || candidate.Trader.ID != order.TraderID {
			return nil, fmt.Errorf("invalid trader binding for queued order %d", order.ID)
		}
		if order.Position <= 0 {
			return nil, fmt.Errorf("queued order %d has invalid position %d", order.ID, order.Position)
		}
		if positions[order.TraderID] == nil {
			positions[order.TraderID] = make(map[int]bool)
		}
		if positions[order.TraderID][order.Position] {
			return nil, fmt.Errorf("duplicate position %d for trader %d", order.Position, order.TraderID)
		}
		positions[order.TraderID][order.Position] = true
		counts[order.TraderID]++
		if settings.MaxOrdersPerTrader > 0 && counts[order.TraderID] > settings.MaxOrdersPerTrader {
			return nil, fmt.Errorf("trader %d exceeds the configured %d-order limit", order.TraderID, settings.MaxOrdersPerTrader)
		}
		batchIndex := (order.Position - 1) / queuedBatchSize
		targets[order.ID] = base.Add(time.Duration(batchIndex) * settings.BatchInterval)
	}
	return targets, nil
}

func (s *Scheduler) runQueuedDay(ctx context.Context, date string, baseTarget time.Time, settings domain.RuntimeSettings) error {
	queuedDispatcher, err := s.queuedDispatcher(settings)
	if err != nil {
		return fmt.Errorf("configure queued dispatcher: %w", err)
	}
	now := time.Now()
	if err := s.store.FreezeQueuedDate(ctx, date, now); err != nil {
		return fmt.Errorf("freeze queued date: %w", err)
	}
	if err := s.store.RecoverPreparedQueuedOrders(ctx, date, now); err != nil {
		return fmt.Errorf("recover prepared queued orders: %w", err)
	}
	// Load restart-UNKNOWN work into RAM, but deliberately postpone its
	// order-book network reads. A slow reconciliation must never make a still
	// future batch miss its absolute target.
	recoveredJobs, recoveredResults, err := s.loadPersistedQueuedUnknown(ctx, date)
	if err != nil {
		return fmt.Errorf("reconcile persisted queued UNKNOWN orders: %w", err)
	}
	candidates, err := s.store.QueuedCandidates(ctx, date)
	if err != nil {
		return fmt.Errorf("load queued candidates: %w", err)
	}
	if len(candidates) == 0 {
		return s.finishRecoveredQueued(ctx, date, baseTarget, recoveredJobs, recoveredResults, "no scheduled orders for "+date)
	}
	planned, err := plannedQueuedTargets(baseTarget, settings, candidates)
	if err != nil {
		return err
	}

	inactiveTargets := make(map[int64]time.Time)
	for _, candidate := range candidates {
		targetAt := planned[candidate.Order.ID]
		if !candidate.Trader.ActiveFor(date, targetAt) {
			inactiveTargets[candidate.Order.ID] = targetAt
		}
	}
	inactive, err := s.store.MarkInactiveQueuedOrders(ctx, date, inactiveTargets, now)
	if err != nil {
		return fmt.Errorf("record inactive queued orders: %w", err)
	}

	missedTargets := make(map[int64]time.Time)
	for _, candidate := range candidates {
		if inactive[candidate.Order.ID] {
			continue
		}
		targetAt := planned[candidate.Order.ID]
		if !now.Before(targetAt) {
			missedTargets[candidate.Order.ID] = targetAt
		}
	}
	missed, err := s.store.MarkMissedQueuedOrders(ctx, date, missedTargets, now, "planned dispatch time passed before the request could be admitted")
	if err != nil {
		return fmt.Errorf("record passed queued targets: %w", err)
	}

	var future []domain.QueueCandidate
	for _, candidate := range candidates {
		if !inactive[candidate.Order.ID] && !missed[candidate.Order.ID] {
			future = append(future, candidate)
		}
	}
	if len(future) == 0 {
		return s.finishRecoveredQueued(ctx, date, baseTarget, recoveredJobs, recoveredResults, "no active future dispatch targets for "+date)
	}

	clients, identityErrors := s.verifyQueuedTraders(ctx, future, planned)
	if err := ctx.Err(); err != nil {
		return err
	}
	earliest := earliestQueuedTarget(future, planned)
	catalogReady := s.catalog.IsFresh(time.Now(), s.location)
	if !catalogReady && len(clients) > 0 {
		refreshDeadline := earliest.Add(-s.warmBefore())
		if time.Now().Add(s.readinessCallReserve).Before(refreshDeadline) {
			if err := s.refreshCatalog(ctx, clients, refreshDeadline); err != nil {
				return fmt.Errorf("temporarily unable to refresh symbol master: %w", err)
			}
			catalogReady = true
		} else {
			// Symbols were selected from the validated catalog at creation time.
			// Near a recovered future target, do not sacrifice that target to a
			// full instrument-master download.
			slog.Warn("symbol master refresh window passed; retaining previously validated queued symbols", "date", date)
		}
	}

	valid := make([]domain.QueueCandidate, 0, len(future))
	for _, candidate := range future {
		preflightErr := identityErrors[candidate.Trader.ID]
		if preflightErr == nil {
			preflightErr = ordercheck.ValidateParams(candidate.Order.Params)
		}
		if preflightErr == nil && catalogReady {
			preflightErr = s.catalog.Validate(candidate.Order.Params.Exchange, candidate.Order.Params.TradingSymbol)
		}
		if preflightErr != nil {
			if err := s.store.RejectQueuedPreflight(ctx, candidate.Order.ID, planned[candidate.Order.ID], "Preflight", preflightErr.Error(), time.Now()); err != nil {
				return fmt.Errorf("record queued preflight rejection: %w", err)
			}
			continue
		}
		valid = append(valid, candidate)
	}
	if len(valid) == 0 {
		return s.finishRecoveredQueued(ctx, date, baseTarget, recoveredJobs, recoveredResults, "all active orders failed preflight for "+date)
	}

	validTargets := make(map[int64]time.Time, len(valid))
	for _, candidate := range valid {
		validTargets[candidate.Order.ID] = planned[candidate.Order.ID]
	}
	prepared, err := s.store.PrepareQueuedOrders(ctx, date, validTargets, time.Now())
	if err != nil {
		return fmt.Errorf("persist queued dispatch plan: %w", err)
	}
	jobs := make([]dispatch.PreparedJob, 0, len(valid))
	for _, candidate := range valid {
		if !prepared[candidate.Order.ID] {
			continue
		}
		jobs = append(jobs, dispatch.PreparedJob{
			IntentID: candidate.Order.ID, AccountID: candidate.Trader.ID,
			Position: candidate.Order.Position, Priority: candidate.Order.Position,
			Client: clients[candidate.Trader.ID], Params: candidate.Order.Params, Tag: candidate.Order.Tag,
		})
	}
	if len(jobs) == 0 {
		return s.finishRecoveredQueued(ctx, date, baseTarget, recoveredJobs, recoveredResults, "no queued orders remained prepared for "+date)
	}

	// Every credential, client, payload, tag, position and target is in RAM.
	// No store call occurs from here until every placement call has returned.
	s.status.Store(fmt.Sprintf("%d queued orders prepared in memory", len(jobs)))
	warmAt := earliestPreparedTarget(jobs, validTargets).Add(-s.warmBefore())
	if !waitUntil(ctx, warmAt) {
		return ctx.Err()
	}
	warmFailures := s.warmQueuedClients(ctx, earliestPreparedTarget(jobs, validTargets), jobs)
	if err := ctx.Err(); err != nil {
		return err
	}
	s.status.Store(fmt.Sprintf("dispatching %d in-memory orders", len(jobs)))
	resultChannel, err := queuedDispatcher.RunBatch(ctx, baseTarget, jobs)
	if err != nil {
		return err
	}
	results := make([]domain.DispatchResult, 0, len(jobs))
	for result := range resultChannel {
		results = append(results, result)
	}
	// Receipt construction begins only after the result channel closes, which
	// proves every placement method has returned. It therefore cannot add work
	// to request release, request transmission, or a sibling placement.
	appendKiteResponseReceipts(results)
	for _, failure := range warmFailures {
		slog.Warn("Kite warm-up failed; placement was still attempted", "trader_id", failure.traderID, "error", failure.err)
	}
	sortedQueuedResults(results)
	// All placement methods have returned. Reconcile both newly ambiguous and
	// restart-UNKNOWN work together, after future batches were protected.
	allJobs := append(recoveredJobs, jobs...)
	allResults := append(recoveredResults, results...)
	s.reconcileUnknown(ctx, allJobs, allResults)
	sortedQueuedResults(allResults)
	persist := resultPersister(s.store.ApplyQueuedDispatchResults)
	if err := s.persistResultsWithRetryUsing(allResults, s.persistenceDeadline(ctx), persist); err != nil {
		return fmt.Errorf("persist queued dispatch results: %w", err)
	}
	s.monitorResultsUsing(ctx, baseTarget, allJobs, allResults, persist)
	s.setStatus(summarize(date, allResults))
	return nil
}

func (s *Scheduler) warmBefore() time.Duration {
	if s.config.WarmBefore > 0 {
		return s.config.WarmBefore
	}
	return 10 * time.Second
}

type queuedWarmFailure struct {
	traderID int64
	err      error
}

// warmQueuedClients performs only bounded network reads and keeps any warning
// in RAM until all placement calls return. This prevents logging/filesystem
// activity in the latency-sensitive window.
func (s *Scheduler) warmQueuedClients(ctx context.Context, target time.Time, jobs []dispatch.PreparedJob) []queuedWarmFailure {
	if time.Now().Add(s.readinessCallReserve).After(target) {
		return nil
	}
	clients := make(map[int64]broker.Client)
	for _, job := range jobs {
		clients[job.AccountID] = job.Client
	}
	type result struct {
		traderID int64
		err      error
	}
	channel := make(chan result, len(clients))
	for traderID, client := range clients {
		traderID, client := traderID, client
		go func() {
			err := s.retryReadiness(ctx, target, func(warmCtx context.Context) error {
				_, err := client.Profile(warmCtx)
				return err
			})
			channel <- result{traderID: traderID, err: err}
		}()
	}
	var failures []queuedWarmFailure
	for range clients {
		observed := <-channel
		if observed.err != nil {
			failures = append(failures, queuedWarmFailure{traderID: observed.traderID, err: observed.err})
		}
	}
	return failures
}

func earliestQueuedTarget(candidates []domain.QueueCandidate, planned map[int64]time.Time) time.Time {
	var earliest time.Time
	for _, candidate := range candidates {
		target := planned[candidate.Order.ID]
		if earliest.IsZero() || target.Before(earliest) {
			earliest = target
		}
	}
	return earliest
}

func earliestPreparedTarget(jobs []dispatch.PreparedJob, planned map[int64]time.Time) time.Time {
	var earliest time.Time
	for _, job := range jobs {
		target := planned[job.IntentID]
		if earliest.IsZero() || target.Before(earliest) {
			earliest = target
		}
	}
	return earliest
}

func (s *Scheduler) verifyQueuedTraders(ctx context.Context, candidates []domain.QueueCandidate, planned map[int64]time.Time) (map[int64]broker.Client, map[int64]error) {
	traders := make(map[int64]domain.Trader)
	firstTargets := make(map[int64]time.Time)
	for _, candidate := range candidates {
		traders[candidate.Trader.ID] = candidate.Trader
		target := planned[candidate.Order.ID]
		if firstTargets[candidate.Trader.ID].IsZero() || target.Before(firstTargets[candidate.Trader.ID]) {
			firstTargets[candidate.Trader.ID] = target
		}
	}
	clients := make(map[int64]broker.Client, len(traders))
	errorsByTrader := make(map[int64]error)
	for id, trader := range traders {
		clients[id] = s.factory.Client(trader.APIKey, trader.AccessToken)
	}

	type verification struct {
		id  int64
		err error
	}
	channel := make(chan verification, len(traders))
	checks := 0
	for id, trader := range traders {
		deadline := firstTargets[id].Add(-s.warmBefore())
		if !time.Now().Add(s.readinessCallReserve).Before(deadline) {
			continue
		}
		checks++
		id, trader, client := id, trader, clients[id]
		go func() {
			identityChanged := false
			err := s.retryReadiness(ctx, deadline, func(profileCtx context.Context) error {
				clientID, profileErr := client.Profile(profileCtx)
				if profileErr != nil {
					return profileErr
				}
				identityChanged = !strings.EqualFold(clientID, trader.ClientID)
				return nil
			})
			if identityChanged {
				err = errors.New("Kite profile identity changed")
			} else if err != nil && ctx.Err() == nil {
				// The same-day token was identity-bound during Kite login. A
				// temporary readiness outage must not suppress placement.
				slog.Warn("Kite profile readiness check exhausted; retaining bound daily session", "trader_id", id, "error", err)
				err = nil
			}
			channel <- verification{id: id, err: err}
		}()
	}
	for range checks {
		verified := <-channel
		if verified.err != nil {
			errorsByTrader[verified.id] = verified.err
			delete(clients, verified.id)
		}
	}
	return clients, errorsByTrader
}

// loadPersistedQueuedUnknown performs only SQLite reads and client creation.
// The caller must reconcile these jobs only after every still-future placement
// call has returned.
func (s *Scheduler) loadPersistedQueuedUnknown(ctx context.Context, date string) ([]dispatch.PreparedJob, []domain.DispatchResult, error) {
	candidates, err := s.store.UnknownQueuedCandidates(ctx, date)
	if err != nil {
		return nil, nil, err
	}
	results, err := s.store.UnknownQueuedResults(ctx, date)
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 0 && len(results) == 0 {
		return nil, nil, nil
	}
	resultIndex := make(map[int64]int, len(results))
	for index, result := range results {
		if _, exists := resultIndex[result.IntentID]; exists {
			return nil, nil, fmt.Errorf("multiple UNKNOWN records for queued order %d", result.IntentID)
		}
		resultIndex[result.IntentID] = index
	}
	jobs := make([]dispatch.PreparedJob, 0, len(candidates))
	seen := make(map[int64]bool, len(candidates))
	now := time.Now()
	for _, candidate := range candidates {
		index, ok := resultIndex[candidate.Order.ID]
		if !ok {
			return nil, nil, fmt.Errorf("UNKNOWN queued order %d has no dispatch record", candidate.Order.ID)
		}
		seen[candidate.Order.ID] = true
		result := &results[index]
		if result.AccountID != candidate.Trader.ID {
			return nil, nil, fmt.Errorf("UNKNOWN queued order %d trader binding changed", candidate.Order.ID)
		}
		if !candidate.Trader.DeletedAt.IsZero() {
			const detail = "order-book reconciliation unavailable: local trader profile was deleted"
			if !strings.Contains(result.ErrorMessage, detail) {
				result.ErrorMessage = appendResultDetail(result.ErrorMessage, detail)
			}
			continue
		}
		if candidate.Trader.AccessToken == "" || !now.Before(candidate.Trader.SessionExpiresAt) {
			result.ErrorMessage = appendResultDetail(result.ErrorMessage, "order-book reconciliation unavailable: daily Kite token is not ready")
			continue
		}
		jobs = append(jobs, dispatch.PreparedJob{
			IntentID: candidate.Order.ID, AccountID: candidate.Trader.ID,
			Position: candidate.Order.Position, Priority: candidate.Order.Position,
			Client: s.factory.Client(candidate.Trader.APIKey, candidate.Trader.AccessToken),
			Params: candidate.Order.Params, Tag: candidate.Order.Tag,
		})
	}
	for _, result := range results {
		if !seen[result.IntentID] {
			return nil, nil, fmt.Errorf("UNKNOWN dispatch record for queued order %d has no order", result.IntentID)
		}
	}
	return jobs, results, nil
}

func (s *Scheduler) finishRecoveredQueued(ctx context.Context, date string, target time.Time, jobs []dispatch.PreparedJob, results []domain.DispatchResult, emptyStatus string) error {
	if len(results) == 0 {
		s.setStatus(emptyStatus)
		return nil
	}
	persist := resultPersister(s.store.ApplyQueuedDispatchResults)
	if err := s.persistResultsWithRetryUsing(results, s.persistenceDeadline(ctx), persist); err != nil {
		return fmt.Errorf("persist recovered queued results: %w", err)
	}
	s.monitorResultsUsing(ctx, target, jobs, results, persist)
	s.setStatus(summarize(date, results))
	return nil
}

func (s *Scheduler) recoverStaleQueuedDates(ctx context.Context, beforeDate string, now time.Time) error {
	dates, err := s.store.StaleQueuedDates(ctx, beforeDate)
	if err != nil {
		return fmt.Errorf("list stale queued dates: %w", err)
	}
	for _, date := range dates {
		settings, err := s.store.SettingsForDate(ctx, date)
		if err != nil {
			return fmt.Errorf("load settings for stale date %s: %w", date, err)
		}
		target, err := store.SettingsDispatchAt(settings, date)
		if err != nil {
			return err
		}
		if err := s.store.RecoverPreparedQueuedOrders(ctx, date, now); err != nil {
			return fmt.Errorf("recover stale prepared queue %s: %w", date, err)
		}
		candidates, err := s.store.QueuedCandidates(ctx, date)
		if err != nil {
			return err
		}
		planned, err := plannedQueuedTargets(target, settings, candidates)
		if err != nil {
			return err
		}
		inactiveTargets := make(map[int64]time.Time)
		for _, candidate := range candidates {
			if !candidate.Trader.ActiveFor(date, planned[candidate.Order.ID]) {
				inactiveTargets[candidate.Order.ID] = planned[candidate.Order.ID]
			}
		}
		inactive, err := s.store.MarkInactiveQueuedOrders(ctx, date, inactiveTargets, now)
		if err != nil {
			return err
		}
		missedTargets := make(map[int64]time.Time)
		for _, candidate := range candidates {
			if !inactive[candidate.Order.ID] {
				missedTargets[candidate.Order.ID] = planned[candidate.Order.ID]
			}
		}
		if _, err := s.store.MarkMissedQueuedOrders(ctx, date, missedTargets, now, "server was not running through this dispatch date"); err != nil {
			return err
		}
		slog.Warn("finalized stale queued work without resubmission", "date", date)
	}
	return nil
}

// sortedQueuedResults gives deterministic persistence and test output without
// affecting request-start order, which is already captured in timestamps.
func sortedQueuedResults(results []domain.DispatchResult) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].TargetAt.Equal(results[j].TargetAt) {
			return results[i].IntentID < results[j].IntentID
		}
		return results[i].TargetAt.Before(results[j].TargetAt)
	})
}
