package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bangdoni/container-watch/internal/auth"
	"github.com/bangdoni/container-watch/internal/database"
)

// usernameRe constrains account names to a safe, portable character set.
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,31}$`)

// authPage is the view model shared by login, setup and signup.
type authPage struct {
	PageData
	Subtitle         string
	Error            string
	Next             string
	RegistrationOpen bool
	AdminExists      bool
	Username         string
	Email            string
}

// registrationOpen resolves the public-signup setting, falling back to the
// environment default until an admin saves a choice.
func (s *Server) registrationOpen(r *http.Request) bool {
	v := s.db.GetSetting(r.Context(), SettingRegistrationOpen, "")
	if v == "" {
		return s.cfg.RegistrationOpen
	}
	return v == "true"
}

// Settings keys persisted in the settings table.
const (
	SettingRegistrationOpen = "registration_open"
)

// safeNext rejects open-redirect targets.
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return "/"
	}
	if strings.ContainsAny(v, "\r\n") {
		return "/"
	}
	return v
}

// authView builds the shared auth page payload and guarantees a CSRF cookie.
func (s *Server) authView(w http.ResponseWriter, r *http.Request, subtitle string) authPage {
	csrf := s.auth.EnsureCSRF(w, r)
	return authPage{
		PageData:         Base(auth.CurrentUser(r.Context()), csrf, ""),
		Subtitle:         subtitle,
		Next:             safeNext(r.URL.Query().Get("next")),
		RegistrationOpen: s.registrationOpen(r),
		AdminExists:      auth.AdminExists(r.Context(), s.db),
	}
}

// ---------------------------------------------------------------- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if auth.CurrentUser(r.Context()) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	if !auth.AdminExists(r.Context(), s.db) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	v := s.authView(w, r, "Sign in to monitor your containers")
	v.Title = "Sign in"
	s.renderPage(w, r, http.StatusOK, "login", "auth", v)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !s.auth.CheckCSRF(r) {
		s.renderLoginError(w, r, "Your session expired. Reload the page and try again.")
		return
	}
	ip := auth.ClientIP(r, s.cfg.TrustProxy)
	if ok, wait := s.limiter.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.renderLoginError(w, r, "Too many attempts. Try again in "+humanDuration(wait)+".")
		return
	}

	login := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if login == "" || password == "" {
		s.renderLoginError(w, r, "Enter your username and password.")
		return
	}

	u, err := s.db.UserByLogin(r.Context(), login)
	if err != nil {
		// Burn comparable CPU time so a missing account is not distinguishable
		// from a wrong password by response latency.
		_, _ = auth.VerifyPassword(password, dummyHash)
		s.renderLoginError(w, r, "Invalid username or password.")
		return
	}
	ok, verr := auth.VerifyPassword(password, u.PasswordHash)
	if verr != nil || !ok {
		s.renderLoginError(w, r, "Invalid username or password.")
		return
	}

	if err := s.auth.Issue(w, r, u); err != nil {
		s.log.Error("issue session", "user", u.Username, "error", err)
		s.renderLoginError(w, r, "Could not start a session. Please try again.")
		return
	}
	s.limiter.Reset(ip)
	s.log.Info("user signed in", "user", u.Username, "ip", ip)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

// dummyHash is a valid Argon2id hash used to equalise login timing.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$MDAwMDAwMDAwMDAwMDAwMA$Zm9vYmFyYmF6cXV4Zm9vYmFyYmF6cXV4Zm9vYmFyYmE"

func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, msg string) {
	v := s.authView(w, r, "Sign in to monitor your containers")
	v.Title = "Sign in"
	v.Error = msg
	v.Username = r.FormValue("username")
	s.renderPage(w, r, http.StatusUnauthorized, "login", "auth", v)
}

// ---------------------------------------------------------------- setup ----

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if auth.AdminExists(r.Context(), s.db) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	v := s.authView(w, r, "Create the administrator account to get started")
	v.Title = "First-run setup"
	s.renderPage(w, r, http.StatusOK, "setup", "auth", v)
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !s.auth.CheckCSRF(r) {
		s.renderSetupError(w, r, "Your session expired. Reload the page and try again.")
		return
	}

	// Serialise setup so two simultaneous submissions cannot both create an
	// administrator.
	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	if auth.AdminExists(r.Context(), s.db) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	if msg := validateAccount(username, email, password, confirm); msg != "" {
		s.renderSetupError(w, r, msg)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.renderSetupError(w, r, err.Error())
		return
	}
	u := &database.User{Username: username, Email: email, PasswordHash: hash, Role: auth.RoleAdmin}
	if err := s.db.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, database.ErrConflict) {
			s.renderSetupError(w, r, "That username is already taken.")
			return
		}
		s.log.Error("create admin", "error", err)
		s.renderSetupError(w, r, "Could not create the account. Check the server logs.")
		return
	}
	if err := s.auth.Issue(w, r, u); err != nil {
		s.log.Error("issue session after setup", "error", err)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.log.Info("administrator account created", "user", u.Username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) renderSetupError(w http.ResponseWriter, r *http.Request, msg string) {
	v := s.authView(w, r, "Create the administrator account to get started")
	v.Title = "First-run setup"
	v.Error = msg
	v.Username = r.FormValue("username")
	v.Email = r.FormValue("email")
	s.renderPage(w, r, http.StatusBadRequest, "setup", "auth", v)
}

// --------------------------------------------------------------- signup ----

func (s *Server) signupPage(w http.ResponseWriter, r *http.Request) {
	if auth.CurrentUser(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !auth.AdminExists(r.Context(), s.db) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if !s.registrationOpen(r) {
		v := s.authView(w, r, "Sign in to monitor your containers")
		v.Title = "Sign in"
		v.Error = "Public registration is disabled. Ask an administrator for an account."
		s.renderPage(w, r, http.StatusForbidden, "login", "auth", v)
		return
	}
	v := s.authView(w, r, "Request a read-only viewer account")
	v.Title = "Create account"
	s.renderPage(w, r, http.StatusOK, "signup", "auth", v)
}

func (s *Server) signupSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !s.auth.CheckCSRF(r) {
		s.renderSignupError(w, r, "Your session expired. Reload the page and try again.")
		return
	}
	if !s.registrationOpen(r) {
		s.renderSignupError(w, r, "Public registration is disabled.")
		return
	}

	ip := auth.ClientIP(r, s.cfg.TrustProxy)
	if ok, wait := s.limiter.Allow(ip); !ok {
		s.renderSignupError(w, r, "Too many attempts. Try again in "+humanDuration(wait)+".")
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")
	if msg := validateAccount(username, email, password, confirm); msg != "" {
		s.renderSignupError(w, r, msg)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.renderSignupError(w, r, err.Error())
		return
	}
	u := &database.User{Username: username, Email: email, PasswordHash: hash, Role: auth.RoleUser}
	if err := s.db.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, database.ErrConflict) {
			s.renderSignupError(w, r, "That username is already taken.")
			return
		}
		s.log.Error("create user", "error", err)
		s.renderSignupError(w, r, "Could not create the account. Check the server logs.")
		return
	}
	if err := s.auth.Issue(w, r, u); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.log.Info("user registered", "user", u.Username, "ip", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) renderSignupError(w http.ResponseWriter, r *http.Request, msg string) {
	v := s.authView(w, r, "Request a read-only viewer account")
	v.Title = "Create account"
	v.Error = msg
	v.Username = r.FormValue("username")
	v.Email = r.FormValue("email")
	s.renderPage(w, r, http.StatusBadRequest, "signup", "auth", v)
}

// --------------------------------------------------------------- logout ----

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	// Logout is a state change, so it is CSRF-protected like other writes.
	if !s.auth.CheckCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if u := auth.CurrentUser(r.Context()); u != nil {
		s.log.Info("user signed out", "user", u.Username)
	}
	s.auth.Revoke(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --------------------------------------------------------------- helpers ----

// validateAccount applies the shared username/email/password rules.
func validateAccount(username, email, password, confirm string) string {
	switch {
	case username == "":
		return "Choose a username."
	case !usernameRe.MatchString(username):
		return "Username must be 3–32 characters: letters, numbers, dot, dash or underscore."
	case len(password) < 8:
		return "Password must be at least 8 characters."
	case len(password) > 200:
		return "Password must be at most 200 characters."
	case password != confirm:
		return "The two passwords do not match."
	case email != "" && (!strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n")):
		return "Enter a valid email address, or leave it blank."
	}
	return ""
}

// humanDuration renders a short, human-friendly wait time.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "a few seconds"
	}
	m := int(d.Minutes()) + 1
	if m == 1 {
		return "1 minute"
	}
	return strconv.Itoa(m) + " minutes"
}

// redirectWith keeps query parameters when redirecting (used by the node
// switcher and pagination).
func redirectWith(w http.ResponseWriter, r *http.Request, path string, q url.Values) {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}
