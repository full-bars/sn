package miner

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// This file ports the pure pacing math of the v33 provider's resource-pressure
// module (resource_pressure.go) that the URL fetch/reaper/cleanup loops use,
// plus the AIMD pool controller T-07: the loop that discovers and PERSISTS the
// URL pool's TargetPoolSize (proxy_url.json), the regrowth ceiling that stops
// the loop sawtoothing after a pressure cut, and the shed path that returns
// over-target URL proxies to the cache.
//
// The pressure MONITOR itself (CPU/IO sampling, the GC governor, the thrash
// watchdog) is still P2: nothing writes the pressure atomics yet, so
// currentPressure() reports calm and every pressure-driven actuator
// degenerates to its calm-box behavior exactly as it would on an idle
// 3.23-fix box. The controller is deliberately complete against that seam:
// when the monitor lands it starts writing setPressure/setPressureNoCPU and
// the shrink path activates with no further porting.
//
// The ported tests pin the ramp shapes, the ceiling behavior and the
// controller's persistence.

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

// globalPressure is the actuating normalized pressure (0..1) every self-heal
// actuator reads. The sampler that produces it is the pressure monitor, which
// is P2; until it is wired nothing writes the atomic, so currentPressure()
// reports calm.
var globalPressure atomic.Uint64

// globalPressureNoCPU is the same score with the CPU component excluded. The
// pool controller reads it outside the shrink regime so a busy core cannot pin
// the pool below its ceiling forever (see runPoolController). No writer until
// the monitor lands.
var globalPressureNoCPU atomic.Uint64

// currentPressure is this box's normalized pressure (0..1). P2 installs the
// sampler behind setPressure; with no writer it stays calm.
func currentPressure() float64 { return math.Float64frombits(globalPressure.Load()) }

// setPressure is the pressure monitor's write seam.
func setPressure(v float64) { globalPressure.Store(math.Float64bits(v)) }

// currentPressureNoCPU is the CPU-excluded score the pool controller reads.
func currentPressureNoCPU() float64 { return math.Float64frombits(globalPressureNoCPU.Load()) }

// setPressureNoCPU is the pressure monitor's CPU-excluded write seam.
func setPressureNoCPU(v float64) { globalPressureNoCPU.Store(math.Float64bits(v)) }

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

	paidStaleCalm = 6 * time.Hour
	paidStaleHot  = 3 * time.Hour
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

// paidStaleThreshold is the same shape for the PAID/file-proxy window, which
// is deliberately wider: the operator pays for paid-probe bandwidth, and paid
// proxies are stable by construction, so they should be re-probed far less
// often than URL-sourced ones. Combined with the earn-skip in the paid grader
// (P1(c2)), this drives paid probe spend toward zero in steady state. The grade
// summary reads it for the per-source stale ratio.
func paidStaleThreshold(pressure float64) time.Duration {
	t := normalizeRamp(pressure, cleanupScaleStart, cleanupScaleFull)
	return paidStaleCalm - time.Duration(t*float64(paidStaleCalm-paidStaleHot))
}

// The cleanup/reaper/pool-controller constants below implement the inversion:
// under the old design, load-shedding actuators (cleanup, reaper) ran LESS
// often under pressure, on the theory that pressure means "back off
// everything." But cleanup and the reaper are the actuators that SHED load -
// gating them under pressure is exactly backwards. They run MORE often as
// pressure rises. The AIMD pool controller is the same idea applied
// continuously: discover the sustainable proxy_url pool size per box instead
// of relying on a single operator-set ceiling.
const (
	// AIMD pool controller: additive increase / multiplicative decrease,
	// TCP-congestion style. The configured proxy_url_max is only a ceiling;
	// the operating point is discovered per box.
	aimdIncrement    = 25
	aimdDecreaseMult = 0.7
	aimdFloor        = 50
	aimdGrowBelow    = 0.3  // pressure below this -> grow
	aimdShrinkAbove  = 0.75 // pressure above this (2 consecutive samples) -> shrink
	shedBackoff      = time.Hour

	// aimdCeilingFrac and aimdCeilingDecay shape the regrowth ceiling. After
	// a pressure cut the pool may not regrow past aimdCeilingFrac of the
	// level where pressure appeared; the ceiling then relaxes back to that
	// level over aimdCeilingDecay and is released, so a box whose capacity
	// has changed can rediscover it. Without the memory the loop sawtooths:
	// +aimdIncrement every poolControlInterval until it hits the same wall,
	// then shed again.
	aimdCeilingFrac  = 0.9
	aimdCeilingDecay = 24 * time.Hour
)

