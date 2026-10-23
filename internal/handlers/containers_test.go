package handlers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bangdoni/container-watch/internal/auth"
	"github.com/bangdoni/container-watch/internal/config"
	"github.com/bangdoni/container-watch/internal/database"
	"github.com/bangdoni/container-watch/internal/engine"
	"github.com/bangdoni/container-watch/internal/enginetest"
	"github.com/bangdoni/container-watch/internal/handlers"
	"github.com/bangdoni/container-watch/internal/sse"
)

// env is the handler test environment: a real SQLite store, a real session
// manager, a real engine manager pointed at a fake engine, and a logged-in
// admin.
type env struct {
	srv     *handlers.Server
	routes  http.Handler
	store   *database.Store
	fake    *enginetest.Server
	nodes   *engine.Manager
	hub     *sse.Hub
	metrics *sse.MetricsBroker
	cookie  *http.Cookie
	csrf    string
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

	// Take the CSRF token the server hands to a rendered page, so postForm
	// submits exactly what the browser would.
	csrf := am.EnsureCSRF(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	return &env{
		srv:     srv,
		routes:  srv.Routes(),
		store:   store,
		fake:    fake,
		nodes:   nodes,
		hub:     hub,
		metrics: metrics,
		cookie:  cookie,
		csrf:    csrf,
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

// postForm performs an authenticated POST with form values and the session's
// CSRF token, mirroring what the browser sends.
func (e *env) postForm(t *testing.T, path string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.AddCookie(e.cookie)
	// Double-submit CSRF: the token is compared between the cookie and the
	// form field, so both must carry it.
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: e.csrf})
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	// Reuse the token the server already issued to this session.
	form.Set("_csrf", e.csrf)

	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.ContentLength = int64(len(form.Encode()))

	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	return rec
}

// TestNodeCreateStartsMetricStream is the regression test for the two bugs
// that made a node added through the UI produce no metrics at all.
//
//  1. afterNodeChange called StartNodeStreams only when Status.Online was
//     already true, but a node that has just been created has never been
//     probed, so its status was still zeroed and the streams were skipped.
//     Nothing recovered this: the health loop re-probes but only ever updates
//     status, it never starts the streams.
//
//  2. StartNodeStreams passed the caller's context to the brokers. afterNodeChange
//     uses "ctx, cancel := context.WithTimeout(...); defer cancel()", so the
//     samplers were torn down the instant the HTTP request returned.
//
// The node still listed containers, because that path queries the engine on
// demand — which is why neither bug looked like a wiring problem.
func TestNodeCreateStartsMetricStream(t *testing.T) {
	e := newEnv(t)
	e.fake.AddContainer(enginetest.SampleContainer(
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"web-frontend", "nginx:1.27"))

	// A second engine, because the node endpoint is unique and the bootstrap
	// node already owns the first fake's URL.
	other := enginetest.New()
	t.Cleanup(other.Close)
	other.AddContainer(enginetest.SampleContainer(
		"2222222222222222222222222222222222222222222222222222222222222222",
		"api-server", "redis:7"))

	// Add a second node at runtime, the way the admin UI does.
	rec := e.postForm(t, "/admin/nodes", map[string]string{
		"name":     "added-at-runtime",
		"endpoint": other.URL(),
		"engine":   "docker",
	})
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("node create returned %d, want 204", rec.Code)
	}

	// Find the new node's id.
	nodes, err := e.store.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes in the store, got %d", len(nodes))
	}
	var addedID int64
	for _, n := range nodes {
		if n.Name == "added-at-runtime" {
			addedID = n.ID
		}
	}
	if addedID == 0 {
		t.Fatal("the new node was not found in the store")
	}

	// The broker must begin sampling it on its own. Poll rather than sleep a
	// fixed amount so the test is not timing-fragile.
	// Watch the node's real SSE topic. The broker's container cache is written
	// once and never expires, so it would still look populated after the
	// sampler died; only a published event proves the goroutine is alive.
	events, unsubscribe := e.hub.Subscribe(sse.TopicMetrics(addedID))
	defer unsubscribe()

	if !e.waitForEvent(events, 6*time.Second) {
		t.Fatal("no metrics event was published for the runtime-added node; " +
			"the sampler either never started or died with the request context")
	}
}

