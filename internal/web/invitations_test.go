package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abinthomasonline/queueframe/internal/secure"
)

func TestInvitationAdminBoundaryCookieExchangeAndSignupGate(t *testing.T) {
	app, database := newTestApp(t)
	defer database.Close()
	app.config.ExpectedPublicIP = "203.0.113.10"
	app.config.RedirectURL = "http://localhost:8080/auth/kite/callback"

	unauthenticated := httptest.NewRequest(http.MethodGet, "/admin/invites", nil)
	unauthenticatedResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusSeeOther || unauthenticatedResponse.Header().Get("Location") != "/admin/login" {
		t.Fatalf("unauthenticated admin invitations = %d location=%q", unauthenticatedResponse.Code, unauthenticatedResponse.Header().Get("Location"))
	}

	admins, err := database.ListAdmins(context.Background())
	if err != nil || len(admins) != 1 {
		t.Fatalf("admins=%#v err=%v", admins, err)
	}
	adminToken, err := database.CreateAdminWebSession(context.Background(), admins[0].ID, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	adminCSRF := app.signCSRF(strings.Repeat("a", 32), "admin:"+secure.TokenHash(adminToken))
	createForm := url.Values{"csrf": {adminCSRF}, "expires_in_days": {"7"}}
	createRequest := httptest.NewRequest(http.MethodPost, "/admin/invites", strings.NewReader(createForm.Encode()))
	createRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createRequest.Header.Set("Origin", "http://localhost:8080")
	createRequest.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: adminToken})
	createRequest.AddCookie(&http.Cookie{Name: csrfCookie, Value: adminCSRF})
	createResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create invitation = %d: %s", createResponse.Code, createResponse.Body.String())
	}
	match := regexp.MustCompile(`http://localhost:8080/\?invite=([A-Za-z0-9_-]{43})`).FindStringSubmatch(createResponse.Body.String())
	if len(match) != 2 {
		t.Fatalf("one-time invitation URL missing: %s", createResponse.Body.String())
	}
	rawToken := match[1]
	if strings.Count(createResponse.Body.String(), rawToken) != 1 {
		t.Fatalf("raw invitation appeared %d times in one-time response", strings.Count(createResponse.Body.String(), rawToken))
	}

	laterAdminPage := httptest.NewRequest(http.MethodGet, "/admin/invites", nil)
	laterAdminPage.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: adminToken})
	laterResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(laterResponse, laterAdminPage)
	if laterResponse.Code != http.StatusOK || strings.Contains(laterResponse.Body.String(), rawToken) {
		t.Fatalf("later invitation list exposed raw token: status=%d", laterResponse.Code)
	}

	withoutInvite := httptest.NewRequest(http.MethodGet, "/", nil)
	withoutInviteResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(withoutInviteResponse, withoutInvite)
	if !strings.Contains(withoutInviteResponse.Body.String(), "Invitation required") {
		t.Fatal("public signup was not visibly gated without an invitation")
	}

	exchange := httptest.NewRequest(http.MethodGet, "/?invite="+url.QueryEscape(rawToken), nil)
	exchangeResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(exchangeResponse, exchange)
	if exchangeResponse.Code != http.StatusSeeOther {
		t.Fatalf("invite exchange = %d: %s", exchangeResponse.Code, exchangeResponse.Body.String())
	}
	if location := exchangeResponse.Header().Get("Location"); strings.Contains(location, rawToken) || !strings.HasPrefix(location, "/?") {
		t.Fatalf("invite exchange location retained bearer: %q", location)
	}
	if exchangeResponse.Header().Get("Referrer-Policy") != "no-referrer" || strings.Contains(exchangeResponse.Body.String(), rawToken) {
		t.Fatal("invite exchange response could expose bearer through content or referrer")
	}
	var inviteCookie *http.Cookie
	for _, cookie := range exchangeResponse.Result().Cookies() {
		if cookie.Name == signupInviteCookie {
			inviteCookie = cookie
		}
	}
	if inviteCookie == nil || !inviteCookie.HttpOnly || inviteCookie.SameSite != http.SameSiteStrictMode || inviteCookie.MaxAge <= 0 || inviteCookie.Value != rawToken {
		t.Fatalf("signup invite cookie = %#v", inviteCookie)
	}

	withInvite := httptest.NewRequest(http.MethodGet, "/", nil)
	withInvite.AddCookie(inviteCookie)
	withInviteResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(withInviteResponse, withInvite)
	if withInviteResponse.Code != http.StatusOK || strings.Contains(withInviteResponse.Body.String(), "Invitation required") || strings.Contains(withInviteResponse.Body.String(), rawToken) {
		t.Fatalf("accepted invite page status=%d leaked=%t body=%s", withInviteResponse.Code, strings.Contains(withInviteResponse.Body.String(), rawToken), withInviteResponse.Body.String())
	}

	anonymousCSRF := app.signCSRF(strings.Repeat("b", 32), "anonymous")
	signupForm := url.Values{
		"csrf": {anonymousCSRF}, "client_id": {"AB1234"}, "api_key": {"key-one"},
		"api_secret": {"secret-one"}, "family_attestation": {"yes"},
	}
	blockedRequest := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(signupForm.Encode()))
	blockedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	blockedRequest.Header.Set("Origin", "http://localhost:8080")
	blockedRequest.AddCookie(&http.Cookie{Name: csrfCookie, Value: anonymousCSRF})
	blockedResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(blockedResponse, blockedRequest)
	if blockedResponse.Code != http.StatusSeeOther || !strings.Contains(blockedResponse.Header().Get("Location"), "invitation") {
		t.Fatalf("signup without invite = %d location=%q", blockedResponse.Code, blockedResponse.Header().Get("Location"))
	}

	signupRequest := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(signupForm.Encode()))
	signupRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signupRequest.Header.Set("Origin", "http://localhost:8080")
	signupRequest.AddCookie(&http.Cookie{Name: csrfCookie, Value: anonymousCSRF})
	signupRequest.AddCookie(inviteCookie)
	signupResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(signupResponse, signupRequest)
	if signupResponse.Code != http.StatusSeeOther || !strings.HasPrefix(signupResponse.Header().Get("Location"), "https://kite.zerodha.com/") {
		t.Fatalf("invited signup = %d location=%q body=%s", signupResponse.Code, signupResponse.Header().Get("Location"), signupResponse.Body.String())
	}
	cleared := false
	for _, cookie := range signupResponse.Result().Cookies() {
		if cookie.Name == signupInviteCookie && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("successful signup start did not clear transient invite cookie")
	}
}