// poolControlInterval is how often the AIMD pool controller samples pressure
// and steps the target. Var (not const) so tests can shorten it instead of
// waiting on the real 5 minute cadence.
var poolControlInterval = 5 * time.Minute

// applyShedBackoff keeps a shed proxy down for at least shedBackoff from now.
// It only ever lengthens: a shed picks addresses from its own snapshot, so the
// address may already be held by something longer (proxy audit's park, a URL
// give-up), and overwriting that with a short window would relaunch it early.
// Pool shed and trim shed both come through here.
func applyShedBackoff(addr string, now time.Time) {
	globalProxyFailureHistory.ExtendBackoffUntil(addr, now.Add(shedBackoff))
}

// aimdCeilingState remembers where pressure last forced a cut. In memory only:
// a restart forgets it and the pool rediscovers its level.
type aimdCeilingState struct {
	level int       // starting ceiling: aimdCeilingFrac x hit, never below aimdFloor
	hit   int       // pool target when pressure forced the cut
	at    time.Time // when the cut happened
}

// record notes a cut made at pool size hit. While a ceiling is still active
// the highest hit wins: a sustained episode cuts again every sample at 0.7x,
// and remembering the last, lowest cut would pin the pool near the floor for a
// day after what is often an external, transient host event. A ceiling that
// has been released starts fresh.
func (c *aimdCeilingState) record(hit int, now time.Time) {
	if c.effective(now) > 0 && c.hit > hit {
		hit = c.hit
	}
	level := int(float64(hit) * aimdCeilingFrac)
	if level < aimdFloor {
		level = aimdFloor
	}
	if hit < level {
		hit = level
	}
	c.level, c.hit, c.at = level, hit, now
}

// effective returns the current regrowth ceiling, or 0 for none.
func (c *aimdCeilingState) effective(now time.Time) int {
	if c.at.IsZero() {
		return 0
	}
	age := now.Sub(c.at)
	if age < 0 {
		age = 0
	}
	if age >= aimdCeilingDecay {
		return 0
	}
	return c.level + int(float64(c.hit-c.level)*float64(age)/float64(aimdCeilingDecay))
}

// ceilingHit is the pool size to remember for a cut: the smaller of the target
// and the pool that actually exists, because a target the cache never reached
// would record a ceiling that constrains nothing. pool <= 0 means unknown.
func ceilingHit(target, pool int) int {
	if pool > 0 && pool < target {
		return pool
	}
	return target
}

// combineCeilings returns the tightest positive of the operator-configured
// ceiling and the learned one; 0 means no ceiling.
func combineCeilings(configured, learned int) int {
	switch {
	case configured <= 0:
		return learned
	case learned <= 0:
		return configured
	case learned < configured:
		return learned
	}
	return configured
}

// capURLTarget clamps the URL pool target to what the running-proxy cap leaves
// after paid proxies, which the cap also counts and which are never shed. It
// never returns below 1: a zero target reads as "first run".
func capURLTarget(next, cap, paid int) int {
	if cap <= 0 {
		return next
	}
	room := cap - paid
	if room < 1 {
		room = 1
	}
	if next > room {
		return room
	}
	return next
}

// aimdStep computes the next target pool size. cacheSize anchors growth so the
// target never runs far ahead of what actually exists.
func aimdStep(target, cacheSize int, pressure float64, ceiling int) int {
	switch {
	case pressure > aimdShrinkAbove:
		next := int(float64(target) * aimdDecreaseMult)
		if next < aimdFloor {
			next = aimdFloor
		}
		return next
	case pressure < aimdGrowBelow:
		base := target
		if cacheSize+aimdIncrement < base {
			base = cacheSize + aimdIncrement // track reality when cache lags target
		} else {
			base = target + aimdIncrement
		}
		if ceiling > 0 && base > ceiling {
			base = ceiling
		}
		return base
	default:
		return target
	}
}