// waitForEvent blocks until a metrics event arrives on ch, or within elapses.
func (e *env) waitForEvent(ch <-chan sse.Event, within time.Duration) bool {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case <-ch:
			return true
		case <-timer.C:
			return false
		}
	}
}

// TestPagesRenderingTheTablePartialDoNotError guards the field layout of the
// shared "containers-table" partial.
//
// The partial reads .NodeOnline and .NodeError. It is rendered from three
// different payloads — dashboardView, containersView and tablePartialView —
// and Go templates resolve a missing field as a hard render error, so any
// payload that omits one turns the whole page into a 500 rather than a
// partial render. That is not hypothetical: adding .NodeError to the partial
// while only teaching containersView about it 500'd the dashboard, and it
// only showed up against a running server because no test rendered the
// dashboard with the offline branch taken.
//
// Every page that embeds the partial must return 200 in both the online and
// the offline case.
func TestPagesRenderingTheTablePartialDoNotError(t *testing.T) {
	pages := []string{"/", "/containers?node=0"}

	for _, online := range []bool{true, false} {
		name := "offline"
		if online {
			name = "online"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if online {
				e.fake.AddContainer(enginetest.SampleContainer(
					"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
					"web-frontend", "nginx:1.27"))
			} else {
				e.fake.SetOffline(true)
			}
			for _, n := range e.nodes.List() {
				_ = e.nodes.ProbeByID(context.Background(), n.Record.ID)
			}

			for _, page := range pages {
				rec := e.get(t, page)
				if rec.Code != http.StatusOK {
					t.Errorf("%s returned %d, want 200", page, rec.Code)
					continue
				}
				body := rec.Body.String()
				// A template render error leaks into the body as a 500 page;
				// a clean render never contains the engine's error text.
				if strings.Contains(body, "can't evaluate field") {
					t.Errorf("%s failed to render the table partial:\n%s", page, body)
				}
			}
		})
	}
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

// TestImagesPageCachesList proves /images does not block on the engine's slow
// /images/json endpoint on every view.
//
// Measured against a real 57-image Podman store, the engine call took
// 300-460ms while the template rendered in 2-5ms, so the page was almost
// entirely engine wait. The handler now serves a cached list and revalidates
// in the background, which makes a warm view a pure render.
func TestImagesPageCachesList(t *testing.T) {
	e := newEnv(t)
	e.fake.SetImages(enginetest.Image{
		ID:       "sha256:cafebabe",
		RepoTags: []string{"redis:7"},
		Size:     10 << 20,
		Created:  time.Now().Add(-time.Hour).Unix(),
	})
	for _, n := range e.nodes.List() {
		_ = e.nodes.ProbeByID(context.Background(), n.Record.ID)
	}

	// First view populates the cache (the probe above may already have).
	if rec := e.get(t, "/images"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	calls := e.fake.ImageCalls()

	// Two more views inside the TTL must be served from memory.
	for i := 0; i < 2; i++ {
		rec := e.get(t, "/images")
		if rec.Code != http.StatusOK {
			t.Fatalf("view %d status = %d, want 200", i, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "cafebabe") {
			t.Fatalf("view %d is missing the cached image", i)
		}
	}
	if got := e.fake.ImageCalls(); got != calls {
		t.Errorf("cached views hit the engine: calls went %d -> %d", calls, got)
	}

	// Refresh forces a live read, so a newly pulled image appears.
	e.fake.SetImages(enginetest.Image{
		ID:       "sha256:feedface",
		RepoTags: []string{"postgres:16"},
		Size:     300 << 20,
		Created:  time.Now().Unix(),
	})
	rec := e.get(t, "/images?refresh=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "feedface") {
		t.Error("refresh did not pick up the new image")
	}
	if got := e.fake.ImageCalls(); got <= calls {
		t.Errorf("refresh did not reach the engine: calls still %d", got)
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
