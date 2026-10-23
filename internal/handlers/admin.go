package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bangdoni/container-watch/internal/auth"
	"github.com/bangdoni/container-watch/internal/database"
	"github.com/bangdoni/container-watch/internal/engine"
	"github.com/bangdoni/container-watch/internal/version"
)

// Settings keys persisted in the settings table.
const (
	SettingMetricsInterval = "metrics_interval"
	SettingDefaultNode     = "default_node"
)

// nodesView is the payload for the node administration page.
type nodesView struct {
	viewData
	Cards        []nodeCardView
	LocalSockets []string
	Disabled     bool
}

// nodesPage lists every configured engine endpoint with its live status.
func (s *Server) nodesPage(w http.ResponseWriter, r *http.Request) {
	vd := s.base(w, r, "nodes", "Nodes")
	csrf := s.auth.EnsureCSRF(w, r)
	nodes := s.nodes.List()
	cards := make([]nodeCardView, 0, len(nodes))
	for _, n := range nodes {
		cards = append(cards, nodeCard(n, s.nodes.IsLocal(n.Record.Endpoint), csrf))
	}
	s.renderPage(w, r, http.StatusOK, "admin_nodes", "layout", nodesView{
		viewData:     vd,
		Cards:        cards,
		LocalSockets: s.cfg.LocalSockets,
		Disabled:     s.cfg.DisableLocalDiscovery,
	})
}

// nodeForm is the create/edit form model.
type nodeForm struct {
	viewData
	Mode     string // "create" or "edit"
	ID       int64
	Name     string
	Engine   string
	Endpoint string
	TLSCA    string
	TLSCert  string
	TLSKey   string
	Error    string
	IsLocal  bool
}

// nodeFormPartial renders the "add node" modal.
func (s *Server) nodeFormPartial(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	s.renderPartial(w, r, "node-form", nodeForm{
		viewData: viewData{PageData: Base(currentUser(r), s.auth.EnsureCSRF(w, r), "nodes")},
		Mode:     "create",
		Engine:   "auto",
	})
}

// nodeEditPartial renders the edit modal for one node.
func (s *Server) nodeEditPartial(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return
	}
	rec, err := s.db.NodeByID(r.Context(), id)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	s.renderPartial(w, r, "node-form", nodeForm{
		viewData: viewData{PageData: Base(currentUser(r), s.auth.EnsureCSRF(w, r), "nodes")},
		Mode:     "edit",
		ID:       rec.ID,
		Name:     rec.Name,
		Engine:   defaultStr(rec.Engine, "auto"),
		Endpoint: rec.Endpoint,
		TLSCA:    rec.TLSCA,
		TLSCert:  rec.TLSCert,
		TLSKey:   rec.TLSKey,
		IsLocal:  s.nodes.IsLocal(rec.Endpoint),
	})
}

// validateNode checks the submitted connection settings.
func validateNode(name, endpoint, eng string) string {
	switch {
	case strings.TrimSpace(name) == "":
		return "Give the node a name."
	case len(name) > 80:
		return "The name must be at most 80 characters."
	case endpoint == "":
		return "Enter an endpoint such as unix:///var/run/docker.sock or tcp://10.0.0.5:2376."
	}
	switch eng {
	case "auto", "docker", "podman", "":
	default:
		return "Unknown engine type."
	}
	scheme := endpoint
	if i := strings.Index(scheme, "://"); i > 0 {
		scheme = scheme[:i]
	} else {
		return "The endpoint must start with unix://, tcp://, http:// or https://."
	}
	switch scheme {
	case "unix", "tcp", "http", "https":
		return ""
	default:
		return "Unsupported endpoint scheme " + scheme + "."
	}
}

