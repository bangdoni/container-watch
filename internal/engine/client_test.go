package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bangdoni/container-watch/internal/engine"
	"github.com/bangdoni/container-watch/internal/enginetest"
)

// newClient builds an engine client pointed at a fake engine.
func newClient(t *testing.T, srv *enginetest.Server) *engine.Client {
	t.Helper()
	c, err := engine.New(engine.Options{
		Endpoint: srv.URL(),
		Kind:     engine.KindAuto,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return c
}

// TestProbeDetectsDocker checks that Probe negotiates the API version and
// classifies a Docker-flavoured engine.
func TestProbeDetectsDocker(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()

	c := newClient(t, srv)
	info, err := c.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.ServerVersion != "24.0.7" {
		t.Errorf("ServerVersion = %q, want 24.0.7", info.ServerVersion)
	}
	if c.APIVersion() == "" {
		t.Error("APIVersion() is empty")
	}
	if c.Kind() != engine.KindDocker {
		t.Errorf("Kind() = %q, want %q", c.Kind(), engine.KindDocker)
	}
}

// TestProbeFailsWhenOffline checks that an unreachable engine surfaces an
// error rather than a zero-valued Info.
func TestProbeFailsWhenOffline(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	srv.SetOffline(true)

	c := newClient(t, srv)
	if _, err := c.Probe(context.Background()); err == nil {
		t.Fatal("expected Probe to fail against an offline engine")
	}
}

// TestPing checks the cheap reachability probe.
func TestPing(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()

	c := newClient(t, srv)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	srv.SetOffline(true)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected Ping to fail against an offline engine")
	}
}

// TestListContainers checks the container list and the helper accessors.
func TestListContainers(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	srv.AddContainer(enginetest.SampleContainer(
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"web", "nginx:1.27"))
	srv.AddContainer(enginetest.SampleStoppedContainer(
		"1111111111111111111111111111111111111111111111111111111111111111",
		"db", "postgres:16"))

	c := newClient(t, srv)
	list, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d containers, want 2", len(list))
	}
	byName := map[string]engine.Container{}
	for _, ct := range list {
		byName[ct.Name()] = ct
	}
	web, ok := byName["web"]
	if !ok {
		t.Fatal("container named \"web\" not found")
	}
	if web.State != "running" {
		t.Errorf("web.State = %q, want running", web.State)
	}
	if got := web.ShortID(); got != "abcdef012345" {
		t.Errorf("ShortID() = %q, want abcdef012345", got)
	}
	if got := web.Health(); got != "healthy" {
		t.Errorf("Health() = %q, want healthy", got)
	}
	if got := web.Uptime(); got != "3 hours" {
		t.Errorf("Uptime() = %q, want \"3 hours\"", got)
	}
	if got := web.Network(); got != "default" {
		t.Errorf("Network() = %q, want default", got)
	}
	db, ok := byName["db"]
	if !ok {
		t.Fatal("container named \"db\" not found")
	}
	if db.State != "exited" {
		t.Errorf("db.State = %q, want exited", db.State)
	}
	if got := db.Uptime(); got != "" {
		t.Errorf("Uptime() for a stopped container = %q, want empty", got)
	}
}

// TestListContainersOffline checks that a wedged daemon produces an error
// rather than an empty slice. The distinction matters: the UI has to tell
// "no containers" apart from "cannot reach the engine".
func TestListContainersOffline(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	srv.SetFailList(true)

	c := newClient(t, srv)
	if _, err := c.ListContainers(context.Background()); err == nil {
		t.Fatal("expected an error when the engine cannot list containers")
	}
}

// TestListImages checks the image list.
func TestListImages(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	srv.SetImages(enginetest.Image{
		ID:       "sha256:deadbeef",
		RepoTags: []string{"nginx:1.27"},
		Size:     42 << 20,
		Created:  time.Now().Add(-time.Hour).Unix(),
	})

	c := newClient(t, srv)
	imgs, err := c.ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("got %d images, want 1", len(imgs))
	}
	if imgs[0].RepoTags[0] != "nginx:1.27" {
		t.Errorf("RepoTags[0] = %q", imgs[0].RepoTags[0])
	}
}

