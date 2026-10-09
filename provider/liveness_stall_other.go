//go:build !linux

package provider

// systemd and the thrash restart ring are Linux-only here: there is nothing to
// gate or record elsewhere, and the watchdog is never enabled off Linux.
func livenessGate(episodeStart bool) (bool, string) { return false, "not supported on this platform" }

func recordLivenessStall() func() { return nil }
