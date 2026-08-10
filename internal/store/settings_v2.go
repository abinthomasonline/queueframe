package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

func ValidateRuntimeSettings(settings domain.RuntimeSettings) error {
	for name, value := range map[string]string{
		"dispatch time":  settings.DispatchClock,
		"blackout start": settings.UserBlackoutStart,
		"blackout end":   settings.UserBlackoutEnd,
	} {
		if _, err := time.Parse("15:04:05", value); err != nil {
			return fmt.Errorf("%s must use HH:MM:SS: %w", name, err)
		}
	}
	if settings.UserBlackoutStart == settings.UserBlackoutEnd {
		return errors.New("user login blackout start and end must differ")
	}
	if settings.FreezeLead <= 0 {
		return errors.New("freeze lead must be positive")
	}
	if settings.BatchInterval < time.Second {
		return errors.New("batch interval must be at least one second")
	}
	if settings.MaxOrdersPerTrader <= 0 || settings.MaxOrdersPerTrader > 400 {
		return errors.New("maximum orders per trader must be between 1 and 400")
	}
	if !blackoutCoversDispatchWindow(settings) {
		return errors.New("trader login blackout must cover the full queue-preparation and dispatch window")
	}
	if !settingsLeaveDailyActivationWindow(settings) {
		return errors.New("schedule must leave a trader-login window after 06:00 IST and before queue preparation")
	}
	return nil
}

func blackoutCoversDispatchWindow(settings domain.RuntimeSettings) bool {
	const referenceDate = "2000-01-02 "
	dispatch, dispatchErr := time.ParseInLocation("2006-01-02 15:04:05", referenceDate+settings.DispatchClock, indiaLocation)
	blackoutStart, startErr := time.ParseInLocation("2006-01-02 15:04:05", referenceDate+settings.UserBlackoutStart, indiaLocation)
	blackoutEnd, endErr := time.ParseInLocation("2006-01-02 15:04:05", referenceDate+settings.UserBlackoutEnd, indiaLocation)
	if dispatchErr != nil || startErr != nil || endErr != nil {
		return false
	}
	if !blackoutEnd.After(blackoutStart) {
		blackoutEnd = blackoutEnd.Add(24 * time.Hour)
	}
	blackoutDuration := blackoutEnd.Sub(blackoutStart)
	freezeAt := dispatch.Add(-settings.FreezeLead)
	lastBatch := (settings.MaxOrdersPerTrader - 1) / 10
	lastTarget := dispatch.Add(time.Duration(lastBatch) * settings.BatchInterval)
	for dayShift := -2; dayShift <= 2; dayShift++ {
		candidateStart := blackoutStart.AddDate(0, 0, dayShift)
		candidateEnd := candidateStart.Add(blackoutDuration)
		if !freezeAt.Before(candidateStart) && lastTarget.Before(candidateEnd) {
			return true
		}
	}
	return false
}

func settingsLeaveDailyActivationWindow(settings domain.RuntimeSettings) bool {
	const referenceDate = "2000-01-02 "
	dispatch, dispatchErr := time.ParseInLocation("2006-01-02 15:04:05", referenceDate+settings.DispatchClock, indiaLocation)
	blackoutStart, startErr := time.ParseInLocation("2006-01-02 15:04:05", referenceDate+settings.UserBlackoutStart, indiaLocation)
	blackoutEnd, endErr := time.ParseInLocation("2006-01-02 15:04:05", referenceDate+settings.UserBlackoutEnd, indiaLocation)
	if dispatchErr != nil || startErr != nil || endErr != nil {
		return false
	}
	if !blackoutEnd.After(blackoutStart) {
		blackoutEnd = blackoutEnd.Add(24 * time.Hour)
	}
	blackoutDuration := blackoutEnd.Sub(blackoutStart)
	freezeAt := dispatch.Add(-settings.FreezeLead)
	lastBatch := (settings.MaxOrdersPerTrader - 1) / 10
	lastTarget := dispatch.Add(time.Duration(lastBatch) * settings.BatchInterval)
	activationAt := time.Date(dispatch.Year(), dispatch.Month(), dispatch.Day(), 6, 0, 0, 0, indiaLocation)
	if !freezeAt.After(activationAt) {
		return false
	}
	for dayShift := -2; dayShift <= 2; dayShift++ {
		candidateStart := blackoutStart.AddDate(0, 0, dayShift)
		candidateEnd := candidateStart.Add(blackoutDuration)
		if !freezeAt.Before(candidateStart) && lastTarget.Before(candidateEnd) && candidateStart.After(activationAt) {
			return true
		}
	}
	return false
}

func (s *Store) CurrentSettings(ctx context.Context, at time.Time) (domain.RuntimeSettings, error) {
	return s.SettingsForDate(ctx, at.In(indiaLocation).Format("2006-01-02"))
}

