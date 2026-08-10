package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/store"
)

func (a *App) accountPageV2(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	now := time.Now()
	settings, err := a.store.CurrentSettings(r.Context(), now)
	if err != nil {
		http.Error(w, "runtime settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	today := now.In(a.location).Format("2006-01-02")
	queue, err := a.store.ListTraderOrders(r.Context(), trader.ID, today)
	if err != nil {
		http.Error(w, "could not load today's order queue", http.StatusInternalServerError)
		return
	}
	activation, activationFound, err := a.store.TraderDailyActivation(r.Context(), trader.ID, today)
	if err != nil {
		http.Error(w, "could not load daily activation", http.StatusInternalServerError)
		return
	}
	activationState, err := resolveActivationView(now, queue, settings, activation, activationFound)
	if err != nil {
		http.Error(w, "could not calculate daily activation", http.StatusInternalServerError)
		return
	}
	nextDate := a.minimumMutableDate(r, now)
	nextSettings, err := a.store.SettingsForDate(r.Context(), nextDate)
	if err != nil {
		http.Error(w, "next runtime settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	nextTarget, err := dispatchAt(nextSettings, nextDate)
	if err != nil {
		http.Error(w, "could not calculate the next dispatch", http.StatusInternalServerError)
		return
	}
	a.render(w, r, "account", pageData{
		Title: "Account", Trader: trader,
		Activation: activationState, KiteSessionCurrent: trader.AccessToken != "" && now.Before(trader.SessionExpiresAt),
		MaskedAPIKey:   maskCredential(trader.APIKey),
		RedirectURL:    a.config.RedirectURL,
		BlackoutActive: store.UserLoginBlocked(settings, now), BlackoutStart: formatClock(settings.UserBlackoutStart),
		BlackoutEnd: formatClock(settings.UserBlackoutEnd), NextDispatchDate: nextDate,
		DispatchTime: nextTarget.In(a.location).Format("15:04:05.000"),
	})
}

func (a *App) deleteTraderProfile(w http.ResponseWriter, r *http.Request) {
	trader := traderFrom(r)
	if !strings.EqualFold(strings.TrimSpace(r.Form.Get("client_id")), trader.ClientID) {
		a.redirect(w, r, "/account", "error", "Enter the exact Zerodha client ID to delete this local profile.")
		return
	}
	if err := a.store.DeleteTrader(r.Context(), trader.ID, time.Now()); err != nil {
		message := err.Error()
		if err == store.ErrTraderDeleted {
			message = "This local profile has already been deleted."
		}
		a.redirect(w, r, "/account", "error", message)
		return
	}
	a.operatorAudit(r, "TRADER", trader.ID, "TRADER_DELETED", "TRADER", trader.ID, "trader deleted local Queueframe profile")
	if cookie, err := r.Cookie(traderSessionCookie); err == nil {
		_ = a.store.DeleteTraderWebSession(r.Context(), cookie.Value)
	}
	a.clearCookie(w, traderSessionCookie, http.SameSiteLaxMode)
	a.clearCSRFCookie(w)
	a.redirect(w, r, "/", "notice", "Local Queueframe profile deleted. Your Zerodha account and Kite Connect app were not changed.")
}

func maskCredential(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 6 {
		return "••••••"
	}
	return value[:3] + "•••" + value[len(value)-3:]
}
