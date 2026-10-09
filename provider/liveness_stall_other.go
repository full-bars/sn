//go:build !linux

package provider

// recordLivenessStall: the thrash cap bookkeeping is Linux-only here, and so is
// systemd, so there is nothing to record elsewhere.
func recordLivenessStall() {}
