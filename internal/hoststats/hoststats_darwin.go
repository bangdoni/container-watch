//go:build darwin

package hoststats

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const platformSource = "sysctl"

// readCPUTimes reads the cumulative `kern.cp_time` counters. They were removed
// from the kernel on macOS 15, so callers must tolerate an error here and use
// readCPUPercent instead.
func readCPUTimes() (cpuTimes, error) {
	raw, err := unix.SysctlRaw("kern.cp_time")
	if err != nil {
		return cpuTimes{}, err
	}
	// kern.cp_time is CPUSTATES (5) longs: user, nice, sys, intr, idle.
	var vals []uint64
	switch {
	case len(raw)%8 == 0:
		for i := 0; i+8 <= len(raw); i += 8 {
			vals = append(vals, binary.LittleEndian.Uint64(raw[i:i+8]))
		}
	case len(raw)%4 == 0:
		for i := 0; i+4 <= len(raw); i += 4 {
			vals = append(vals, uint64(binary.LittleEndian.Uint32(raw[i:i+4])))
		}
	default:
		return cpuTimes{}, errors.New("hoststats: unexpected kern.cp_time size")
	}
	if len(vals) < 5 {
		return cpuTimes{}, errors.New("hoststats: short kern.cp_time")
	}
	var total, idle uint64
	for _, v := range vals {
		total += v
	}
	idle = vals[4]
	return cpuTimes{busy: total - idle, total: total}, nil
}

// readCPUPercent is the fallback for hosts without cumulative CPU counters.
// `ps -o %cpu` reports per-process CPU (100 = one full core); summing and
// dividing by the core count yields system-wide utilisation.
func readCPUPercent() (float64, bool) {
	out, err := exec.Command("ps", "-A", "-o", "%cpu=").Output()
	if err != nil {
		return 0, false
	}
	var sum float64
	for _, f := range strings.Fields(string(out)) {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			sum += v
		}
	}
	if n := runtime.NumCPU(); n > 0 {
		sum /= float64(n)
	}
	if sum < 0 {
		sum = 0
	}
	if sum > 100 {
		sum = 100
	}
	return sum, true
}

// readMem reports physical memory and available bytes. The `vm.page_*` sysctls
// are also gone on macOS 15, so `vm_stat` is parsed instead. "Used" mirrors
// Activity Monitor: active + wired + compressed pages.
func readMem() (total, available int64, err error) {
	mem, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	pageSize := uint64(unix.Getpagesize())
	if pageSize == 0 {
		pageSize = 4096
	}

	// Preferred: sysctl counters (older releases).
	active, _ := unix.SysctlUint64("vm.page_active_count")
	wired, _ := unix.SysctlUint64("vm.page_wire_count")
	compressed, _ := unix.SysctlUint64("vm.page_compressor_count")
	usedPages := active + wired + compressed

	if usedPages == 0 {
		// Fallback: parse `vm_stat`.
		if c, ok := readVMStat(); ok {
			usedPages = c.active + c.wired + c.compressed
			pageSize = c.pageSize
		}
	}

	used := int64(usedPages * pageSize)
	if used < 0 || used > int64(mem) {
		used = int64(mem)
	}
	return int64(mem), int64(mem) - used, nil
}

type vmCounts struct {
	pageSize   uint64
	active     uint64
	wired      uint64
	compressed uint64
}

// readVMStat parses the page counters printed by `vm_stat`.
func readVMStat() (vmCounts, bool) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return vmCounts{}, false
	}
	counts := vmCounts{pageSize: 4096}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "page size of") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				if j := strings.Index(rest, " bytes"); j > 0 {
					if v, err := strconv.ParseUint(strings.TrimSpace(rest[:j]), 10, 64); err == nil && v > 0 {
						counts.pageSize = v
					}
				}
			}
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val), ".")), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages active":
			counts.active = n
		case "Pages wired down":
			counts.wired = n
		case "Pages occupied by compressor":
			counts.compressed = n
		}
	}
	if counts.active+counts.wired+counts.compressed == 0 {
		return vmCounts{}, false
	}
	return counts, true
}

// loadAvgScale decodes the fscale field of `struct loadavg { uint32 ldavg[3];
// long fscale; }`. fscale is a C `long`, so it sits at a different offset on
// 32-bit kernels.
func loadAvgScale(raw []byte) uint64 {
	if len(raw) >= 24 {
		if v := binary.LittleEndian.Uint64(raw[16:24]); v > 0 {
			return v
		}
	}
	if len(raw) >= 16 {
		return uint64(binary.LittleEndian.Uint32(raw[12:16]))
	}
	return 0
}

func readLoad() float64 {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 8 {
		return 0
	}
	// struct loadavg { uint32 ldavg[3]; long fscale; }
	scale := loadAvgScale(raw)
	if scale == 0 {
		return 0
	}
	first := binary.LittleEndian.Uint32(raw[0:4])
	return float64(first) / float64(scale)
}
