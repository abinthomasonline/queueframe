package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/broker"
	"github.com/abinthomasonline/queueframe/internal/config"
	"github.com/abinthomasonline/queueframe/internal/domain"
	"github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/secure"
	"github.com/abinthomasonline/queueframe/internal/store"
)

const csrfCookie = "queueframe_csrf"

const signupInviteCookie = "queueframe_signup_invite"

type App struct {
	store      *store.Store
	broker     broker.Factory
	catalog    *orders.Catalog
	config     config.Config
	location   *time.Location
	templates  *template.Template
	status     func() string
	loginIPs   *attemptLimiter
	loginUsers *attemptLimiter
	authSlots  chan struct{}
	csrfKey    []byte
	origin     *url.URL
}

//go:embed templates/*.html static/*
var assets embed.FS

type Options struct {
	Store   *store.Store
	Broker  broker.Factory
	Catalog *orders.Catalog
	Config  config.Config
	Status  func() string
}

func New(options Options) (*App, error) {
	if options.Store == nil || options.Broker == nil || options.Catalog == nil {
		return nil, errors.New("Queueframe web dependencies are required")
	}
	if options.Config.SessionLifetime <= 0 {
		return nil, errors.New("Queueframe web session lifetime must be positive")
	}
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return nil, err
	}
	functions := template.FuncMap{
		"time": func(value time.Time) string {
			if value.IsZero() {
				return "—"
			}
			return value.In(location).Format("02 Jan 2006 15:04:05 MST")
		},
		"precisetime": func(value time.Time) string {
			if value.IsZero() {
				return "—"
			}
			return value.In(location).Format("02 Jan 2006 15:04:05.000000 MST")
		},
		"receiptSource": func(source string) string {
			switch source {
			case domain.DispatchReceiptKiteResponse:
				return "Kite API response"
			case domain.DispatchReceiptOrderBook:
				return "Kite order-book receipt"
			case domain.DispatchReceiptCurrentSnapshot:
				return "Recorded broker snapshot"
			default:
				return "Broker receipt"
			}
		},
		"latency": func(start, finish time.Time) string {
			if start.IsZero() || finish.IsZero() {
				return "—"
			}
			return finish.Sub(start).Round(time.Microsecond).String()
		},
		"offset": func(target, start time.Time) string {
			if target.IsZero() || start.IsZero() {
				return "—"
			}
			delta := start.Sub(target).Round(time.Microsecond)
			if delta > 0 {
				return "+" + delta.String()
			}
			return delta.String()
		},
		"mask": func(value string) string {
			if len(value) <= 6 {
				return "••••••"
			}
			return value[:3] + "•••" + value[len(value)-3:]
		},
	}
	templates, err := template.New("pages").Funcs(functions).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if options.Status == nil {
		options.Status = func() string { return "starting" }
	}
	origin, err := url.Parse(options.Config.BaseURL)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return nil, errors.New("QUEUEFRAME_BASE_URL must be an absolute http(s) URL")
	}
	csrfKey := make([]byte, 32)
	if _, err := rand.Read(csrfKey); err != nil {
		return nil, err
	}
	return &App{store: options.Store, broker: options.Broker, catalog: options.Catalog, config: options.Config, location: location, templates: templates, status: options.Status, loginIPs: newAttemptLimiter(10, time.Minute), loginUsers: newAttemptLimiter(5, time.Minute), authSlots: make(chan struct{}, 4), csrfKey: csrfKey, origin: origin}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("GET /", a.publicHome)
	mux.Handle("POST /signup", a.csrf(http.HandlerFunc(a.beginSignup)))
	mux.Handle("POST /signin", a.csrf(http.HandlerFunc(a.beginSignin)))
	mux.HandleFunc("GET /auth/kite/callback", a.kiteCallbackV2)
	mux.HandleFunc("GET /kite/callback", a.kiteCallbackV2) // migration alias for an existing Kite app.
	mux.HandleFunc("GET /admin/login", a.adminLoginPageV2)
	mux.Handle("POST /admin/login", a.csrf(http.HandlerFunc(a.adminLoginV2)))
	mux.Handle("POST /logout", a.csrf(http.HandlerFunc(a.logoutV2)))

	mux.Handle("GET /orders", a.traderAuth(http.HandlerFunc(a.traderOrdersPage)))
	mux.Handle("GET /orders/new", a.traderAuth(http.HandlerFunc(a.newTraderOrderPage)))
	mux.Handle("GET /orders/schedule-preview", a.traderAuth(http.HandlerFunc(a.traderOrderSchedulePreview)))
	mux.Handle("POST /orders", a.traderAuth(a.csrf(http.HandlerFunc(a.createTraderOrder))))
	mux.Handle("POST /orders/reorder", a.traderAuth(a.csrf(http.HandlerFunc(a.reorderTraderOrders))))
	mux.Handle("POST /orders/{id}/delete", a.traderAuth(a.csrf(http.HandlerFunc(a.deleteTraderOrder))))
	mux.Handle("GET /instruments/search", a.traderAuth(http.HandlerFunc(a.instrumentSearch)))
	mux.Handle("GET /dispatches", a.traderAuth(http.HandlerFunc(a.traderDispatchesPage)))
	mux.Handle("GET /account", a.traderAuth(http.HandlerFunc(a.accountPageV2)))
	mux.Handle("POST /account/delete", a.traderAuth(a.csrf(http.HandlerFunc(a.deleteTraderProfile))))

	mux.Handle("GET /admin", a.adminAuth(http.HandlerFunc(a.adminDashboardV2)))
	mux.Handle("GET /admin/invites", a.adminAuth(http.HandlerFunc(a.adminInvitationsPage)))
	mux.Handle("POST /admin/invites", a.adminAuth(a.csrf(http.HandlerFunc(a.createSignupInvitation))))
	mux.Handle("POST /admin/invites/{id}/revoke", a.adminAuth(a.csrf(http.HandlerFunc(a.revokeSignupInvitation))))
	mux.Handle("GET /admin/users", a.adminAuth(http.HandlerFunc(a.adminUsersPage)))
	mux.Handle("POST /admin/users/{id}/delete", a.adminAuth(a.csrf(http.HandlerFunc(a.deleteTraderAsAdmin))))
	mux.Handle("GET /admin/orders", a.adminAuth(http.HandlerFunc(a.adminOrdersPageV2)))
	mux.Handle("GET /admin/instruments/search", a.adminAuth(http.HandlerFunc(a.adminInstrumentSearch)))
	mux.Handle("POST /admin/orders/reorder", a.adminAuth(a.csrf(http.HandlerFunc(a.reorderAdminOrders))))
	mux.Handle("GET /admin/orders/{id}/edit", a.adminAuth(http.HandlerFunc(a.adminOrderEditPage)))
	mux.Handle("POST /admin/orders/{id}/edit", a.adminAuth(a.csrf(http.HandlerFunc(a.updateAdminOrder))))
	mux.Handle("POST /admin/orders/{id}/delete", a.adminAuth(a.csrf(http.HandlerFunc(a.deleteAdminOrder))))
	mux.Handle("GET /admin/dispatches", a.adminAuth(http.HandlerFunc(a.adminDispatchesPageV2)))
	mux.Handle("GET /admin/settings", a.adminAuth(http.HandlerFunc(a.adminSettingsPage)))
	mux.Handle("POST /admin/settings", a.adminAuth(a.csrf(http.HandlerFunc(a.updateAdminSettings))))
	mux.Handle("GET /admin/security", a.adminAuth(http.HandlerFunc(a.adminSecurityPage)))
	mux.Handle("POST /admin/security/password", a.adminAuth(a.csrf(http.HandlerFunc(a.changeAdminPasswordV2))))

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusMovedPermanently) })
	mux.HandleFunc("GET /accounts", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/account", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /executions", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dispatches", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /healthz", a.health)
	return a.securityHeaders(mux)
}