func (s *Store) SettingsForDate(ctx context.Context, executionDate string) (domain.RuntimeSettings, error) {
	if _, err := time.Parse("2006-01-02", executionDate); err != nil {
		return domain.RuntimeSettings{}, errors.New("execution date must use YYYY-MM-DD")
	}
	return scanRuntimeSettings(s.db.QueryRowContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
       freeze_lead_ms,batch_interval_ms,max_orders_per_trader,
       COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions WHERE effective_date<=?
ORDER BY effective_date DESC,version DESC LIMIT 1`, executionDate))
}

func scanRuntimeSettings(row scanner) (domain.RuntimeSettings, error) {
	var settings domain.RuntimeSettings
	var freezeMS, batchMS, createdMS int64
	err := row.Scan(&settings.ID, &settings.Version, &settings.EffectiveDate, &settings.DispatchClock,
		&settings.UserBlackoutStart, &settings.UserBlackoutEnd, &freezeMS, &batchMS,
		&settings.MaxOrdersPerTrader, &settings.CreatedByAdminID, &createdMS)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RuntimeSettings{}, ErrNotFound
	}
	if err != nil {
		return domain.RuntimeSettings{}, err
	}
	settings.FreezeLead = time.Duration(freezeMS) * time.Millisecond
	settings.BatchInterval = time.Duration(batchMS) * time.Millisecond
	settings.CreatedAt = fromMillis(createdMS)
	return settings, nil
}

func (s *Store) ScheduleSettings(ctx context.Context, settings domain.RuntimeSettings, effectiveDate string, adminID int64, now time.Time) (domain.RuntimeSettings, error) {
	if _, err := time.Parse("2006-01-02", effectiveDate); err != nil {
		return domain.RuntimeSettings{}, errors.New("effective date must use YYYY-MM-DD")
	}
	if err := ValidateRuntimeSettings(settings); err != nil {
		return domain.RuntimeSettings{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RuntimeSettings{}, err
	}
	defer tx.Rollback()
	var adminExists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM admins WHERE id=?)`, adminID).Scan(&adminExists); err != nil {
		return domain.RuntimeSettings{}, err
	}
	if adminExists == 0 {
		return domain.RuntimeSettings{}, ErrNotFound
	}
	var frozen int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM queued_date_freezes WHERE execution_date=?)`, effectiveDate).Scan(&frozen); err != nil {
		return domain.RuntimeSettings{}, err
	}
	if frozen != 0 {
		return domain.RuntimeSettings{}, ErrQueueFrozen
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM runtime_settings_versions`).Scan(&version); err != nil {
		return domain.RuntimeSettings{}, err
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO runtime_settings_versions(
 version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
 freeze_lead_ms,batch_interval_ms,max_orders_per_trader,created_by_admin_id,created_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?)`, version, effectiveDate, settings.DispatchClock, settings.UserBlackoutStart,
		settings.UserBlackoutEnd, settings.FreezeLead.Milliseconds(), settings.BatchInterval.Milliseconds(),
		settings.MaxOrdersPerTrader, adminID, millis(now))
	if err != nil {
		return domain.RuntimeSettings{}, err
	}
	settings.ID, _ = result.LastInsertId()
	settings.Version = version
	settings.EffectiveDate = effectiveDate
	settings.CreatedByAdminID = adminID
	settings.CreatedAt = now
	if err := tx.Commit(); err != nil {
		return domain.RuntimeSettings{}, err
	}
	return settings, nil
}

func (s *Store) SettingsHistory(ctx context.Context) ([]domain.RuntimeSettings, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
       freeze_lead_ms,batch_interval_ms,max_orders_per_trader,
       COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions ORDER BY version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []domain.RuntimeSettings
	for rows.Next() {
		settings, err := scanRuntimeSettings(rows)
		if err != nil {
			return nil, err
		}
		history = append(history, settings)
	}
	return history, rows.Err()
}

// UserLoginBlocked applies the configured half-open blackout interval in IST.
// It supports an interval that crosses midnight even though the defaults do
// not.
func UserLoginBlocked(settings domain.RuntimeSettings, now time.Time) bool {
	local := now.In(indiaLocation)
	clock := local.Format("15:04:05")
	if settings.UserBlackoutStart < settings.UserBlackoutEnd {
		return clock >= settings.UserBlackoutStart && clock < settings.UserBlackoutEnd
	}
	return clock >= settings.UserBlackoutStart || clock < settings.UserBlackoutEnd
}

func SettingsDispatchAt(settings domain.RuntimeSettings, executionDate string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04:05", executionDate+" "+settings.DispatchClock, indiaLocation)
}

// DailyActivationWindow returns the absolute same-day Kite activation window
// for an execution date. The closing instant is the beginning of the
// configured blackout interval that protects this date's preparation and all
// possible dispatch batches. Runtime-settings validation guarantees that such
// an interval exists and that it begins after 06:00 IST.
func DailyActivationWindow(settings domain.RuntimeSettings, executionDate string) (time.Time, time.Time, error) {
	dispatch, err := SettingsDispatchAt(settings, executionDate)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	date, err := time.ParseInLocation("2006-01-02", executionDate, indiaLocation)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("execution date must use YYYY-MM-DD")
	}
	openAt := time.Date(date.Year(), date.Month(), date.Day(), 6, 0, 0, 0, indiaLocation)
	start, err := time.ParseInLocation("2006-01-02 15:04:05", executionDate+" "+settings.UserBlackoutStart, indiaLocation)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := time.ParseInLocation("2006-01-02 15:04:05", executionDate+" "+settings.UserBlackoutEnd, indiaLocation)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
	}
	duration := end.Sub(start)
	freezeAt := dispatch.Add(-settings.FreezeLead)
	lastTarget := dispatch
	if settings.MaxOrdersPerTrader > 0 {
		lastTarget = dispatch.Add(time.Duration((settings.MaxOrdersPerTrader-1)/10) * settings.BatchInterval)
	}
	for shift := -2; shift <= 2; shift++ {
		candidateStart := start.AddDate(0, 0, shift)
		candidateEnd := candidateStart.Add(duration)
		if !freezeAt.Before(candidateStart) && lastTarget.Before(candidateEnd) {
			if !candidateStart.After(openAt) {
				return time.Time{}, time.Time{}, errors.New("daily activation window must close after 06:00 IST")
			}
			return openAt, candidateStart, nil
		}
	}
	return time.Time{}, time.Time{}, errors.New("trader-login blackout does not cover this dispatch window")
}
