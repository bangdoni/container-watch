// Package enginetest provides an in-process fake of the Docker/Podman
// Engine API.
//
// The real engine speaks HTTP over a unix socket (or TLS on tcp://). This
// fake serves the same subset ContainerWatch uses, over plain HTTP, so the
// engine client, the HTTP handlers and the SSE pipeline can all be exercised
// without a container runtime installed.
//
// It exists because "the daemon is not running" is the single most common
// state a monitoring tool has to render correctly, and that state is
// impossible to test against a real daemon once it is up.
package enginetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Container is a container as the fake engine reports it.
type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Ports   []Port            `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	Mounts  []Mount           `json:"Mounts"`
	// NetworkMode mirrors engine.Container.NetworkMode.
	NetworkMode string `json:"NetworkMode"`
	// HostConfig mirrors engine.Container.HostConfig, which carries the
	// network mode on the list endpoint of some engine versions.
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
}

// Port is an exposed port.
type Port struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Mount is a bind/volume mount.
type Mount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
}

// Server is a fake engine. The zero value is not usable; call New.
type Server struct {
	mu sync.Mutex

	containers []Container
	images     []Image
	logs       map[string][]string // container id -> log lines
	stats      map[string]Stats    // container id -> latest stats
	actions    []Action            // lifecycle actions received

	// eventSubs holds the live /events subscribers, so a lifecycle action can
	// push an event to them the way a real daemon does.
	eventSubs map[chan ContainerEvent]struct{}
	// eventDone is closed by Close to release every /events handler.
	// httptest.Server.Close blocks until all in-flight requests return, so
	// without this a test that merely connected to /events would hang the
	// whole package instead of failing.
	eventDone chan struct{}

	// offline makes every endpoint fail with 503, mimicking a daemon
	// that is present but wedged.
	offline bool
	// failList makes only the container list fail.
	failList bool

	http *httptest.Server
}

// ContainerEvent is one entry of the /events stream. The field names are
// capitalised because that is what engine.Event decodes.
type ContainerEvent struct {
	Type     string `json:"Type"`
	Action   string `json:"Action"`
	Status   string `json:"Status"`
	ID       string `json:"ID"`
	From     string `json:"from"`
	Time     int64  `json:"time"`
	TimeNano int64  `json:"timeNano"`
	Actor    struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// Action records a lifecycle call (start/stop/restart/kill).
type Action struct {
	ID     string
	Action string
}

// Stats is the one-shot stats payload the fake reports.
type Stats struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemUsage   int64   `json:"mem_usage"`
	MemLimit   int64   `json:"mem_limit"`
	NetRx      int64   `json:"net_rx"`
	NetTx      int64   `json:"net_tx"`
	BlockRead  int64   `json:"block_read"`
	BlockWrite int64   `json:"block_write"`
}

// Image is an image as the fake engine reports it.
type Image struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
	Created  int64    `json:"Created"`
}

// New starts a fake engine and returns it. The caller must Close it.
func New() *Server {
	s := &Server{
		logs:      map[string][]string{},
		stats:     map[string]Stats{},
		eventSubs: map[chan ContainerEvent]struct{}{},
		eventDone: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.http = httptest.NewServer(mux)
	return s
}

// URL returns the base URL of the fake engine, suitable for
// engine.Options.Endpoint.
func (s *Server) URL() string { return s.http.URL }

// Close shuts the fake engine down. Any open /events stream is released first
// so the HTTP server's shutdown does not block on it.
func (s *Server) Close() {
	s.mu.Lock()
	select {
	case <-s.eventDone: // already closed
	default:
		close(s.eventDone)
	}
	s.mu.Unlock()
	s.http.Close()
}

// SetContainers replaces the container list.
func (s *Server) SetContainers(c ...Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containers = c
}

// AddContainer appends one container.
func (s *Server) AddContainer(c Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containers = append(s.containers, c)
}

// SetImages replaces the image list.
func (s *Server) SetImages(i ...Image) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images = i
}

// SetLogs sets the log lines returned for a container.
func (s *Server) SetLogs(id string, lines ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs[id] = lines
}

// SetStats sets the stats reported for a container.
func (s *Server) SetStats(id string, st Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats[id] = st
}

// SetOffline makes every endpoint fail, mimicking an unreachable daemon.
func (s *Server) SetOffline(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offline = v
}

// SetFailList makes only the container list fail.
func (s *Server) SetFailList(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failList = v
}

// Actions returns the lifecycle actions received so far.
func (s *Server) Actions() []Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Action, len(s.actions))
	copy(out, s.actions)
	return out
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	offline, failList := s.offline, s.failList
	s.mu.Unlock()

	if offline {
		http.Error(w, `{"message":"daemon is not running"}`, http.StatusServiceUnavailable)
		return
	}

	p := r.URL.Path
	switch {
	case p == "/_ping":
		w.Header().Set("Api-Version", "1.41")
		w.Header().Set("OSType", "linux")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	case p == "/version":
		s.writeJSON(w, map[string]any{
			"Version":    "24.0.7",
			"ApiVersion": "1.41",
			"Platform":   map[string]any{"Name": "Docker Engine - Community"},
		})
	case p == "/info":
		s.writeJSON(w, map[string]any{
			"ServerVersion": "24.0.7",
			"NCPU":          4,
			"MemTotal":      8 << 30,
			"Containers":    len(s.containersLocked()),
		})
	case p == "/containers/json":
		if failList {
			http.Error(w, `{"message":"cannot connect to the daemon"}`, http.StatusInternalServerError)
			return
		}
		list := s.containersLocked()
		for i := range list {
			if list[i].HostConfig.NetworkMode == "" {
				list[i].HostConfig.NetworkMode = list[i].EffectiveNetworkMode()
			}
		}
		s.writeJSON(w, list)
	case p == "/images/json":
		s.writeJSON(w, s.imagesLocked())
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		c, ok := s.containerByID(id)
		if !ok {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		s.writeJSON(w, map[string]any{
			"Id":     c.ID,
			"Name":   strings.TrimPrefix(c.Names[0], "/"),
			"State":  map[string]any{"Status": c.State, "StartedAt": time.Unix(c.Created, 0).Format(time.RFC3339)},
			"Config": map[string]any{"Image": c.Image, "Cmd": []string{"sh"}, "Labels": c.Labels},
			"HostConfig": map[string]any{
				"NetworkMode": c.EffectiveNetworkMode(),
			},
			"NetworkSettings": map[string]any{"Ports": map[string]any{}},
		})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/stats"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stats")
		st, ok := s.statsFor(id)
		if !ok {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		s.writeJSON(w, map[string]any{
			"cpu_stats": map[string]any{
				"cpu_usage":        map[string]any{"total_usage": 1_000_000_000},
				"system_cpu_usage": 10_000_000_000,
			},
			"memory_stats": map[string]any{
				"usage": st.MemUsage, "limit": st.MemLimit,
			},
			"networks": map[string]any{
				"eth0": map[string]any{"rx_bytes": st.NetRx, "tx_bytes": st.NetTx},
			},
			"blkio_stats": map[string]any{
				"io_service_bytes_recursive": []any{
					map[string]any{"op": "read", "value": st.BlockRead},
					map[string]any{"op": "write", "value": st.BlockWrite},
				},
			},
		})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/logs"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/logs")
		if _, ok := s.containerByID(id); !ok {
			// The real engine answers 404 for an unknown container; the
			// client maps that to engine.ErrNotFound.
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		for _, line := range s.logsFor(id) {
			// The Docker log format is an 8-byte header followed by the
			// payload. The trailing newline is part of the PAYLOAD and
			// must be counted in the header length -- writing it outside
			// the frame desynchronises every following frame, because the
			// reader takes the next 8 bytes as the next header.
			payload := line + "\n"
			fmt.Fprintf(w, "%s%s", logHeader(payload), payload)
		}
	case p == "/events":
		s.streamEvents(w, r)
	case strings.HasPrefix(p, "/containers/") && (strings.HasSuffix(p, "/start") ||
		strings.HasSuffix(p, "/stop") || strings.HasSuffix(p, "/restart") || strings.HasSuffix(p, "/kill")):
		parts := strings.Split(strings.TrimPrefix(p, "/containers/"), "/")
		if len(parts) == 2 {
			s.mu.Lock()
			s.actions = append(s.actions, Action{ID: parts[0], Action: parts[1]})
			s.mu.Unlock()
			// A real daemon emits an event for the action; clients rely on
			// this to refresh, so the fake must too.
			s.emitLifecycle(parts[0], parts[1])
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// streamEvents serves the Engine API event stream: newline-delimited JSON,
// held open until the client goes away.
//
// It deliberately does not honour the ?filters= parameter. The real daemon
// filters server-side; here every subscriber receives every event, which is
// enough to prove a client is consuming the stream.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Api-Version", "1.41")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := make(chan ContainerEvent, 8)
	s.mu.Lock()
	s.eventSubs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.eventSubs, ch)
		s.mu.Unlock()
	}()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.eventDone:
			return
		case ev := <-ch:
			if err := enc.Encode(ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// emitLifecycle pushes a container event to every /events subscriber.
func (s *Server) emitLifecycle(id, action string) {
	ev := ContainerEvent{
		Type:     "container",
		Action:   action,
		Status:   "restarting",
		ID:       id,
		Time:     time.Now().Unix(),
		TimeNano: time.Now().UnixNano(),
	}
	ev.Actor.ID = id
	ev.Actor.Attributes = map[string]string{"name": id, "image": ""}
	if c, ok := s.containerByID(id); ok {
		if n := c.Names; len(n) > 0 {
			ev.Actor.Attributes["name"] = strings.TrimPrefix(n[0], "/")
		}
		if len(c.Image) > 0 {
			ev.Actor.Attributes["image"] = c.Image
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.eventSubs {
		select {
		case ch <- ev:
		default: // a subscriber that is not keeping up misses the event
		}
	}
}

// logHeader builds an 8-byte Docker multiplexed-stream header:
// [stream type][0][0][0][payload length, big-endian uint32].
//
// The length MUST occupy bytes 4..7 -- the reader takes
// binary.BigEndian.Uint32(header[4:]). Writing only three length bytes
// shifts the field and corrupts every following frame.
func logHeader(payload string) string {
	n := uint32(len(payload))
	return string([]byte{
		1, 0, 0, 0,
		byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Api-Version", "1.41")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) containersLocked() []Container {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Container, len(s.containers))
	copy(out, s.containers)
	return out
}

func (s *Server) imagesLocked() []Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Image, len(s.images))
	copy(out, s.images)
	return out
}

func (s *Server) containerByID(id string) (Container, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.containers {
		if c.ID == id || strings.HasPrefix(c.ID, id) {
			return c, true
		}
	}
	return Container{}, false
}

func (s *Server) statsFor(id string) (Stats, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stats[id]
	return st, ok
}

func (s *Server) logsFor(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logs[id]
}

// HostConfig returns the network mode for the fake, kept here so the
// Container type stays free of engine-specific plumbing.
// EffectiveNetworkMode is the host config network mode reported on the
// list endpoint. The real engine includes it in every /containers/json
// entry. Named differently from the NetworkMode field because Go forbids
// a method and a field sharing a name.
func (c Container) EffectiveNetworkMode() string {
	if c.NetworkMode != "" {
		return c.NetworkMode
	}
	return "default"
}

// SampleContainer builds a container that looks like a running web server.
func SampleContainer(id, name, image string) Container {
	return Container{
		ID:      id,
		Names:   []string{"/" + name},
		Image:   image,
		ImageID: "sha256:" + id,
		Command: "nginx -g daemon off;",
		Created: time.Now().Add(-3 * time.Hour).Unix(),
		State:   "running",
		Status:  "Up 3 hours (healthy)",
		Ports: []Port{
			{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
		},
		Labels: map[string]string{"com.docker.compose.project": "demo"},
	}
}

// SampleStoppedContainer builds a container that is not running.
func SampleStoppedContainer(id, name, image string) Container {
	c := SampleContainer(id, name, image)
	c.State = "exited"
	c.Status = "Exited (0) 2 hours ago"
	c.Ports = nil
	return c
}

// ParsePort parses a "host:container" port pair.
func ParsePort(host, container int) Port {
	return Port{IP: "0.0.0.0", PrivatePort: container, PublicPort: host, Type: "tcp"}
}

// AtUnix returns a unix timestamp for n seconds ago.
func AtUnix(n int64) int64 { return time.Now().Add(-time.Duration(n) * time.Second).Unix() }

// Itoa is a small helper for building ids in tests.
func Itoa(n int) string { return strconv.Itoa(n) }
