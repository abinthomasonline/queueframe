package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/store"
)

func (a *App) adminSettingsPage(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	now := time.Now()
	settings, err := a.store.CurrentSettings(r.Context(), now)
	if err != nil {
		http.Error(w, "runtime settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	earliest := nextWeekdayDate(now.In(a.location)).Format("2006-01-02")
	target, _ := dispatchAt(settings, earliest)
	history, _ := a.store.SettingsHistory(r.Context())
	a.render(w, r, "admin_settings", pageData{
		Title: "Settings", Admin: admin, Settings: settings, SettingsHistory: history,
		RedirectURL: a.config.RedirectURL, EarliestSettingsDate: earliest,
		FreezeLeadMinutes: durationMinutes(settings.FreezeLead), BatchIntervalMillis: settings.BatchInterval.Milliseconds(),
		FreezeTime: target.Add(-settings.FreezeLead).In(a.location).Format("15:04:05"),
	})
}

func (a *App) updateAdminSettings(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	now := time.Now()
	earliest := nextWeekdayDate(now.In(a.location)).Format("2006-01-02")
	effectiveDate := strings.TrimSpace(r.Form.Get("effective_date"))
	if effectiveDate < earliest {
		a.redirect(w, r, "/admin/settings", "error", "Settings must become effective on a future weekday.")
		return
	}
	effective, err := time.ParseInLocation("2006-01-02", effectiveDate, a.location)
	if err != nil || effective.Weekday() == time.Saturday || effective.Weekday() == time.Sunday {
		a.redirect(w, r, "/admin/settings", "error", "Settings must become effective on a weekday using YYYY-MM-DD.")
		return
	}
	freezeMinutes, err := strconv.Atoi(r.Form.Get("freeze_lead_minutes"))
	if err != nil || freezeMinutes <= 0 {
		a.redirect(w, r, "/admin/settings", "error", "Preparation lead must be a positive whole number of minutes.")
		return
	}
	batchMS, err := strconv.Atoi(r.Form.Get("batch_interval_ms"))
	if err != nil {
		a.redirect(w, r, "/admin/settings", "error", "Batch interval must be a whole number of milliseconds.")
		return
	}
	maxOrders, err := strconv.Atoi(r.Form.Get("max_orders_per_trader"))
	if err != nil {
		a.redirect(w, r, "/admin/settings", "error", "Maximum orders must be a whole number.")
		return
	}
	settings := domain.RuntimeSettings{
		DispatchClock:      normalizeHTMLClock(r.Form.Get("dispatch_clock")),
		UserBlackoutStart:  normalizeHTMLClock(r.Form.Get("blackout_start")),
		UserBlackoutEnd:    normalizeHTMLClock(r.Form.Get("blackout_end")),
		FreezeLead:         time.Duration(freezeMinutes) * time.Minute,
		BatchInterval:      time.Duration(batchMS) * time.Millisecond,
		MaxOrdersPerTrader: maxOrders,
	}
	saved, err := a.store.ScheduleSettings(r.Context(), settings, effectiveDate, admin.ID, now)
	if err != nil {
		a.redirect(w, r, "/admin/settings", "error", err.Error())
		return
	}
	a.operatorAudit(r, "ADMIN", admin.ID, "SETTINGS_SCHEDULED", "RUNTIME_SETTINGS", saved.ID, "administrator scheduled settings version")
	a.redirect(w, r, "/admin/settings", "notice", "A new immutable settings version was scheduled for "+effectiveDate+".")
}

func (a *App) adminSecurityPage(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	count, _ := a.store.CountAdminSessions(r.Context(), admin.ID, time.Now())
	a.render(w, r, "admin_security", pageData{Title: "Security", Admin: admin, AdminSessionCount: count})
}

func (a *App) changeAdminPasswordV2(w http.ResponseWriter, r *http.Request) {
	admin := adminFrom(r)
	count, _ := a.store.CountAdminSessions(r.Context(), admin.ID, time.Now())
	newPassword := r.Form.Get("new_password")
	if newPassword != r.Form.Get("confirm_password") {
		a.render(w, r, "admin_security", pageData{Title: "Security", Admin: admin, AdminSessionCount: count, Error: "The new passwords do not match."})
		return
	}
	if err := a.store.ChangeAdminPassword(r.Context(), admin.ID, r.Form.Get("current_password"), newPassword, time.Now()); err != nil {
		a.render(w, r, "admin_security", pageData{Title: "Security", Admin: admin, AdminSessionCount: count, Error: err.Error()})
		return
	}
	a.operatorAudit(r, "ADMIN", admin.ID, "PASSWORD_CHANGED", "ADMIN", admin.ID, "administrator password changed; sessions revoked")
	a.clearCookie(w, adminSessionCookie, http.SameSiteStrictMode)
	a.clearCSRFCookie(w)
	a.redirect(w, r, "/admin/login", "notice", "Password changed. Every administrator session was revoked; sign in again.")
}

func (a *App) deleteTraderAsAdmin(w http.ResponseWriter, r *http.Request) {
	traderID, err := parseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid trader", http.StatusBadRequest)
		return
	}
	trader, err := a.store.TraderByID(r.Context(), traderID)
	if err != nil {
		http.Error(w, "trader not found", http.StatusNotFound)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.Form.Get("confirm_client_id")), trader.DisplayClientID) {
		a.redirect(w, r, "/admin/users", "error", "Enter the exact Zerodha client ID to delete this local trader profile.")
		return
	}
	if err := a.store.DeleteTrader(r.Context(), traderID, time.Now()); err != nil {
		status := http.StatusConflict
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	a.operatorAudit(r, "ADMIN", adminFrom(r).ID, "TRADER_DELETED", "TRADER", traderID, "administrator deleted local trader profile")
	a.redirect(w, r, "/admin/users", "notice", "The local trader profile was deleted and its credentials were erased.")
}

func nextWeekdayDate(now time.Time) time.Time {
	date := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	for date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		date = date.AddDate(0, 0, 1)
	}
	return date
}
