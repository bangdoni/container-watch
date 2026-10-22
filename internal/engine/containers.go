package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Port is one published/exposed port mapping from the container list API.
type Port struct {
	IP          string `json:"IP"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Mount is a volume or bind mount.
type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
	Driver      string `json:"Driver"`
}

// Container is a row from GET /containers/json.
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
	// NetworkMode is present on newer API versions.
	NetworkMode string `json:"NetworkMode"`
	// HostConfig.NetworkMode is present on the list endpoint in some versions.
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
}

// Name returns the primary container name without the leading slash.
func (c *Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// ShortID returns the 12-character id used in the UI.
func (c *Container) ShortID() string { return c.ID[:min(12, len(c.ID))] }

// Health extracts the health check verdict from the human status string, e.g.
// "Up 3 days (healthy)". Returns "" when the container defines no healthcheck.
func (c *Container) Health() string {
	if i := strings.LastIndex(c.Status, "("); i >= 0 && strings.HasSuffix(c.Status, ")") {
		inner := strings.TrimSuffix(c.Status[i+1:], ")")
		switch inner {
		case "healthy", "unhealthy", "health: starting":
			return strings.TrimPrefix(inner, "health: ")
		}
	}
	if h, ok := c.Labels["com.docker.compose.project"]; ok && h != "" {
		return ""
	}
	return ""
}

// Uptime returns the human readable runtime portion of the status string, e.g.
// "3 days" for "Up 3 days (healthy)". Empty for stopped containers.
func (c *Container) Uptime() string {
	if !strings.HasPrefix(c.Status, "Up ") {
		return ""
	}
	s := strings.TrimPrefix(c.Status, "Up ")
	if i := strings.Index(s, " ("); i >= 0 {
		s = s[:i]
	}
	return s
}

// Network returns the primary network name.
func (c *Container) Network() string {
	if c.NetworkMode != "" && c.NetworkMode != "default" {
		return c.NetworkMode
	}
	if c.HostConfig.NetworkMode != "" {
		return c.HostConfig.NetworkMode
	}
	return ""
}

// ListContainers returns every container, running or not.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var out []Container
	if err := c.getJSON(ctx, "/containers/json", &out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Labels == nil {
			out[i].Labels = map[string]string{}
		}
	}
	return out, nil
}

// PortBinding is a host binding for a container port.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// HealthLog is one healthcheck execution.
type HealthLog struct {
	Start    string `json:"Start"`
	End      string `json:"End"`
	ExitCode int    `json:"ExitCode"`
	Output   string `json:"Output"`
}

// Inspect is the subset of GET /containers/{id}/json the UI consumes.
type Inspect struct {
	ID      string   `json:"Id"`
	Name    string   `json:"Name"`
	Created string   `json:"Created"`
	Path    string   `json:"Path"`
	Args    []string `json:"Args"`
	State   struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		Dead       bool   `json:"Dead"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status        string      `json:"Status"`
			FailingStreak int         `json:"FailingStreak"`
			Log           []HealthLog `json:"Log"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Hostname     string              `json:"Hostname"`
		User         string              `json:"User"`
		Image        string              `json:"Image"`
		Tty          bool                `json:"Tty"`
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		WorkingDir   string              `json:"WorkingDir"`
		Labels       map[string]string   `json:"Labels"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	HostConfig struct {
		Memory        int64  `json:"Memory"`
		NanoCPUs      int64  `json:"NanoCpus"`
		CPUShares     int64  `json:"CpuShares"`
		CPUQuota      int64  `json:"CpuQuota"`
		Privileged    bool   `json:"Privileged"`
		NetworkMode   string `json:"NetworkMode"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		PortBindings map[string][]PortBinding `json:"PortBindings"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		IPAddress string                   `json:"IPAddress"`
		Gateway   string                   `json:"Gateway"`
		Ports     map[string][]PortBinding `json:"Ports"`
		Networks  map[string]struct {
			IPAddress  string `json:"IPAddress"`
			Gateway    string `json:"Gateway"`
			MacAddress string `json:"MacAddress"`
			NetworkID  string `json:"NetworkID"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// InspectContainer fetches full container metadata.
func (c *Client) InspectContainer(ctx context.Context, id string) (*Inspect, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	var out Inspect
	if err := c.getJSON(ctx, "/containers/"+id+"/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Action is a supported lifecycle operation.
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionPause   Action = "pause"
	ActionUnpause Action = "unpause"
	ActionKill    Action = "kill"
)

// Do performs a lifecycle action. A stop/restart waits up to 10 seconds for a
// clean shutdown before the engine sends SIGKILL.
func (c *Client) Do(ctx context.Context, id string, action Action) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	q := url.Values{}
	switch action {
	case ActionStart, ActionPause, ActionUnpause:
	case ActionStop, ActionRestart:
		q.Set("t", "10")
	case ActionKill:
		q.Set("signal", "SIGTERM")
	default:
		return fmt.Errorf("engine: unsupported action %q", action)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/containers/"+id+"/"+string(action), q, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("engine: %s %s: %w", action, id, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusNotModified:
		// Already in the requested state.
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	default:
		body, _ := readLimited(resp.Body, 1024)
		return fmt.Errorf("engine: %s %s: %s: %s", action, id, resp.Status, strings.TrimSpace(string(body)))
	}
}

// StartedAt parses the inspect start timestamp.
func (i *Inspect) StartedAt() time.Time {
	t, err := time.Parse(time.RFC3339Nano, i.State.StartedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// HealthStatus returns the health verdict, or "" when none is configured.
func (i *Inspect) HealthStatus() string {
	if i.State.Health == nil {
		return ""
	}
	return i.State.Health.Status
}

// CPUQuotaPercent returns the configured CPU limit as a percentage of one core,
// or 0 when unlimited.
func (i *Inspect) CPUQuotaPercent() float64 {
	if i.HostConfig.NanoCPUs > 0 {
		return float64(i.HostConfig.NanoCPUs) / 1e9 * 100
	}
	if i.HostConfig.CPUQuota > 0 {
		return float64(i.HostConfig.CPUQuota) / 1000
	}
	return 0
}

// CommandString renders the container's command for display.
func (i *Inspect) CommandString() string {
	parts := append(append([]string{}, i.Config.Entrypoint...), i.Config.Cmd...)
	return strings.Join(parts, " ")
}

// SortedPorts returns published ports sorted by public port number.
func (i *Inspect) SortedPorts() []struct {
	Container string
	Host      string
} {
	type row struct{ container, host string }
	var rows []row
	for containerPort, bindings := range i.NetworkSettings.Ports {
		if len(bindings) == 0 {
			rows = append(rows, row{containerPort, ""})
			continue
		}
		for _, b := range bindings {
			rows = append(rows, row{containerPort, b.HostIP + ":" + b.HostPort})
		}
	}
	sort.Slice(rows, func(a, b int) bool {
		return portNumber(rows[a].container) < portNumber(rows[b].container)
	})
	out := make([]struct {
		Container string
		Host      string
	}, len(rows))
	for idx, r := range rows {
		out[idx].Container = r.container
		out[idx].Host = r.host
	}
	return out
}

func portNumber(p string) int {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	n, _ := strconv.Atoi(p)
	return n
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
