package handlers

import (
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bangdoni/container-watch/web"
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
//
// 'unsafe-eval' is required by Alpine 3: it compiles every x-* / @event
// expression with `new AsyncFunction`. All expressions are authored in our own
// templates (never built from user input), so the added risk is bounded; a
// strict-CSP deployment would need Alpine's CSP build plus a full template
// rewrite to component methods.
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
					"script-src 'self' 'unsafe-eval'; "+
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
//
// The response reveals the version and the node counts, so by default it only
// answers callers on this machine. Container probes (Docker HEALTHCHECK, the
// -healthcheck flag) run inside the container and qualify; an unauthenticated
// visitor on the network does not. Set CW_PUBLIC_HEALTHCHECK=true to serve
// everyone, for example when an external uptime monitor needs to reach it.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PublicHealthcheck && !s.isLocalRequest(r) {
		// 404 rather than 403: a "no such endpoint" answer does not confirm
		// that the probe exists.
		http.NotFound(w, r)
		return
	}

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

// isLocalRequest reports whether the request came from this machine.
//
// The client address is taken from X-Forwarded-For only when the operator has
// declared a trusted proxy (CW_TRUST_PROXY). That matters behind a reverse
// proxy on the same host: every request then arrives from 127.0.0.1, so
// looking at the peer address alone would let the whole internet read the
// probe. When the proxy is trusted, the forwarded address is the client.
//
// Without a trusted proxy the header is ignored entirely - a caller could
// otherwise claim to be 127.0.0.1 and read the version and node counts.
//
// A peer is local when it is a loopback address, or when it is one of this
// host's own addresses - the latter covers a self-probe that targets a
// specific interface rather than 127.0.0.1 (CW_ADDR=10.0.0.5:8080).
func (s *Server) isLocalRequest(r *http.Request) bool {
	return isLocalAddr(s.clientAddr(r))
}

// clientAddr resolves the address of the caller, honouring the forwarded
// header only when a proxy is trusted.
func (s *Server) clientAddr(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLocalAddr reports whether addr belongs to this machine.
func isLocalAddr(addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// SetPublicHealthcheck turns the /healthz probe on or off for callers that
// are not on this machine. It exists so the behaviour can be exercised from
// tests; at runtime the setting comes from CW_PUBLIC_HEALTHCHECK.
func (s *Server) SetPublicHealthcheck(v bool) { s.cfg.PublicHealthcheck = v }

// SetTrustProxy toggles whether forwarded client headers are believed. It
// exists so the behaviour can be exercised from tests; at runtime the setting
// comes from CW_TRUST_PROXY.
func (s *Server) SetTrustProxy(v bool) { s.cfg.TrustProxy = v }

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
