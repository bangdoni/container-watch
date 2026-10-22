package handlers_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerwatch/containerwatch/internal/auth"
	"github.com/containerwatch/containerwatch/internal/config"
	"github.com/containerwatch/containerwatch/internal/database"
	"github.com/containerwatch/containerwatch/internal/engine"
	"github.com/containerwatch/containerwatch/internal/enginetest"
	"github.com/containerwatch/containerwatch/internal/handlers"
	"github.com/containerwatch/containerwatch/internal/sse"
)

// env is the handler test environment: a real SQLite store, a real session
// manager, a real engine manager pointed at a fake engine, and a logged-in
// admin.
type env struct {
	srv    *handlers.Server
	routes http.Handler
	store  *database.Store
	fake   *enginetest.Server
	nodes  *engine.Manager
	cookie *http.Cookie
}

// newEnv builds the test environment. The node endpoint is the fake engine.
func newEnv(t *testing.T) *env {
	t.Helper()

	fake := enginetest.New()
	t.Cleanup(fake.Close)

	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	am := auth.NewManager(store, time.Hour, nil, false)

	// The node manager is told about the fake engine through a stored node
	// record, so the engine client is built from it.
	node := &database.Node{
		Name:     "fake",
		Endpoint: fake.URL(),
		Engine:   "docker",
	}
	if err := store.CreateNode(context.Background(), node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	nodes := engine.NewManager(store, quiet, nil, true)
	if err := nodes.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	cfg := &config.Config{
		Addr:            "127.0.0.1:0",
		DataDir:         dir,
		DatabasePath:    filepath.Join(dir, "test.db"),
		SessionTTL:      time.Hour,
		MetricsInterval: 2 * time.Second,
	}

	hub := sse.NewHub()
	metrics := sse.NewMetricsBroker(hub, quiet, 2*time.Second, 60)
	events := sse.NewEventBroker(hub, quiet)

	srv, err := handlers.New(cfg, store, am, nodes, hub, metrics, events, quiet)
	if err != nil {
		t.Fatalf("handlers.New: %v", err)
	}

	// Log in as an admin and keep the session cookie.
	hash, err := auth.HashPassword("Password123!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u := &database.User{
		Username:     "admin",
		Email:        "admin@example.com",
		PasswordHash: hash,
		Role:         "admin",
	}
	if err := store.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := am.Issue(rec, httptest.NewRequest(http.MethodGet, "/", nil), u); err != nil {
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

	return &env{
		srv:    srv,
		routes: srv.Routes(),
		store:  store,
		fake:   fake,
		nodes:  nodes,
		cookie: cookie,
	}
}

// get performs an authenticated GET against a handler route.
func (e *env) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	return rec
}

// TestContainersTableShowsEngineUnreachable is the regression test for the
// bug where an unreachable engine rendered as "The engine is reachable but
// reports no containers".
//
// The fake engine is switched offline, so the container list call fails. The
// table partial must say the engine is unreachable, and must NOT claim the
// engine is reachable.
func TestContainersTableShowsEngineUnreachable(t *testing.T) {
	e := newEnv(t)
	e.fake.SetOffline(true)

	// Force the node to be re-probed so its status reflects the failure.
	for _, n := range e.nodes.List() {
		_ = e.nodes.ProbeByID(context.Background(), n.Record.ID)
	}

	rec := e.get(t, "/containers/table?node=0&state=&q=")
	body := rec.Body.String()

	if strings.Contains(body, "The engine is reachable but reports no containers") {
		t.Errorf("the table claims the engine is reachable while it is offline:\n%s", body)
	}
	if !strings.Contains(body, "Engine unreachable") {
		t.Errorf("the table does not report the engine as unreachable:\n%s", body)
	}
}

// TestContainersTableShowsNoContainersWhenReachable is the counterpart: with
// a healthy engine that has no containers, the empty state must still say
// the engine is reachable.
func TestContainersTableShowsNoContainersWhenReachable(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContainers() // healthy, but empty

	for _, n := range e.nodes.List() {
		_ = e.nodes.ProbeByID(context.Background(), n.Record.ID)
	}

	rec := e.get(t, "/containers/table?node=0&state=&q=")
	body := rec.Body.String()

	if !strings.Contains(body, "The engine is reachable but reports no containers") {
		t.Errorf("the table does not report the empty-but-reachable state:\n%s", body)
	}
	if strings.Contains(body, "Engine unreachable") {
		t.Errorf("the table reports the engine as unreachable while it is healthy:\n%s", body)
	}
}

// TestContainersPageListsContainers checks that a healthy engine with
// containers renders them.
func TestContainersPageListsContainers(t *testing.T) {
	e := newEnv(t)
	e.fake.AddContainer(enginetest.SampleContainer(
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"web-frontend", "nginx:1.27"))
	e.fake.AddContainer(enginetest.SampleStoppedContainer(
		"1111111111111111111111111111111111111111111111111111111111111111",
		"batch-worker", "python:3.12"))

	for _, n := range e.nodes.List() {
		_ = e.nodes.ProbeByID(context.Background(), n.Record.ID)
	}

	rec := e.get(t, "/containers")
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{"web-frontend", "batch-worker", "nginx:1.27"} {
		if !strings.Contains(body, want) {
			t.Errorf("the containers page does not mention %q", want)
		}
	}
	if strings.Contains(body, "Engine unreachable") {
		t.Errorf("the page reports the engine as unreachable while it is healthy")
	}
}

// TestContainersPageShowsOfflineIndicator checks the header pill agrees with
// the table: an offline node must not be labelled "live".
func TestContainersPageShowsOfflineIndicator(t *testing.T) {
	e := newEnv(t)
	e.fake.SetOffline(true)

	for _, n := range e.nodes.List() {
		_ = e.nodes.ProbeByID(context.Background(), n.Record.ID)
	}

	rec := e.get(t, "/containers")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "node offline") {
		t.Errorf("the page does not show the offline indicator:\n%s", body[:min(len(body), 800)])
	}
}

// TestImagesPageListsImages checks the images route against the fake engine.
func TestImagesPageListsImages(t *testing.T) {
	e := newEnv(t)
	e.fake.SetImages(enginetest.Image{
		ID:       "sha256:deadbeef",
		RepoTags: []string{"nginx:1.27"},
		Size:     42 << 20,
		Created:  time.Now().Add(-time.Hour).Unix(),
	})

	for _, n := range e.nodes.List() {
		_ = e.nodes.ProbeByID(context.Background(), n.Record.ID)
	}

	rec := e.get(t, "/images")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The template splits the repo tag into two cells, so the literal
	// "nginx:1.27" never appears -- the repository and the tag are
	// rendered separately.
	for _, want := range []string{"nginx", "1.27", "deadbeef"} {
		if !strings.Contains(body, want) {
			t.Errorf("the images page does not mention %q", want)
		}
	}
}

// TestDashboardRenders checks the dashboard route.
func TestDashboardRenders(t *testing.T) {
	e := newEnv(t)
	rec := e.get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ContainerWatch") {
		t.Error("the dashboard does not mention the product name")
	}
}

// TestHealthz checks the health endpoint.
func TestHealthz(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestAnonymousIsRedirected checks the auth gate on a real page.
func TestAnonymousIsRedirected(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/containers", nil)
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("an anonymous request reached the containers page")
	}
	if !strings.Contains(rec.Header().Get("Location"), "/login") {
		t.Errorf("Location = %q, want a /login redirect", rec.Header().Get("Location"))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
