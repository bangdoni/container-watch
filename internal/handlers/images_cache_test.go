package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/bangdoni/container-watch/internal/engine"
	"github.com/bangdoni/container-watch/internal/enginetest"
)

// newTestClient builds an engine client pointed at a fake engine.
func newTestClient(t *testing.T, fake *enginetest.Server) *engine.Client {
	t.Helper()
	c, err := engine.New(engine.Options{Endpoint: fake.URL(), Kind: engine.KindDocker})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return c
}

// TestImageCacheServesStaleWhileRevalidating covers the stale path: an entry
// older than the TTL must still be returned immediately (so the page never
// blocks) and a background fetch must replace it shortly after.
//
// If the refresh half of this regressed, the cache would serve the same list
// forever; if the serve-stale half regressed, every view after 30s would block
// on the engine again — the exact bug this cache exists to fix.
func TestImageCacheServesStaleWhileRevalidating(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	fake.SetImages(enginetest.Image{ID: "sha256:aaaa1111", RepoTags: []string{"old:1"}, Size: 1 << 20})

	client := newTestClient(t, fake)
	c := newImageCache()
	bg := context.Background()

	imgs, total, err := c.Get(bg, bg, 1, client, false)
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	if len(imgs) != 1 || total != 1<<20 {
		t.Fatalf("prime returned %d images, total %d", len(imgs), total)
	}

	// Age the entry past the TTL and change what the engine reports.
	c.mu.Lock()
	e := c.entries[1]
	e.fetched = time.Now().Add(-2 * imageTTL)
	c.entries[1] = e
	c.mu.Unlock()
	fake.SetImages(enginetest.Image{ID: "sha256:bbbb2222", RepoTags: []string{"new:2"}, Size: 2 << 20})

	// The stale read is immediate and still carries the old list.
	imgs, _, err = c.Get(bg, bg, 1, client, false)
	if err != nil {
		t.Fatalf("stale read: %v", err)
	}
	if len(imgs) != 1 || imgs[0].ShortID() != "aaaa1111" {
		t.Fatalf("stale read returned %v, want the cached aaaa1111 entry", imgs)
	}

	// The background refresh replaces it without another page view.
	deadline := time.Now().Add(3 * time.Second)
	for {
		imgs, _, err = c.Get(bg, bg, 1, client, false)
		if err != nil {
			t.Fatalf("after refresh: %v", err)
		}
		if len(imgs) == 1 && imgs[0].ShortID() == "bbbb2222" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background refresh never landed; still %v", imgs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestImageCacheForgetDropsEntry guards the node-deleted/node-edited path: a
// stale entry must not outlive its node.
func TestImageCacheForgetDropsEntry(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	fake.SetImages(enginetest.Image{ID: "sha256:aaaa1111", RepoTags: []string{"old:1"}, Size: 1})

	client := newTestClient(t, fake)
	c := newImageCache()
	bg := context.Background()

	if _, _, err := c.Get(bg, bg, 7, client, false); err != nil {
		t.Fatalf("prime: %v", err)
	}
	calls := fake.ImageCalls()
	c.Forget(7)
	if _, _, err := c.Get(bg, bg, 7, client, false); err != nil {
		t.Fatalf("after forget: %v", err)
	}
	if got := fake.ImageCalls(); got <= calls {
		t.Errorf("Forget left the entry in place: calls still %d", got)
	}
}

// TestImageCacheCachesErrorsBriefly checks a dead engine is not re-queried on
// every view, but that the failure is still reported to the page.
func TestImageCacheCachesErrorsBriefly(t *testing.T) {
	fake := enginetest.New()
	defer fake.Close()
	// An engine with no images still answers, so force a failure instead by
	// pointing the client at a closed server.
	dead := enginetest.New()
	dead.Close()
	client := newTestClient(t, dead)

	c := newImageCache()
	bg := context.Background()

	if _, _, err := c.Get(bg, bg, 3, client, false); err == nil {
		t.Fatal("expected an error from the closed engine")
	}
	c.mu.Lock()
	_, cached := c.entries[3]
	c.mu.Unlock()
	if !cached {
		t.Error("the failure was not cached, so every view will retry a dead engine")
	}

	// Within the error TTL the cached failure is returned without a new call.
	start := time.Now()
	if _, _, err := c.Get(bg, bg, 3, client, false); err == nil {
		t.Fatal("expected the cached error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cached error took %v, want an immediate return", elapsed)
	}
}
