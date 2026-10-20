//go:build !linux

package provider

import "time"

func RunSystemAudit(stateDir string, skipDisk bool) (slowDisk bool, lowSpace bool) {
	return false, false
}

func DiskBenchStatus(dir string) (mbps float64, slow bool, age time.Duration, ok bool) {
	return 0, false, 0, false
}

func DiskBenchSkipReason() string { return "not supported on this platform" }
