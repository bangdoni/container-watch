// Package handlers wires the HTTP surface: authentication, the dashboard,
// htmx partials, SSE streams and the admin panels.
package handlers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/containerwatch/containerwatch/internal/auth"
	"github.com/containerwatch/containerwatch/internal/config"
	"github.com/containerwatch/containerwatch/internal/database"
	"github.com/containerwatch/containerwatch/internal/engine"
	"github.com/containerwatch/containerwatch/internal/hoststats"
	"github.com/containerwatch/containerwatch/internal/sse"
)

// Server holds every dependency the handlers need.
type Server struct {
	cfg     *config.Config
	db      *database.Store
	auth    *auth.Manager
	mw      *auth.Middleware
	nodes   *engine.Manager
	metrics *sse.MetricsBroker
	events  *sse.EventBroker
	hub     *sse.Hub
	render  *Renderer
	host    *hoststats.Collector
	log     *slog.Logger
	started time.Time

	// limiter throttles authentication attempts per client IP.
	limiter *rateLimiter
	// setupMu serialises first-run admin creation.
	setupMu sync.Mutex
}

// New builds a Server.
func New(
	cfg *config.Config,
	db *database.Store,
	am *auth.Manager,
	nodes *engine.Manager,
	hub *sse.Hub,
	metrics *sse.MetricsBroker,
	events *sse.EventBroker,
	log *slog.Logger,
) (*Server, error) {
	r, err := NewRenderer()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:     cfg,
		db:      db,
		auth:    am,
		mw:      auth.NewMiddleware(am),
		nodes:   nodes,
		metrics: metrics,
		events:  events,
		hub:     hub,
		render:  r,
		host:    hoststats.New(),
		log:     log,
		started: time.Now(),
		limiter: newRateLimiter(6, 5*time.Minute),
	}, nil
}

// viewData is embedded in every template payload.
type viewData struct {
	PageData
	Nodes      []*engine.Node
	NodeID     int64
	NodeOnline bool
	// Flash carries a one-shot message rendered at the top of the content.
	Flash string
	// FlashKind is "error", "success", "warn" or "info".
	FlashKind string
	// Data holds arbitrary page-specific values.
	Data map[string]any
}

// flashKind maps a kind to the CSS class suffix used by the alert component.
func (v viewData) alertClass() string {
	switch v.FlashKind {
	case "success", "warn", "info":
		return v.FlashKind
	default:
		return "error"
	}
}

// AlertClass is the exported form of alertClass.
//
// html/template resolves a field or method through reflection, and
// reflect cannot see unexported methods. The templates call
// `{{ .alertClass }}`, which therefore failed with
//
//	can't evaluate field alertClass in type handlers.viewData
//
// and turned every page that sets a Flash into a 500 — precisely the
// error paths (offline node, image list failure, user list failure)
// that exist to show a friendly message. Exporting the method keeps
// the template syntax unchanged and lets the embedded value promote
// it, because only exported methods are promoted through embedding
// for the template package.
func (v viewData) AlertClass() string { return v.alertClass() }

