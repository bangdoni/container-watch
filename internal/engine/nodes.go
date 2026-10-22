package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerwatch/containerwatch/internal/database"
)

// NodeStatus is the live state of a configured endpoint.
type NodeStatus struct {
	ID        int64
	Name      string
	Endpoint  string
	Kind      Kind
	Version   string
	Platform  string
	CPUs      int
	MemTotal  int64
	Online    bool
	Local     bool
	Error     string
	CheckedAt time.Time
}

// Node combines the persisted node with its live client and status.
type Node struct {
	Record *database.Node
	Client *Client
	Status NodeStatus
}

// Manager owns the set of monitored endpoints: the local auto-discovered
// socket(s) plus every admin-configured remote node.
type Manager struct {
	store *database.Store
	log   *slog.Logger

	mu    sync.RWMutex
	nodes map[int64]*Node
	// localIDs holds ids for auto-discovered nodes (persisted as nodes so the
	// UI can reference them stably).
	localSocketPaths map[string]int64

	localSockets []string
	disableLocal bool

	// refreshMu serialises Refresh calls.
	refreshMu sync.Mutex
}

// NewManager builds a node manager.
func NewManager(store *database.Store, log *slog.Logger, localSockets []string, disableLocal bool) *Manager {
	return &Manager{
		store:            store,
		log:              log,
		nodes:            make(map[int64]*Node),
		localSocketPaths: make(map[string]int64),
		localSockets:     localSockets,
		disableLocal:     disableLocal,
	}
}

// Bootstrap discovers local sockets, persists them as nodes (marked internal by
// name convention) and connects every configured node.
func (m *Manager) Bootstrap(ctx context.Context) error {
	if !m.disableLocal {
		for _, raw := range m.localSockets {
			ep := strings.TrimSpace(raw)
			if ep == "" {
				continue
			}
			path := strings.TrimPrefix(strings.TrimPrefix(ep, "docker://"), "podman://")
			if _, err := os.Stat(path); err != nil {
				m.log.Debug("local engine socket not present", "path", path)
				continue
			}
			normalised := "unix://" + path
			rec, err := m.store.NodeByEndpoint(ctx, normalised)
			if errors.Is(err, database.ErrNotFound) {
				name := localNodeName(path)
				rec = &database.Node{
					Name:     name,
					Engine:   KindAuto.String(),
					Endpoint: normalised,
				}
				if cerr := m.store.CreateNode(ctx, rec); cerr != nil {
					return fmt.Errorf("register local node: %w", cerr)
				}
				m.log.Info("discovered local container engine", "name", name, "endpoint", normalised)
			} else if err != nil {
				return fmt.Errorf("lookup local node: %w", err)
			}
			m.localSocketPaths[normalised] = rec.ID
		}
	}
	return m.Refresh(ctx)
}

func localNodeName(path string) string {
	switch {
	case strings.Contains(path, "podman"):
		return "Local Podman"
	case strings.Contains(path, "docker"):
		return "Local Docker"
	default:
		return "Local Engine"
	}
}

// applyRecords reconciles the cached node set with the persisted records and
// returns the nodes that should be probed.
func (m *Manager) applyRecords(records []*database.Node) []*Node {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[int64]struct{}, len(records))
	for _, rec := range records {
		seen[rec.ID] = struct{}{}
		if existing, ok := m.nodes[rec.ID]; ok && existing.Client != nil && existing.Record.Endpoint == rec.Endpoint {
			// Reuse the client; only the metadata changed.
			existing.Record = rec
			continue
		}
		node := &Node{Record: rec}
		if client, err := FromNode(rec); err != nil {
			node.Status = NodeStatus{ID: rec.ID, Name: rec.Name, Endpoint: rec.Endpoint, Error: err.Error()}
		} else {
			node.Client = client
		}
		m.nodes[rec.ID] = node
	}
	for id, node := range m.nodes {
		if _, ok := seen[id]; !ok {
			delete(m.localSocketPaths, node.Record.Endpoint)
			delete(m.nodes, id)
		}
	}
	targets := make([]*Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		targets = append(targets, n)
	}
	return targets
}

// Refresh reloads node records from the database and probes each endpoint.
// Nodes that were removed in the database are dropped from the cache.
func (m *Manager) Refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	records, err := m.store.ListNodes(ctx)
	if err != nil {
		return err
	}

	// Mutate the cache under a single lock, released by defer so a panic in
	// this block cannot leave the manager wedged.
	targets := m.applyRecords(records)

	// Probe in parallel with a short timeout so a dead VPS doesn't stall boot.
	var wg sync.WaitGroup
	for _, n := range targets {
		if n.Client == nil {
			continue
		}
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			m.probe(pctx, n)
		}(n)
	}
	wg.Wait()
	return nil
}

func (m *Manager) probe(ctx context.Context, n *Node) {
	info, err := n.Client.Probe(ctx)
	now := time.Now()
	if err != nil {
		m.log.Warn("node unreachable", "node", n.Record.Name, "endpoint", n.Record.Endpoint, "error", err)
		m.mu.Lock()
		n.Status = NodeStatus{
			ID: n.Record.ID, Name: n.Record.Name, Endpoint: n.Record.Endpoint,
			Kind: n.Client.Kind(), Error: err.Error(), CheckedAt: now,
		}
		m.mu.Unlock()
		return
	}
	// Resolve locality before taking the write lock: IsLocal takes the read
	// lock and sync.RWMutex is not reentrant.
	local := m.IsLocal(n.Record.Endpoint)
	m.mu.Lock()
	n.Status = NodeStatus{
		ID:        n.Record.ID,
		Name:      n.Record.Name,
		Endpoint:  n.Record.Endpoint,
		Kind:      n.Client.Kind(),
		Version:   info.ServerVersion,
		Platform:  info.OperatingSystem,
		CPUs:      info.NCPU,
		MemTotal:  info.MemTotal,
		Online:    true,
		Local:     local,
		CheckedAt: now,
	}
	m.mu.Unlock()
}

// List returns a snapshot of all nodes ordered by id.
func (m *Manager) List() []*Node {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, n)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Record.ID < out[j-1].Record.ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Get returns a node by id.
func (m *Manager) Get(id int64) (*Node, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[id]
	return n, ok
}

// Default returns the first online node, or the first node when none are
// online (so the UI can show the connection error).
func (m *Manager) Default() (*Node, bool) {
	nodes := m.List()
	for _, n := range nodes {
		if n.Status.Online {
			return n, true
		}
	}
	if len(nodes) > 0 {
		return nodes[0], true
	}
	return nil, false
}

// IsLocal reports whether the endpoint is an auto-discovered local socket.
func (m *Manager) IsLocal(endpoint string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.localSocketPaths[endpoint]
	return ok
}

// ProbeByID re-probes a single node (used by the "retry" button).
func (m *Manager) ProbeByID(ctx context.Context, id int64) error {
	n, ok := m.Get(id)
	if !ok {
		return database.ErrNotFound
	}
	if n.Client == nil {
		c, err := FromNode(n.Record)
		if err != nil {
			return err
		}
		m.mu.Lock()
		n.Client = c
		m.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	m.probe(ctx, n)
	return nil
}

// StartHealthLoop re-probes every node periodically so the node switcher stays
// accurate. It returns when ctx is done.
func (m *Manager) StartHealthLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, n := range m.List() {
				if n.Client == nil {
					continue
				}
				pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
				m.probe(pctx, n)
				cancel()
			}
		}
	}
}
