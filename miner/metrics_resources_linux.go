//go:build linux

package miner

import (
	"os"
	"strconv"
	"strings"
)

// readProcRSS reads the process's resident set size from /proc/self/statm
// ("size resident shared ...", in pages). Linux only; the gauge is omitted
// elsewhere.
func readProcRSS() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(data))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}
