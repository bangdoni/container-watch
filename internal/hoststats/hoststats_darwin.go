//go:build darwin

package hoststats

import (
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

const platformSource = "sysctl"

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

func readMem() (total, available int64, err error) {
	mem, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	pageSize, err := unix.SysctlUint64("hw.pagesize")
	if err != nil || pageSize == 0 {
		pageSize = 4096
	}
	active, _ := unix.SysctlUint64("vm.page_active_count")
	inactive, _ := unix.SysctlUint64("vm.page_inactive_count")
	wired, _ := unix.SysctlUint64("vm.page_wire_count")
	compressed, _ := unix.SysctlUint64("vm.page_compressor_count")
	free, _ := unix.SysctlUint64("vm.page_free_count")

	usedPages := active + wired + compressed
	if usedPages == 0 {
		// Older kernels lack page counts; fall back to total-free.
		usedPages = mem/pageSize - free
	}
	used := int64(usedPages * pageSize)
	if used > int64(mem) {
		used = int64(mem)
	}
	_ = inactive
	return int64(mem), int64(mem) - used, nil
}

func readLoad() float64 {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 8 {
		return 0
	}
	// struct loadavg { uint32 ldavg[3]; long fscale; }
	scale := binary.LittleEndian.Uint64(raw[len(raw)-8:])
	if scale == 0 {
		return 0
	}
	first := binary.LittleEndian.Uint32(raw[0:4])
	return float64(first) / float64(scale)
}