func TestInvitationCreateAndRevokeRequireBoundCSRF(t *testing.T) {
	app, database := newTestApp(t)
	defer database.Close()
	admins, _ := database.ListAdmins(context.Background())
	adminToken, err := database.CreateAdminWebSession(context.Background(), admins[0].ID, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	badForm := url.Values{"csrf": {"invalid"}, "expires_in_days": {"7"}}
	badRequest := httptest.NewRequest(http.MethodPost, "/admin/invites", strings.NewReader(badForm.Encode()))
	badRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badRequest.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: adminToken})
	badRequest.AddCookie(&http.Cookie{Name: csrfCookie, Value: "invalid"})
	badResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(badResponse, badRequest)
	if badResponse.Code != http.StatusForbidden {
		t.Fatalf("unbound admin CSRF = %d", badResponse.Code)
	}

	invitation, _, err := database.CreateSignupInvitation(context.Background(), admins[0].ID, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	adminCSRF := app.signCSRF(strings.Repeat("c", 32), "admin:"+secure.TokenHash(adminToken))
	revokeForm := url.Values{"csrf": {adminCSRF}}
	revokeRequest := httptest.NewRequest(http.MethodPost, "/admin/invites/"+int64Text(invitation.ID)+"/revoke", strings.NewReader(revokeForm.Encode()))
	revokeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokeRequest.Header.Set("Origin", "http://localhost:8080")
	revokeRequest.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: adminToken})
	revokeRequest.AddCookie(&http.Cookie{Name: csrfCookie, Value: adminCSRF})
	revokeResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusSeeOther {
		t.Fatalf("revoke invitation = %d: %s", revokeResponse.Code, revokeResponse.Body.String())
	}
	listed, err := database.ListSignupInvitations(context.Background())
	if err != nil || len(listed) != 1 || listed[0].RevokedAt.IsZero() || listed[0].RevokedByAdminID != admins[0].ID {
		t.Fatalf("revoked invitation metadata=%#v err=%v", listed, err)
	}
}

func TestInvitationExchangeUsesConfiguredSecureCookie(t *testing.T) {
	app, database := newTestApp(t)
	defer database.Close()
	app.config.SecureCookies = true
	admins, _ := database.ListAdmins(context.Background())
	_, rawToken, err := database.CreateSignupInvitation(context.Background(), admins[0].ID, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/?invite="+url.QueryEscape(rawToken), nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	var inviteCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == signupInviteCookie {
			inviteCookie = cookie
		}
	}
	if inviteCookie == nil || !inviteCookie.Secure || !inviteCookie.HttpOnly || inviteCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("secure signup invite cookie = %#v", inviteCookie)
	}
}