// nodeCreate persists a new node, probes it and starts its streams.
func (s *Server) nodeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	f := nodeForm{
		Mode:     "create",
		Name:     strings.TrimSpace(r.FormValue("name")),
		Engine:   defaultStr(r.FormValue("engine"), "auto"),
		Endpoint: strings.TrimSpace(r.FormValue("endpoint")),
		TLSCA:    strings.TrimSpace(r.FormValue("tls_ca")),
		TLSCert:  strings.TrimSpace(r.FormValue("tls_cert")),
		TLSKey:   strings.TrimSpace(r.FormValue("tls_key")),
	}
	if msg := validateNode(f.Name, f.Endpoint, f.Engine); msg != "" {
		f.Error = msg
		f.viewData = viewData{PageData: Base(currentUser(r), s.auth.EnsureCSRF(w, r), "nodes")}
		s.renderPartialStatus(w, r, http.StatusUnprocessableEntity, "node-form", f)
		return
	}
	rec := &database.Node{
		Name: f.Name, Engine: f.Engine, Endpoint: f.Endpoint,
		TLSCA: f.TLSCA, TLSCert: f.TLSCert, TLSKey: f.TLSKey,
	}
	if err := s.db.CreateNode(r.Context(), rec); err != nil {
		f.Error = "Could not save the node. Is that endpoint already configured?"
		if errors.Is(err, database.ErrConflict) {
			f.Error = "A node with that endpoint already exists."
		}
		f.viewData = viewData{PageData: Base(currentUser(r), s.auth.EnsureCSRF(w, r), "nodes")}
		s.renderPartialStatus(w, r, http.StatusUnprocessableEntity, "node-form", f)
		return
	}
	s.afterNodeChange(r, rec.ID, "created", f.Name)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// nodeUpdate saves edits to an existing node.
func (s *Server) nodeUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return
	}
	rec, err := s.db.NodeByID(r.Context(), id)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	f := nodeForm{
		Mode:     "edit",
		ID:       id,
		Name:     strings.TrimSpace(r.FormValue("name")),
		Engine:   defaultStr(r.FormValue("engine"), "auto"),
		Endpoint: strings.TrimSpace(r.FormValue("endpoint")),
		TLSCA:    strings.TrimSpace(r.FormValue("tls_ca")),
		TLSCert:  strings.TrimSpace(r.FormValue("tls_cert")),
		TLSKey:   strings.TrimSpace(r.FormValue("tls_key")),
	}
	if msg := validateNode(f.Name, f.Endpoint, f.Engine); msg != "" {
		f.Error = msg
		f.viewData = viewData{PageData: Base(currentUser(r), s.auth.EnsureCSRF(w, r), "nodes")}
		s.renderPartialStatus(w, r, http.StatusUnprocessableEntity, "node-form", f)
		return
	}
	rec.Name, rec.Engine, rec.Endpoint = f.Name, f.Engine, f.Endpoint
	rec.TLSCA, rec.TLSCert, rec.TLSKey = f.TLSCA, f.TLSCert, f.TLSKey
	if err := s.db.UpdateNode(r.Context(), rec); err != nil {
		f.Error = "Could not save the node."
		if errors.Is(err, database.ErrConflict) {
			f.Error = "Another node already uses that endpoint."
		}
		f.viewData = viewData{PageData: Base(currentUser(r), s.auth.EnsureCSRF(w, r), "nodes")}
		s.renderPartialStatus(w, r, http.StatusUnprocessableEntity, "node-form", f)
		return
	}
	s.afterNodeChange(r, rec.ID, "updated", rec.Name)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// afterNodeChange refreshes the manager and restarts the node's streams.
func (s *Server) afterNodeChange(r *http.Request, id int64, verb, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.nodes.Refresh(ctx); err != nil {
		s.log.Error("refresh nodes", "error", err)
	}
	s.metrics.StopNode(id)
	// The endpoint or credentials may have changed, so the cached image list
	// belongs to the old engine.
	s.images.Forget(id)
	// Probe before deciding. A node that was just added has never been
	// probed, so its status is still zeroed and Online false; testing
	// Status.Online first would skip the streams entirely. The health loop
	// does not recover this — it re-probes periodically but only ever updates
	// status, it never starts the streams — so without this the node has no
	// metrics or events until the process restarts.
	//
	// Note that ctx is cancelled by the defer above as soon as this returns,
	// which is why StartNodeStreams deliberately ignores it.
	if n, ok := s.nodes.Get(id); ok && n.Client != nil {
		_ = s.nodes.ProbeByID(ctx, id)
		if n, ok := s.nodes.Get(id); ok && n.Status.Online {
			s.StartNodeStreams(ctx, n)
		} else {
			s.log.Warn("node "+verb+" but its engine is unreachable; streams not started",
				"node", name)
		}
	}
	s.log.Info("node "+verb, "node", name, "by", currentUsername(r))
}

