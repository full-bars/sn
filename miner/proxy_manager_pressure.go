package miner

import (
	"math"
	"time"
)

// This file ports the pure pacing math of the v33 provider's resource-pressure
// module (resource_pressure.go) that the URL fetch/reaper/cleanup loops use.
// The pressure MONITOR (CPU/IO sampling, AIMD pool controller, self-heal fast
// paths) is P2/P1(c): until it is wired, currentPressure() reports calm, so
// every pacing function degenerates to its calm-box behavior exactly as it
// would on an idle 3.23-fix box. The ported tests pin the ramp shapes.

const (
	// fetchStretchMax is the ceiling on how far pressure can stretch the URL
	// fetch interval (8x base at full pressure).
	fetchStretchStart = 0.3
	fetchStretchFull  = 0.9
	fetchStretchMax   = 8.0
)

// normalizeRamp maps v into 0..1 across lo..hi.
func normalizeRamp(v, lo, hi float64) float64 {
	if lo == hi {
		return 0
	}
	t := (v - lo) / (hi - lo)
	if t < 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

// currentPressure is this box's normalized pressure (0..1). P1(a) has no
// pressure monitor yet, so it reports calm; P2 installs the sampler behind
// this same function.
func currentPressure() float64 { return 0 }

// fetchStretch maps pressure to a fetch-interval multiplier: 1x while calm,
// growing linearly to fetchStretchMax at fetchStretchFull.
func fetchStretch(pressure float64) float64 {
	t := normalizeRamp(pressure, fetchStretchStart, fetchStretchFull)
	return 1.0 + t*(fetchStretchMax-1.0)
}

// scaledProbeConcurrency shrinks the probe worker pool as pressure rises.
// Probe bursts are the provider's main self-generated load spike; the floor
// of 1 keeps the reaper/fetch pipelines draining even at full pressure.
func scaledProbeConcurrency(pressure float64) int {
	n := int(math.Round(float64(proxyProbeConcurrency) * (1 - pressure)))
	if n < 1 {
		return 1
	}
	return n
}

const (
	cleanupScaleStart = 0.3
	cleanupScaleFull  = 0.8
	cleanupScaleMin   = 1.0 / 6.0 // 6h base to 1h at full pressure

	reaperStaleCalm = 3 * time.Hour // matches the pre-pressure fixed value
	reaperStaleHot  = 1 * time.Hour
)

// cleanupIntervalScale shortens the cleanup cadence under pressure: cleanup
// sheds load, so overload is when it should run MORE often.
func cleanupIntervalScale(pressure float64) float64 {
	t := normalizeRamp(pressure, cleanupScaleStart, cleanupScaleFull)
	return 1.0 - t*(1.0-cleanupScaleMin)
}

// reaperStaleThreshold shrinks the once-good re-probe window under pressure:
// when the box is drowning, finding dead weight faster matters more.
func reaperStaleThreshold(pressure float64) time.Duration {
	t := normalizeRamp(pressure, cleanupScaleStart, cleanupScaleFull)
	return reaperStaleCalm - time.Duration(t*float64(reaperStaleCalm-reaperStaleHot))
}
