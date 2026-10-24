package miner

// thrash_watchdog.go ports the relay's swap-thrash watchdog
// (provider/thrash_watchdog.go) into the miner.
//
// The miner had nothing of this: no swap reads, no PSI, no vmstat
// pswpin/pswpout, no watchdog state and no status file. What is ported here is
// the sensing, the reset-safe counter plumbing, the four-state detection
// ladder, the human-readable attribution, the operator status file, and the
// escalation rung: a supervised restart (exit 75), the thrash cap the next
// start opens smaller with (thrash_cap.go), and the attribution / hot-swap /
// supervisor / anti-loop gates (thrash_escalation.go):
//
//   - detect ACTIVITY, not level: swap usage alone is not thrash.
//   - every state change logs one plain sentence with human units, who holds
//     the memory, and what happens next (no decoding).
//   - a missing source is UNAVAILABLE, never a readable zero: a flapping
//     source must not read as a calm tick and reset the sustain clocks.
//   - platform sensing lives behind swapThrashReadFn: thrash_sensors_linux.go
//     reads /proc and cgroup v2, thrash_sensors_other.go returns what little a
//     platform without /proc can say (the heap), so darwin, freebsd and
//     windows still build.
//   - sensing and status are always visible; ESCALATION is gated on self-heal
//     and on a real supervisor, so an unsupervised `miner provide` only logs.
//
// NOT ported: the relay's freeze-growth rung (its pool controller is a
// different shape here), and the audit ledger the relay records the restart
// into (the miner has none; the reason goes to events.log instead).

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Tunables. Durations are vars so tests can shrink them; thresholds are
// consts (tests drive them by feeding crafted readings into the pure
// machine/parse/attribution functions instead).
var (
	swapThrashSampleInterval = 30 * time.Second
	// swapThrashSustain is how long the mild thrash condition must hold before
	// escalation-grade thrashing is declared.
	swapThrashSustain = 3 * time.Minute
	// swapThrashSevereSustain shortens the wait once PSI full is severe (>=25%).
	swapThrashSevereSustain = 90 * time.Second
	// swapThrashCalmRelax is the sustained calm required to step down ONE state.
	swapThrashCalmRelax = 5 * time.Minute
	// swapThrashCriticalAfter marks a thrash that is not resolving by itself.
	swapThrashCriticalAfter = 15 * time.Minute

	// swapThrashNowFn is the clock the default watch reads.
	swapThrashNowFn = time.Now
)

const (
	// PSI memory full: fraction of wall time ALL tasks were stalled on memory.
	swapThrashSevereFrac = 0.25
	swapThrashMildFrac   = 0.10
	// When NOTHING corroborates (no swap counters, no refaults, no reclaim),
	// the mild band needs a stricter PSI-only bar: never AND with an
	// unavailable signal, and never treat absence of corroborators as absence
	// of thrash.
	swapThrashMildNoCorroboratorFrac = 0.20

	// Corroboration thresholds (pages/sec) for the 10-25% band.
	swapThrashSwapCorroboratePS    = 250.0
	swapThrashRefaultCorroboratePS = 50.0
	swapThrashPgscanCorroboratePS  = 100.0

	// swapThrashEarlyWarnSwapPS is the swap-activity floor for the heap early
	// warning: one page/s of ambient host swap must not trigger it.
	swapThrashEarlyWarnSwapPS = 25.0

	// swapThrashAttributionShare is the share of used swap this cgroup must
	// hold for the thrash to be ours.
	swapThrashAttributionShare = 0.5

	// swapThrashSwapWarnFrac triggers the distinct "swap nearly full" note.
	swapThrashSwapWarnFrac = 0.95

	// swapThrashHeapRunawayFrac is how far over its soft limit the live heap
	// may be, with the box short of free RAM, before the heap alone counts as
	// severe.
	swapThrashHeapRunawayFrac = 1.4
	// swapThrashHeapRunawayAvailFrac and swapThrashHeapRunawayAvailFloorMiB
	// define "short of free RAM": under a tenth of the box, and never under
	// 256 MiB.
	swapThrashHeapRunawayAvailFrac     = 0.10
	swapThrashHeapRunawayAvailFloorMiB = 256
)

const swapThrashStatusFileName = "thrash_status"

// ---------------------------------------------------------------------------
// Counter delta plumbing (reset-safe)
// ---------------------------------------------------------------------------

// swapThrashCounterDelta is the previous observation of one cumulative kernel
// counter, so the next observation yields a per-second rate. Reset-safe: a
// counter that moved backwards (reboot, cgroup recreation, rollover) or a
// fresh baseline reports 0 rather than underflowing into a bogus giant rate.
type swapThrashCounterDelta struct {
	have  bool
	at    time.Time
	value uint64
}

// rate records cur and returns its non-negative per-second delta against the
// previous observation. ok=false on a fresh baseline, a source flip, a
// backwards counter or a non-advancing clock: the value is UNAVAILABLE, not
// zero.
func (d *swapThrashCounterDelta) rate(cur uint64, now time.Time) (float64, bool) {
	prev, prevAt, had := d.value, d.at, d.have
	d.have, d.at, d.value = true, now, cur
	if !had || !now.After(prevAt) || cur < prev {
		return 0, false
	}
	return float64(cur-prev) / now.Sub(prevAt).Seconds(), true
}

// parseSwapThrashPSI extracts the cumulative total= microseconds for the some
// and full lines of a PSI pressure file (/proc/pressure/memory or a cgroup v2
// memory.pressure). ok=false per line when that line or its total= is
// missing, so the caller can report the signal unavailable instead of
// inventing a zero.
func parseSwapThrashPSI(content string) (someTotal, fullTotal uint64, someOK, fullOK bool) {
	for _, line := range strings.Split(content, "\n") {
		which := ""
		switch {
		case strings.HasPrefix(line, "some "):
			which = "some"
		case strings.HasPrefix(line, "full "):
			which = "full"
		default:
			continue
		}
		for _, field := range strings.Fields(line)[1:] {
			k, v, ok := strings.Cut(field, "=")
			if !ok || k != "total" {
				continue
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				continue
			}
			if which == "some" {
				someTotal, someOK = n, true
			} else {
				fullTotal, fullOK = n, true
			}
		}
	}
	return someTotal, fullTotal, someOK, fullOK
}

