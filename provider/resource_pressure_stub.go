//go:build !linux

package provider

import (
	"context"
	"time"
)

// Stub implementations of resource_pressure functions for non-Linux platforms.
// These provide safe defaults so that cross-platform code compiles.

const shedBackoff = time.Hour

func currentPressure() float64                           { return 0 }
func detectEffectiveRAMLimitBytes() int64                { return 0 }
func runPressureMonitor(_ context.Context, _ bool)       {}
func runPoolController(_ context.Context, _ int, _ bool) {}
func cleanupIntervalScale(_ float64) float64             { return 1.0 }
func reaperStaleThreshold(_ float64) time.Duration       { return 3 * time.Hour }
func paidStaleThreshold(_ float64) time.Duration         { return 6 * time.Hour }
func scaledProbeConcurrency(_ float64) int               { return 0 }
func fetchStretch(_ float64) float64                     { return 1.0 }