// TestDoLifecycleAction checks that start/stop/restart reach the engine with
// the right container id.
func TestDoLifecycleAction(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	srv.AddContainer(enginetest.SampleContainer(
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"web", "nginx:1.27"))

	c := newClient(t, srv)
	if err := c.Do(context.Background(), "abcdef012345", engine.ActionStart); err != nil {
		t.Fatalf("Do: %v", err)
	}
	acts := srv.Actions()
	if len(acts) != 1 {
		t.Fatalf("got %d actions, want 1", len(acts))
	}
	if acts[0].Action != "start" {
		t.Errorf("action = %q, want start", acts[0].Action)
	}
	if acts[0].ID != "abcdef012345" {
		t.Errorf("id = %q", acts[0].ID)
	}
}

// TestLogsStream checks that log lines are decoded and delivered to the
// callback in order.
func TestLogsStream(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	const id = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	srv.AddContainer(enginetest.SampleContainer(id, "web", "nginx:1.27"))
	srv.SetLogs(id, "first line", "second line", "third line")

	c := newClient(t, srv)
	var got []string
	err := c.Logs(context.Background(), id, engine.LogOptions{Tail: 50}, func(l engine.LogLine) error {
		got = append(got, l.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d log lines, want 3: %v", len(got), got)
	}
	if got[0] != "first line" || got[2] != "third line" {
		t.Errorf("log lines out of order: %v", got)
	}
}

// TestLogsCallbackErrorStops checks that returning an error from the
// callback aborts the stream — the SSE handler relies on this to stop when
// the browser disconnects.
func TestLogsCallbackErrorStops(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	const id = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	srv.AddContainer(enginetest.SampleContainer(id, "web", "nginx:1.27"))
	srv.SetLogs(id, "one", "two", "three")

	c := newClient(t, srv)
	sentinel := errors.New("client went away")
	count := 0
	err := c.Logs(context.Background(), id, engine.LogOptions{Tail: 50}, func(engine.LogLine) error {
		count++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Logs error = %v, want %v", err, sentinel)
	}
	if count != 1 {
		t.Errorf("callback ran %d times, want 1 (stream should stop on error)", count)
	}
}

// TestValidateID checks that container ids are validated before they reach
// the engine, since they are interpolated into a URL path.
func TestValidateID(t *testing.T) {
	good := []string{
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"abc123",
	}
	for _, id := range good {
		if err := engine.ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", id, err)
		}
	}
	bad := []string{
		"../etc/passwd",
		"a/b",
		"a?b=1",
		"a#frag",
		"a b",
		"",
	}
	for _, id := range bad {
		if err := engine.ValidateID(id); err == nil {
			t.Errorf("ValidateID(%q) = nil, want an error", id)
		}
	}
}

// TestNewRejectsEmptyEndpoint checks the client constructor.
func TestNewRejectsEmptyEndpoint(t *testing.T) {
	if _, err := engine.New(engine.Options{Endpoint: ""}); err == nil {
		t.Fatal("expected an error for an empty endpoint")
	}
}

// TestInspectContainer checks the inspect path and its "no such container"
// handling.
func TestInspectContainer(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	const id = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	srv.AddContainer(enginetest.SampleContainer(id, "web", "nginx:1.27"))

	c := newClient(t, srv)
	ins, err := c.InspectContainer(context.Background(), id)
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if ins.Name != "web" {
		t.Errorf("Name = %q, want web", ins.Name)
	}
	if _, err := c.InspectContainer(context.Background(), "000000000000"); err == nil {
		t.Error("expected an error inspecting a missing container")
	}
}

// TestStats checks the one-shot stats call.
func TestStats(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	const id = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	srv.AddContainer(enginetest.SampleContainer(id, "web", "nginx:1.27"))
	srv.SetStats(id, enginetest.Stats{
		MemUsage: 100 << 20,
		MemLimit: 512 << 20,
		NetRx:    1024,
		NetTx:    2048,
	})

	c := newClient(t, srv)
	st, err := c.Stats(context.Background(), id)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.MemUsed != 100<<20 {
		t.Errorf("MemUsed = %d, want %d", st.MemUsed, 100<<20)
	}
	if st.NetTx != 2048 {
		t.Errorf("NetTx = %d, want 2048", st.NetTx)
	}
}

// TestOfflineEngineErrorsSurface checks that every network-facing call
// reports an error while the engine is offline, so callers never mistake
// an unreachable daemon for an empty one.
func TestOfflineEngineErrorsSurface(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()
	srv.SetOffline(true)

	c := newClient(t, srv)
	ctx := context.Background()

	if _, err := c.ListContainers(ctx); err == nil {
		t.Error("ListContainers: expected an error")
	}
	if _, err := c.ListImages(ctx); err == nil {
		t.Error("ListImages: expected an error")
	}
	if _, err := c.Stats(ctx, "abc"); err == nil {
		t.Error("Stats: expected an error")
	}
	if err := c.Ping(ctx); err == nil {
		t.Error("Ping: expected an error")
	}
	if err := c.Do(ctx, "abc", engine.ActionStart); err == nil {
		t.Error("Do: expected an error")
	}
}

// TestLogsOnMissingContainer checks the not-found path returns an error
// rather than an empty stream.
func TestLogsOnMissingContainer(t *testing.T) {
	srv := enginetest.New()
	defer srv.Close()

	c := newClient(t, srv)
	called := false
	err := c.Logs(context.Background(), "doesnotexist", engine.LogOptions{Tail: 10},
		func(engine.LogLine) error { called = true; return nil })
	if err == nil {
		t.Fatal("expected an error streaming logs of a missing container")
	}
	if called {
		t.Error("callback should not run for a missing container")
	}
}

// TestContainerNameFallback checks Name() when the engine reports no names.
func TestContainerNameFallback(t *testing.T) {
	c := engine.Container{ID: "0123456789abcdef"}
	got := c.Name()
	if !strings.HasPrefix(c.ID, got) {
		t.Errorf("Name() = %q, want a prefix of the id", got)
	}
}

// TestStreamEventsDeliversLifecycleEvents checks the event stream against the
// fake engine.
//
// Without this, the fake returned 404 for /events, every EventBroker client
// failed to connect and retried with backoff, and the container-change SSE
// stream was permanently silent -- which looked identical to the broker never
// having been started. That ambiguity is what let a real lifetime bug hide
// behind a gap in the fake.
func TestStreamEventsDeliversLifecycleEvents(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()

	const id = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	fake.AddContainer(enginetest.SampleContainer(id, "web-frontend", "nginx:1.27"))

	c := newClient(t, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events, err := c.StreamEvents(ctx)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	// Give the stream a moment to connect before triggering the action.
	time.Sleep(100 * time.Millisecond)
	if err := c.Do(ctx, id, engine.ActionRestart); err != nil {
		t.Fatalf("Do(restart): %v", err)
	}

	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("the event channel closed without delivering anything")
		}
		if ev.Type != "container" {
			t.Errorf("event Type = %q, want container", ev.Type)
		}
		if ev.Action != "restart" {
			t.Errorf("event Action = %q, want restart", ev.Action)
		}
		if got := ev.ContainerName(); got != "web-frontend" {
			t.Errorf("ContainerName() = %q, want web-frontend", got)
		}
	case <-ctx.Done():
		t.Fatal("no event was delivered for a container restart")
	}
}

// TestEventStreamClosesWithTheFake makes sure Close releases an open
// /events subscriber. httptest.Server.Close blocks until every in-flight
// request returns, so a fake that leaks its stream hangs the whole test
// binary instead of failing.
func TestEventStreamClosesWithTheFake(t *testing.T) {
	fake := enginetest.New()
	c := newClient(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.StreamEvents(ctx); err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		fake.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on an open event stream")
	}
}
