package handlers

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/containerwatch/containerwatch/internal/version"
)

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONStatus encodes v with an explicit status code.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonError writes a JSON error envelope.
func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSONStatus(w, status, map[string]string{"error": msg})
}

// stackTrace returns the current goroutine's stack for panic logging.
func stackTrace() string { return string(debug.Stack()) }

// versionString centralises the build version for handlers.
func versionString() string { return version.String() }

// rateLimiter is a tiny fixed-window limiter keyed by client IP. It is used to
// throttle authentication attempts so a password cannot be brute-forced over
// the network at high speed.
type rateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	counters map[string]*counter
	lastGC   time.Time
}

type counter struct {
	n     int
	start time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:    limit,
		window:   window,
		counters: make(map[string]*counter),
		lastGC:   time.Now(),
	}
}

// Allow reports whether key may perform another attempt.
func (l *rateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.gc(now)

	c, ok := l.counters[key]
	if !ok || now.Sub(c.start) > l.window {
		l.counters[key] = &counter{n: 1, start: now}
		return true, 0
	}
	if c.n >= l.limit {
		return false, l.window - now.Sub(c.start)
	}
	c.n++
	return true, 0
}

// Reset clears the counter for a key (used after a successful login).
func (l *rateLimiter) Reset(key string) {
	l.mu.Lock()
	delete(l.counters, key)
	l.mu.Unlock()
}

// gc drops expired buckets every window to bound memory use.
func (l *rateLimiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < l.window {
		return
	}
	l.lastGC = now
	for k, c := range l.counters {
		if now.Sub(c.start) > l.window {
			delete(l.counters, k)
		}
	}
}
