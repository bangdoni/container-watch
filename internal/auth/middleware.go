package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/bangdoni/container-watch/internal/database"
)

// RoleAdmin and RoleUser are the only RBAC roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Middleware provides the authentication and authorization HTTP wrappers.
type Middleware struct {
	mgr *Manager
}

// NewMiddleware builds middleware around a session manager.
func NewMiddleware(mgr *Manager) *Middleware { return &Middleware{mgr: mgr} }

// AttachUser resolves the session (if any) and stores the user in the request
// context. It never rejects; use RequireUser / RequireAdmin for gating.
func (m *Middleware) AttachUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, err := m.mgr.Authenticate(r); err == nil && u != nil {
			r = r.WithContext(WithUser(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}

// IsHTMX reports whether the request was issued by htmx.
func IsHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// wantsHTML reports whether we should redirect (browser navigation) rather
// than return a bare status code (htmx/API calls).
func wantsHTML(r *http.Request) bool {
	if IsHTMX(r) {
		return false
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		return false
	}
	return r.Method == http.MethodGet
}

// RequireUser gates a handler behind any authenticated account.
func (m *Middleware) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if CurrentUser(r.Context()) == nil {
			if wantsHTML(r) {
				redirectToLogin(w, r)
				return
			}
			// htmx: ask the client to perform a full-page redirect.
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin gates a handler behind the admin role. The CSRF token is also
// validated here because every admin route mutates state.
func (m *Middleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := CurrentUser(r.Context())
		if u == nil {
			if wantsHTML(r) {
				redirectToLogin(w, r)
				return
			}
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !u.IsAdmin() {
			http.Error(w, "Administrator role required", http.StatusForbidden)
			return
		}
		if !m.mgr.CheckCSRF(r) {
			http.Error(w, "Your session token expired. Reload the page and try again.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireCSRF gates write endpoints available to any authenticated user
// (for example changing one's own password).
func (m *Middleware) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if CurrentUser(r.Context()) == nil {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !m.mgr.CheckCSRF(r) {
			http.Error(w, "Your session token expired. Reload the page and try again.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if r.Method == http.MethodGet && r.URL.Path != "/" {
		target += "?next=" + urlEncode(r.URL.RequestURI())
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// urlEncode escapes a path+query for use as a query parameter value.
func urlEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0xF])
	}
	return b.String()
}

// AdminExists reports whether at least one admin account exists, which gates
// the first-run setup wizard.
func AdminExists(ctx context.Context, db *database.Store) bool {
	n, err := db.CountAdmins(ctx)
	return err == nil && n > 0
}
