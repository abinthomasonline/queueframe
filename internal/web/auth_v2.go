package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/store"
)

const (
	traderSessionCookie = "queueframe_trader_session"
	adminSessionCookie  = "queueframe_admin_session"
)

type traderContextKey struct{}
type adminContextKey struct{}

func traderFrom(r *http.Request) domain.Trader {
	trader, _ := r.Context().Value(traderContextKey{}).(domain.Trader)
	return trader
}

func adminFrom(r *http.Request) domain.Admin {
	admin, _ := r.Context().Value(adminContextKey{}).(domain.Admin)
	return admin
}

func (a *App) traderAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(traderSessionCookie)
		if err != nil {
			a.redirect(w, r, "/", "error", "Sign in with Kite to continue.")
			return
		}
		trader, err := a.store.TraderByWebSession(r.Context(), cookie.Value, time.Now())
		if err != nil {
			a.clearCookie(w, traderSessionCookie, http.SameSiteLaxMode)
			a.clearCSRFCookie(w)
			a.redirect(w, r, "/", "error", "Your local session expired. Sign in with Kite again.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traderContextKey{}, trader)))
	})
}

func (a *App) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(adminSessionCookie)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		admin, err := a.store.AdminByWebSession(r.Context(), cookie.Value, time.Now())
		if err != nil {
			a.clearCookie(w, adminSessionCookie, http.SameSiteStrictMode)
			a.clearCSRFCookie(w)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if admin.MustChangePassword && !requiredAdminPasswordRoute(r) {
			http.Redirect(w, r, "/admin/security", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminContextKey{}, admin)))
	})
}

func requiredAdminPasswordRoute(r *http.Request) bool {
	return (r.Method == http.MethodGet && r.URL.Path == "/admin/security") ||
		(r.Method == http.MethodPost && (r.URL.Path == "/admin/security/password" || r.URL.Path == "/logout"))
}

