package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/bangdoni/container-watch/internal/engine"
	"github.com/bangdoni/container-watch/internal/hoststats"
	"github.com/bangdoni/container-watch/internal/sse"
)

// sseKeepAlive is the heartbeat interval for all streams. It sits below the
// common 60s proxy idle timeout.
const sseKeepAlive = 20 * time.Second

// hostStream pushes host CPU/memory readings for the selected node.
func (s *Server) hostStream(w http.ResponseWriter, r *http.Request) {
	n, ok := s.nodeParam(r)
	if !ok || n == nil {
		jsonError(w, http.StatusNotFound, "no node selected")
		return
	}

	flusher := sse.WriteHeaders(w)
	done := make(chan struct{})
	defer close(done)
	sse.KeepAlive(w, flusher, done, sseKeepAlive)

	interval := s.metrics.Interval()
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	send := func() bool {
		sample := s.hostSample(n)
		b, err := jsonMarshal(map[string]any{
			"node_id":   n.Record.ID,
			"cpu":       round1(sample.CPUPercent),
			"mem_pct":   round1(sample.MemUsedPercent()),
			"mem_used":  sample.MemUsed,
			"mem_total": sample.MemTotal,
			"load":      sample.LoadAvg,
			"source":    sample.Source,
			"ts":        time.Now().UnixMilli(),
		})
		if err != nil {
			return true
		}
		if err := sse.Send(w, sse.Event{Name: "host", Data: b}); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

// hostSample returns a host reading appropriate for the node: local procfs for
// local sockets, engine totals for remote hosts.
func (s *Server) hostSample(n *engine.Node) hoststats.Sample {
	if n.Status.Local {
		if smp := s.host.Sample(); smp.MemTotal > 0 {
			return smp
		}
	}
	// Remote node: use the engine's memory total and the sum of the cached
	// container working sets.
	var used int64
	if list, _, ok := s.metrics.Containers(n.Record.ID); ok {
		for _, c := range list {
			if m, found := s.metrics.Latest(n.Record.ID, c.ID); found {
				used += m.Mem
			}
		}
	}
	return hoststats.FromEngineTotals(n.Status.MemTotal, used, 0)
}

// metricsStream relays the broker's per-container snapshots for a node.
func (s *Server) metricsStream(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := s.nodeIDFromRequest(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "no node selected")
		return
	}

	flusher := sse.WriteHeaders(w)
	ch, cancel := s.hub.Subscribe(sse.TopicMetrics(nodeID))
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	sse.KeepAlive(w, flusher, done, sseKeepAlive)

	// Prime the stream with the latest snapshot so charts are not blank until
	// the next sampling tick.
	s.primeMetrics(w, nodeID)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := sse.Send(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// primeMetrics publishes an immediate snapshot from cached state.
func (s *Server) primeMetrics(w http.ResponseWriter, nodeID int64) {
	list, _, ok := s.metrics.Containers(nodeID)
	if !ok {
		return
	}
	snap := sse.NodeSnapshot{NodeID: nodeID, Ts: time.Now().UnixMilli()}
	for _, c := range list {
		if m, found := s.metrics.Latest(nodeID, c.ID); found {
			snap.Containers = append(snap.Containers, m)
		}
	}
	if len(snap.Containers) == 0 {
		return
	}
	b, err := jsonMarshal(snap)
	if err != nil {
		return
	}
	_ = sse.Send(w, sse.Event{Name: "metrics", Data: b})
}

// containerEventsStream notifies the page when containers change so htmx can
// re-fetch the table fragment.
func (s *Server) containerEventsStream(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := s.nodeIDFromRequest(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "no node selected")
		return
	}
	flusher := sse.WriteHeaders(w)
	ch, cancel := s.hub.Subscribe(sse.TopicEvents(nodeID))
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	sse.KeepAlive(w, flusher, done, sseKeepAlive)

	// A comment frames the stream open for browsers that wait for a byte.
	_ = sse.Comment(w, "connected")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := sse.Send(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// round1 rounds to one decimal for compact SSE payloads.
func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// StartNodeStreams attaches the brokers to a node once it is online. Called by
// the server wiring at boot and after a node is added or edited.
//
// ctx is deliberately ignored. The brokers start a goroutine that is meant to
// run for the lifetime of the process, so it must not inherit a caller's
// context. The caller in afterNodeChange uses
//
//	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
//	defer cancel()
//
// so handing that ctx to the brokers killed every sampler the moment the HTTP
// request finished. The node still rendered its container list — that path
// queries the engine on demand — which is exactly why the failure looked like
// "the node is wired up but metrics are missing" rather than a lifetime bug.
//
// ctx stays in the signature so the boot-time call site in main reads
// naturally, and so this stays a one-line change if a caller ever needs to pass
// a real lifetime in explicitly.
func (s *Server) StartNodeStreams(_ context.Context, n *engine.Node) {
	if n == nil || n.Client == nil || !n.Status.Online {
		return
	}
	ctx := s.lifetime()
	s.metrics.StartNode(ctx, n.Record.ID, n.Client)
	s.events.StartNode(ctx, n.Record.ID, n.Client)
	// Prime the image list so the first /images visit after a node comes
	// online is a cache hit instead of a 300ms+ engine round-trip.
	s.images.Warm(ctx, n.Record.ID, n.Client)
}

// lifetime returns the server-wide context that background streams run under.
// It is cancelled by Shutdown.
func (s *Server) lifetime() context.Context {
	s.bgOnce.Do(func() {
		s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	})
	return s.bgCtx
}

// Shutdown stops the background streams started by StartNodeStreams.
func (s *Server) Shutdown() {
	if s.bgCancel != nil {
		s.bgCancel()
	}
}
