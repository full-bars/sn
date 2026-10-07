//go:build !linux

package provider

import (
	"context"
	"sync/atomic"
)

// Stub for non-Linux platforms: there is no /proc, no cgroup and no PSI to
// read, so the thrash watchdog has nothing to sense and nothing to do. The
// thrash cap file plumbing (thrash_cap.go) stays portable because
// effectiveTrimCapSource consults it on every platform.
func runThrashWatchdog(_ context.Context, _ bool) {}

// thrashFreeze is referenced by provide.go's supervision wiring on every
// platform; off Linux it never becomes true (the watchdog is a no-op) and the
// pool controller that reads it is a no-op stub too.
var thrashFreeze atomic.Bool

// thrashStateName keeps the pressure_status writer compiling where the
// watchdog never runs.
func thrashStateName() string { return "" }

func thrashSummaryForStatus() string { return "" }
