package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abinthomasonline/queueframe/internal/config"
	"github.com/abinthomasonline/queueframe/internal/kitebroker"
	"github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/secure"
	"github.com/abinthomasonline/queueframe/internal/store"
)

func TestPublicHomeRendersQueueframeSignup(t *testing.T) {
	app, database := newTestApp(t)
	defer database.Close()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if !strings.Contains(response.Body.String(), "Queueframe") || !strings.Contains(response.Body.String(), "Zerodha client ID") {
		t.Fatalf("home does not contain the Queueframe onboarding UI: %s", response.Body.String())
	}
}

func TestCSRFTokenIsSessionBound(t *testing.T) {
	app, database := newTestApp(t)
	defer database.Close()
	token := app.signCSRF("a-long-random-test-nonce", "anonymous")
	if !app.validCSRF(token, "anonymous") {
		t.Fatal("signed token should validate for its binding")
	}
	if app.validCSRF(token, secure.TokenHash("another-session")) {
		t.Fatal("anonymous token must not validate for an authenticated session")
	}
}

func TestCSRFRejectsCrossOriginPost(t *testing.T) {
	app, database := newTestApp(t)
	defer database.Close()
	form := url.Values{"username": {"admin"}, "password": {"a-long-admin-password"}}
	request := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func newTestApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	vault, err := secure.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "web.db"), vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BootstrapAdmin(context.Background(), "admin", "a-long-admin-password"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	app, err := New(Options{Store: database, Broker: kitebroker.NewFactory(), Catalog: orders.NewCatalog(), Config: config.Config{BaseURL: "http://localhost:8080", SessionLifetime: 3600}})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	return app, database
}
