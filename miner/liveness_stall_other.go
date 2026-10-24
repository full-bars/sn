//go:build !linux

package miner

// The stall gate and the lean start cap ride the thrash cap and the Linux-only
// self-exit path; on the other platforms only the systemd watchdog feed exists,
// and it never withholds the ping without a gate that can say yes.

func livenessGate(episodeStart bool) (bool, string) { return false, "not supported on this platform" }

func recordLivenessStall() func() { return nil }
