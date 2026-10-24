package miner

import "fmt"

// memory_headroom.go watches real free memory and logs when it runs short and
// when it recovers. It is observation only: it changes nothing. The pressure
// score is not a usable signal for this (it read a fixed 1.00 on a healthy box
// and 0.00 on a box at 54% memory PSI), and connected clients, not just
// proxies, drive memory (about 0.7-0.9 MiB each), so the box can get short
// with no change in its proxy count.
//
// Ported from the v33 provider's provider/memory_headroom.go: same thresholds,
// same hysteresis, same lines. It matters more here than in the relay: a miner
// holds a provider per identity, H3 roughly doubles a proxy's footprint, and
// H3 is admission-controlled per identity precisely because of that. This is
// the other half of that guard -- know before an allocation spike pushes the
// box into swap, which is where the whole pool starts thrashing.
//
// It reads free memory through the pressure monitor's hostAvailMiB (the tighter
// of host MemAvailable and cgroup headroom), so the guard and the pressure
// fraction share one definition of how much room the box has. They do not share
// one sample: the tick reads hostAvailMiB for the guard, then the pressure
// sampler reads it again a moment later, so a box in rapid flux can show the
// guard and the fraction two slightly different numbers. Only the definition is
// common, not the reading.

const (
	headroomLowFloorMiB = 150
	headroomLowCapMiB   = 400
	headroomEnterCount  = 2 // consecutive low samples to announce
	headroomLeaveCount  = 4 // consecutive clear samples to announce recovery
)

// headroomLowThresholdMiB is the free-memory level below which a box is short:
// 10% of RAM, never below 150 MiB nor above 400 MiB.
func headroomLowThresholdMiB(ramMiB int64) int64 {
	return min(max(ramMiB/10, headroomLowFloorMiB), headroomLowCapMiB)
}

// headroomLowForRAMLimit is headroomLowThresholdMiB for an effective RAM limit
// in bytes, with the unreadable case handled explicitly: it takes the CAP, not
// the floor.
//
// The floor is 10% of a known-small box, so it is only correct when a small
// limit was actually detected. An unreadable limit is not evidence of a small
// box -- it is no evidence at all -- and shifting it (0 >> 20 = 0) into the
// threshold would clamp to 150 MiB. An 8 GiB host whose detection failed would
// then never report at 350 or 200 MiB free, which is well into swap: exactly the
// failure this guard exists to catch. The cap is the conservative choice for an
// unknown host, since it is the highest threshold the guard ever uses and so
// warns earliest.
func headroomLowForRAMLimit(ramBytes int64) int64 {
	if ramBytes <= 0 {
		return headroomLowCapMiB
	}
	return headroomLowThresholdMiB(ramBytes >> 20)
}

// headroomTracker is the announce/recover state machine with hysteresis:
// entering takes two consecutive low samples; leaving takes four consecutive
// samples above the threshold by a 25% margin, so a reading hovering at the
// line does not flap.
type headroomTracker struct {
	low          bool
	below, clear int
}

// Observe feeds one free-memory reading (MiB; negative = unknown, ignored) and
// returns "low" or "recovered" on a transition, else "".
func (h *headroomTracker) Observe(availMiB, lowMiB int64) string {
	if availMiB < 0 {
		return ""
	}
	if !h.low {
		if availMiB < lowMiB {
			h.below++
			if h.below >= headroomEnterCount {
				h.low, h.below, h.clear = true, 0, 0
				return "low"
			}
		} else {
			h.below = 0
		}
		return ""
	}
	if availMiB*4 >= lowMiB*5 {
		h.clear++
		if h.clear >= headroomLeaveCount {
			h.low, h.below, h.clear = false, 0, 0
			return "recovered"
		}
	} else {
		h.clear = 0
	}
	return ""
}

// headroomLogLine formats the transition, or "" when there is none.
func headroomLogLine(transition string, availMiB, lowMiB int64, proxies, goroutines int) string {
	switch transition {
	case "low":
		return fmt.Sprintf("[proxy][resources] low memory headroom: %d MiB available, below %d MiB (%d proxies, %d goroutines); connected clients drive memory, so this can happen without any change in the proxy count. Consider `urnet-tools proxy trim` if it persists",
			availMiB, lowMiB, proxies, goroutines)
	case "recovered":
		return fmt.Sprintf("[proxy][resources] memory headroom recovered: %d MiB available (%d proxies, %d goroutines)", availMiB, proxies, goroutines)
	}
	return ""
}

// headroomProxyAddresses is runningProxyAddresses behind a seam, so a test can
// prove the count is only taken on a transition. Enumeration copies and sorts
// the whole running set under the bandwidth registry lock, which on a
// multi-thousand-proxy node is far too much work for a tick that logs nothing.
var headroomProxyAddresses = runningProxyAddresses

// headroomProxyCount is the pool size the headroom line reports: the running
// set minus the native direct transport. The provider excludes direct for the
// same reason (a single fixed goroutine, not a pool member), so a direct-only
// node does not log "1 proxies".
func headroomProxyCount() int {
	n := 0
	for _, key := range headroomProxyAddresses() {
		if key == directProxyKey {
			continue
		}
		n++
	}
	return n
}