func (a *App) publicHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Has("invite") {
		// Exchange the URL bearer immediately, then redirect to a clean URL.
		// The raw token is never rendered into a form or retained in browser
		// history/referrers after this response.
		w.Header().Set("Referrer-Policy", "no-referrer")
		rawToken := strings.TrimSpace(r.URL.Query().Get("invite"))
		invitation, err := a.store.AvailableSignupInvitation(r.Context(), rawToken, time.Now())
		if err != nil {
			a.clearCookie(w, signupInviteCookie, http.SameSiteStrictMode)
			a.redirect(w, r, "/", "error", "This signup invitation is invalid, expired, revoked, already used, or currently in use.")
			return
		}
		cookieTTL := 30 * time.Minute
		if remaining := time.Until(invitation.ExpiresAt); remaining < cookieTTL {
			cookieTTL = remaining
		}
		if cookieTTL <= 0 {
			a.clearCookie(w, signupInviteCookie, http.SameSiteStrictMode)
			a.redirect(w, r, "/", "error", "This signup invitation is no longer available.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: signupInviteCookie, Value: rawToken, Path: "/", Secure: a.config.SecureCookies,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(cookieTTL.Seconds()), Expires: time.Now().Add(cookieTTL),
		})
		a.redirect(w, r, "/", "notice", "Invitation accepted. Complete first-time setup within 30 minutes.")
		return
	}
	if cookie, err := r.Cookie(traderSessionCookie); err == nil {
		if _, err := a.store.TraderByWebSession(r.Context(), cookie.Value, time.Now()); err == nil {
			http.Redirect(w, r, "/orders", http.StatusSeeOther)
			return
		}
	}
	// Auth flows are short-lived and may contain encrypted onboarding secrets.
	// Cleanup is best-effort so a maintenance failure does not hide the login
	// page; consumed failures are scrubbed immediately in the callback below.
	_ = a.store.PurgeExpiredKiteAuthFlows(r.Context(), time.Now())
	settings, err := a.store.CurrentSettings(r.Context(), time.Now())
	if err != nil {
		http.Error(w, "runtime settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	signupInviteValid := false
	if cookie, err := r.Cookie(signupInviteCookie); err == nil {
		if _, err := a.store.AvailableSignupInvitation(r.Context(), cookie.Value, time.Now()); err == nil {
			signupInviteValid = true
		} else {
			a.clearCookie(w, signupInviteCookie, http.SameSiteStrictMode)
		}
	}
	a.render(w, r, "login", pageData{
		Title: "Connect", RedirectURL: a.config.RedirectURL,
		SignupEnabled:     a.config.ExpectedPublicIP != "" && a.config.RedirectURL != "",
		SignupInviteValid: signupInviteValid,
		BlackoutActive:    store.UserLoginBlocked(settings, time.Now()),
		BlackoutStart:     formatClock(settings.UserBlackoutStart), BlackoutEnd: formatClock(settings.UserBlackoutEnd),
	})
}

func (a *App) beginSignup(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if !a.userLoginAllowed(w, r, now) {
		return
	}
	if !a.allowKiteStart(w, r, r.Form.Get("client_id"), now) {
		return
	}
	if a.config.ExpectedPublicIP == "" || a.config.RedirectURL == "" {
		a.redirect(w, r, "/", "error", "Signup is disabled until the operator configures this instance's static egress IP and public base URL.")
		return
	}
	inviteCookie, err := r.Cookie(signupInviteCookie)
	if err != nil || strings.TrimSpace(inviteCookie.Value) == "" {
		a.redirect(w, r, "/", "error", "A valid administrator-created invitation is required for first-time setup.")
		return
	}
	if r.Form.Get("family_attestation") != "yes" {
		a.redirect(w, r, "/", "error", "You must confirm the private household and static-IP declaration.")
		return
	}
	flow, err := a.store.BeginTraderSignup(r.Context(), domain.TraderSignup{
		ClientID: strings.TrimSpace(r.Form.Get("client_id")), APIKey: strings.TrimSpace(r.Form.Get("api_key")),
		APISecret:         strings.TrimSpace(r.Form.Get("api_secret")),
		InviteToken:       inviteCookie.Value,
		PolicyAttestation: domain.PolicyAttestation{Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText},
	}, now)
	if err != nil {
		if errors.Is(err, store.ErrSignupInvitationUnavailable) {
			a.clearCookie(w, signupInviteCookie, http.SameSiteStrictMode)
		}
		a.redirect(w, r, "/", "error", publicSignupError(err))
		return
	}
	a.clearCookie(w, signupInviteCookie, http.SameSiteStrictMode)
	http.Redirect(w, r, a.broker.LoginURL(flow.APIKey, flow.State), http.StatusSeeOther)
}

func (a *App) beginSignin(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if !a.userLoginAllowed(w, r, now) {
		return
	}
	if !a.allowKiteStart(w, r, r.Form.Get("client_id"), now) {
		return
	}
	flow, err := a.store.BeginTraderSignin(r.Context(), r.Form.Get("client_id"), now)
	if err != nil {
		// Do not expose whether a client ID is registered.
		a.redirect(w, r, "/", "error", "Unable to start Kite sign-in. Check the client ID or use first-time setup.")
		return
	}
	http.Redirect(w, r, a.broker.LoginURL(flow.APIKey, flow.State), http.StatusSeeOther)
}

func (a *App) kiteCallbackV2(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	settings, err := a.store.CurrentSettings(r.Context(), now)
	if err != nil || store.UserLoginBlocked(settings, now) {
		a.redirect(w, r, "/", "error", "Kite login cannot be completed during the configured trader-login blackout.")
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	requestToken := strings.TrimSpace(r.URL.Query().Get("request_token"))
	if state == "" || requestToken == "" {
		a.redirect(w, r, "/", "error", "Kite login was cancelled or returned an incomplete response.")
		return
	}
	flow, err := a.store.ConsumeKiteAuthFlow(r.Context(), state, now)
	if err != nil {
		a.redirect(w, r, "/", "error", "This Kite login link expired or was already used. Start again.")
		return
	}
	flowCompleted := false
	defer func() {
		if !flowCompleted {
			_ = a.store.AbandonKiteAuthFlow(context.WithoutCancel(r.Context()), flow.ID, time.Now())
		}
	}()
	session, err := a.broker.GenerateSession(r.Context(), flow.APIKey, flow.APISecret, requestToken)
	if err != nil {
		a.redirect(w, r, "/", "error", "Kite could not validate this login. Start again.")
		return
	}
	if session.LoginAt.IsZero() {
		session.LoginAt = now
	}
	profileID, err := a.broker.Client(flow.APIKey, session.AccessToken).Profile(r.Context())
	if err != nil || !strings.EqualFold(strings.TrimSpace(profileID), strings.TrimSpace(flow.ClientID)) {
		a.redirect(w, r, "/", "error", "The Kite profile did not match the Zerodha client ID supplied to Queueframe.")
		return
	}
	if current, err := a.store.CurrentSettings(r.Context(), time.Now()); err != nil || store.UserLoginBlocked(current, time.Now()) {
		a.redirect(w, r, "/", "error", "The trader-login blackout began before Kite validation completed. Sign in again after it ends.")
		return
	}
	trader, err := a.store.CompleteKiteAuth(r.Context(), flow.ID, session, nextSixAM(session.LoginAt, a.location))
	if err != nil {
		a.redirect(w, r, "/", "error", "The Kite login identity or API key did not match this account setup.")
		return
	}
	flowCompleted = true
	event := "KITE_SIGNIN_VERIFIED"
	if flow.Purpose == domain.KiteAuthSignup {
		event = "KITE_SIGNUP_VERIFIED"
	}
	a.operatorAudit(r, "TRADER", trader.ID, event, "TRADER", trader.ID, "Kite client ID and API key verified")
	// Symbol loading is read-only and outside dispatch. A temporary failure does
	// not invalidate the verified session; order creation remains unavailable
	// until a later refresh succeeds.
	_ = a.catalog.Refresh(r.Context(), a.broker.Client(trader.APIKey, trader.AccessToken))
	token, err := a.store.CreateTraderWebSession(r.Context(), trader.ID, a.config.SessionLifetime, now)
	if err != nil {
		http.Error(w, "could not create local session", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, traderSessionCookie, token, http.SameSiteLaxMode)
	a.clearCookie(w, adminSessionCookie, http.SameSiteStrictMode)
	a.clearCSRFCookie(w)
	a.redirect(w, r, "/orders", "notice", a.kiteAuthSuccessNotice(r, trader, session, time.Now()))
}

func (a *App) kiteAuthSuccessNotice(r *http.Request, trader domain.Trader, session broker.Session, completedAt time.Time) string {
	local := completedAt.In(a.location)
	executionDate := local.Format("2006-01-02")
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return resolveKiteAuthSuccessNotice(completedAt, session.LoginAt, time.Time{}, time.Time{}, false, false, false)
	}
	settings, err := a.store.SettingsForDate(r.Context(), executionDate)
	if err != nil {
		return "Kite sign-in succeeded."
	}
	openAt, closeAt, err := store.DailyActivationWindow(settings, executionDate)
	if err != nil {
		return "Kite sign-in succeeded."
	}
	target, err := dispatchAt(settings, executionDate)
	if err != nil {
		return "Kite sign-in succeeded."
	}
	activation, found, err := a.store.TraderDailyActivation(r.Context(), trader.ID, executionDate)
	thisLoginActivated := err == nil && found && activation.ActiveFor(executionDate, target) &&
		activation.LoginAt.UnixMilli() == session.LoginAt.UnixMilli()
	queue, queueErr := a.store.ListTraderOrders(r.Context(), trader.ID, executionDate)
	hasScheduled := false
	if queueErr == nil {
		for _, order := range queue.Orders {
			if order.State == domain.OrderStateScheduled {
				hasScheduled = true
				break
			}
		}
	}
	return resolveKiteAuthSuccessNotice(completedAt, session.LoginAt, openAt, closeAt, true, thisLoginActivated, hasScheduled)
}

func resolveKiteAuthSuccessNotice(completedAt, sessionLoginAt, openAt, closeAt time.Time, weekday, thisLoginActivated, hasScheduled bool) string {
	if !weekday {
		return "Kite sign-in succeeded. No daily activation is needed today because Queueframe does not dispatch weekend orders."
	}
	if thisLoginActivated {
		if hasScheduled {
			return "Kite sign-in succeeded. Today's scheduled orders are activated for their planned request times."
		}
		return "Kite sign-in succeeded. This login is inside today's activation window; no orders are currently scheduled today."
	}
	if completedAt.Before(openAt) || sessionLoginAt.Before(openAt) {
		return "Kite sign-in succeeded. If you schedule orders for today, sign in again between 06:00 and " + closeAt.In(india).Format("15:04:05") + " IST to activate them."
	}
	if !completedAt.Before(closeAt) {
		return "Kite sign-in succeeded. Today's activation window closed at " + closeAt.In(india).Format("15:04:05") + " IST, so this login provides Queueframe access but does not activate today's orders."
	}
	return "Kite sign-in succeeded, but this session did not qualify for today's scheduled dispatch."
}

func (a *App) allowKiteStart(w http.ResponseWriter, r *http.Request, clientID string, now time.Time) bool {
	remote := a.clientIP(r)
	clientID = strings.ToUpper(strings.TrimSpace(clientID))
	if !a.loginIPs.Allow("kite:"+remote, now) || !a.loginUsers.Allow("kite:"+clientID, now) {
		http.Error(w, "too many Kite sign-in attempts; wait before trying again", http.StatusTooManyRequests)
		return false
	}
	return true
}

func (a *App) userLoginAllowed(w http.ResponseWriter, r *http.Request, now time.Time) bool {
	settings, err := a.store.CurrentSettings(r.Context(), now)
	if err != nil {
		http.Error(w, "runtime settings are unavailable", http.StatusServiceUnavailable)
		return false
	}
	if store.UserLoginBlocked(settings, now) {
		a.redirect(w, r, "/", "error", "Trader login is unavailable from "+formatClock(settings.UserBlackoutStart)+" to "+formatClock(settings.UserBlackoutEnd)+" IST.")
		return false
	}
	return true
}

func publicSignupError(err error) string {
	switch {
	case errors.Is(err, store.ErrConflict):
		return "That client ID or API key is already registered. Use returning-user sign in."
	case errors.Is(err, store.ErrPolicyAttestationRequired):
		return "The private household and static-IP declaration is required."
	case errors.Is(err, store.ErrSignupInvitationUnavailable):
		return "This signup invitation is invalid, expired, revoked, already used, or currently in use. Ask the administrator for a new link."
	case err.Error() == "client ID, API key, and API secret are required":
		return err.Error()
	default:
		return "Unable to save this setup. Check the client ID, API key, and API secret, then try again."
	}
}

func (a *App) adminLoginPageV2(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminSessionCookie); err == nil {
		if _, err := a.store.AdminByWebSession(r.Context(), cookie.Value, time.Now()); err == nil {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
	}
	a.render(w, r, "admin_login", pageData{Title: "Administrator sign in"})
}

func (a *App) adminLoginV2(w http.ResponseWriter, r *http.Request) {
	remote := a.clientIP(r)
	username := strings.ToLower(strings.TrimSpace(r.Form.Get("username")))
	now := time.Now()
	if !a.loginIPs.Allow("admin:"+remote, now) || !a.loginUsers.Allow("admin:"+username, now) {
		http.Error(w, "too many sign-in attempts; wait before trying again", http.StatusTooManyRequests)
		return
	}
	select {
	case a.authSlots <- struct{}{}:
		defer func() { <-a.authSlots }()
	default:
		http.Error(w, "sign-in service is busy; try again shortly", http.StatusTooManyRequests)
		return
	}
	admin, token, err := a.store.AuthenticateAdminAndCreateWebSession(r.Context(), username, r.Form.Get("password"), a.config.SessionLifetime, now)
	if err != nil {
		if errors.Is(err, store.ErrInvalidAdminCredentials) {
			a.render(w, r, "admin_login", pageData{Title: "Administrator sign in", Error: "Invalid username or password."})
			return
		}
		http.Error(w, "could not create administrator session", http.StatusInternalServerError)
		return
	}
	a.loginIPs.Reset("admin:" + remote)
	a.loginUsers.Reset("admin:" + username)
	a.setCookie(w, adminSessionCookie, token, http.SameSiteStrictMode)
	a.clearCookie(w, traderSessionCookie, http.SameSiteLaxMode)
	a.clearCSRFCookie(w)
	a.operatorAudit(r, "ADMIN", admin.ID, "ADMIN_LOGIN", "ADMIN", admin.ID, "local administrator login succeeded")
	if admin.MustChangePassword {
		http.Redirect(w, r, "/admin/security", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// clientIP returns the nearest untrusted address in an X-Forwarded-For chain,
// but only when the socket peer is an explicitly configured trusted proxy.
// This prevents public clients from spoofing rate-limit identities while
// allowing a loopback/Docker reverse proxy to preserve per-client limits.
func (a *App) clientIP(r *http.Request) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	} else {
		remote = strings.Trim(remote, "[]")
	}
	remoteIP := net.ParseIP(remote)
	if remoteIP == nil {
		return remote
	}
	remote = remoteIP.String()
	if !a.trustedProxy(remoteIP) {
		return remote
	}

	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return remote
	}
	chain := make([]net.IP, 0, len(values))
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			ip := net.ParseIP(strings.TrimSpace(item))
			if ip == nil {
				// A malformed chain is not trustworthy. Fall back to the socket
				// peer instead of accepting a potentially attacker-chosen value.
				return remote
			}
			chain = append(chain, ip)
		}
	}
	for index := len(chain) - 1; index >= 0; index-- {
		if !a.trustedProxy(chain[index]) {
			return chain[index].String()
		}
	}
	return remote
}

func (a *App) trustedProxy(ip net.IP) bool {
	for _, network := range a.config.TrustedProxyCIDRs {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *App) logoutV2(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(traderSessionCookie); err == nil {
		_ = a.store.DeleteTraderWebSession(r.Context(), cookie.Value)
	}
	if cookie, err := r.Cookie(adminSessionCookie); err == nil {
		_ = a.store.DeleteAdminWebSession(r.Context(), cookie.Value)
	}
	a.clearCookie(w, traderSessionCookie, http.SameSiteLaxMode)
	a.clearCookie(w, adminSessionCookie, http.SameSiteStrictMode)
	a.clearCSRFCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) setCookie(w http.ResponseWriter, name, token string, sameSite http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", Secure: a.config.SecureCookies,
		HttpOnly: true, SameSite: sameSite, MaxAge: int(a.config.SessionLifetime.Seconds())})
}

func (a *App) clearCookie(w http.ResponseWriter, name string, sameSite http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", Secure: a.config.SecureCookies,
		HttpOnly: true, SameSite: sameSite, MaxAge: -1})
}
