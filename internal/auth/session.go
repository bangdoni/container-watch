package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bangdoni/container-watch/internal/database"
)

// SessionCookieName is the cookie that carries the opaque session token.
const SessionCookieName = "cw_session"

// CSRFCookieName carries the double-submit CSRF token.
const CSRFCookieName = "cw_csrf"

// contextKey is a private type for request context values.
type contextKey int

const (
	userContextKey contextKey = iota
)

// Manager ties sessions to the database and knows how to issue/revoke them.
type Manager struct {
	db         *database.Store
	ttl        time.Duration
	secure     *bool
	trustProxy bool
}

// NewManager builds a session manager.
func NewManager(db *database.Store, ttl time.Duration, secure *bool, trustProxy bool) *Manager {
	return &Manager{db: db, ttl: ttl, secure: secure, trustProxy: trustProxy}
}

// TTL exposes the configured session lifetime (used by templates for expiry hints).
func (m *Manager) TTL() time.Duration { return m.ttl }

// cookieSecure decides whether to set the Secure flag.
func (m *Manager) cookieSecure(r *http.Request) bool {
	if m.secure != nil {
		return *m.secure
	}
	if r.TLS != nil {
		return true
	}
	if m.trustProxy {
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			return strings.EqualFold(p, "https")
		}
	}
	return false
}

// Issue creates a session row and writes the session + CSRF cookies.
func (m *Manager) Issue(w http.ResponseWriter, r *http.Request, user *database.User) error {
	token, hash, err := NewSessionToken()
	if err != nil {
		return err
	}
	now := time.Now()
	s := &database.Session{
		TokenHash: hash,
		UserID:    user.ID,
		CreatedAt: now,
		ExpiresAt: now.Add(m.ttl),
		IP:        ClientIP(r, m.trustProxy),
		UserAgent: truncate(r.UserAgent(), 255),
	}
	if err := m.db.CreateSession(r.Context(), s); err != nil {
		return err
	}
	m.writeSessionCookie(w, r, token, s.ExpiresAt)
	m.EnsureCSRF(w, r)
	_ = m.db.TouchLogin(r.Context(), user.ID)
	return nil
}

func (m *Manager) writeSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   m.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// Revoke deletes the current session and clears the cookie.
func (m *Manager) Revoke(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		_ = m.db.DeleteSession(r.Context(), HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   m.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// Authenticate resolves the request's session into a user, if present.
// Expired sessions are deleted opportunistically.
func (m *Manager) Authenticate(r *http.Request) (*database.User, *database.Session, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return nil, nil, database.ErrNotFound
	}
	s, u, err := m.db.SessionByHash(r.Context(), HashToken(c.Value))
	if err != nil {
		return nil, nil, err
	}
	if time.Now().After(s.ExpiresAt) {
		_ = m.db.DeleteSession(r.Context(), s.TokenHash)
		return nil, nil, database.ErrNotFound
	}
	return u, s, nil
}

// CurrentUser returns the user attached to the context by Middleware.
func CurrentUser(ctx context.Context) *database.User {
	u, _ := ctx.Value(userContextKey).(*database.User)
	return u
}

// WithUser attaches a user to a context (used by middleware and tests).
func WithUser(ctx context.Context, u *database.User) context.Context {
	return context.WithValue(ctx, userContextKey, u)
}

// EnsureCSRF returns the request's CSRF token, creating the cookie if absent.
// Calling it from a handler guarantees templates can render the token.
func (m *Manager) EnsureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(CSRFCookieName); err == nil && len(c.Value) >= 16 {
		return c.Value
	}
	token := RandomID(24)
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int((24 * time.Hour).Seconds()),
		HttpOnly: false, // readable by htmx/Alpine for the header
		Secure:   m.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	return token
}

// CheckCSRF validates the double-submit token for unsafe methods. htmx sends
// the token via X-CSRF-Token; normal forms send it as a _csrf field.
func (m *Manager) CheckCSRF(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	c, err := r.Cookie(CSRFCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.FormValue("_csrf")
	}
	return subtleEqual(got, c.Value)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ClientIP extracts the caller IP honouring forwarding headers when trusted.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ErrUnauthorized is used by callers that need to distinguish auth failures.
var ErrUnauthorized = errors.New("unauthorized")
