//go:build !linux

package miner

import "time"

// Non-Linux stub for the startup disk audit. The relay has no portable
// statfs/write-benchmark pair it trusts, so every target outside Linux reports
// nothing rather than a fabricated number: the metrics seam then stays silent
// and the startup audit measures no disk. This keeps every cross-build (darwin,
// freebsd, windows) compiling against the same miner code.

func CheckDiskSpace(dir string) (int64, bool) { return 0, false }

func RunDiskAudit(dir string) (slowDisk bool, lowSpace bool) { return false, false }

func DiskBenchStatus(dir string) (mbps float64, slow bool, age time.Duration, ok bool) {
	return 0, false, 0, false
}

func DiskBenchSkipReason() string { return "not supported on this platform" }
