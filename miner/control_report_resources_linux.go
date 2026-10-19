//go:build linux

package miner

import (
	"os"

	"golang.org/x/sys/unix"
)

// controlReportOpenFDs returns the process's open file descriptor count and
// its RLIMIT_NOFILE soft limit. ok is false when either cannot be read, so
// the snapshot omits both rather than reporting a zero.
func controlReportOpenFDs() (open int, limit int, ok bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, 0, false
	}
	// The directory handle itself is one of the entries; it is still an open
	// descriptor at this instant, so the count is honest as read.
	open = len(entries)

	var rlimit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rlimit); err != nil {
		return open, 0, false
	}
	if rlimit.Cur > uint64(^uint(0)>>1) {
		limit = int(^uint(0) >> 1)
	} else {
		limit = int(rlimit.Cur)
	}
	return open, limit, true
}
