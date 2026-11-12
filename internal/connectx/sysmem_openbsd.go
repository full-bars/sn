package connectx

import "golang.org/x/sys/unix"

// OpenBSD's available-memory counters are not currently supported. If the
// width of hw.physmem differs from uint64, the total is also unknown.
func HostMemoryTotalBytes() (int64, bool) {
	v, err := unix.SysctlUint64("hw.physmem")
	if err != nil || v == 0 || v > uint64(1<<63-1) {
		return 0, false
	}
	return int64(v), true
}

func HostMemoryAvailableBytes() (int64, bool) { return 0, false }
