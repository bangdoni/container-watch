package sse

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bangdoni/container-watch/internal/engine"
)

// Sample is one point in a container's resource time series.
//
// NetRx/NetTx are monotonic engine counters, so a chart drawn from them only
// ever climbs; RxRate/TxRate (bytes/second since the previous sample) are what
// the network chart actually plots. BlkRead/BlkWrite carry the same story for
// the disk chart.
type Sample struct {
	CPU   float64 `json:"cpu"`
	Mem   int64   `json:"mem"`
	Limit int64   `json:"limit"`
	NetRx uint64  `json:"net_rx"`
	NetTx uint64  `json:"net_tx"`
	// Block I/O totals, kept so the disk chart can be seeded from history.
	BlkRead  uint64 `json:"blk_read"`
	BlkWrite uint64 `json:"blk_write"`
	// Rates are the per-second deltas between this sample and the previous one.
	RxRate    float64 `json:"rx_rate"`
	TxRate    float64 `json:"tx_rate"`
	ReadRate  float64 `json:"read_rate"`
	WriteRate float64 `json:"write_rate"`
	Ts        int64   `json:"ts"`
}

// ContainerMetrics is the live resource state of one container.
type ContainerMetrics struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	CPU       float64 `json:"cpu"`
	Mem       int64   `json:"mem"`
	MemLimit  int64   `json:"mem_limit"`
	NetRx     uint64  `json:"net_rx"`
	NetTx     uint64  `json:"net_tx"`
	BlkRead   uint64  `json:"blk_read"`
	BlkWrite  uint64  `json:"blk_write"`
	RxRate    float64 `json:"rx_rate"`
	TxRate    float64 `json:"tx_rate"`
	ReadRate  float64 `json:"read_rate"`
	WriteRate float64 `json:"write_rate"`
	PIDs      uint64  `json:"pids"`
	Ts        int64   `json:"ts"`
}

// MemPercent returns working-set memory as a percentage of the limit.
func (m ContainerMetrics) MemPercent() float64 {
	if m.MemLimit <= 0 {
		return 0
	}
	return float64(m.Mem) / float64(m.MemLimit) * 100
}

// NodeSnapshot is the metrics payload published for a node.
type NodeSnapshot struct {
	NodeID     int64              `json:"node_id"`
	Ts         int64              `json:"ts"`
	Containers []ContainerMetrics `json:"containers"`
}

type listCache struct {
	containers []engine.Container
	at         time.Time
}

// MetricsBroker samples every running container on a node once per interval,
// keeps a bounded ring buffer per container, and publishes snapshots to the
// SSE hub. The container list fetched during sampling is cached so page
// renders never block on the engine.
type MetricsBroker struct {
	hub      *Hub
	log      *slog.Logger
	interval time.Duration
	window   int

	mu       sync.RWMutex
	series   map[string][]Sample // key: nodeID/containerID
	latest   map[string]ContainerMetrics
	lists    map[int64]listCache
	samplers map[int64]context.CancelFunc
	triggers map[int64]chan struct{}
}

// NewMetricsBroker builds a broker.
func NewMetricsBroker(hub *Hub, log *slog.Logger, interval time.Duration, window int) *MetricsBroker {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if window <= 0 {
		window = 60
	}
	return &MetricsBroker{
		hub:      hub,
		log:      log,
		interval: interval,
		window:   window,
		series:   make(map[string][]Sample),
		latest:   make(map[string]ContainerMetrics),
		lists:    make(map[int64]listCache),
		samplers: make(map[int64]context.CancelFunc),
		triggers: make(map[int64]chan struct{}),
	}
}

func keyOf(nodeID int64, containerID string) string {
	return itoa(nodeID) + "/" + containerID
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// StartNode begins sampling a node. Calling it twice for the same node is a
// no-op.
func (b *MetricsBroker) StartNode(ctx context.Context, nodeID int64, client *engine.Client) {
	b.mu.Lock()
	if _, running := b.samplers[nodeID]; running {
		b.mu.Unlock()
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	trigger := make(chan struct{}, 1)
	b.samplers[nodeID] = cancel
	b.triggers[nodeID] = trigger
	b.mu.Unlock()

	go func() {
		defer func() {
			b.mu.Lock()
			delete(b.samplers, nodeID)
			delete(b.triggers, nodeID)
			b.mu.Unlock()
		}()
		b.run(sctx, nodeID, client, trigger)
	}()
}

// StopNode halts sampling and forgets the node's data.
func (b *MetricsBroker) StopNode(nodeID int64) {
	b.mu.Lock()
	cancel, ok := b.samplers[nodeID]
	delete(b.samplers, nodeID)
	b.mu.Unlock()
	if ok {
		cancel()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.lists, nodeID)
	prefix := itoa(nodeID) + "/"
	for k := range b.series {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(b.series, k)
		}
	}
	for k := range b.latest {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(b.latest, k)
		}
	}
}

// Trigger asks the sampler to run immediately (after a lifecycle action).
func (b *MetricsBroker) Trigger(nodeID int64) {
	b.mu.RLock()
	ch := b.triggers[nodeID]
	b.mu.RUnlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (b *MetricsBroker) run(ctx context.Context, nodeID int64, client *engine.Client, trigger <-chan struct{}) {
	b.collect(ctx, nodeID, client)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.collect(ctx, nodeID, client)
		case <-trigger:
			b.collect(ctx, nodeID, client)
		}
	}
}

