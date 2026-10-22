package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/containerwatch/containerwatch/internal/engine"
	"github.com/containerwatch/containerwatch/internal/hoststats"
	"github.com/containerwatch/containerwatch/internal/sse"
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

// startNodeStreams attaches the brokers to a node once it is online. Called by
// the server wiring at boot and after a node is added or edited.
func (s *Server) StartNodeStreams(ctx context.Context, n *engine.Node) {
	if n == nil || n.Client == nil || !n.Status.Online {
		return
	}
	s.metrics.StartNode(ctx, n.Record.ID, n.Client)
	s.events.StartNode(ctx, n.Record.ID, n.Client)
}
