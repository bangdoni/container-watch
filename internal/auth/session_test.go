package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerwatch/containerwatch/internal/auth"
	"github.com/containerwatch/containerwatch/internal/database"
)

// newStore opens a throwaway SQLite store in a temp dir.
func newStore(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newUser creates an admin user with a known password.
func newUser(t *testing.T, store *database.Store, username, password, role string) *database.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u := &database.User{
		Username:     username,
		Email:        username + "@example.com",
		PasswordHash: hash,
		Role:         role,
	}
	if err := store.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// TestIssueAndAuthenticate checks the session round trip: issue a session,
// then authenticate with the cookie that was set.
func TestIssueAndAuthenticate(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)
	u := newUser(t, store, "admin", "Password123!", "admin")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := mgr.Issue(rec, req, u); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cookies := rec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie was set")
	}
	if sessionCookie.Value == "" {
		t.Fatal("the session cookie has an empty value")
	}

	// Authenticate with the cookie.
	authReq := httptest.NewRequest(http.MethodGet, "/", nil)
	authReq.AddCookie(sessionCookie)
	got, sess, err := mgr.Authenticate(authReq)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got == nil {
		t.Fatal("Authenticate returned a nil user")
	}
	if got.Username != "admin" {
		t.Errorf("username = %q, want admin", got.Username)
	}
	if sess == nil {
		t.Fatal("Authenticate returned a nil session")
	}
}

// TestAuthenticateRejectsGarbageCookie checks that a forged cookie does not
// authenticate.
func TestAuthenticateRejectsGarbageCookie(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "not-a-real-token"})
	got, _, err := mgr.Authenticate(req)
	if err == nil {
		t.Fatal("expected an error for a forged session token")
	}
	if got != nil {
		t.Error("a forged session token returned a user")
	}
}

// TestAuthenticateRejectsNoCookie checks the anonymous path.
func TestAuthenticateRejectsNoCookie(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, _, err := mgr.Authenticate(req); err == nil {
		t.Fatal("expected an error when no session cookie is present")
	}
}

// TestRevokeInvalidatesSession checks that logout actually kills the
// session: the same cookie must stop working.
func TestRevokeInvalidatesSession(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)
	u := newUser(t, store, "admin", "Password123!", "admin")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := mgr.Issue(rec, req, u); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}

	// Revoke.
	revokeRec := httptest.NewRecorder()
	revokeReq := httptest.NewRequest(http.MethodPost, "/logout", nil)
	revokeReq.AddCookie(cookie)
	mgr.Revoke(revokeRec, revokeReq)

	// The old cookie must no longer authenticate.
	after := httptest.NewRequest(http.MethodGet, "/", nil)
	after.AddCookie(cookie)
	if _, _, err := mgr.Authenticate(after); err == nil {
		t.Fatal("the session still authenticates after Revoke")
	}
}

// TestCSRFTokenRoundTrip checks the double-submit CSRF contract: the token
// is issued as a cookie and the same value must be accepted in a form field.
func TestCSRFTokenRoundTrip(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	token := mgr.EnsureCSRF(rec, req)
	if token == "" {
		t.Fatal("EnsureCSRF returned an empty token")
	}

	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CSRFCookieName {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("no CSRF cookie was set")
	}
	if csrfCookie.Value != token {
		t.Errorf("the CSRF cookie value (%q) differs from the issued token (%q)",
			csrfCookie.Value, token)
	}

	// A form POST carrying both the cookie and the field must pass.
	post := httptest.NewRequest(http.MethodPost, "/admin/nodes", nil)
	post.AddCookie(csrfCookie)
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Form = map[string][]string{"_csrf": {token}}
	if !mgr.CheckCSRF(post) {
		t.Error("CheckCSRF rejected a valid token")
	}
}

// TestCSRFRejectsMismatch checks that a field value differing from the
// cookie is rejected.
func TestCSRFRejectsMismatch(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mgr.EnsureCSRF(rec, req)

	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CSRFCookieName {
			csrfCookie = c
		}
	}

	post := httptest.NewRequest(http.MethodPost, "/admin/nodes", nil)
	post.AddCookie(csrfCookie)
	post.Form = map[string][]string{"_csrf": {"a-different-token"}}
	if mgr.CheckCSRF(post) {
		t.Error("CheckCSRF accepted a token that differs from the cookie")
	}
}

// TestCSRFRejectsMissingToken checks the empty case.
func TestCSRFRejectsMissingToken(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)

	post := httptest.NewRequest(http.MethodPost, "/admin/nodes", nil)
	post.Form = map[string][]string{}
	if mgr.CheckCSRF(post) {
		t.Error("CheckCSRF accepted a request with no token at all")
	}
}

// TestClientIP checks that the client IP is taken from the socket by
// default, and only from X-Forwarded-For when the proxy is trusted.
func TestClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.7:54321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := auth.ClientIP(req, false); got != "10.0.0.7" {
		t.Errorf("ClientIP(untrusted proxy) = %q, want 10.0.0.7", got)
	}
	if got := auth.ClientIP(req, true); got != "1.2.3.4" {
		t.Errorf("ClientIP(trusted proxy) = %q, want 1.2.3.4", got)
	}
}

// TestRequireUserRedirectsAnonymous checks the auth gate for a browser
// request: it must redirect to the login page, not render a page.
func TestRequireUserRedirectsAnonymous(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)
	mw := auth.NewMiddleware(mgr)

	// AttachUser must run first: RequireUser reads the user from the
	// request context, not from the cookie directly.
	h := mw.AttachUser(mw.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login?next=/dashboard" {
		t.Errorf("Location = %q, want /login?next=/dashboard", loc)
	}
	if body := rec.Body.String(); body == "secret" {
		t.Error("the protected handler ran for an anonymous request")
	}
}

// TestRequireUserPassesAuthenticated checks the allow path.
func TestRequireUserPassesAuthenticated(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)
	u := newUser(t, store, "admin", "Password123!", "admin")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := mgr.Issue(rec, req, u); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}

	mw := auth.NewMiddleware(mgr)
	h := mw.AttachUser(mw.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	})))

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req2.AddCookie(cookie)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}
	if rec2.Body.String() != "secret" {
		t.Error("the protected handler did not run")
	}
}

// TestRequireAdminBlocksNonAdmin checks the role gate.
func TestRequireAdminBlocksNonAdmin(t *testing.T) {
	store := newStore(t)
	mgr := auth.NewManager(store, time.Hour, nil, false)
	viewer := newUser(t, store, "viewer", "Password123!", "user")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := mgr.Issue(rec, req, viewer); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}

	mw := auth.NewMiddleware(mgr)
	h := mw.AttachUser(mw.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("admin-only"))
	})))

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req2.AddCookie(cookie)
	h.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusOK {
		t.Error("a non-admin user reached an admin-only handler")
	}
	if rec2.Body.String() == "admin-only" {
		t.Error("the admin handler ran for a non-admin user")
	}
}

// TestAdminExists checks the first-run detection helper.
func TestAdminExists(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if auth.AdminExists(ctx, store) {
		t.Error("AdminExists returned true on an empty database")
	}
	newUser(t, store, "admin", "Password123!", "admin")
	if !auth.AdminExists(ctx, store) {
		t.Error("AdminExists returned false after an admin was created")
	}
}