func (b *MetricsBroker) collect(ctx context.Context, nodeID int64, client *engine.Client) {
	cctx, cancel := context.WithTimeout(ctx, b.interval+5*time.Second)
	defer cancel()

	containers, err := client.ListContainers(cctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Debug("metrics: list containers failed", "node", nodeID, "error", err)
		}
		return
	}

	b.mu.Lock()
	b.lists[nodeID] = listCache{containers: containers, at: time.Now()}
	b.mu.Unlock()

	snap := NodeSnapshot{NodeID: nodeID, Ts: time.Now().UnixMilli()}
	live := make(map[string]bool, len(containers))

	for _, c := range containers {
		if c.State != "running" && c.State != "paused" {
			continue
		}
		live[c.ID] = true
		if c.State == "paused" {
			// Paused containers report frozen counters; reuse the last sample.
			b.mu.Lock()
			if m, ok := b.latest[keyOf(nodeID, c.ID)]; ok {
				snap.Containers = append(snap.Containers, m)
			}
			b.mu.Unlock()
			continue
		}
		st, err := client.Stats(cctx, c.ID)
		if err != nil {
			if ctx.Err() == nil {
				b.log.Debug("metrics: stats failed", "container", c.Name(), "error", err)
			}
			continue
		}
		cm := ContainerMetrics{
			ID:       c.ID,
			Name:     c.Name(),
			CPU:      round2(st.CPUPercent),
			Mem:      st.MemWorkingSet(),
			MemLimit: st.MemLimit,
			NetRx:    st.NetRx,
			NetTx:    st.NetTx,
			BlkRead:  st.BlockRead,
			BlkWrite: st.BlockWrite,
			PIDs:     st.PIDs,
			Ts:       snap.Ts,
		}
		b.record(nodeID, cm)
		snap.Containers = append(snap.Containers, cm)
	}

	b.prune(nodeID, live)
	b.hub.PublishJSON(TopicMetrics(nodeID), "metrics", snap)
}

func (b *MetricsBroker) record(nodeID int64, cm ContainerMetrics) {
	key := keyOf(nodeID, cm.ID)
	b.mu.Lock()
	defer b.mu.Unlock()

	if prev, ok := b.latest[key]; ok && cm.Ts > prev.Ts {
		dt := float64(cm.Ts-prev.Ts) / 1000
		if dt > 0 {
			cm.RxRate = rate(cm.NetRx, prev.NetRx, dt)
			cm.TxRate = rate(cm.NetTx, prev.NetTx, dt)
			cm.ReadRate = rate(cm.BlkRead, prev.BlkRead, dt)
			cm.WriteRate = rate(cm.BlkWrite, prev.BlkWrite, dt)
		}
	}

	s := b.series[key]
	if len(s) >= b.window {
		s = append(s[:0], s[len(s)-b.window+1:]...)
	}
	s = append(s, Sample{
		CPU:       cm.CPU,
		Mem:       cm.Mem,
		Limit:     cm.MemLimit,
		NetRx:     cm.NetRx,
		NetTx:     cm.NetTx,
		BlkRead:   cm.BlkRead,
		BlkWrite:  cm.BlkWrite,
		RxRate:    cm.RxRate,
		TxRate:    cm.TxRate,
		ReadRate:  cm.ReadRate,
		WriteRate: cm.WriteRate,
		Ts:        cm.Ts,
	})
	b.series[key] = s
	b.latest[key] = cm
}

// rate computes a per-second rate, treating counter resets as zero.
func rate(cur, prev uint64, dt float64) float64 {
	if cur < prev {
		return 0
	}
	return round2(float64(cur-prev) / dt)
}

func (b *MetricsBroker) prune(nodeID int64, live map[string]bool) {
	prefix := itoa(nodeID) + "/"
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.series {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix && !live[k[len(prefix):]] {
			delete(b.series, k)
			delete(b.latest, k)
		}
	}
}

// Series returns a copy of the retained samples for chart initialisation.
func (b *MetricsBroker) Series(nodeID int64, containerID string) []Sample {
	b.mu.RLock()
	defer b.mu.RUnlock()
	src := b.series[keyOf(nodeID, containerID)]
	out := make([]Sample, len(src))
	copy(out, src)
	return out
}

// Latest returns the most recent metrics for a container.
func (b *MetricsBroker) Latest(nodeID int64, containerID string) (ContainerMetrics, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m, ok := b.latest[keyOf(nodeID, containerID)]
	return m, ok
}

// Containers returns the cached container list for a node.
func (b *MetricsBroker) Containers(nodeID int64) ([]engine.Container, time.Time, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	c, ok := b.lists[nodeID]
	return c.containers, c.at, ok
}

// Interval exposes the sampling period.
func (b *MetricsBroker) Interval() time.Duration { return b.interval }

// Window exposes the ring buffer length.
func (b *MetricsBroker) Window() int { return b.window }

// TopicMetrics is the SSE topic name for a node's metrics stream.
func TopicMetrics(nodeID int64) string { return "metrics:" + itoa(nodeID) }

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
