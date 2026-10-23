//go:build !linux

package miner

// thrash_sensors_other.go is the non-Linux half of the swap-thrash sensing.
// There is no /proc and no cgroup v2 memory pressure on darwin, freebsd or
// windows, so the reading carries only what the platform can say: the heap
// against its soft limit. Every memory/swap signal stays unavailable, which
// the watchdog reports as such (never as a zero), and the detection ladder
// stays calm.
func readSwapThrash() swapThrashRead {
	var rd swapThrashRead
	rd.heapFrac, rd.heapUsedMiB, rd.heapLimitMiB, rd.heapOK = swapThrashHeapReading()
	return rd
}