// parseSwapThrashVMStatSwap extracts the cumulative pswpin and pswpout page
// counters from /proc/vmstat content. ok=false when either counter is
// missing, so the caller reports the sensor unavailable rather than a fake
// zero.
func parseSwapThrashVMStatSwap(content string) (pswpin, pswpout uint64, ok bool) {
	var haveIn, haveOut bool
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "pswpin":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				pswpin, haveIn = v, true
			}
		case "pswpout":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				pswpout, haveOut = v, true
			}
		}
	}
	return pswpin, pswpout, haveIn && haveOut
}

// parseSwapThrashCounter extracts one counter from a "key value" file: cgroup
// v2 memory.stat when the kernel carries the swap counters there, or host
// /proc/vmstat for pgscan_direct.
func parseSwapThrashCounter(content, key string) (uint64, bool) {
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, " ")
		if !ok || k != key {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// parseSwapThrashMeminfoSwap extracts SwapTotal/SwapFree (MiB) from
// /proc/meminfo content. ok=false when either is missing.
func parseSwapThrashMeminfoSwap(content string) (totalMiB, freeMiB int64, ok bool) {
	var haveTotal, haveFree bool
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var dst *int64
		var seen *bool
		switch fields[0] {
		case "SwapTotal:":
			dst, seen = &totalMiB, &haveTotal
		case "SwapFree:":
			dst, seen = &freeMiB, &haveFree
		default:
			continue
		}
		if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			*dst, *seen = n/1024, true
		}
	}
	return totalMiB, freeMiB, haveTotal && haveFree
}

// parseSwapThrashMeminfoValue extracts one kB value from /proc/meminfo and
// returns it in MiB (MemAvailable, MemTotal).
func parseSwapThrashMeminfoValue(content, key string) (int64, bool) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != key+":" {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return n / 1024, true
	}
	return 0, false
}

// swapThrashHeapReading is the heap-vs-soft-limit numerator: the same live
// heap the memory plan sizes against, over the runtime soft limit (or the
// effective host memory when no limit is installed). Portable: it reads only
// the Go runtime and the platform host-memory helper.
func swapThrashHeapReading() (frac float64, usedMiB, limitMiB int64, ok bool) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	used := ms.HeapAlloc + ms.StackInuse
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit == math.MaxInt64 {
		limit = int64(hostMemoryByteCount())
	}
	if used == 0 || limit <= 0 {
		return 0, 0, 0, false
	}
	return float64(used) / float64(limit), int64(used >> 20), limit >> 20, true
}

// ---------------------------------------------------------------------------
// One reading: counters + levels
// ---------------------------------------------------------------------------

// swapThrashRead is one best-effort reading of every thrash source. Each
// group carries its own ok flag; a missing source is "unavailable", never 0.
type swapThrashRead struct {
	psiSomeTotal, psiFullTotal uint64
	psiSomeOK, psiFullOK       bool
	psiSomeUnit                bool // some line read from this cgroup
	psiUnit                    bool // full line read from this cgroup (preferred)

	swapIn, swapOut uint64
	swapOK          bool
	swapUnit        bool // unit memory.stat counters (kernel-dependent)

	refault   uint64
	refaultOK bool

	pgscan   uint64
	pgscanOK bool
	// pgscanUnit records which source the pgscan counter came from: a flip
	// between them must re-baseline or it fabricates a huge forward delta and
	// a false corroborator.
	pgscanUnit bool

	unitSwapMiB                       int64
	unitSwapOK                        bool
	hostSwapUsedMiB, hostSwapTotalMiB int64
	hostSwapOK                        bool
	ramAvailMiB                       int64
	ramAvailOK                        bool
	ramTotalMiB                       int64
	ramTotalOK                        bool
	heapFrac                          float64
	heapUsedMiB, heapLimitMiB         int64
	heapOK                            bool
}

// ---------------------------------------------------------------------------
// Rates
// ---------------------------------------------------------------------------

// swapThrashTracker carries the previous counters and their source between
// samples. Host counters are cumulative since boot and cgroup counters since
// the cgroup was created: a tick that flips source must re-baseline, or the
// forward delta fabricates a huge rate the reset clamp cannot see (only
// backward moves are clamped).
type swapThrashTracker struct {
	someTotal, fullTotal, swapIn, swapOut, refault, pgscan swapThrashCounterDelta
	someSource, fullSource, swapSource, pgscanSource       string
	someTagged, fullTagged, swapTagged, pgscanTagged       bool
}

// swapThrashRates is the derived per-second view of one reading. PSI totals
// are microseconds: fullFrac/someFrac are fractions of WALL time (0..1)
// computed over REAL elapsed time, not avg60 and not sample counts (a
// swapped-out ticker can slip 90s+ between samples and the math must survive
// that).
type swapThrashRates struct {
	fullFrac, someFrac  float64
	fullOK, someOK      bool
	swapInPS, swapOutPS float64
	swapOK              bool
	refaultPS           float64
	refaultOK           bool
	pgscanPS            float64
	pgscanOK            bool
}

// rearmSwapThrashDelta resets one delta so the next rate call re-baselines.
func rearmSwapThrashDelta(d *swapThrashCounterDelta) { *d = swapThrashCounterDelta{} }

