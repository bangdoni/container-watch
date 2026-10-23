//go:build !darwin && !linux

package hoststats

import "errors"

const platformSource = "unsupported"

var errUnsupported = errors.New("hoststats: host statistics unsupported on this platform")

func readCPUTimes() (cpuTimes, error) { return cpuTimes{}, errUnsupported }

func readMem() (total, available int64, err error) { return 0, 0, errUnsupported }

func readLoad() float64 { return 0 }

// readCPUPercent reports "no instantaneous reading available" so Sample falls
// through to engine totals.
func readCPUPercent() (float64, bool) { return 0, false }
