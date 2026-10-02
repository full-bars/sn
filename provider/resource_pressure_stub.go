//go:build !linux

package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/urfoundation/sn/internal/connectx"
)

// Stub implementations of resource_pressure functions for non-Linux platforms.
// These provide safe defaults so that cross-platform code compiles.

const shedBackoff = time.Hour
const paidStaleCalm = 6 * time.Hour

func currentPressure() float64 { return 0 }

// detectEffectiveRAMLimitBytes is the host's physical RAM where the platform
// reader supports it (macOS, Windows), else 0 for "unmeasured". There is no
// cgroup to consult off Linux.
func detectEffectiveRAMLimitBytes() int64 {
	if v, ok := connectx.HostMemoryTotalBytes(); ok {
		return v
	}
	return 0
}

const pressureLoopsSupported = false

func runPressureMonitor(_ context.Context, _ bool)       {}
func runPoolController(_ context.Context, _ int, _ bool) {}
func cleanupIntervalScale(_ float64) float64             { return 1.0 }
func reaperStaleThreshold(_ float64) time.Duration       { return 3 * time.Hour }
func paidStaleThreshold(_ float64) time.Duration         { return 6 * time.Hour }
func scaledProbeConcurrency(_ float64) int               { return 0 }
func fetchStretch(_ float64) float64                     { return 1.0 }

// applyShedBackoff keeps a shed proxy down for at least shedBackoff from now.
// Mirrors the portable definition in resource_pressure.go; the Linux file pads
// its list of suspended platform reads with this same short-circuit.
func applyShedBackoff(addr string, now time.Time) {
	globalProxyFailureHistory.ExtendBackoffUntil(addr, now.Add(shedBackoff))
}

// The baseline recorder reads these on every platform. Off Linux there is no
// /proc, cgroup or PSI to read, so each reports "unmeasured" the way the Linux
// readers do on error (-1 or an error), and the recorder omits those fields
// rather than writing a zero.
var errNoHostReadings = errors.New("host readings are not available on this platform")

func readMemAvailableMiB() int64 {
	if v, ok := connectx.HostMemoryAvailableBytes(); ok {
		return v / 1024 / 1024
	}
	return -1
}

func readCgroupAvailableMiB() int64                    { return -1 }
func readPSI(_ string) (avg60 float64, err error)      { return 0, errNoHostReadings }
func getSystemLoad() (load1, load5 float64, err error) { return 0, 0, errNoHostReadings }

// gcTightening and readGOGCPercent back the baseline recorder's GC signals,
// which live in files that build everywhere. There is no GC governor off Linux,
// so the flag is never set and the GOGC reading is reported as unavailable,
// which omits the gc block instead of writing zeros.
var gcTightening atomic.Bool

func readGOGCPercent() (int, bool) { return 0, false }
