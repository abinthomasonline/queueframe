package web

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/config"
	"github.com/abinthomasonline/queueframe/internal/orders"
	"github.com/abinthomasonline/queueframe/internal/secure"
	"github.com/abinthomasonline/queueframe/internal/store"
)

func TestAdminRequiredPasswordChangeCompletesAndRevokesSession(t *testing.T) {
	app, database := newMustChangeAdminApp(t)
	defer database.Close()
	handler := app.Handler()

	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if loginPage.Code != http.StatusOK {
		t.Fatalf("GET /admin/login = %d: %s", loginPage.Code, loginPage.Body.String())
	}
	anonymousCSRF := mustChangeResponseCookie(t, loginPage, csrfCookie)

	loginForm := url.Values{
		"csrf":     {anonymousCSRF.Value},
		"username": {"forced-admin"},
		"password": {"initial-admin-password"},
	}
	loginRequest := formRequest(http.MethodPost, "/admin/login", loginForm, anonymousCSRF)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusSeeOther || loginResponse.Header().Get("Location") != "/admin/security" {
		t.Fatalf("forced admin login = %d location=%q body=%s", loginResponse.Code, loginResponse.Header().Get("Location"), loginResponse.Body.String())
	}
	adminSession := mustChangeResponseCookie(t, loginResponse, adminSessionCookie)

	blockedRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	blockedRequest.AddCookie(adminSession)
	blockedResponse := httptest.NewRecorder()
	handler.ServeHTTP(blockedResponse, blockedRequest)
	if blockedResponse.Code != http.StatusSeeOther || blockedResponse.Header().Get("Location") != "/admin/security" {
		t.Fatalf("forced admin dashboard = %d location=%q", blockedResponse.Code, blockedResponse.Header().Get("Location"))
	}

	securityRequest := httptest.NewRequest(http.MethodGet, "/admin/security", nil)
	securityRequest.AddCookie(adminSession)
	securityResponse := httptest.NewRecorder()
	handler.ServeHTTP(securityResponse, securityRequest)
	if securityResponse.Code != http.StatusOK || !strings.Contains(securityResponse.Body.String(), "Required before continuing") {
		t.Fatalf("GET /admin/security = %d: %s", securityResponse.Code, securityResponse.Body.String())
	}
	adminCSRF := mustChangeResponseCookie(t, securityResponse, csrfCookie)

	passwordForm := url.Values{
		"csrf":             {adminCSRF.Value},
		"current_password": {"initial-admin-password"},
		"new_password":     {"replacement-admin-password"},
		"confirm_password": {"replacement-admin-password"},
	}
	passwordRequest := formRequest(http.MethodPost, "/admin/security/password", passwordForm, adminSession, adminCSRF)
	passwordResponse := httptest.NewRecorder()
	handler.ServeHTTP(passwordResponse, passwordRequest)
	if passwordResponse.Code != http.StatusSeeOther || !strings.HasPrefix(passwordResponse.Header().Get("Location"), "/admin/login?notice=") {
		t.Fatalf("POST /admin/security/password = %d location=%q body=%s", passwordResponse.Code, passwordResponse.Header().Get("Location"), passwordResponse.Body.String())
	}
	assertMustChangeCookieCleared(t, passwordResponse, adminSessionCookie)
	assertMustChangeCookieCleared(t, passwordResponse, csrfCookie)

	admin, err := database.AuthenticateAdmin(context.Background(), "forced-admin", "replacement-admin-password")
	if err != nil {
		t.Fatalf("authenticate with replacement password: %v", err)
	}
	if admin.MustChangePassword {
		t.Fatal("must-change-password flag remained set")
	}
	if _, err := database.AuthenticateAdmin(context.Background(), "forced-admin", "initial-admin-password"); !errors.Is(err, store.ErrInvalidAdminCredentials) {
		t.Fatalf("old password authentication error = %v, want invalid credentials", err)
	}
	if _, err := database.AdminByWebSession(context.Background(), adminSession.Value, time.Now()); err == nil {
		t.Fatal("password change did not revoke the login session")
	}

	staleSessionRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	staleSessionRequest.AddCookie(adminSession)
	staleSessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleSessionResponse, staleSessionRequest)
	if staleSessionResponse.Code != http.StatusSeeOther || staleSessionResponse.Header().Get("Location") != "/admin/login" {
		t.Fatalf("stale session dashboard = %d location=%q", staleSessionResponse.Code, staleSessionResponse.Header().Get("Location"))
	}

	freshLoginPage := httptest.NewRecorder()
	handler.ServeHTTP(freshLoginPage, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	freshCSRF := mustChangeResponseCookie(t, freshLoginPage, csrfCookie)
	freshLoginForm := url.Values{
		"csrf":     {freshCSRF.Value},
		"username": {"forced-admin"},
		"password": {"replacement-admin-password"},
	}
	freshLoginResponse := httptest.NewRecorder()
	handler.ServeHTTP(freshLoginResponse, formRequest(http.MethodPost, "/admin/login", freshLoginForm, freshCSRF))
	if freshLoginResponse.Code != http.StatusSeeOther || freshLoginResponse.Header().Get("Location") != "/admin" {
		t.Fatalf("replacement password login = %d location=%q body=%s", freshLoginResponse.Code, freshLoginResponse.Header().Get("Location"), freshLoginResponse.Body.String())
	}
	freshSession := mustChangeResponseCookie(t, freshLoginResponse, adminSessionCookie)
	dashboardRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashboardRequest.AddCookie(freshSession)
	dashboardResponse := httptest.NewRecorder()
	handler.ServeHTTP(dashboardResponse, dashboardRequest)
	if dashboardResponse.Code != http.StatusOK {
		t.Fatalf("dashboard after replacement-password login = %d: %s", dashboardResponse.Code, dashboardResponse.Body.String())
	}
}

