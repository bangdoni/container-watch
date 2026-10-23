//go:build linux

package hoststats

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

const platformSource = "proc"

func readCPUTimes() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 4 {
			return cpuTimes{}, errors.New("hoststats: malformed cpu line")
		}
		var vals []uint64
		for _, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, err
			}
			vals = append(vals, v)
		}
		var total uint64
		for _, v := range vals {
			total += v
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4] // iowait counts as idle for utilisation purposes
		}
		return cpuTimes{busy: total - idle, total: total}, nil
	}
	return cpuTimes{}, errors.New("hoststats: no aggregate cpu line in /proc/stat")
}

func readMem() (total, available int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var cached, free, buffers int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, perr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = v * 1024
		case "MemAvailable":
			available = v * 1024
		case "MemFree":
			free = v * 1024
		case "Buffers":
			buffers = v * 1024
		case "Cached":
			cached = v * 1024
		}
	}
	if available == 0 {
		available = free + buffers + cached
	}
	if total == 0 {
		return 0, 0, errors.New("hoststats: no MemTotal in /proc/meminfo")
	}
	return total, available, nil
}

func readLoad() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// readCPUPercent is unused on Linux: /proc/stat provides cumulative counters,
// so Sample derives the percentage from deltas.
func readCPUPercent() (float64, bool) { return 0, false }
