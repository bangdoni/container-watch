package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// readLimited reads up to n bytes from r.
func readLimited(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

// Stats is a normalised container resource sample.
type Stats struct {
	// CPUPercent is total CPU usage across all cores (0..N*100).
	CPUPercent float64
	// MemUsed / MemLimit are in bytes. MemLimit reports the cgroup limit, which
	// may equal the host memory when no limit is set.
	MemLimit int64
	MemUsed  int64
	// MemCache is reclaimable page cache, excluded from "working set" usage.
	MemCache int64
	// NetRx / NetTx are cumulative bytes.
	NetRx uint64
	NetTx uint64
	// BlockRead / BlockWrite are cumulative bytes.
	BlockRead  uint64
	BlockWrite uint64
	// PIDs is the current process count.
	PIDs uint64
	// OnlineCPUs is the cpu count the engine reports for quota maths.
	OnlineCPUs uint32
	// CPUUsageTotal and SystemUsage are the raw cumulative counters used to
	// derive CPU% between two samples.
	CPUUsageTotal uint64
	SystemUsage   uint64
}

// MemUsedPercent returns memory usage as a percentage of the limit.
func (s *Stats) MemUsedPercent() float64 {
	if s.MemLimit <= 0 {
		return 0
	}
	used := s.MemUsed
	if s.MemCache > 0 && s.MemCache < used {
		used -= s.MemCache
	}
	if used < 0 {
		used = 0
	}
	return float64(used) / float64(s.MemLimit) * 100
}

// MemWorkingSet returns memory usage minus reclaimable cache.
func (s *Stats) MemWorkingSet() int64 {
	if s.MemCache > 0 && s.MemCache < s.MemUsed {
		return s.MemUsed - s.MemCache
	}
	return s.MemUsed
}

type cpuUsage struct {
	TotalUsage        uint64   `json:"total_usage"`
	UsageInKernelmode uint64   `json:"usage_in_kernelmode"`
	UsageInUsermode   uint64   `json:"usage_in_usermode"`
	PercpuUsage       []uint64 `json:"percpu_usage"`
}

type cpuStats struct {
	CPUUsage       cpuUsage `json:"cpu_usage"`
	SystemUsage    uint64   `json:"system_cpu_usage"`
	OnlineCPUs     uint32   `json:"online_cpus"`
	ThrottlingData struct {
		Periods          uint64 `json:"periods"`
		ThrottledPeriods uint64 `json:"throttled_periods"`
		ThrottledTime    uint64 `json:"throttled_time"`
	} `json:"throttling_data"`
}

type memoryStats struct {
	Usage    uint64            `json:"usage"`
	Limit    uint64            `json:"limit"`
	MaxUsage uint64            `json:"max_usage"`
	Stats    map[string]uint64 `json:"stats"`
}

// statsResponse covers both the cgroup v1 and v2 JSON shapes emitted by Docker
// and Podman. Podman additionally returns a top-level Error field.
type statsResponse struct {
	Read        string      `json:"read"`
	Preread     string      `json:"preread"`
	CPUStats    cpuStats    `json:"cpu_stats"`
	PreCPUStats cpuStats    `json:"precpu_stats"`
	MemoryStats memoryStats `json:"memory_stats"`
	PidsStats   struct {
		Current uint64 `json:"current"`
		Limit   uint64 `json:"limit"`
	} `json:"pids_stats"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Major uint64 `json:"major"`
			Minor uint64 `json:"minor"`
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	Error string `json:"error"`
}

// OneShotStatsSample is the cached data needed to compute a second delta-based
// sample (the engine needs two reads to compute CPU%).
type OneShotStatsSample struct {
	TotalUsage  uint64
	SystemUsage uint64
	PreReadTime int64
	ReadTime    int64
}

// StatsStreamSample is delivered by StreamStats.
type StatsStreamSample struct {
	Stats
	Timestamp int64
}

// Stats fetches one stats snapshot. CPU% is computed from the engine's
// precpu_stats when available; otherwise it is zero for the first call and
// callers should use StreamStats for accurate figures.
func (c *Client) Stats(ctx context.Context, id string) (*Stats, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	req, err := c.newRequest(ctx, http.MethodGet, "/containers/"+id+"/stats", q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := readLimited(resp.Body, 512)
		return nil, errors.New("engine: stats " + id + ": " + resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	var sr statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	if sr.Error != "" {
		return nil, errors.New("engine: stats: " + sr.Error)
	}
	return normaliseStats(&sr), nil
}

// normaliseStats converts an engine stats payload into a Stats sample.
func normaliseStats(sr *statsResponse) *Stats {
	s := &Stats{}

	// ---- CPU ----
	cpuDelta := float64(sr.CPUStats.CPUUsage.TotalUsage) - float64(sr.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(sr.CPUStats.SystemUsage) - float64(sr.PreCPUStats.SystemUsage)
	cpus := float64(sr.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(sr.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = float64(sr.PreCPUStats.OnlineCPUs)
	}
	if cpus == 0 {
		cpus = 1
	}
	s.OnlineCPUs = uint32(cpus)
	if sysDelta > 0 && cpuDelta > 0 {
		s.CPUPercent = (cpuDelta / sysDelta) * cpus * 100.0
	}
	s.CPUUsageTotal = sr.CPUStats.CPUUsage.TotalUsage
	s.SystemUsage = sr.CPUStats.SystemUsage
	if s.CPUPercent < 0 || s.CPUPercent > cpus*100 {
		// Guard against counter resets (container restarted mid-stream).
		if s.CPUPercent < 0 {
			s.CPUPercent = 0
		}
	}

	// ---- Memory ----
	s.MemUsed = int64(sr.MemoryStats.Usage)
	s.MemLimit = int64(sr.MemoryStats.Limit)
	// cgroup v1 exposes cache/active_file; v2 exposes inactive_file.
	for _, key := range []string{"cache", "inactive_file", "total_inactive_file", "active_file"} {
		if v, ok := sr.MemoryStats.Stats[key]; ok && v > 0 {
			s.MemCache = int64(v)
			break
		}
	}
	if s.MemCache > s.MemUsed {
		s.MemCache = 0
	}

	// ---- PIDs ----
	s.PIDs = sr.PidsStats.Current

	// ---- Network (sum of all interfaces) ----
	for _, n := range sr.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}

	// ---- Block I/O ----
	for _, b := range sr.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(b.Op) {
		case "read":
			s.BlockRead += b.Value
		case "write":
			s.BlockWrite += b.Value
		}
	}
	return s
}

// StreamStats opens a streaming stats connection. The returned channel is
// closed when the stream ends or ctx is cancelled. The error return covers
// connection failures only.
func (c *Client) StreamStats(ctx context.Context, id string) (<-chan StatsStreamSample, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	q := url.Values{"stream": {"true"}}
	req, err := c.newRequest(ctx, http.MethodGet, "/containers/"+id+"/stats", q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.New("engine: stream stats " + id + ": " + resp.Status)
	}

	out := make(chan StatsStreamSample, 4)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var sr statsResponse
			if err := dec.Decode(&sr); err != nil {
				return
			}
			if sr.Error != "" {
				return
			}
			ts := parseTimeMillis(sr.Read)
			select {
			case out <- StatsStreamSample{Stats: *normaliseStats(&sr), Timestamp: ts}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func parseTimeMillis(s string) int64 {
	// The engine emits RFC3339Nano timestamps like 2026-10-22T10:00:00.123456789Z.
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// -------------------------------------------------------- demux helper ----
// Docker's log endpoints use an 8-byte header framing when TTY is disabled:
// [stream byte][0][0][0][uint32 big-endian length][payload].
// A byte value of 0/1/2 marks stream type; other values mean the payload is
// raw (Podman sometimes streams unwrapped data).
const frameHeaderSize = 8

// demuxReader splits a multiplexed engine log stream into payload bytes.
type demuxReader struct {
	r       io.Reader
	pending []byte
	err     error
}

func newDemuxReader(r io.Reader) *demuxReader { return &demuxReader{r: r} }

func (d *demuxReader) Read(p []byte) (int, error) {
	for len(d.pending) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		var header [frameHeaderSize]byte
		if _, err := io.ReadFull(d.r, header[:]); err != nil {
			d.err = err
			return 0, err
		}
		stream := header[0]
		size := binary.BigEndian.Uint32(header[4:])
		if stream > 2 || size > 32<<20 {
			// Not a framed stream: treat the bytes as raw payload.
			d.pending = append(d.pending, header[:]...)
			buf := make([]byte, 32<<10)
			n, err := d.r.Read(buf)
			if n > 0 {
				d.pending = append(d.pending, buf[:n]...)
			}
			if err != nil {
				d.err = err
			}
			break
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(d.r, payload); err != nil {
			d.err = err
			return 0, err
		}
		d.pending = append(d.pending, payload...)
	}
	n := copy(p, d.pending)
	d.pending = d.pending[n:]
	return n, nil
}