// renderPage executes a page template into a buffer (so template errors do not
// produce a half-written response) and writes it.
func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, status int, page, layout string, data any) {
	var buf bytes.Buffer
	if err := s.render.Render(&buf, page, layout, data); err != nil {
		s.log.Error("render page", "page", page, "error", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderPartial writes a named partial.
func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := s.render.RenderPartial(&buf, name, data); err != nil {
		s.log.Error("render partial", "partial", name, "error", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// base builds page chrome for a request. The ResponseWriter is required so the
// CSRF cookie can be written when a client arrives without one.
func (s *Server) base(w http.ResponseWriter, r *http.Request, active, title string) viewData {
	u := auth.CurrentUser(r.Context())
	pd := Base(u, s.auth.EnsureCSRF(w, r), active)
	pd.Title = title
	return viewData{PageData: pd, Nodes: s.nodes.List()}
}

// page builds the full view data for a rendered page: chrome, selected node
// and — when the node is reachable — its container list plus live metrics.
func (s *Server) page(w http.ResponseWriter, r *http.Request, active, title string) (viewData, *engine.Node, bool) {
	vd := s.base(w, r, active, title)
	n, ok := s.nodeParam(r)
	if !ok {
		return vd, nil, false
	}
	vd.NodeID = n.Record.ID
	vd.NodeOnline = n.Status.Online
	vd.Data = map[string]any{"node": n}
	return vd, n, true
}

// containersFor returns the container list for a node, preferring the metrics
// broker's cache so a render never blocks on the engine. When the cache is cold
// the engine is queried directly and the result is adapted for rendering.
func (s *Server) containersFor(r *http.Request, n *engine.Node) ([]engine.Container, error) {
	if list, _, ok := s.metrics.Containers(n.Record.ID); ok {
		return list, nil
	}
	if n.Client == nil {
		return nil, errors.New("node has no active connection")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	return n.Client.ListContainers(ctx)
}

// withNode fills the selected node into the view data.
func (s *Server) withNode(r *http.Request, vd viewData) (viewData, *engine.Node, bool) {
	n, ok := s.nodeParam(r)
	if ok {
		vd.NodeID = n.Record.ID
		vd.NodeOnline = n.Status.Online
	}
	return vd, n, ok
}

// nodeParam resolves ?node=, falling back to the default node.
func (s *Server) nodeParam(r *http.Request) (*engine.Node, bool) {
	if v := r.URL.Query().Get("node"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if n, ok := s.nodes.Get(id); ok {
				return n, true
			}
		}
	}
	return s.nodes.Default()
}

// nodeIDFromRequest returns the node id for streaming endpoints.
func (s *Server) nodeIDFromRequest(r *http.Request) (int64, bool) {
	n, ok := s.nodeParam(r)
	if !ok {
		return 0, false
	}
	return n.Record.ID, true
}

// parseIntDefault parses an int with a fallback.
func parseIntDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Routes builds the application router.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", staticHandler())
	mux.Handle("GET /healthz", http.HandlerFunc(s.healthz))

	// Public auth pages.
	mux.Handle("GET /login", http.HandlerFunc(s.loginPage))
	mux.Handle("POST /login", http.HandlerFunc(s.loginSubmit))
	mux.Handle("GET /setup", http.HandlerFunc(s.setupPage))
	mux.Handle("POST /setup", http.HandlerFunc(s.setupSubmit))
	mux.Handle("GET /signup", http.HandlerFunc(s.signupPage))
	mux.Handle("POST /signup", http.HandlerFunc(s.signupSubmit))
	mux.Handle("POST /logout", http.HandlerFunc(s.logout))

	// Authenticated UI.
	user := func(h http.HandlerFunc) http.Handler {
		return s.mw.AttachUser(s.mw.RequireUser(h))
	}
	mux.Handle("GET /{$}", user(s.dashboardPage))
	mux.Handle("GET /containers", user(s.containersPage))
	mux.Handle("GET /containers/search", user(s.containersSearchPartial))
	mux.Handle("GET /containers/table", user(s.containersTablePartial))
	mux.Handle("GET /containers/{id}", user(s.containerDetailPage))
	mux.Handle("GET /containers/{id}/detail", user(s.containerDetailPartial))
	mux.Handle("GET /containers/{id}/metrics-panel", user(s.containerMetricsPartial))
	mux.Handle("GET /containers/{id}/logs-panel", user(s.containerLogsPartial))
	mux.Handle("GET /containers/{id}/logs/stream", user(s.containerLogsStream))
	mux.Handle("GET /api/containers/{id}/series", user(s.containerSeriesAPI))
	mux.Handle("GET /images", user(s.imagesPage))
	mux.Handle("GET /account", user(s.accountPage))
	mux.Handle("POST /account/password", user(s.accountPassword))

	// SSE streams.
	mux.Handle("GET /events/host", user(s.hostStream))
	mux.Handle("GET /events/metrics", user(s.metricsStream))
	mux.Handle("GET /events/containers", user(s.containerEventsStream))

	// Lifecycle actions: admin + CSRF.
	for _, action := range []string{"start", "stop", "restart", "pause", "unpause", "kill"} {
		mux.Handle("POST /containers/{id}/"+action,
			s.mw.AttachUser(s.mw.RequireAdmin(s.containerAction(action))))
	}

	// Admin panels.
	admin := func(h http.HandlerFunc) http.Handler {
		return s.mw.AttachUser(s.mw.RequireAdmin(h))
	}
	mux.Handle("GET /admin/nodes", admin(s.nodesPage))
	mux.Handle("GET /admin/nodes/new", admin(s.nodeFormPartial))
	mux.Handle("GET /admin/nodes/{id}/edit", admin(s.nodeEditPartial))
	mux.Handle("POST /admin/nodes", admin(s.nodeCreate))
	mux.Handle("POST /admin/nodes/{id}", admin(s.nodeUpdate))
	mux.Handle("POST /admin/nodes/{id}/delete", admin(s.nodeDelete))
	mux.Handle("POST /admin/nodes/{id}/probe", admin(s.nodeProbe))
	mux.Handle("GET /admin/users", admin(s.usersPage))
	mux.Handle("POST /admin/users/{id}/role", admin(s.userRoleUpdate))
	mux.Handle("POST /admin/users/{id}/delete", admin(s.userDelete))
	mux.Handle("GET /admin/settings", admin(s.settingsPage))
	mux.Handle("POST /admin/settings", admin(s.settingsUpdate))

	mux.Handle("GET /favicon.ico", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/img/favicon.svg", http.StatusFound)
	}))

	return s.recoverer(s.securityHeaders(mux))
}
