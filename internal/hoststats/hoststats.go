// Package hoststats samples CPU and memory utilisation for the host running
// ContainerWatch. Linux uses procfs; macOS uses sysctl. Remote hosts fall back
// to engine-reported totals.
package hoststats

import (
	"sync"
	"time"
)

// Sample is one host resource reading.
type Sample struct {
	CPUPercent   float64
	MemTotal     int64
	MemAvailable int64
	MemUsed      int64
	// LoadAvg is the 1-minute load average when available.
	LoadAvg float64
	// Source records where the numbers came from ("proc", "sysctl", "engine").
	Source string
	At     time.Time
}

// MemUsedPercent returns memory in use as a percentage.
func (s Sample) MemUsedPercent() float64 {
	if s.MemTotal <= 0 {
		return 0
	}
	return float64(s.MemUsed) / float64(s.MemTotal) * 100
}

// Collector keeps the previous CPU counters so deltas can be computed.
type Collector struct {
	mu      sync.Mutex
	prev    cpuTimes
	hasPrev bool
}

// New returns a collector for the current host.
func New() *Collector { return &Collector{} }

type cpuTimes struct {
	busy  uint64
	total uint64
}

// Sample computes a host reading. CPU percentage requires two calls; the first
// returns 0% because no previous counters exist yet.
func (c *Collector) Sample() Sample {
	s := Sample{At: time.Now()}

	ct, err := readCPUTimes()
	if err != nil {
		return s
	}
	total, available, merr := readMem()

	c.mu.Lock()
	if c.hasPrev && ct.total > c.prev.total {
		dTot := float64(ct.total - c.prev.total)
		dBusy := float64(ct.busy - c.prev.busy)
		if dBusy < 0 {
			dBusy = 0
		}
		s.CPUPercent = dBusy / dTot * 100
	}
	c.prev = ct
	c.hasPrev = true
	c.mu.Unlock()

	if merr == nil && total > 0 {
		s.MemTotal = total
		s.MemAvailable = available
		s.MemUsed = total - available
		if s.MemUsed < 0 {
			s.MemUsed = 0
		}
	}
	s.LoadAvg = readLoad()
	s.Source = platformSource
	return s
}

// FromEngineTotals derives a sample from engine-reported memory for remote
// hosts where the local procfs/sysctl cannot describe the target machine.
func FromEngineTotals(memTotal, memUsed int64, cpuPercent float64) Sample {
	s := Sample{
		At:           time.Now(),
		MemTotal:     memTotal,
		MemUsed:      memUsed,
		MemAvailable: memTotal - memUsed,
		CPUPercent:   cpuPercent,
		Source:       "engine",
	}
	if s.MemAvailable < 0 {
		s.MemAvailable = 0
	}
	return s
}
