package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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

func TestQueueframeV2PagesRenderForTraderAndAdmin(t *testing.T) {
	database := openV2WebStore(t)
	defer database.Close()
	factory := &webTestFactory{}
	catalog := orders.NewCatalog()
	if err := catalog.Refresh(context.Background(), factory.Client("key-one", "token")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		BaseURL: "http://localhost:8080", RedirectURL: "http://localhost:8080/auth/kite/callback",
		ExpectedPublicIP: "203.0.113.10", SessionLifetime: time.Hour, DryRun: true,
	}
	app, err := New(Options{Store: database, Broker: factory, Catalog: catalog, Config: cfg, Status: func() string { return "ready" }})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/admin/login"} {
		t.Run("public "+path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", path, response.Code, response.Body.String())
			}
			assertResponsivePageShell(t, response)
		})
	}
	t.Run("favicon asset", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/static/favicon.svg", nil)
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET favicon = %d: %s", response.Code, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "image/svg+xml") {
			t.Fatalf("favicon Content-Type = %q", contentType)
		}
		if !strings.Contains(response.Body.String(), `viewBox="0 0 32 32"`) {
			t.Fatal("favicon is missing its 32px coordinate system")
		}
	})

	now := time.Now()
	adminsForInvite, err := database.ListAdmins(context.Background())
	if err != nil || len(adminsForInvite) != 1 {
		t.Fatalf("admins for invite = %#v, %v", adminsForInvite, err)
	}
	_, inviteToken, err := database.CreateSignupInvitation(context.Background(), adminsForInvite[0].ID, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	flow, err := database.BeginTraderSignup(context.Background(), domain.TraderSignup{
		ClientID: "AB1234", APIKey: "key-one", APISecret: "secret-one",
		InviteToken:       inviteToken,
		PolicyAttestation: domain.PolicyAttestation{Version: domain.HostingPolicyVersion, SourceURL: domain.HostingPolicySourceURL, Text: domain.HostingPolicyExactText},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := database.ConsumeKiteAuthFlow(context.Background(), flow.State, now)
	if err != nil {
		t.Fatal(err)
	}
	trader, err := database.CompleteKiteAuth(context.Background(), consumed.ID, broker.Session{
		UserID: "AB1234", APIKey: "key-one", AccessToken: "token", LoginAt: now,
	}, nextSixAM(now, india))
	if err != nil {
		t.Fatal(err)
	}
	executionDate := nextWeekdayDate(now.In(india)).Format("2006-01-02")
	queued, _, err := database.CreateQueuedOrder(context.Background(), trader.ID, domain.QueuedOrder{
		ExecutionDate: executionDate,
		Params:        domain.OrderParams{Exchange: "NSE", TradingSymbol: "ACME", TransactionType: "SELL", Product: "CNC", OrderType: "LIMIT", Quantity: 2, Price: "10.5"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	traderToken, err := database.CreateTraderWebSession(context.Background(), trader.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}

	traderPages := []string{
		"/orders?date=" + executionDate,
		"/orders/new?date=" + executionDate,
		"/orders/schedule-preview?date=" + executionDate,
		"/dispatches?date=" + executionDate,
		"/account",
		"/instruments/search?q=AC&exchange=NSE",
	}
	for _, path := range traderPages {
		t.Run("trader "+path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.AddCookie(&http.Cookie{Name: traderSessionCookie, Value: traderToken})
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", path, response.Code, response.Body.String())
			}
			if strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
				assertResponsivePageShell(t, response)
			}
		})
	}
	t.Run("schedule preview contract and weekend rejection", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/orders/schedule-preview?date="+executionDate, nil)
		request.AddCookie(&http.Cookie{Name: traderSessionCookie, Value: traderToken})
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"dispatch_time":"09:00:00.000"`) || !strings.Contains(response.Body.String(), `"freeze_time":"08:55:00"`) {
			t.Fatalf("schedule preview = %d: %s", response.Code, response.Body.String())
		}
		weekend := nextWeekdayDate(now.In(india))
		for weekend.Weekday() != time.Saturday {
			weekend = weekend.AddDate(0, 0, 1)
		}
		request = httptest.NewRequest(http.MethodGet, "/orders/schedule-preview?date="+weekend.Format("2006-01-02"), nil)
		request.AddCookie(&http.Cookie{Name: traderSessionCookie, Value: traderToken})
		response = httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("weekend schedule preview = %d: %s", response.Code, response.Body.String())
		}
	})

	admins, err := database.ListAdmins(context.Background())
	if err != nil || len(admins) != 1 {
		t.Fatalf("admins = %#v, %v", admins, err)
	}
	adminToken, err := database.CreateAdminWebSession(context.Background(), admins[0].ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	futureSettingsDate := nextWeekdayDate(nextWeekdayDate(now.In(india))).Format("2006-01-02")
	if _, err := database.ScheduleSettings(context.Background(), domain.RuntimeSettings{
		DispatchClock: "10:15:20", UserBlackoutStart: "10:00:00", UserBlackoutEnd: "10:30:00",
		FreezeLead: 5 * time.Minute, BatchInterval: 1050 * time.Millisecond, MaxOrdersPerTrader: 100,
	}, futureSettingsDate, admins[0].ID, now); err != nil {
		t.Fatal(err)
	}
	adminPages := []string{
		"/admin",
		"/admin/invites",
		"/admin/users",
		"/admin/orders?date=" + executionDate,
		"/admin/orders/" + int64Text(queued.ID) + "/edit",
		"/admin/instruments/search?trader_id=" + int64Text(trader.ID) + "&q=AC&exchange=NSE",
		"/admin/dispatches?date=" + executionDate,
		"/admin/settings",
		"/admin/security",
	}
	for _, path := range adminPages {
		t.Run("admin "+path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: adminToken})
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", path, response.Code, response.Body.String())
			}
			if strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
				assertResponsivePageShell(t, response)
			}
			if path == "/admin/settings" && (!strings.Contains(response.Body.String(), futureSettingsDate) || !strings.Contains(response.Body.String(), "10:15:20")) {
				t.Fatalf("GET %s did not show scheduled settings history", path)
			}
		})
	}

	t.Run("dispatch receipt timeline is visible to trader and admin", func(t *testing.T) {
		target, err := time.ParseInLocation("2006-01-02 15:04:05", executionDate+" 09:00:00", india)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.PrepareQueuedOrders(context.Background(), executionDate, map[int64]time.Time{queued.ID: target}, now); err != nil {
			t.Fatal(err)
		}
		startedAt := target.Add(157548 * time.Nanosecond)
		finishedAt := target.Add(116986669 * time.Nanosecond)
		exchangeAt := target.Add(5 * time.Second)
		result := domain.DispatchResult{
			IntentID: queued.ID, AccountID: trader.ID, Attempt: 1, RequestCount: 1,
			TargetAt: target, StartedAt: startedAt, FinishedAt: finishedAt,
			Outcome: domain.OrderStateAcknowledged, OrderID: "ORDER-1", ExchangeOrderID: "EXCHANGE-1", BrokerStatus: "OPEN",
			OrderTimestamp: exchangeAt, ExchangeTimestamp: exchangeAt, ExchangeUpdateTimestamp: exchangeAt,
			Receipts: []domain.DispatchReceipt{
				{Source: domain.DispatchReceiptKiteResponse, ObservedAt: finishedAt, OrderID: "ORDER-1"},
				{Source: domain.DispatchReceiptOrderBook, ObservedAt: target.Add(time.Second), OrderID: "ORDER-1", BrokerStatus: "OPEN PENDING", OrderTimestamp: target},
				{Source: domain.DispatchReceiptOrderBook, ObservedAt: target.Add(6 * time.Second), OrderID: "ORDER-1", ExchangeOrderID: "EXCHANGE-1", BrokerStatus: "OPEN", OrderTimestamp: exchangeAt, ExchangeTimestamp: exchangeAt, ExchangeUpdateTimestamp: exchangeAt},
			},
		}
		if err := database.ApplyQueuedDispatchResults(context.Background(), []domain.DispatchResult{result}); err != nil {
			t.Fatal(err)
		}
		checks := []struct {
			path   string
			cookie *http.Cookie
		}{
			{path: "/dispatches?date=" + executionDate, cookie: &http.Cookie{Name: traderSessionCookie, Value: traderToken}},
			{path: "/admin/dispatches?date=" + executionDate, cookie: &http.Cookie{Name: adminSessionCookie, Value: adminToken}},
		}
		for _, check := range checks {
			request := httptest.NewRequest(http.MethodGet, check.path, nil)
			request.AddCookie(check.cookie)
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", check.path, response.Code, response.Body.String())
			}
			body := response.Body.String()
			for _, want := range []string{"Timing receipts", "Kite API response", "Kite order-book receipt", "OPEN PENDING", "EXCHANGE-1", "09:00:00.000157 IST", "09:00:00.116986 IST"} {
				if !strings.Contains(body, want) {
					t.Fatalf("GET %s is missing %q", check.path, want)
				}
			}
		}
	})

	request := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", response.Code)
	}
}

func assertResponsivePageShell(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	body := response.Body.String()
	if strings.Count(body, "<main") != 1 || strings.Count(body, "</main>") != 1 {
		t.Fatalf("page shell has unbalanced main landmark: opens=%d closes=%d", strings.Count(body, "<main"), strings.Count(body, "</main>"))
	}
	if !strings.Contains(body, `content="width=device-width, initial-scale=1, viewport-fit=cover"`) {
		t.Fatal("page shell is missing the safe-area-aware mobile viewport")
	}
	if !strings.Contains(body, `rel="icon" href="/static/favicon.svg" type="image/svg+xml" sizes="any"`) {
		t.Fatal("page shell is missing the Queueframe favicon")
	}
}

func openV2WebStore(t *testing.T) *store.Store {
	t.Helper()
	vault, err := secure.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "queueframe.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BootstrapAdmin(context.Background(), "admin", "a-long-admin-password"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return database
}

func int64Text(value int64) string {
	return strconv.FormatInt(value, 10)
}

type webTestFactory struct{}

func (*webTestFactory) LoginURL(_, _ string) string { return "https://kite.example/login" }
func (*webTestFactory) GenerateSession(context.Context, string, string, string) (broker.Session, error) {
	return broker.Session{}, nil
}
func (*webTestFactory) Client(_, _ string) broker.Client { return webTestClient{} }

type webTestClient struct{}

func (webTestClient) Profile(context.Context) (string, error) { return "AB1234", nil }
func (webTestClient) Instruments(_ context.Context, exchange string) ([]domain.Instrument, error) {
	return []domain.Instrument{{Exchange: exchange, TradingSymbol: "ACME", Name: "Acme Limited", InstrumentType: "EQ"}}, nil
}
func (webTestClient) PlaceRegularOrder(context.Context, domain.OrderParams, string) (broker.Order, error) {
	return broker.Order{}, nil
}
func (webTestClient) Orders(context.Context) ([]broker.Order, error) { return nil, nil }
func (webTestClient) InitiateHoldingsAuthorization(context.Context) (string, error) {
	return "", nil
}
