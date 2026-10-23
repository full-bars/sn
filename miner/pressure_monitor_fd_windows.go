//go:build windows

package miner

// readFDFrac is unavailable on Windows: there is no RLIMIT_NOFILE or
// /proc/self/fd equivalent, so the FD component of the pressure score fails
// open and the remaining sensors score alone.
func readFDFrac() float64 { return -1 }
