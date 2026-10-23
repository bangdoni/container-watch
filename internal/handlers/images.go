package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/bangdoni/container-watch/internal/engine"
)

// The engine's GET /images/json is expensive: it walks the local image store
// and took 300-460ms on a 57-image Podman store, while every other page render
// in the app is single-digit milliseconds. Fetching it inline on every /images
// view therefore dominated the response time (measured: 2-5ms of template work
// against 300-460ms of engine wait).
//
// The cache below serves the last list immediately and refreshes it in the
// background, so a visit is a pure template render. Freshness is a UI concern
// only — images change rarely, and a stale list for a few seconds is harmless
// next to a half-second blank page.
const (
	// imageTTL is how long a fetched list is served without revalidating.
	imageTTL = 30 * time.Second
	// imageErrorTTL is the much shorter window an engine error is cached for,
	// so a node that recovers is noticed quickly but a dead engine is not
	// re-queried on every page view.
	imageErrorTTL = 5 * time.Second
	// imageFetchTimeout bounds a single engine call, matching the page
	// handler's previous 10s budget.
	imageFetchTimeout = 10 * time.Second
)

type imageEntry struct {
	images  []*engine.Image
	total   int64
	fetched time.Time
	err     error
}

// imageCache keeps the most recent image list per node.
type imageCache struct {
	mu      sync.Mutex
	entries map[int64]imageEntry
	// refreshing records nodes with a background fetch in flight, so a burst
	// of requests collapses into one engine call.
	refreshing map[int64]bool
	// now is time.Now, overridable in tests to exercise the stale path
	// without sleeping for a whole TTL.
	now func() time.Time
}

func newImageCache() *imageCache {
	return &imageCache{
		entries:    make(map[int64]imageEntry),
		refreshing: make(map[int64]bool),
		now:        time.Now,
	}
}

// clock reads the cache clock, defaulting to time.Now for the zero value.
func (c *imageCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Get returns the image list for a node, preferring the cache.
//
// bg is the server-wide lifetime context; background refreshes run under it so
// they survive the request that triggered them. force bypasses the cache and
// waits for the engine (the Refresh button).
func (c *imageCache) Get(ctx context.Context, bg context.Context, nodeID int64, client *engine.Client, force bool) ([]*engine.Image, int64, error) {
	c.mu.Lock()
	e, ok := c.entries[nodeID]
	c.mu.Unlock()

	if ok && !force {
		ttl := imageTTL
		if e.err != nil {
			ttl = imageErrorTTL
		}
		if c.clock().Sub(e.fetched) < ttl {
			return e.images, e.total, e.err
		}
		if e.err == nil {
			// Stale but usable: hand back the previous list and revalidate
			// behind the response.
			c.refresh(bg, nodeID, client)
			return e.images, e.total, nil
		}
		// A stale error is not worth showing again; retry synchronously below.
	}

	images, err := client.ListImages(ctx)
	if err != nil {
		c.store(nodeID, nil, err)
		return nil, 0, err
	}
	out, total := toImages(images)
	c.store(nodeID, out, nil)
	return out, total, nil
}

// Warm fills the cache for a node in the background. Called when a node comes
// online so the first /images visit does not pay the engine round-trip.
func (c *imageCache) Warm(bg context.Context, nodeID int64, client *engine.Client) {
	c.refresh(bg, nodeID, client)
}

// Forget drops a node's cached list (node deleted or reconfigured).
func (c *imageCache) Forget(nodeID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, nodeID)
	delete(c.refreshing, nodeID)
}

// refresh starts a background fetch unless one is already running.
func (c *imageCache) refresh(bg context.Context, nodeID int64, client *engine.Client) {
	if client == nil {
		return
	}
	c.mu.Lock()
	if c.refreshing[nodeID] {
		c.mu.Unlock()
		return
	}
	c.refreshing[nodeID] = true
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.refreshing, nodeID)
			c.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(bg, imageFetchTimeout)
		defer cancel()
		images, err := client.ListImages(ctx)
		if err != nil {
			c.store(nodeID, nil, err)
			return
		}
		out, _ := toImages(images)
		c.store(nodeID, out, nil)
	}()
}

func (c *imageCache) store(nodeID int64, images []*engine.Image, err error) {
	var total int64
	for _, img := range images {
		total += img.Size
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[nodeID] = imageEntry{
		images:  images,
		total:   total,
		fetched: c.clock(),
		err:     err,
	}
}

// toImages adapts the engine slice for templates and sums its on-disk size.
func toImages(images []engine.Image) ([]*engine.Image, int64) {
	var total int64
	out := make([]*engine.Image, 0, len(images))
	for i := range images {
		total += images[i].Size
		out = append(out, &images[i])
	}
	return out, total
}