type pageData struct {
	Title        string
	CSRF         string
	Notice       string
	Error        string
	Now          time.Time
	Today        string
	DryRun       bool
	Status       string
	ExpectedIP   string
	CatalogAt    time.Time
	SelectedDate string

	// Queueframe v2 authentication principals and deployment metadata.
	Trader             domain.Trader
	Admin              domain.Admin
	RedirectURL        string
	SignupEnabled      bool
	SignupInviteValid  bool
	BlackoutActive     bool
	BlackoutStart      string
	BlackoutEnd        string
	DispatchTime       string
	FreezeTime         string
	ActivationTime     string
	BatchIntervalLabel string
	BatchGapMillis     int64
	MaxOrdersPerDate   int
	MinExecutionDate   string
	QueueFrozen        bool
	QueueRevision      int64
	ActiveToday        bool
	ActivationLoginAt  time.Time
	Activation         activationView
	KiteSessionCurrent bool
	MaskedAPIKey       string

	Queue              domain.OrderQueue
	OrderRows          []orderRow
	DispatchRows       []dispatchRow
	RecentDispatchRows []dispatchRow
	DispatchSummary    dispatchSummary
	DispatchStatuses   []selectOption
	SelectedStatus     string
	SelectedSymbol     string

	Settings             domain.RuntimeSettings
	SettingsHistory      []domain.RuntimeSettings
	FreezeLeadMinutes    string
	BatchIntervalMillis  int64
	EarliestSettingsDate string
	NextDispatchDate     string

	AdminStats        adminStats
	Traders           []domain.Trader
	TraderRows        []traderRow
	AdminQueues       []adminQueue
	OrderStates       []selectOption
	SelectedClientID  string
	SearchQuery       string
	Admins            []domain.Admin
	AdminSessionCount int
	EditOrder         domain.QueuedOrder
	EditTrader        domain.Trader
	FormAction        string

	SignupInvitations  []signupInvitationRow
	GeneratedInviteURL string
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, data pageData) {
	data.CSRF = a.ensureCSRF(w, r)
	data.Notice = r.URL.Query().Get("notice")
	if data.Error == "" {
		data.Error = r.URL.Query().Get("error")
	}
	data.Now = time.Now().In(a.location)
	data.Today = data.Now.Format("2006-01-02")
	data.DryRun = a.config.DryRun
	data.Status = a.status()
	data.ExpectedIP = a.config.ExpectedPublicIP
	if data.RedirectURL == "" {
		data.RedirectURL = a.config.RedirectURL
	}
	data.CatalogAt = a.catalog.LoadedAt()
	var output bytes.Buffer
	if err := a.templates.ExecuteTemplate(&output, name, data); err != nil {
		slog.Error("render template", "template", name, "error", err)
		http.Error(w, "page rendering failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = output.WriteTo(w)
}

func (a *App) redirect(w http.ResponseWriter, r *http.Request, path, key, message string) {
	target, err := url.Parse(path)
	if err != nil {
		target = &url.URL{Path: "/"}
	}
	values := target.Query()
	if message != "" {
		values.Set(key, message)
	}
	target.RawQuery = values.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

func (a *App) parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	return true
}

func parseID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid ID")
	}
	return id, nil
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; form-action 'self' https://kite.zerodha.com https://kite.trade https://api.kite.trade https://cdslindia.com https://*.cdslindia.com; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		w.Header().Set("Cache-Control", "no-store, private")
		if a.config.SecureCookies {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (a *App) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	binding := a.csrfBinding(r)
	if cookie, err := r.Cookie(csrfCookie); err == nil && a.validCSRF(cookie.Value, binding) {
		return cookie.Value
	}
	nonce, _, err := secure.RandomToken(24)
	if err != nil {
		return ""
	}
	token := a.signCSRF(nonce, binding)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: token, Path: "/", Secure: a.config.SecureCookies, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	return token
}

func (a *App) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.sameOrigin(r) {
			http.Error(w, "cross-origin form submission rejected", http.StatusForbidden)
			return
		}
		if !a.parseForm(w, r) {
			return
		}
		cookie, err := r.Cookie(csrfCookie)
		formToken := r.Form.Get("csrf")
		if err != nil || len(cookie.Value) != len(formToken) || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formToken)) != 1 || !a.validCSRF(formToken, a.csrfBinding(r)) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) csrfBinding(r *http.Request) string {
	if cookie, err := r.Cookie(traderSessionCookie); err == nil && cookie.Value != "" {
		return "trader:" + secure.TokenHash(cookie.Value)
	}
	if cookie, err := r.Cookie(adminSessionCookie); err == nil && cookie.Value != "" {
		return "admin:" + secure.TokenHash(cookie.Value)
	}
	return "anonymous"
}

func (a *App) signCSRF(nonce, binding string) string {
	mac := hmac.New(sha256.New, a.csrfKey)
	_, _ = mac.Write([]byte(nonce))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(binding))
	return nonce + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) validCSRF(token, binding string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[0]) < 16 {
		return false
	}
	return hmac.Equal([]byte(token), []byte(a.signCSRF(parts[0], binding)))
}

func (a *App) sameOrigin(r *http.Request) bool {
	originHeader := r.Header.Get("Origin")
	if originHeader == "" {
		return !strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site")
	}
	origin, err := url.Parse(originHeader)
	return err == nil && strings.EqualFold(origin.Scheme, a.origin.Scheme) && strings.EqualFold(origin.Host, a.origin.Host)
}

func (a *App) clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Path: "/", Secure: a.config.SecureCookies, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func nextSixAM(login time.Time, location *time.Location) time.Time {
	local := login.In(location)
	expiry := time.Date(local.Year(), local.Month(), local.Day(), 6, 0, 0, 0, location)
	if !login.Before(expiry) {
		expiry = expiry.AddDate(0, 0, 1)
	}
	return expiry
}