// aimdMoveMessage explains one pool-target move in a plain sentence,
// attributing the cause honestly: AIMD pressure steps, cache tracking and cap
// or ceiling changes all share this path, and a wrong attribution sends an
// operator chasing a cause that never existed.
func aimdMoveMessage(target, next, cacheSize int, pressure float64) string {
	switch {
	case next > target && pressure < aimdGrowBelow:
		return fmt.Sprintf("Pool size target raised %d -> %d: pressure is low (%.2f), growing toward the allowed maximum. (pressure=%.2f cache=%d)\n", target, next, pressure, pressure, cacheSize)
	case next > target:
		return fmt.Sprintf("Pool size target raised %d -> %d: a cap or ceiling changed. (pressure=%.2f cache=%d)\n", target, next, pressure, cacheSize)
	case pressure > aimdShrinkAbove:
		return fmt.Sprintf("Pool size target lowered %d -> %d: pressure has been high (%.2f), shrinking to fit. (pressure=%.2f cache=%d)\n", target, next, pressure, pressure, cacheSize)
	case pressure < aimdGrowBelow && next == cacheSize+aimdIncrement:
		return fmt.Sprintf("Pool size target lowered %d -> %d: only %d proxies are cached, so the target follows the live pool. (pressure=%.2f cache=%d)\n", target, next, cacheSize, pressure, cacheSize)
	default:
		return fmt.Sprintf("Pool size target lowered %d -> %d: a cap or ceiling lowered it. (pressure=%.2f cache=%d)\n", target, next, pressure, cacheSize)
	}
}

// selectURLProxiesToShed ranks URL-sourced proxies for removal under sustained
// pressure: dead first, then degraded tiers, then healthy ones by ascending
// persisted earnings, then ascending lifetime traffic. Lifetime traffic alone
// is empty right after a restart, which made every proxy look idle; the
// earnings score survives restarts. Shedding an earning proxy is the last
// resort, and the caller logs each healthy shed individually.
func selectURLProxiesToShed(state *ProxyState, traffic map[string]uint64, earnings map[string]float64, n int) []string {
	rank := healthRank // shared with the operator proxy-trim shed ranking
	type cand struct {
		addr string
		r    int
		earn float64
		tx   uint64
	}
	var cands []cand
	for addr, e := range state.Proxies {
		if e.Source != "url" {
			continue
		}
		cands = append(cands, cand{addr, rank(e.Health), earnings[addr], traffic[addr]})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.r != b.r {
			return a.r - b.r
		}
		if a.earn != b.earn {
			if a.earn < b.earn {
				return -1
			}
			return 1
		}
		if a.tx != b.tx {
			if a.tx < b.tx {
				return -1
			}
			return 1
		}
		return strings.Compare(a.addr, b.addr)
	})
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]string, n)
	for i := range out {
		out[i] = cands[i].addr
	}
	return out
}