func TestRequiredAdminPasswordRouteIsMethodSpecific(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{method: http.MethodGet, path: "/admin/security", want: true},
		{method: http.MethodPost, path: "/admin/security/password", want: true},
		{method: http.MethodPost, path: "/logout", want: true},
		{method: http.MethodPost, path: "/admin/security", want: false},
		{method: http.MethodGet, path: "/admin/security/password", want: false},
		{method: http.MethodGet, path: "/logout", want: false},
		{method: http.MethodGet, path: "/admin", want: false},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if got := requiredAdminPasswordRoute(request); got != test.want {
				t.Fatalf("requiredAdminPasswordRoute() = %t, want %t", got, test.want)
			}
		})
	}
}

func newMustChangeAdminApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "must-change.db")
	hash, err := secure.HashPassword("initial-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL COLLATE NOCASE UNIQUE,
  password_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  must_change_password INTEGER NOT NULL DEFAULT 0,
  created_at_ms INTEGER NOT NULL
);
INSERT INTO users(username,password_hash,is_admin,must_change_password,created_at_ms)
VALUES(?,?,1,1,?)`, "forced-admin", hash, time.Now().UnixMilli()); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	vault, err := secure.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(path, vault)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{
		Store: database, Broker: &webTestFactory{}, Catalog: orders.NewCatalog(),
		Config: config.Config{BaseURL: "http://localhost:8080", SessionLifetime: time.Hour},
	})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	return app, database
}

func mustChangeResponseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name && cookie.MaxAge >= 0 && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("response did not set %s cookie", name)
	return nil
}

func assertMustChangeCookieCleared(t *testing.T, response *httptest.ResponseRecorder, name string) {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name && cookie.MaxAge < 0 {
			return
		}
	}
	t.Fatalf("response did not clear %s cookie", name)
}

func formRequest(method, path string, form url.Values, cookies ...*http.Cookie) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://localhost:8080")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	return request
}
