package handlers

import (
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/containerwatch/containerwatch/web"
)

// staticHandler serves the embedded asset tree with long-lived immutable
// caching. Assets are content-addressed by build, so a cache-busting query
// string is unnecessary; operators upgrade the binary to change them.
func staticHandler() http.Handler {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		// The embed is compile-time; this cannot fail in practice.
		panic("handlers: static filesystem: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		// Strip the /static/ prefix so the file server sees the sub-path.
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/static")
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		files.ServeHTTP(w, r2)
	})
}

// securityHeaders applies conservative defaults suitable for a self-hosted
// admin UI. The CSP allows the inline styles produced by Alpine's :style
// bindings and the inline SVG icons rendered by the templates.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; "+
					"font-src 'self'; "+
					"connect-src 'self'; "+
					"frame-ancestors 'none'; "+
					"base-uri 'self'; "+
					"form-action 'self'")
		}
		if isStreaming(r) {
			// Never buffer SSE responses (nginx, Caddy, cloud LBs).
			h.Set("X-Accel-Buffering", "no")
		}
		next.ServeHTTP(w, r)
	})
}

// isStreaming reports whether the request targets an SSE endpoint.
func isStreaming(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/events/") || strings.HasSuffix(r.URL.Path, "/stream")
}

// recoverer converts panics into 500s (or aborts the stream if headers were
// already sent) and logs them once.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path, "panic", rec, "stack", stackTrace())
				// Best effort: if the response already started, this is a no-op
				// and the client will see a truncated body.
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// healthz is the liveness/readiness probe. It verifies the database is
// readable and reports how many nodes are online, without touching the engines
// (which may be slow or unreachable).
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	body := map[string]any{
		"status":  "ok",
		"uptime":  int64(time.Since(s.started).Seconds()),
		"version": s.Version(),
	}
	nodes := s.nodes.List()
	online := 0
	for _, n := range nodes {
		if n.Status.Online {
			online++
		}
	}
	body["nodes"] = len(nodes)
	body["nodes_online"] = online

	if err := s.db.DB().PingContext(r.Context()); err != nil {
		status = http.StatusServiceUnavailable
		body["status"] = "degraded"
		body["error"] = "database unavailable"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	writeJSON(w, body)
}

// Version exposes the build version for health output.
func (s *Server) Version() string {
	return versionString()
}

// noStore marks a response as uncacheable (used by all dynamic fragments).
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
}

// setHXTrigger queues a client-side event (used after mutations).
func setHXTrigger(w http.ResponseWriter, evt string) {
	if evt != "" {
		w.Header().Set("HX-Trigger", evt)
	}
}

// Int64 from a string, defaulting when malformed.
func atoi64(v string, def int64) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