// nodeDelete removes a node and stops its streams.
func (s *Server) nodeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return
	}
	rec, err := s.db.NodeByID(r.Context(), id)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	if s.nodes.IsLocal(rec.Endpoint) {
		http.Error(w, "The local engine is discovered automatically and cannot be removed. Set CW_LOCAL_SOCKETS=none to hide it.", http.StatusConflict)
		return
	}
	if err := s.db.DeleteNode(r.Context(), id); err != nil {
		s.log.Error("delete node", "node", rec.Name, "error", err)
		http.Error(w, "Could not delete the node.", http.StatusInternalServerError)
		return
	}
	s.metrics.StopNode(id)
	s.images.Forget(id)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.nodes.Refresh(ctx)
	s.log.Info("node deleted", "node", rec.Name, "by", currentUsername(r))
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// nodeProbe re-tests a node and returns the refreshed card.
func (s *Server) nodeProbe(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.nodes.ProbeByID(ctx, id); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	n, ok := s.nodes.Get(id)
	if !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	if n.Client != nil && n.Status.Online {
		s.StartNodeStreams(ctx, n)
	}
	s.renderPartial(w, r, "node-card", nodeCard(n, s.nodes.IsLocal(n.Record.Endpoint), s.auth.EnsureCSRF(w, r)))
}

// nodeCardView is the view model for a node card in the admin grid.
type nodeCardView struct {
	ID        int64
	Name      string
	Engine    string
	Endpoint  string
	Online    bool
	Local     bool
	Version   string
	Platform  string
	CPUs      int
	MemTotal  int64
	Error     string
	CheckedAt time.Time
	CSRF      string
}

func nodeCard(n *engine.Node, local bool, csrf string) nodeCardView {
	st := n.Status
	return nodeCardView{
		ID:        n.Record.ID,
		Name:      n.Record.Name,
		CSRF:      csrf,
		Engine:    defaultStr(string(st.Kind), defaultStr(n.Record.Engine, "auto")),
		Endpoint:  n.Record.Endpoint,
		Online:    st.Online,
		Local:     local,
		Version:   st.Version,
		Platform:  st.Platform,
		CPUs:      st.CPUs,
		MemTotal:  st.MemTotal,
		Error:     st.Error,
		CheckedAt: st.CheckedAt,
	}
}

// usersView is the payload for the user administration page.
type usersView struct {
	viewData
	Users      []*database.User
	AdminCount int
}

// usersPage lists accounts and their roles.
func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) {
	vd := s.base(w, r, "users", "Users")
	users, err := s.db.ListUsers(r.Context())
	if err != nil {
		vd.Flash = "Could not load users: " + err.Error()
		vd.FlashKind = "error"
	}
	admins := 0
	for _, u := range users {
		if u.IsAdmin() {
			admins++
		}
	}
	s.renderPage(w, r, http.StatusOK, "admin_users", "layout", usersView{
		viewData: vd, Users: users, AdminCount: admins,
	})
}

// userRoleUpdate promotes or demotes an account.
func (s *Server) userRoleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	role := r.FormValue("role")
	if role != auth.RoleAdmin && role != auth.RoleUser {
		http.Error(w, "Unknown role.", http.StatusBadRequest)
		return
	}
	target, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		http.Error(w, "User not found.", http.StatusNotFound)
		return
	}
	// Never allow the last administrator to demote themselves: that would lock
	// everyone out of node and user management.
	if target.IsAdmin() && role == auth.RoleUser {
		n, cerr := s.db.CountAdmins(r.Context())
		if cerr == nil && n <= 1 {
			http.Error(w, "At least one administrator must remain.", http.StatusConflict)
			return
		}
	}
	if err := s.db.UpdateUserRole(r.Context(), id, role); err != nil {
		s.log.Error("update role", "user", target.Username, "error", err)
		http.Error(w, "Could not update the role.", http.StatusInternalServerError)
		return
	}
	s.log.Info("role changed", "user", target.Username, "role", role, "by", currentUsername(r))
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// userDelete removes an account.
func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	me := currentUser(r)
	target, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		http.Error(w, "User not found.", http.StatusNotFound)
		return
	}
	if me != nil && me.ID == target.ID {
		http.Error(w, "You cannot delete your own account.", http.StatusConflict)
		return
	}
	if target.IsAdmin() {
		n, cerr := s.db.CountAdmins(r.Context())
		if cerr == nil && n <= 1 {
			http.Error(w, "At least one administrator must remain.", http.StatusConflict)
			return
		}
	}
	if err := s.db.DeleteUser(r.Context(), id); err != nil {
		s.log.Error("delete user", "user", target.Username, "error", err)
		http.Error(w, "Could not delete the account.", http.StatusInternalServerError)
		return
	}
	s.log.Info("user deleted", "user", target.Username, "by", currentUsername(r))
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// settingsView is the payload for the settings page.
type settingsView struct {
	viewData
	RegistrationOpen bool
	MetricsInterval  time.Duration
	MetricsWindow    int
	SessionTTL       time.Duration
	SecureCookies    string
	TrustProxy       bool
	PublicURL        string
	DataDir          string
	DatabasePath     string
	LocalSockets     []string
	Version          string
	BuildDate        string
	Started          time.Time
	Uptime           time.Duration
	DefaultNode      int64
	Host             string
}

