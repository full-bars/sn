//go:build !windows

package miner

import (
	"os"
	"syscall"
)

// readFDFrac returns the fraction of the FD budget the process is using
// (open FDs / RLIMIT_NOFILE), or -1 if it cannot be determined. It counts the
// entries in /proc/self/fd and divides by the soft RLIMIT_NOFILE, so on hosts
// without procfs (macOS, FreeBSD unless mounted) this reports -1 and the FD
// component fails open.
func readFDFrac() float64 {
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		return -1
	}
	limit := rLimit.Cur
	if limit == 0 {
		return -1
	}
	fdDir, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	open := len(fdDir)
	frac := float64(open) / float64(limit)
	if frac > 1 {
		frac = 1
	}
	return frac
}