// clampSwapThrashFrac keeps a derived stalled-fraction inside [0,1]; counter
// quirks must never read as "stalled more than all the time".
func clampSwapThrashFrac(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// rates converts one reading into rates, updating the tracker. A source flip
// re-baselines that stream; a baseline/flip tick reports the stream as
// unavailable (ok=false), never as a readable zero.
func (tr *swapThrashTracker) rates(rd swapThrashRead, now time.Time) swapThrashRates {
	var r swapThrashRates
	if rd.psiSomeOK {
		src := "host"
		if rd.psiSomeUnit {
			src = "unit"
		}
		if tr.someTagged && src != tr.someSource {
			rearmSwapThrashDelta(&tr.someTotal)
		}
		tr.someSource, tr.someTagged = src, true
		if v, ok := tr.someTotal.rate(rd.psiSomeTotal, now); ok {
			r.someFrac, r.someOK = clampSwapThrashFrac(v/1e6), true
		}
	}
	if rd.psiFullOK {
		src := "host"
		if rd.psiUnit {
			src = "unit"
		}
		if tr.fullTagged && src != tr.fullSource {
			rearmSwapThrashDelta(&tr.fullTotal)
		}
		tr.fullSource, tr.fullTagged = src, true
		if v, ok := tr.fullTotal.rate(rd.psiFullTotal, now); ok {
			r.fullFrac, r.fullOK = clampSwapThrashFrac(v/1e6), true
		}
	}
	if rd.swapOK {
		src := "host"
		if rd.swapUnit {
			src = "unit"
		}
		if tr.swapTagged && src != tr.swapSource {
			rearmSwapThrashDelta(&tr.swapIn)
			rearmSwapThrashDelta(&tr.swapOut)
		}
		tr.swapSource, tr.swapTagged = src, true
		vIn, okIn := tr.swapIn.rate(rd.swapIn, now)
		vOut, okOut := tr.swapOut.rate(rd.swapOut, now)
		if okIn && okOut {
			r.swapInPS, r.swapOutPS, r.swapOK = vIn, vOut, true
		}
	}
	if rd.refaultOK {
		if v, ok := tr.refault.rate(rd.refault, now); ok {
			r.refaultPS, r.refaultOK = v, true
		}
	}
	if rd.pgscanOK {
		src := "host"
		if rd.pgscanUnit {
			src = "unit"
		}
		if tr.pgscanTagged && src != tr.pgscanSource {
			rearmSwapThrashDelta(&tr.pgscan)
		}
		tr.pgscanSource, tr.pgscanTagged = src, true
		if v, ok := tr.pgscan.rate(rd.pgscan, now); ok {
			r.pgscanPS, r.pgscanOK = v, true
		}
	}
	return r
}

// swapThrashStallFrac prefers the PSI full fraction (all tasks) and falls
// back to some (at least one) when full is unreadable.
func swapThrashStallFrac(rt swapThrashRates) (float64, bool) {
	if rt.fullOK {
		return rt.fullFrac, true
	}
	if rt.someOK {
		return rt.someFrac, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// State machine
// ---------------------------------------------------------------------------

// swapThrashStateT follows the relay's four-state ladder.
type swapThrashStateT int

const (
	swapThrashCalm swapThrashStateT = iota
	swapThrashUnderPressure
	swapThrashThrashing
	swapThrashCritical
)

func (s swapThrashStateT) String() string {
	switch s {
	case swapThrashUnderPressure:
		return "under-pressure"
	case swapThrashThrashing:
		return "thrashing"
	case swapThrashCritical:
		return "critical"
	}
	return "calm"
}

// swapThrashMachine is the time-based hysteresis. Rise fast on evidence;
// relax one step per sustained-calm window.
type swapThrashMachine struct {
	state          swapThrashStateT
	stateSince     time.Time
	thrashSince    time.Time // when the current thrash episode began (survives the move to critical)
	worseSince     time.Time
	severeSince    time.Time
	calmSince      time.Time
	lastAttempt    time.Time // throttles escalation attempts
	heapWarned     bool
	swapCritWarned bool
	// heapDriven: the standing condition rests on the heap rule alone, so a
	// healthy heap ends it even while PSI is unreadable.
	heapDriven bool
}

// swapThrashStep is one machine advance.
type swapThrashStep struct {
	prev, cur swapThrashStateT
	changed   bool
	condDur   time.Duration // how long the worsening condition has held
	// cond is whether the condition holds on THIS tick. The machine stays in
	// thrashing through its relax window, so a recovered box is not reported
	// as still worsening.
	cond bool
}

// swapThrashHeapRunaway reports a heap well over its soft limit on a box
// short of free RAM. It exists because the spiral it names starves every
// responder within minutes while PSI still reads calm. Both inputs must be
// readable; a box that cannot say how much RAM is free is never judged on the
// heap alone.
func swapThrashHeapRunaway(rd swapThrashRead) bool {
	if !rd.heapOK || rd.heapFrac < swapThrashHeapRunawayFrac || !rd.ramAvailOK {
		return false
	}
	low := int64(swapThrashHeapRunawayAvailFloorMiB)
	if rd.ramTotalOK {
		if tenth := int64(float64(rd.ramTotalMiB) * swapThrashHeapRunawayAvailFrac); tenth > low {
			low = tenth
		}
	}
	return rd.ramAvailMiB < low
}

// step advances the machine. Pure except for the machine's own bookkeeping:
// all time comes from now, so tests drive it with a fake clock.
func (m *swapThrashMachine) step(now time.Time, rt swapThrashRates, rd swapThrashRead) swapThrashStep {
	prev := m.state

	heapRunaway := swapThrashHeapRunaway(rd)
	heapReadable := rd.heapOK && rd.ramAvailOK
	if !rt.fullOK && !heapRunaway {
		// A tick that only the heap rule was driving, now with a readable
		// healthy heap, is a real calm reading: on a box without PSI nothing
		// else would ever move the machine again, and the clocks and the
		// freeze it set would hold for the life of the process.
		if !(m.heapDriven && heapReadable) {
			// PSI full unavailable (fresh baseline, source flip, unreadable):
			// a NEUTRAL tick. Hold every clock and the state. A fake readable
			// zero here used to read as calm and reset the sustain clocks,
			// defeating detection exactly when sources flap.
			return swapThrashStep{prev: prev, cur: m.state, changed: false, cond: false}
		}
	}

	severe := rt.fullOK && rt.fullFrac >= swapThrashSevereFrac || heapRunaway
	swapCorr := rt.swapOK && (rt.swapInPS+rt.swapOutPS) >= swapThrashSwapCorroboratePS
	refCorr := rt.refaultOK && rt.refaultPS >= swapThrashRefaultCorroboratePS
	pgCorr := rt.pgscanOK && rt.pgscanPS >= swapThrashPgscanCorroboratePS
	heapCorrAvail := rd.heapOK && rd.unitSwapOK
	heapCorr := heapCorrAvail && rd.heapFrac >= 1.0 && rd.unitSwapMiB > 0
	corr := swapCorr || refCorr || pgCorr || heapCorr
	// corrAvail includes readable-but-quiet level signals on purpose:
	// availability of a corroborating source is what decides the stricter
	// PSI-only bar, not its current activity.
	corrAvail := rt.swapOK || rt.refaultOK || rt.pgscanOK || heapCorrAvail

	mild := rt.fullOK && rt.fullFrac >= swapThrashMildFrac
	strict := rt.fullOK && rt.fullFrac >= swapThrashMildNoCorroboratorFrac

	// thrashCond: severe alone is sufficient; the mild band needs
	// corroboration when available, and a stricter PSI-only bar when nothing
	// corroborates (never AND with an unavailable signal).
	thrashCond := severe || (mild && corr) || (!corrAvail && strict)
	if thrashCond {
		// The standing condition rests on the heap rule alone when PSI gives
		// no reason of its own for it.
		psiSevere := rt.fullOK && rt.fullFrac >= swapThrashSevereFrac
		m.heapDriven = !(psiSevere || (mild && corr) || (!corrAvail && strict))
	}

	if thrashCond {
		if m.worseSince.IsZero() {
			m.worseSince = now
		}
		if severe {
			if m.severeSince.IsZero() {
				m.severeSince = now
			}
		} else {
			m.severeSince = time.Time{}
		}
		m.calmSince = time.Time{}
	} else {
		m.worseSince = time.Time{}
		m.severeSince = time.Time{}
	}

	var condDur, severeDur time.Duration
	if !m.worseSince.IsZero() {
		condDur = now.Sub(m.worseSince)
	}
	if !m.severeSince.IsZero() {
		severeDur = now.Sub(m.severeSince)
	}

	desired := swapThrashCalm
	switch {
	case severe && severeDur >= swapThrashSevereSustain:
		desired = swapThrashThrashing
	case thrashCond && condDur >= swapThrashSustain:
		desired = swapThrashThrashing
	case thrashCond:
		desired = swapThrashUnderPressure
	}

	target := m.state
	switch {
	case desired > m.state:
		target = desired
	case desired < m.state && !thrashCond:
		// Relax one step per sustained-calm window.
		if m.calmSince.IsZero() {
			m.calmSince = now
		}
		if now.Sub(m.calmSince) >= swapThrashCalmRelax {
			target = m.state - 1
			m.calmSince = time.Time{}
		}
	default:
		target = m.state
	}

	// A thrash that persists is no longer something the box works through.
	// The CURRENT continuous condition must have held for the full window:
	// neither the relax-window state nor a single resumed tick may promote.
	if thrashCond && target == swapThrashThrashing && m.state == swapThrashThrashing &&
		condDur >= swapThrashCriticalAfter {
		target = swapThrashCritical
	}

	if target != m.state {
		m.state = target
		m.stateSince = now
		if target == swapThrashThrashing {
			m.thrashSince = now
		}
	}
	if m.state == swapThrashCalm {
		m.heapDriven = false
	}
	return swapThrashStep{prev: prev, cur: m.state, changed: prev != m.state, condDur: condDur, cond: thrashCond}
}

// swapThrashAttribution decides whose memory is being swapped. Ours only when
// this cgroup holds >= half of used swap, or when the per-unit PSI (this
// process's own subtree) shows the stalls and no better counter exists.
// Unknown attribution is reported, never guessed.
func swapThrashAttribution(rd swapThrashRead) (attr string, share float64, shareOK bool) {
	if rd.unitSwapOK && rd.hostSwapOK && rd.hostSwapUsedMiB > 0 {
		s := float64(rd.unitSwapMiB) / float64(rd.hostSwapUsedMiB)
		if s > 1 {
			// MiB truncation / accounting skew can read >100%; never print it.
			s = 1
		}
		if s >= swapThrashAttributionShare {
			return "unit", s, true
		}
		return "other", s, true
	}
	if rd.unitSwapOK && rd.unitSwapMiB > 0 {
		return "unit", 0, false
	}
	if rd.psiUnit && rd.psiFullOK {
		return "unit", 0, false
	}
	return "unknown", 0, false
}

// ---------------------------------------------------------------------------
// Messages (human-readable output is a hard requirement: no decoding)
// ---------------------------------------------------------------------------

// fmtSwapThrashMiB renders MiB in human units: "3.1 GB" / "512 MB".
func fmtSwapThrashMiB(mib int64) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MB", mib)
}

// fmtSwapThrashMBs renders a pages/sec rate as MB/s (4 KiB pages).
func fmtSwapThrashMBs(pps float64) string {
	return fmt.Sprintf("%.0f MB/s", pps/256.0)
}

func swapThrashOnsetMsg(rt swapThrashRates, rd swapThrashRead, share float64, shareOK bool) string {
	if swapThrashHeapRunaway(rd) && !(rt.fullOK && rt.fullFrac >= swapThrashSevereFrac) {
		held := "an unmeasurable amount of the swap"
		if rd.unitSwapOK {
			held = fmtSwapThrashMiB(rd.unitSwapMiB) + " of swap"
		}
		return fmt.Sprintf("🚨 [memory] The program's heap has run away: %.1fx its size limit (%s of %s) with only %s RAM free, and %s belongs to this provider. Memory is not stalling yet, but the box is about to.",
			rd.heapFrac, fmtSwapThrashMiB(rd.heapUsedMiB), fmtSwapThrashMiB(rd.heapLimitMiB), fmtSwapThrashMiB(rd.ramAvailMiB), held)
	}
	stallTxt := "memory is stalling"
	if stall, ok := swapThrashStallFrac(rt); ok {
		stallTxt = fmt.Sprintf("memory is currently stalled ~%.0f%% of the time", stall*100)
	}
	swapTxt := "swap activity is unreadable right now"
	if rt.swapOK {
		swapTxt = fmt.Sprintf("it is swapping ~%s", fmtSwapThrashMBs(rt.swapInPS+rt.swapOutPS))
	}
	held := "an unmeasurable amount of the swap"
	switch {
	case shareOK:
		held = fmt.Sprintf("%s (%.0f%% of all swap in use)", fmtSwapThrashMiB(rd.unitSwapMiB), share*100)
	case rd.unitSwapOK:
		held = fmt.Sprintf("%s of swap", fmtSwapThrashMiB(rd.unitSwapMiB))
	}
	heapTxt := ""
	if rd.heapOK {
		heapTxt = fmt.Sprintf(" Heap is %.1fx its limit.", rd.heapFrac)
	}
	return fmt.Sprintf("🚨 [memory] The box is out of RAM and thrashing swap — %s, %s, and %s belongs to this provider.%s",
		stallTxt, swapTxt, held, heapTxt)
}

func swapThrashOtherMsg(rd swapThrashRead, share float64) string {
	return fmt.Sprintf("🚨 [memory] The box is thrashing swap, but the memory belongs to another process: this provider holds %s of the %s swapped (%.0f%%). Not restarting.",
		fmtSwapThrashMiB(rd.unitSwapMiB), fmtSwapThrashMiB(rd.hostSwapUsedMiB), share*100)
}

func swapThrashUnknownMsg() string {
	return "🚨 [memory] The box may be thrashing swap but the memory cannot be attributed to this provider (no per-unit swap counters, PSI reads are host-wide). Not restarting; check the box by hand."
}

func swapThrashEarlyWarnMsg(rd swapThrashRead) string {
	ramTxt := "RAM free: unknown"
	if rd.ramAvailOK {
		ramTxt = fmt.Sprintf("%s RAM free", fmtSwapThrashMiB(rd.ramAvailMiB))
	}
	return fmt.Sprintf("⚠️ [memory] The program's heap is %.1fx its size limit (%s of %s) and the machine is swapping. If the heap keeps growing the box will start thrashing. %s.",
		rd.heapFrac, fmtSwapThrashMiB(rd.heapUsedMiB), fmtSwapThrashMiB(rd.heapLimitMiB), ramTxt)
}

func swapThrashClearedMsg(prefix string, rd swapThrashRead, rt swapThrashRates) string {
	freeTxt := "RAM free: unknown"
	if rd.ramAvailOK {
		freeTxt = fmt.Sprintf("%s RAM free", fmtSwapThrashMiB(rd.ramAvailMiB))
	}
	parts := []string{freeTxt}
	if rd.hostSwapOK {
		parts = append(parts, fmt.Sprintf("%s swap in use", fmtSwapThrashMiB(rd.hostSwapUsedMiB)))
	}
	if stall, ok := swapThrashStallFrac(rt); ok {
		parts = append(parts, fmt.Sprintf("memory stalls %.0f%%", stall*100))
	}
	return fmt.Sprintf("✅ [memory] %s: %s.", prefix, strings.Join(parts, ", "))
}

// ---------------------------------------------------------------------------
// Published snapshot + status file
// ---------------------------------------------------------------------------

// swapThrashSnapshot is the published, aggregator-ready view. The field names
// are the relay's (thrashSnapshot) so an existing reader keeps working,
// including the restart-only fields now that the escalation rung exists.
type swapThrashSnapshot struct {
	State            string   `json:"state"`
	SinceUnix        int64    `json:"since_unix"`
	PSIFull          *float64 `json:"psi_mem_full"`      // stalled fraction 0..1; null when unavailable
	SomeFrac         *float64 `json:"some_stalled_frac"` // null when unavailable
	SwapIOPS         *float64 `json:"swap_io_pps"`       // pages/s (in+out); null when unavailable
	UnitSwapMiB      *int64   `json:"unit_swap_mib"`     // null when unavailable
	HostSwapUsedMiB  *int64   `json:"host_swap_used_mib"`
	HostSwapTotalMiB *int64   `json:"host_swap_total_mib"`
	RAMAvailMiB      *int64   `json:"ram_avail_mib"`
	RAMTotalMiB      *int64   `json:"ram_total_mib"`
	HeapFrac         *float64 `json:"heap_frac"`       // vs the soft limit
	Attribution      string   `json:"attribution"`     // unit | other | unknown
	UnitShare        *float64 `json:"unit_swap_share"` // null when unknown
	LastAction       string   `json:"last_action,omitempty"`
	Restarts24h      int      `json:"restarts_24h"`
	Summary          string   `json:"summary"`
	Updated          string   `json:"updated"`
}

func swapThrashPtrF(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}

func swapThrashPtrI(v int64, ok bool) *int64 {
	if !ok {
		return nil
	}
	return &v
}

func buildSwapThrashSnapshot(m *swapThrashMachine, rd swapThrashRead, rt swapThrashRates, attr string, share float64, shareOK bool, restarts int, lastAction string, now time.Time) *swapThrashSnapshot {
	s := &swapThrashSnapshot{
		State:       m.state.String(),
		SinceUnix:   m.stateSince.Unix(),
		Attribution: attr,
		LastAction:  lastAction,
		Restarts24h: restarts,
		Updated:     now.UTC().Format(time.RFC3339),
	}
	s.PSIFull = swapThrashPtrF(rt.fullFrac, rt.fullOK)
	s.SomeFrac = swapThrashPtrF(rt.someFrac, rt.someOK)
	if rt.swapOK {
		s.SwapIOPS = swapThrashPtrF(rt.swapInPS+rt.swapOutPS, true)
	}
	s.UnitSwapMiB = swapThrashPtrI(rd.unitSwapMiB, rd.unitSwapOK)
	s.HostSwapUsedMiB = swapThrashPtrI(rd.hostSwapUsedMiB, rd.hostSwapOK)
	s.HostSwapTotalMiB = swapThrashPtrI(rd.hostSwapTotalMiB, rd.hostSwapOK)
	s.RAMAvailMiB = swapThrashPtrI(rd.ramAvailMiB, rd.ramAvailOK)
	s.RAMTotalMiB = swapThrashPtrI(rd.ramTotalMiB, rd.ramTotalOK)
	s.HeapFrac = swapThrashPtrF(rd.heapFrac, rd.heapOK)
	s.UnitShare = swapThrashPtrF(share, shareOK)
	s.Summary = swapThrashSummaryOf(s)
	return s
}

// swapThrashSummaryOf renders the one-sentence summary carried in the status
// file. Thrash states lead with what is happening and who holds the memory.
func swapThrashSummaryOf(s *swapThrashSnapshot) string {
	switch s.State {
	case "thrashing", "critical":
		stall := "stalled"
		if s.PSIFull != nil {
			stall = fmt.Sprintf("the box was stalled on memory ~%.0f%% of the time", *s.PSIFull*100)
		}
		swapTxt := ""
		if s.SwapIOPS != nil {
			swapTxt = fmt.Sprintf(", swapping ~%s", fmtSwapThrashMBs(*s.SwapIOPS))
		}
		who := "swap attribution unavailable"
		switch {
		case s.Attribution == "unit" && s.UnitShare != nil:
			who = fmt.Sprintf("this provider holds %.0f%% of all swap in use", *s.UnitShare*100)
		case s.Attribution == "unit":
			who = "this provider's own cgroup is the one stalling"
		case s.Attribution == "other":
			who = "the memory belongs to another process"
		}
		heapTxt := ""
		if s.HeapFrac != nil {
			heapTxt = fmt.Sprintf("; heap %.1fx its limit", *s.HeapFrac)
		}
		return fmt.Sprintf("memory thrash: %s%s; %s%s.", stall, swapTxt, who, heapTxt)
	}
	// calm / under-pressure
	parts := []string{}
	if s.HeapFrac != nil {
		parts = append(parts, fmt.Sprintf("heap %.1fx its limit", *s.HeapFrac))
	}
	if s.RAMAvailMiB != nil {
		parts = append(parts, fmt.Sprintf("%s RAM free", fmtSwapThrashMiB(*s.RAMAvailMiB)))
	}
	if s.SwapIOPS != nil && *s.SwapIOPS > 0 {
		parts = append(parts, fmt.Sprintf("swapping %s", fmtSwapThrashMBs(*s.SwapIOPS)))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("memory %s (no signals readable)", s.State)
	}
	return fmt.Sprintf("memory %s: %s.", s.State, strings.Join(parts, "; "))
}

// swapThrashStatusPath is the stable file the operator and any status
// aggregator read, in the miner's state directory (the relay's
// ~/.urnetwork/thrash_status).
func swapThrashStatusPath() (string, error) {
	return providerStatePath(swapThrashStatusFileName)
}

func writeSwapThrashStatusFile(pathFn func() (string, error), s *swapThrashSnapshot) {
	if pathFn == nil {
		pathFn = swapThrashStatusPath
	}
	path, err := pathFn()
	if err != nil || path == "" {
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func clearSwapThrashStatusFile(pathFn func() (string, error)) {
	if pathFn == nil {
		pathFn = swapThrashStatusPath
	}
	if path, err := pathFn(); err == nil && path != "" {
		_ = os.Remove(path)
	}
}

// ---------------------------------------------------------------------------
// Watchdog
// ---------------------------------------------------------------------------

// swapThrashOptions are the injectable seams. Every field has a production
// default; tests supply a scripted sensor, a fake clock and a temp status
// path, so no test reads the real host or writes the real state directory.
type swapThrashOptions struct {
	readFn     func() swapThrashRead
	nowFn      func() time.Time
	statusPath func() (string, error)
	interval   time.Duration
	logf       func(format string, args ...any)
	notef      func(format string, args ...any)
	critf      func(format string, args ...any)
	// exitNotef is the pre-exit diagnostic line: stderr only, never the state
	// directory's events.log (thrash_exit_note.go).
	exitNotef func(format string, args ...any)
	// evidenceFn writes the incident bundle for an escalation that acted;
	// nil means captureThrashIncident. A test substitutes a recorder (or a
	// no-op) so the scripted-storm rig does not write the real state directory.
	evidenceFn func(esc swapThrashEscalation, rt swapThrashRates, rd swapThrashRead, attr string, running int)
}

// swapThrashWatch is the watchdog's process-wide state: one machine, one
// tracker, one published snapshot, sampled on its own interval.
type swapThrashWatch struct {
	readFn     func() swapThrashRead
	nowFn      func() time.Time
	statusPath func() (string, error)
	interval   time.Duration
	logf       func(format string, args ...any)
	notef      func(format string, args ...any)
	critf      func(format string, args ...any)
	exitNotef  func(format string, args ...any)

	// Escalation seams. Every one has a production default; tests inject them
	// so no test exits the test process, writes the real state dir, or guesses
	// at the host's supervisor.
	selfHealFn       func() bool
	supervisorKindFn func() (string, bool)
	capDirWritableFn func() error
	runningFn        func() int
	exitFn           func(int)
	hotSwapBusyFn    func() bool
	// evidenceFn writes the incident bundle for an escalation that acted. It
	// defaults to captureThrashIncident; a test substitutes a recorder (or a
	// no-op) so the scripted-storm tests do not write the real state directory.
	evidenceFn func(esc swapThrashEscalation, rt swapThrashRates, rd swapThrashRead, attr string, running int)

	tracker        swapThrashTracker
	machine        swapThrashMachine
	started        bool
	stopped        bool
	lastAt         time.Time
	announce       bool
	lastAction     string
	lastActionCode string
	snapshotPtr    atomic.Pointer[swapThrashSnapshot]
}

func newSwapThrashWatch(opts swapThrashOptions) *swapThrashWatch {
	w := &swapThrashWatch{
		readFn:     opts.readFn,
		nowFn:      opts.nowFn,
		statusPath: opts.statusPath,
		interval:   opts.interval,
		logf:       opts.logf,
		notef:      opts.notef,
		critf:      opts.critf,
		exitNotef:  opts.exitNotef,
		evidenceFn: opts.evidenceFn,
	}
	if w.readFn == nil {
		w.readFn = readSwapThrash
	}
	if w.nowFn == nil {
		w.nowFn = func() time.Time { return swapThrashNowFn() }
	}
	if w.statusPath == nil {
		w.statusPath = swapThrashStatusPath
	}
	if w.interval <= 0 {
		w.interval = swapThrashSampleInterval
	}
	if w.logf == nil {
		w.logf = tlog
	}
	if w.notef == nil {
		w.notef = importantLogf
	}
	if w.critf == nil {
		w.critf = critLog
	}
	if w.exitNotef == nil {
		w.exitNotef = swapThrashExitNotef
	}
	if w.selfHealFn == nil {
		w.selfHealFn = func() bool { return resolveSelfHealEnabled(os.Getenv("URNETWORK_SELF_HEAL") == "1") }
	}
	if w.supervisorKindFn == nil {
		w.supervisorKindFn = func() (string, bool) { return thrashSupervisorKind(thrashEnvFn, thrashFileExistsFn) }
	}
	if w.capDirWritableFn == nil {
		w.capDirWritableFn = thrashCapDirWritableBounded
	}
	if w.runningFn == nil {
		w.runningFn = oomCapRunning
	}
	if w.exitFn == nil {
		w.exitFn = func(code int) { swapThrashExitFn(code) }
	}
	if w.hotSwapBusyFn == nil {
		w.hotSwapBusyFn = swapThrashHotSwapBusy
	}
	if w.evidenceFn == nil {
		// The evidence lines go out on the same bounded stderr note as the
		// pre-exit diagnostics: this path may be inside the state-directory
		// hang, and critLog would fsync events.log before the exit.
		w.evidenceFn = func(esc swapThrashEscalation, rt swapThrashRates, rd swapThrashRead, attr string, running int) {
			// Read w.exitNotef through the watch on every call, not once at
			// construction: a test or a reconfigured watch that replaces it
			// must redirect the capture's lines too.
			_, _ = captureThrashIncident(esc, rt, rd, attr, running, w.exitNotef)
		}
	}
	return w
}

// The accessors below keep escalate() readable and give every gate one place
// to be injected from a test.
func (w *swapThrashWatch) selfHeal() bool                 { return w.selfHealFn() }
func (w *swapThrashWatch) supervisorKind() (string, bool) { return w.supervisorKindFn() }
func (w *swapThrashWatch) capDirWritable() error          { return w.capDirWritableFn() }
func (w *swapThrashWatch) running() int                   { return w.runningFn() }
func (w *swapThrashWatch) hotSwapBusy() bool              { return w.hotSwapBusyFn() }

// evidence records the escalation in an incident bundle. Never on the gate's
// verdict path: a bundle that cannot be written must not change the action.
func (w *swapThrashWatch) evidence(esc swapThrashEscalation, rt swapThrashRates, rd swapThrashRead, attr string) {
	if w.evidenceFn != nil {
		w.evidenceFn(esc, rt, rd, attr, esc.Running)
	}
}

// swapThrashWatchGlobal is the watch the miner's always-on sampler loop
// drives. It is a variable so a test can substitute a scripted watch.
var swapThrashWatchGlobal = newSwapThrashWatch(swapThrashOptions{})

// The accessors below are the relay's pressure_status seams
// (thrashStateName/thrashPSIFullForStatus/thrashSwapIOForStatus): the miner
// has no pressure-status writer yet, so they are what that writer will read
// when it lands. Atomic snapshot loads; never a lock.
func swapThrashStateName() string {
	if s := swapThrashWatchGlobal.snapshot(); s != nil {
		return s.State
	}
	return ""
}

func swapThrashPSIFullForStatus() (float64, bool) {
	if s := swapThrashWatchGlobal.snapshot(); s != nil && s.PSIFull != nil {
		return *s.PSIFull, true
	}
	return 0, false
}

func swapThrashSwapIOForStatus() (float64, bool) {
	if s := swapThrashWatchGlobal.snapshot(); s != nil && s.SwapIOPS != nil {
		return *s.SwapIOPS, true
	}
	return 0, false
}

func swapThrashSummaryForStatus() (string, bool) {
	if s := swapThrashWatchGlobal.snapshot(); s != nil && s.Summary != "" {
		return s.Summary, true
	}
	return "", false
}

// state is the current ladder state (tests and future status readers).
func (w *swapThrashWatch) state() swapThrashStateT { return w.machine.state }

// snapshot is the last published snapshot, if any.
func (w *swapThrashWatch) snapshot() *swapThrashSnapshot { return w.snapshotPtr.Load() }

// sampleIfDue samples at most once per interval. The miner's existing 1Hz
// loop calls it every second; the host is read at the watchdog's own pace.
func (w *swapThrashWatch) sampleIfDue(now time.Time) {
	if w.stopped {
		return
	}
	if !w.started {
		w.started = true
		w.lastAt = now
		w.sample(now)
		return
	}
	if now.Sub(w.lastAt) < w.interval {
		return
	}
	w.lastAt = now
	w.sample(now)
}

// stop clears the published status on the way out, so a reader does not see a
// dead process's last reading.
func (w *swapThrashWatch) stop() {
	clearSwapThrashStatusFile(w.statusPath)
	w.snapshotPtr.Store(nil)
}

// sample is one observation: read, derive rates, advance the ladder, publish
// the status. All bookkeeping is local to the watch; nothing here blocks.
func (w *swapThrashWatch) sample(now time.Time) {
	if w.machine.stateSince.IsZero() {
		w.machine.stateSince = now
	}
	rd := w.readFn()
	if !w.announce {
		w.announce = true
		active, missing := swapThrashSensorLine(rd)
		if len(active) == 0 {
			w.notef("⚠️ [memory] Thrash watch is INERT on this box: no memory-pressure signals are readable (no PSI, no swap counters). No thrash detection is possible here.\n")
		} else {
			line := fmt.Sprintf("[miner][thrash] monitor started, signals: %s", strings.Join(active, ", "))
			if len(missing) > 0 {
				line += fmt.Sprintf(" (unavailable: %s)", strings.Join(missing, ", "))
			}
			w.logf("%s\n", line)
		}
	}

	rt := w.tracker.rates(rd, now)
	st := w.machine.step(now, rt, rd)
	attr, share, shareOK := swapThrashAttribution(rd)

	if st.changed {
		switch {
		case st.cur == swapThrashUnderPressure && st.prev < swapThrashUnderPressure:
			w.logf("[miner][thrash] memory pressure rising: %s\n", swapThrashDetailLine(rt, rd, attr))
		case st.cur >= swapThrashThrashing && st.prev < swapThrashThrashing:
			switch attr {
			case "unit":
				w.notef("%s\n", swapThrashOnsetMsg(rt, rd, share, shareOK))
			case "other":
				w.notef("%s\n", swapThrashOtherMsg(rd, share))
			default:
				w.notef("%s\n", swapThrashUnknownMsg())
			}
		case st.cur == swapThrashCritical && st.prev == swapThrashThrashing:
			w.critf("[thrash] still thrashing %s after onset (%s)\n",
				roundSwapThrashDur(st.condDur), swapThrashDetailLine(rt, rd, attr))
		case st.cur == swapThrashThrashing && st.prev == swapThrashCritical:
			w.logf("[miner][thrash] pressure easing (critical -> thrashing): %s\n", swapThrashDetailLine(rt, rd, attr))
		case st.cur < swapThrashThrashing && st.prev >= swapThrashThrashing:
			// The one-step relax never lands thrashing->calm directly, so the
			// clearance sentence belongs to LEAVING the thrashing state,
			// whichever calmer state it steps down to.
			w.notef("%s\n", swapThrashClearedMsg("Thrash cleared", rd, rt))
			w.lastActionCode = "" // a new episode must be able to alert again
		case st.cur < swapThrashThrashing && st.prev >= swapThrashUnderPressure:
			w.logf("[miner][thrash] memory pressure settling: %s\n", swapThrashDetailLine(rt, rd, attr))
		}
	}

	// Early warning: heap over its limit while the machine is actually
	// swapping (a floor, not one page/s of ambient host swap).
	if !w.machine.heapWarned && w.machine.state < swapThrashThrashing && rd.heapOK && rd.heapFrac >= 1.0 &&
		rt.swapOK && (rt.swapInPS+rt.swapOutPS) >= swapThrashEarlyWarnSwapPS {
		w.notef("%s\n", swapThrashEarlyWarnMsg(rd))
		w.machine.heapWarned = true
	}
	if w.machine.state == swapThrashCalm {
		w.machine.heapWarned = false
	}

	// Swap-nearly-full: OOM-imminent, distinct and loud, once per episode.
	if rd.hostSwapOK && rd.hostSwapTotalMiB > 0 {
		usedFrac := float64(rd.hostSwapUsedMiB) / float64(rd.hostSwapTotalMiB)
		if usedFrac >= swapThrashSwapWarnFrac && !w.machine.swapCritWarned {
			w.critf("[thrash] swap is %.0f%% full (%s of %s used) — the box is nearly out of memory entirely\n",
				usedFrac*100, fmtSwapThrashMiB(rd.hostSwapUsedMiB), fmtSwapThrashMiB(rd.hostSwapTotalMiB))
			w.machine.swapCritWarned = true
		} else if usedFrac < 0.9 {
			w.machine.swapCritWarned = false
		}
	}

	// Publish status every tick (lock-free: atomics + one atomic rename). The
	// restart ring is read here so `restarts_24h` always describes the same
	// window the escalation gate below just judged.
	restarts := len(thrashRestartsWithin(readThrashCapState(), now))
	snap := buildSwapThrashSnapshot(&w.machine, rd, rt, attr, share, shareOK, restarts, w.lastAction, now)
	w.snapshotPtr.Store(snap)
	writeSwapThrashStatusFile(w.statusPath, snap)

	// Escalation: throttled attempts while the condition ACTUALLY holds — the
	// machine sits in thrashing through its relax window, and a denied earlier
	// attempt must not land on an already-recovered box. A denied attempt does
	// not burn the restart ring; only a restart does.
	if st.cond && (w.machine.state == swapThrashThrashing || w.machine.state == swapThrashCritical) &&
		now.Sub(w.machine.lastAttempt) >= swapThrashRetryInterval {
		w.machine.lastAttempt = now
		esc := w.escalate(now, rt, rd, attr, share, shareOK)
		switch esc.Action {
		case "restart":
			// Evidence before the exit: the bundle is what the next process
			// cannot reconstruct (the sensors, the rings, the logs that rotate).
			// The capture is bounded by incidentThrashWait, so the restart
			// contract — the exit status — still holds on a hung state
			// directory.
			w.evidence(esc, rt, rd, attr)
			// 75 alone cannot tell this planned memory restart from the
			// network sign-in quarantine, which shares the code. Mark the
			// exit for this pid immediately before taking it, so a
			// supervising `provide --all-operators` names the right cause
			// (miner/operators_supervisor.go operatorChildThrashExited).
			// The restart record is not that marker: it is also written
			// mid-life by the liveness path and by a persist goroutine
			// that outlives its timeout.
			//
			// exitOnRestart is the shared pre-exit sequence: the reason line
			// goes to stderr only (never events.log in the state directory
			// whose hang this escalation exists for) and the status clear and
			// the marker write are bounded, so the exit is always taken.
			w.exitOnRestart("[thrash]", "%s", esc.Msg)
			// A test seam replaced the exit: stop sampling cleanly instead of
			// escalating again from a process that was told to die.
			w.stopped = true
		case "alert":
			// Dedupe by reason CODE: re-arm reasons embed changing durations,
			// so comparing the text re-alerts every retry.
			if esc.Code != w.lastActionCode {
				w.critf("[thrash] detected but NOT restarting: %s\n", esc.Reason)
				// Same dedupe for the bundle: one per refusal reason, not one
				// every retry interval.
				w.evidence(esc, rt, rd, attr)
				w.lastActionCode = esc.Code
				w.lastAction = esc.Reason
			}
		}
	}
}

// swapThrashSensorLine names the active and unavailable sources for the
// startup line: never log "monitoring" with zero signals.
func swapThrashSensorLine(rd swapThrashRead) (active, missing []string) {
	add := func(name string, ok bool) {
		if ok {
			active = append(active, name)
		} else {
			missing = append(missing, name)
		}
	}
	if rd.psiSomeOK || rd.psiFullOK {
		if rd.psiUnit {
			active = append(active, "psi-mem(unit)")
		} else {
			active = append(active, "psi-mem(host)")
		}
	} else {
		missing = append(missing, "psi-mem")
	}
	if rd.swapOK {
		if rd.swapUnit {
			active = append(active, "swap-activity(unit)")
		} else {
			active = append(active, "swap-activity(host)")
		}
	} else {
		missing = append(missing, "swap-activity")
	}
	add("refaults", rd.refaultOK)
	add("pgscan_direct", rd.pgscanOK)
	add("unit-swap", rd.unitSwapOK)
	add("swap-total", rd.hostSwapOK)
	add("ram", rd.ramAvailOK)
	add("ram-total", rd.ramTotalOK)
	add("heap", rd.heapOK)
	return active, missing
}

// swapThrashDetailLine is the compact diagnostic line for the log.
func swapThrashDetailLine(rt swapThrashRates, rd swapThrashRead, attr string) string {
	stall := "?"
	if v, ok := swapThrashStallFrac(rt); ok {
		stall = fmt.Sprintf("%.0f%%", v*100)
	}
	swap := "?"
	if rt.swapOK {
		swap = fmtSwapThrashMBs(rt.swapInPS + rt.swapOutPS)
	}
	heapTxt := "?"
	if rd.heapOK {
		heapTxt = fmt.Sprintf("%.2f", rd.heapFrac)
	}
	ramTxt := "?"
	if rd.ramAvailOK {
		ramTxt = fmtSwapThrashMiB(rd.ramAvailMiB)
	}
	return fmt.Sprintf("psi-mem full %s, swap %s, heap %s, ram %s, size-attr %s",
		stall, swap, heapTxt, ramTxt, attr)
}

// roundSwapThrashDur keeps a duration readable in one log line.
func roundSwapThrashDur(d time.Duration) time.Duration {
	return d.Round(time.Second)
}