// settingsPage renders the runtime configuration summary and editable flags.
func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	vd := s.base(w, r, "settings", "Settings")
	secure := "auto"
	if s.cfg.SecureCookies != nil {
		if *s.cfg.SecureCookies {
			secure = "always"
		} else {
			secure = "never"
		}
	}
	s.renderPage(w, r, http.StatusOK, "admin_settings", "layout", settingsView{
		viewData:         vd,
		RegistrationOpen: s.registrationOpen(r),
		MetricsInterval:  s.metrics.Interval(),
		MetricsWindow:    s.metrics.Window(),
		SessionTTL:       s.cfg.SessionTTL,
		SecureCookies:    secure,
		TrustProxy:       s.cfg.TrustProxy,
		PublicURL:        s.cfg.PublicURL,
		DataDir:          s.cfg.DataDir,
		DatabasePath:     s.cfg.DatabasePath,
		LocalSockets:     s.cfg.LocalSockets,
		Version:          s.Version(),
		BuildDate:        version.BuildDate,
		Host:             s.cfg.Addr,
		Started:          s.started,
		Uptime:           time.Since(s.started),
		DefaultNode:      atoi64(s.db.GetSetting(r.Context(), SettingDefaultNode, "0"), 0),
	})
}

// settingsUpdate saves the admin-toggleable settings.
func (s *Server) settingsUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	open := r.FormValue("registration_open") == "on" || r.FormValue("registration_open") == "true"
	if err := s.db.SetSetting(r.Context(), SettingRegistrationOpen, boolStr(open)); err != nil {
		s.log.Error("save settings", "error", err)
		http.Error(w, "Could not save settings.", http.StatusInternalServerError)
		return
	}
	s.log.Info("settings updated", "registration_open", open, "by", currentUsername(r))
	redirectWith(w, r, "/admin/settings", nil)
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// ---------------------------------------------------------------- account ----

// accountView is the payload for the personal account page.
type accountView struct {
	viewData
	Sessions int
	Error    string
	Notice   string
}

// accountPage renders the signed-in user's own settings.
func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	vd := s.base(w, r, "account", "Account")
	s.renderPage(w, r, http.StatusOK, "account", "layout", accountView{viewData: vd})
}

// accountPassword changes the current user's password.
func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !s.auth.CheckCSRF(r) {
		http.Error(w, "Your session token expired. Reload the page and try again.", http.StatusForbidden)
		return
	}
	current := r.FormValue("current_password")
	next := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	fail := func(msg string, status int) {
		vd := s.base(w, r, "account", "Account")
		s.renderPage(w, r, status, "account", "layout", accountView{viewData: vd, Error: msg})
	}

	if current == "" || next == "" {
		fail("Fill in your current and new password.", http.StatusBadRequest)
		return
	}
	if len(next) < 8 {
		fail("The new password must be at least 8 characters.", http.StatusBadRequest)
		return
	}
	if next != confirm {
		fail("The two new passwords do not match.", http.StatusBadRequest)
		return
	}
	if next == current {
		fail("Choose a password different from the current one.", http.StatusBadRequest)
		return
	}
	ok, err := auth.VerifyPassword(current, u.PasswordHash)
	if err != nil || !ok {
		fail("Your current password is incorrect.", http.StatusUnauthorized)
		return
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		fail("Could not hash the password.", http.StatusInternalServerError)
		return
	}
	if err := s.db.UpdateUserPassword(r.Context(), u.ID, hash); err != nil {
		s.log.Error("update password", "user", u.Username, "error", err)
		fail("Could not save the new password.", http.StatusInternalServerError)
		return
	}
	// Sign out every other device; keep this session valid.
	if _, sess, err := s.auth.Authenticate(r); err == nil && sess != nil {
		_ = s.db.DeleteUserSessions(r.Context(), u.ID, sess.TokenHash)
	}
	s.log.Info("password changed", "user", u.Username)
	vd := s.base(w, r, "account", "Account")
	s.renderPage(w, r, http.StatusOK, "account", "layout", accountView{
		viewData: vd,
		Notice:   "Password updated. Other devices have been signed out.",
	})
}

// renderPartialStatus renders a partial with an explicit status code (used so
// htmx keeps an error fragment and its 4xx response in sync).
func (s *Server) renderPartialStatus(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.render.RenderPartial(&buf, name, data); err != nil {
		s.log.Error("render partial", "partial", name, "error", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