// runPoolController runs the AIMD loop: every poolControlInterval it reads
// pressure, steps the target, persists it, and - on a shrink - sheds the worst
// URL-sourced proxies down to the new target via the same removeDeadProxies
// path the cleanup job uses (cache removal, NO blacklist, so shed addresses
// re-enter through a normal fetch+probe once the box recovers and the target
// grows back).
//
// Port adaptations from v33: the pressure monitor's grow-suppression guard
// (gcTightening / thrashFreeze) is not ported because the GC governor and the
// thrash watchdog are P2 - when they land, the guard from resource_pressure.go
// is restored here; and nowFn() is the wall clock.
func runPoolController(ctx context.Context, configuredMax int, selfHealEnabled bool) {
	var highSamples int
	var ceilingMemory aimdCeilingState
	ticker := time.NewTicker(poolControlInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !resolveSelfHealEnabled(selfHealEnabled) {
			highSamples = 0
			continue
		}
		pressure := currentPressure()
		// Grow/hold decisions must not be blocked by CPU stalls alone
		// (scoring fix: a 1-core box's CPU PSI otherwise pins the pool below
		// its ceiling forever). Outside the shrink regime, read the score with
		// psi_cpu excluded; the shrink decision below keeps the full score.
		if pressure <= aimdShrinkAbove {
			if noCPU := currentPressureNoCPU(); noCPU < pressure {
				pressure = noCPU
			}
		}
		if pressure > aimdShrinkAbove {
			highSamples++
		} else {
			highSamples = 0
		}
		// Shrink requires 2 consecutive high samples (10 min sustained);
		// growth and hold act immediately.
		if pressure > aimdShrinkAbove && highSamples < 2 {
			continue
		}

		release, err := acquireProxyLock()
		if err != nil {
			continue
		}
		urlState, err := readProxyURLState()
		if err != nil {
			release()
			continue
		}
		cacheSize := len(urlState.Cache)
		target := urlState.TargetPoolSize
		if target <= 0 {
			// First run: start from where we are (bounded by the ceiling).
			target = cacheSize
			if ceiling := resolveProxyURLMax(configuredMax); ceiling > 0 && target > ceiling {
				target = ceiling
			}
			if target < aimdFloor {
				target = aimdFloor
			}
		}
		next := aimdStep(target, cacheSize, pressure,
			combineCeilings(resolveProxyURLMax(configuredMax), ceilingMemory.effective(time.Now())))
		// An operator trim cap overrides the AIMD operating point: never grow
		// the URL pool target above what the running-proxy cap leaves after paid
		// proxies (they fight otherwise, burning fetch/probe work on proxies
		// that can never launch). The cap counts every running proxy, paid
		// included.
		if tc, _ := effectiveTrimCap(); tc > 0 {
			next = capURLTarget(next, tc, paidProxyCount())
		}
		if next != urlState.TargetPoolSize {
			urlState.TargetPoolSize = next
			if err := writeProxyURLState(urlState); err != nil {
				tlog("[proxy][pressure] warn: could not persist target: %v\n", err)
			}
		}
		release()
		if next != target {
			tlog("[proxy][pressure] %s", aimdMoveMessage(target, next, cacheSize, pressure))
		}

		if pressure > aimdShrinkAbove {
			// Remember where pressure hit so regrowth stops short of it.
			ceilingMemory.record(ceilingHit(target, cacheSize), time.Now())
			shedPoolToTarget(next)
			highSamples = 0 // one cut per sustained-high episode; re-arm
		}
	}
}

// shedPoolToTarget removes the worst URL proxies until the live url-sourced
// count is at most target. Healthy sheds are logged individually - removing an
// earning proxy is deliberate and visible, never silent.
func shedPoolToTarget(target int) {
	state, err := readProxyState()
	if err != nil {
		return
	}
	urlCount := 0
	for _, e := range state.Proxies {
		if e.Source == "url" {
			urlCount++
		}
	}
	excess := urlCount - target
	if excess <= 0 {
		return
	}

	// Per-proxy traffic for last-resort ranking, keyed by proxy identity to
	// match state.Proxies.
	traffic := runningProxyEarnings()
	now := time.Now()
	earnings := make(map[string]float64, len(state.Proxies))
	for addr, e := range state.Proxies {
		if e.Source == "url" {
			earnings[addr] = proxyEarningsScore(addr, now)
		}
	}

	shed := selectURLProxiesToShed(state, traffic, earnings, excess)
	for _, addr := range shed {
		if state.Proxies[addr].Health == "up" {
			tlog("[proxy][pressure] shedding HEALTHY proxy %s (last resort, pool over target)\n", proxyKeyDisplay(addr))
		}
		applyShedBackoff(addr, time.Now())
	}
	if err := removeDeadProxies(state, map[string][]string{"url": shed}); err != nil {
		tlog("[proxy][pressure] warn: shed failed: %v\n", err)
		return
	}
	tlog("[proxy][pressure] shed %d url proxies to reach target %d\n", len(shed), target)
}

// paidRunningCount is how many of the running proxies are paid or file
// sourced: in proxy.state, not URL-sourced, and not the direct transport. The
// trim/OOM cap counts RUNNING proxies, so this must too. proxy.state also holds
// every desired entry, dead or backed-off or held by trim, and counting those
// drove cap minus paid to nothing and pinned the URL pool at 1. The pool
// controller never sheds these, though the operator trim can.
func paidRunningCount(state *ProxyState, running []string) int {
	n := 0
	for _, key := range running {
		if key == directProxyKey {
			continue
		}
		if e, ok := state.Proxies[key]; ok && e.Source != "url" {
			n++
		}
	}
	return n
}

// paidProxyCount reads proxy.state and the running set for paidRunningCount.
// Returns 0 when the state cannot be read, which leaves the cap clamp at its
// pre-existing behaviour.
func paidProxyCount() int {
	state, err := readProxyState()
	if err != nil {
		return 0
	}
	return paidRunningCount(state, runningProxyAddresses())
}
