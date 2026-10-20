//go:build !linux

// Retain the existing non-Linux descendant assertion without Linux prctl.
package main

import (
	"testing"
	"time"
)

func runSupervisorDescendantOwnerTest(t *testing.T, surviving int, timeout time.Duration, run func(*testing.T)) {
	t.Helper()
	run(t)
}
