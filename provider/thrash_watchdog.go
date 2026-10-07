//go:build linux

package provider

// thrash_watchdog.go is Phase 2a of the LA7 incident response: swap-thrash
// detection and response, built as the seed of the unified self-healing
// supervisor's lock-free core (docs/design/self-healing-supervisor.md, PR
// #784; tracking issue #785 step 2 — "fold the reload watchdog and the thrash
// responder in as the component-restart rung").
//
// Design rules (spec: scratch/la7-incident-20261006/phase2a-thrash-detection-spec.md):
//   - lock-free relative to every lock a wedge can hold: no proxy lock, no
//     reload mutex, no JWT store lock. Atomics + best-effort file writes only,
//     so the responder survives the disease it responds to.
//   - detect ACTIVITY, not level: swap usage alone is not thrash.
//   - human-readable output is a hard requirement: every state change logs one
//     plain sentence with human units, who holds the memory, and what happens
//     next (no decoding).
//   - ladder (IP-preserving): freeze growth -> supervised restart -> escape +
//     remember (thrash cap). No proxy shedding here (that is the supervisor's
//     later rung d).
//   - sensing and status are always visible; ESCALATION is gated on self-heal
//     and on a real supervisor.
//
// The action is a plain SUPERVISED restart (non-zero exit; systemd
// Restart=on-failure), never a hot-swap: hot-swapping spawns a child next to
// the multi-GB-swapped parent and bypasses the CLI's memory-headroom
// preflight, exactly when headroom is gone.
//
// The cgroup counter parsers and reset-safe delta pattern follow the parked
// "heal-sensors" work (scratch/heal-sensors-stopped.patch) that was absorbed
// into this build; the sensor set here extends it with PSI total= deltas and
// per-unit swap counters, per the review findings.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	thrashSampleInterval = 30 * time.Second
	// thrashSustain is how long the mild thrash condition must hold before
	// escalation-grade thrashing is declared.
	thrashSustain = 3 * time.Minute
	// thrashSevereSustain shortens the wait once PSI full is severe (>=25%).
	thrashSevereSustain = 90 * time.Second
	// thrashCalmRelax is the sustained calm required to step down ONE state.
	thrashCalmRelax = 5 * time.Minute
	// thrashCriticalAfter marks a thrash that is not resolving by itself.
	thrashCriticalAfter = 15 * time.Minute
	// thrashRetryInterval throttles escalation attempts while thrashing
	// continues (denied attempts do not burn the cap; only restarts do).
	thrashRetryInterval = 10 * time.Minute
	// thrashRecoveryCheckAfter is how long a restarted process watches before
	// it reports the previous restart's outcome.
	thrashRecoveryCheckAfter = 5 * time.Minute

	thrashNowFn = time.Now
	// thrashExitFn is the process exit used for the supervised restart; a test
	// seam, production always os.Exit.
	thrashExitFn = os.Exit
)

const (
	// PSI memory full: fraction of wall time ALL tasks were stalled on memory.
	thrashSevereFrac = 0.25
	thrashMildFrac   = 0.10
	// When NOTHING corroborates (no swap counters, no refaults, no reclaim),
	// the mild band needs a stricter PSI-only bar — never AND with an
	// unavailable signal, and never treat absence of corroborators as absence
	// of thrash.
	thrashMildNoCorroboratorFrac = 0.20

	// Corroboration thresholds (pages/sec) for the 10-25% band.
	thrashSwapCorroboratePS    = 250.0
	thrashRefaultCorroboratePS = 50.0
	thrashPgscanCorroboratePS  = 100.0

	// thrashAttributionShare is the share of used swap the unit must hold for
	// the restart to be ours to take.
	thrashAttributionShare = 0.5

	// thrashSwapWarnFrac triggers the distinct "swap nearly full" note.
	thrashSwapWarnFrac = 0.95

	// thrashExitCode is the deliberate non-zero exit the service manager
	// (Restart=on-failure) restarts.
	thrashExitCode = 75 // EX_TEMPFAIL: temporary failure, restart is the response
)

// thrashFreeze is the freeze-growth rung (ladder rung a): set while the box
// is under memory pressure at or above under-pressure, read by the pool
// controller (which may still SHRINK; growth is what freezes). Atomic: the
// watchdog never takes a lock.
var thrashFreeze atomic.Bool

// ---------------------------------------------------------------------------
// Counter delta plumbing (reset-safe; follows the parked heal-sensors patch)
// ---------------------------------------------------------------------------

// pressureCounterDelta is the previous observation of one cumulative kernel
// counter, so the next observation yields a per-second rate. Reset-safe: a
// counter that moved backwards (reboot, cgroup recreation, rollover) or a
// fresh baseline reports 0 rather than underflowing into a bogus giant rate —
// exactly the skip-first-delta semantics the spec requires.
type pressureCounterDelta struct {
	have  bool
	at    time.Time
	value uint64
}

// rate records cur and returns its non-negative per-second delta against the
// previous observation.
func (d *pressureCounterDelta) rate(cur uint64, now time.Time) float64 {
	prev, prevAt, had := d.value, d.at, d.have
	d.have, d.at, d.value = true, now, cur
	if !had || !now.After(prevAt) || cur < prev {
		return 0
	}
	return float64(cur-prev) / now.Sub(prevAt).Seconds()
}

// parsePSITotals extracts the cumulative total= microseconds for the some
// and full lines of a PSI pressure file (cgroup v2 memory.pressure or
// /proc/pressure/memory). ok=false per line when that line or its total= is
// missing, so the caller can report the signal unavailable instead of
// inventing a zero.
func parsePSITotals(content string) (someTotal, fullTotal uint64, someOK, fullOK bool) {
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

// parseVMStatSwap extracts the cumulative pswpin and pswpout page counters
// from /proc/vmstat content. ok=false when either counter is missing, so the
// caller reports the sensor unavailable rather than a fake zero.
func parseVMStatSwap(content string) (pswpin, pswpout uint64, ok bool) {
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

// parseVmstatCounter extracts one arbitrary counter from /proc/vmstat content
// (used for host-level pgscan_direct when the unit's memory.stat lacks it).
func parseVmstatCounter(content, key string) (uint64, bool) {
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

// parseMemoryStatCounter extracts one counter from cgroup v2 memory.stat
// content.
func parseMemoryStatCounter(content, key string) (uint64, bool) {
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

// parseMeminfoSwap extracts SwapTotal/SwapFree (MiB) from /proc/meminfo
// content. ok=false when either is missing.
func parseMeminfoSwap(content string) (totalMiB, freeMiB int64, ok bool) {
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

// cgroupV2SelfDir resolves this process's own cgroup v2 directory the same
// way connectx does: the "0::" line of /proc/self/cgroup, rooted at
// /sys/fs/cgroup. Empty when unavailable. Unlike the OOM counter (which must
// read an ancestor that survives restarts), PSI and swap are read from the
// OWN cgroup: they describe exactly this service's subtree.
func cgroupV2SelfDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		if rel == "" {
			rel = "/"
		}
		return filepath.Join("/sys/fs/cgroup", rel)
	}
	return ""
}

// ---------------------------------------------------------------------------
// One reading: counters + levels
// ---------------------------------------------------------------------------

// thrashRead is one best-effort reading of every thrash source. Each group
// carries its own ok flag; a missing source is "unavailable", never 0.
type thrashRead struct {
	psiSomeTotal, psiFullTotal uint64
	psiSomeOK, psiFullOK       bool
	psiUnit                    bool // read from the unit's cgroup (preferred)

	swapIn, swapOut uint64
	swapOK          bool
	swapUnit        bool // unit memory.stat counters (kernel-dependent)

	refault   uint64
	refaultOK bool

	pgscan   uint64
	pgscanOK bool

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

// readRAMTotalMiB returns the effective RAM in MiB: the cgroup-aware limit
// the pressure system already uses, else /proc/meminfo MemTotal.
func readRAMTotalMiB() (int64, bool) {
	if n := detectEffectiveRAMLimitBytes(); n > 0 {
		return n >> 20, true
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return n / 1024, true
			}
		}
	}
	return 0, false
}

// readThrashRead assembles one reading. Unit-level sources are preferred
// (they describe this service); host sources are the fallback and are what
// makes detection work on kernels without unit swap counters.
func readThrashRead() thrashRead {
	var rd thrashRead
	unitDir := cgroupV2SelfDir()

	// PSI memory: prefer the unit's memory.pressure, fall back to
	// /proc/pressure/memory. Both are parsed as total= deltas.
	if unitDir != "" {
		if b, err := os.ReadFile(filepath.Join(unitDir, "memory.pressure")); err == nil {
			rd.psiSomeTotal, rd.psiFullTotal, rd.psiSomeOK, rd.psiFullOK = parsePSITotals(string(b))
			rd.psiUnit = rd.psiSomeOK || rd.psiFullOK
		}
	}
	if !rd.psiUnit {
		if b, err := os.ReadFile("/proc/pressure/memory"); err == nil {
			rd.psiSomeTotal, rd.psiFullTotal, rd.psiSomeOK, rd.psiFullOK = parsePSITotals(string(b))
		}
	}

	// Unit memory.stat: swap counters (kernel-dependent), refaults, direct
	// reclaim. One read serves all three.
	var memstat string
	var haveMemstat bool
	if unitDir != "" {
		if b, err := os.ReadFile(filepath.Join(unitDir, "memory.stat")); err == nil {
			memstat, haveMemstat = string(b), true
		}
	}

	// Swap activity: unit counters when the kernel has them, else host
	// /proc/vmstat. Never a fake zero.
	if haveMemstat {
		in, inOK := parseMemoryStatCounter(memstat, "pswpin")
		out, outOK := parseMemoryStatCounter(memstat, "pswpout")
		if inOK && outOK {
			rd.swapIn, rd.swapOut, rd.swapOK, rd.swapUnit = in, out, true, true
		}
	}
	var vmstat string
	var haveVmstat bool
	if !rd.swapOK {
		if b, err := os.ReadFile("/proc/vmstat"); err == nil {
			vmstat, haveVmstat = string(b), true
			if in, out, ok := parseVMStatSwap(vmstat); ok {
				rd.swapIn, rd.swapOut, rd.swapOK = in, out, true
			}
		}
	}

	// Refaults: our own anonymous pages read back in — direct thrash evidence.
	if haveMemstat {
		if v, ok := parseMemoryStatCounter(memstat, "workingset_refault_anon"); ok {
			rd.refault, rd.refaultOK = v, true
		}
	}

	// Direct reclaim: unit pgscan_direct, else host vmstat.
	if haveMemstat {
		if v, ok := parseMemoryStatCounter(memstat, "pgscan_direct"); ok {
			rd.pgscan, rd.pgscanOK = v, true
		}
	}
	if !rd.pgscanOK {
		if !haveVmstat {
			if b, err := os.ReadFile("/proc/vmstat"); err == nil {
				vmstat, haveVmstat = string(b), true
			}
		}
		if haveVmstat {
			if v, ok := parseVmstatCounter(vmstat, "pgscan_direct"); ok {
				rd.pgscan, rd.pgscanOK = v, true
			}
		}
	}

	// Unit swap LEVEL: is this unit the swapped-out one?
	if unitDir != "" {
		if b, err := os.ReadFile(filepath.Join(unitDir, "memory.swap.current")); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n >= 0 {
				rd.unitSwapMiB, rd.unitSwapOK = n>>20, true
			}
		}
	}

	// Host swap: used and total, from /proc/meminfo.
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		if total, free, ok := parseMeminfoSwap(string(b)); ok {
			rd.hostSwapTotalMiB, rd.hostSwapOK = total, true
			rd.hostSwapUsedMiB = total - free
		}
	}

	// RAM availability/total (independent signals: show whichever is readable).
	if avail := hostAvailMiB(); avail >= 0 {
		rd.ramAvailMiB, rd.ramAvailOK = avail, true
	}
	if total, ok := readRAMTotalMiB(); ok {
		rd.ramTotalMiB, rd.ramTotalOK = total, true
	}

	// Heap vs its soft limit (the same numerator the GC governor uses).
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit == 1<<63-1 {
		limit = detectEffectiveRAMLimitBytes()
	}
	if used := readLiveHeapBytes() + readStackBytes(); used > 0 && limit > 0 {
		rd.heapFrac = float64(used) / float64(limit)
		rd.heapUsedMiB, rd.heapLimitMiB, rd.heapOK = int64(used>>20), limit>>20, true
	}

	return rd
}

// ---------------------------------------------------------------------------
// Rates
// ---------------------------------------------------------------------------

// thrashTracker carries the previous counters between samples.
type thrashTracker struct {
	someTotal, fullTotal, swapIn, swapOut, refault, pgscan pressureCounterDelta
}

// thrashRates is the derived per-second view of one reading. PSI totals are
// microseconds: fullFrac/someFrac are fractions of WALL time (0..1) computed
// over REAL elapsed time, not avg60 and not sample counts (a swapped-out
// ticker can slip 90s+ between samples and the math must survive that).
type thrashRates struct {
	fullFrac, someFrac  float64
	fullOK, someOK      bool
	swapInPS, swapOutPS float64
	swapOK              bool
	refaultPS           float64
	refaultOK           bool
	pgscanPS            float64
	pgscanOK            bool
}

// rates converts one reading into rates, updating the tracker.
func (tr *thrashTracker) rates(rd thrashRead, now time.Time) thrashRates {
	var r thrashRates
	if rd.psiSomeOK {
		r.someFrac, r.someOK = tr.someTotal.rate(rd.psiSomeTotal, now)/1e6, true
	}
	if rd.psiFullOK {
		r.fullFrac, r.fullOK = tr.fullTotal.rate(rd.psiFullTotal, now)/1e6, true
	}
	if rd.swapOK {
		r.swapInPS = tr.swapIn.rate(rd.swapIn, now)
		r.swapOutPS = tr.swapOut.rate(rd.swapOut, now)
		r.swapOK = true
	}
	if rd.refaultOK {
		r.refaultPS, r.refaultOK = tr.refault.rate(rd.refault, now), true
	}
	if rd.pgscanOK {
		r.pgscanPS, r.pgscanOK = tr.pgscan.rate(rd.pgscan, now), true
	}
	return r
}

// thrashStallFrac prefers the PSI full fraction (all tasks) and falls back to
// some (at least one) when full is unreadable.
func thrashStallFrac(rt thrashRates) (float64, bool) {
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

// thrashStateT follows the unified supervisor's state names.
type thrashStateT int

const (
	thrashCalm thrashStateT = iota
	thrashUnderPressure
	thrashThrashing
	thrashCritical
)

func (s thrashStateT) String() string {
	switch s {
	case thrashUnderPressure:
		return "under-pressure"
	case thrashThrashing:
		return "thrashing"
	case thrashCritical:
		return "critical"
	}
	return "calm"
}

// thrashMachine is the time-based hysteresis. Rise fast on evidence;
// relax one step per sustained-calm window.
type thrashMachine struct {
	state          thrashStateT
	stateSince     time.Time
	worseSince     time.Time
	severeSince    time.Time
	calmSince      time.Time
	lastAttempt    time.Time // throttles escalation attempts
	episode        int       // increments each entry into thrashing
	heapWarned     bool
	swapCritWarned bool
}

// thrashStep is one machine advance.
type thrashStep struct {
	prev, cur thrashStateT
	changed   bool
	condDur   time.Duration // how long the worsening condition has held
}

// step advances the machine. Pure except for the machine's own bookkeeping:
// all time comes from now, so tests drive it with a fake clock.
func (m *thrashMachine) step(now time.Time, rt thrashRates, rd thrashRead) thrashStep {
	prev := m.state

	severe := rt.fullOK && rt.fullFrac >= thrashSevereFrac
	swapCorr := rt.swapOK && (rt.swapInPS+rt.swapOutPS) >= thrashSwapCorroboratePS
	refCorr := rt.refaultOK && rt.refaultPS >= thrashRefaultCorroboratePS
	pgCorr := rt.pgscanOK && rt.pgscanPS >= thrashPgscanCorroboratePS
	heapCorrAvail := rd.heapOK && rd.unitSwapOK
	heapCorr := heapCorrAvail && rd.heapFrac >= 1.0 && rd.unitSwapMiB > 0
	corr := swapCorr || refCorr || pgCorr || heapCorr
	corrAvail := rt.swapOK || rt.refaultOK || rt.pgscanOK || heapCorrAvail

	mild := rt.fullOK && rt.fullFrac >= thrashMildFrac
	strict := rt.fullOK && rt.fullFrac >= thrashMildNoCorroboratorFrac

	// thrashCond: severe alone is sufficient; the mild band needs
	// corroboration when available, and a stricter PSI-only bar when nothing
	// corroborates (never AND with an unavailable signal).
	thrashCond := severe || (mild && corr) || (!corrAvail && strict)

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

	desired := thrashCalm
	switch {
	case severe && severeDur >= thrashSevereSustain:
		desired = thrashThrashing
	case thrashCond && condDur >= thrashSustain:
		desired = thrashThrashing
	case thrashCond:
		desired = thrashUnderPressure
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
		if now.Sub(m.calmSince) >= thrashCalmRelax {
			target = m.state - 1
			m.calmSince = time.Time{}
		}
	default:
		target = m.state
	}

	// A thrash that persists is no longer something the box works through.
	if target == thrashThrashing && m.state == thrashThrashing &&
		now.Sub(m.stateSince) >= thrashCriticalAfter {
		target = thrashCritical
	}

	if target != m.state {
		m.state = target
		m.stateSince = now
		if target == thrashThrashing {
			m.episode++
		}
	}
	return thrashStep{prev: prev, cur: m.state, changed: prev != m.state, condDur: condDur}
}

// thrashAttribution decides whose memory is being swapped. ours only when
// the unit holds >= half of used swap, or when the per-unit PSI (this
// service's own subtree) shows the stalls and no better counter exists.
// Unknown attribution never restarts: never restart someone else's thrash.
func thrashAttribution(rd thrashRead) (attr string, share float64, shareOK bool) {
	if rd.unitSwapOK && rd.hostSwapOK && rd.hostSwapUsedMiB > 0 {
		s := float64(rd.unitSwapMiB) / float64(rd.hostSwapUsedMiB)
		if s >= thrashAttributionShare {
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

// fmtMiBHuman renders MiB in human units: "3.1 GB" / "512 MB".
func fmtMiBHuman(mib int64) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MB", mib)
}

// fmtSwapMBs renders a pages/sec rate as MB/s (4 KiB pages).
func fmtSwapMBs(pps float64) string {
	return fmt.Sprintf("%.0f MB/s", pps/256.0)
}

func thrashOnsetMsg(condDur time.Duration, rt thrashRates, rd thrashRead, share float64, shareOK bool) string {
	stallTxt := "stalled on memory"
	if stall, ok := thrashStallFrac(rt); ok {
		stallTxt = fmt.Sprintf("stalled on memory ~%.0f%% of the time", stall*100)
	}
	held := "an unmeasurable amount of the swap"
	switch {
	case shareOK:
		held = fmt.Sprintf("%s (%.0f%% of all swap in use)", fmtMiBHuman(rd.unitSwapMiB), share*100)
	case rd.unitSwapOK:
		held = fmt.Sprintf("%s of swap", fmtMiBHuman(rd.unitSwapMiB))
	}
	heapTxt := ""
	if rd.heapOK {
		heapTxt = fmt.Sprintf(" Heap is %.1fx its limit.", rd.heapFrac)
	}
	return fmt.Sprintf("🚨 [memory] The box is out of RAM and thrashing swap — for the last %s it was %s, it is swapping ~%s, and %s belongs to this provider.%s",
		roundDur(condDur), stallTxt, fmtSwapMBs(rt.swapInPS+rt.swapOutPS), held, heapTxt)
}

func thrashOtherMsg(rd thrashRead, share float64) string {
	return fmt.Sprintf("🚨 [memory] The box is thrashing swap, but the memory belongs to another process: this provider holds %s of the %s swapped (%.0f%%). Not restarting.",
		fmtMiBHuman(rd.unitSwapMiB), fmtMiBHuman(rd.hostSwapUsedMiB), share*100)
}

func thrashUnknownMsg() string {
	return "🚨 [memory] The box may be thrashing swap but the memory cannot be attributed to this provider (no per-unit swap counters, PSI reads are host-wide). Not restarting; check the box by hand."
}

func thrashEarlyWarnMsg(rd thrashRead) string {
	return fmt.Sprintf("⚠️ [memory] The program's heap is %.1fx its size limit (%s of %s) and the machine is swapping. If the heap keeps growing the box will start thrashing. %s RAM free.",
		rd.heapFrac, fmtMiBHuman(rd.heapUsedMiB), fmtMiBHuman(rd.heapLimitMiB), fmtMiBHuman(rd.ramAvailMiB))
}

func thrashActionMsg(restartN, maxN, running, cap int) string {
	capTxt := "without a proxy cap change"
	if cap > 0 {
		capTxt = fmt.Sprintf("with the proxy cap reduced %d -> %d", running, cap)
	}
	return fmt.Sprintf("🚨 [memory] This will not recover on its own. Restarting the provider now (restart %d of max %d per day) %s so the next run fits in RAM.",
		restartN, maxN, capTxt)
}

func thrashClearedMsg(prefix string, rd thrashRead, rt thrashRates) string {
	parts := []string{fmt.Sprintf("%s RAM free", fmtMiBHuman(rd.ramAvailMiB))}
	if rd.hostSwapOK {
		parts = append(parts, fmt.Sprintf("%s swap in use", fmtMiBHuman(rd.hostSwapUsedMiB)))
	}
	if stall, ok := thrashStallFrac(rt); ok {
		parts = append(parts, fmt.Sprintf("memory stalls %.0f%%", stall*100))
	}
	return fmt.Sprintf("✅ [memory] %s: %s.", prefix, strings.Join(parts, ", "))
}

// ---------------------------------------------------------------------------
// Published snapshot + status file
// ---------------------------------------------------------------------------

// thrashSnapshot is the published, aggregator-ready view (fixed field names;
// the deferred heal-status surface wraps these, it does not re-derive them).
type thrashSnapshot struct {
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

var globalThrashSnap atomic.Pointer[thrashSnapshot]

func ptrF(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}
func ptrI(v int64, ok bool) *int64 {
	if !ok {
		return nil
	}
	return &v
}

func buildThrashSnapshot(m *thrashMachine, rd thrashRead, rt thrashRates, attr string, share float64, shareOK bool, restarts int, lastAction string, now time.Time) *thrashSnapshot {
	s := &thrashSnapshot{
		State:       m.state.String(),
		SinceUnix:   m.stateSince.Unix(),
		Attribution: attr,
		LastAction:  lastAction,
		Restarts24h: restarts,
		Updated:     now.UTC().Format(time.RFC3339),
	}
	s.PSIFull = ptrF(rt.fullFrac, rt.fullOK)
	s.SomeFrac = ptrF(rt.someFrac, rt.someOK)
	if rt.swapOK {
		s.SwapIOPS = ptrF(rt.swapInPS+rt.swapOutPS, true)
	}
	s.UnitSwapMiB = ptrI(rd.unitSwapMiB, rd.unitSwapOK)
	s.HostSwapUsedMiB = ptrI(rd.hostSwapUsedMiB, rd.hostSwapOK)
	s.HostSwapTotalMiB = ptrI(rd.hostSwapTotalMiB, rd.hostSwapOK)
	s.RAMAvailMiB = ptrI(rd.ramAvailMiB, rd.ramAvailOK)
	s.RAMTotalMiB = ptrI(rd.ramTotalMiB, rd.ramTotalOK)
	s.HeapFrac = ptrF(rd.heapFrac, rd.heapOK)
	s.UnitShare = ptrF(share, shareOK)
	s.Summary = thrashSummaryOf(s)
	return s
}

// thrashSummaryOf renders the one-sentence summary carried in the status
// files. Thrash states lead with what is happening and who holds the memory.
func thrashSummaryOf(s *thrashSnapshot) string {
	switch s.State {
	case "thrashing", "critical":
		stall := "stalled"
		if s.PSIFull != nil {
			stall = fmt.Sprintf("the box was stalled on memory ~%.0f%% of the time", *s.PSIFull*100)
		}
		swapTxt := ""
		if s.SwapIOPS != nil {
			swapTxt = fmt.Sprintf(", swapping ~%s", fmtSwapMBs(*s.SwapIOPS))
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
		parts = append(parts, fmt.Sprintf("%s RAM free", fmtMiBHuman(*s.RAMAvailMiB)))
	}
	if s.SwapIOPS != nil && *s.SwapIOPS > 0 {
		parts = append(parts, fmt.Sprintf("swapping %s", fmtSwapMBs(*s.SwapIOPS)))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("memory %s (no signals readable)", s.State)
	}
	return fmt.Sprintf("memory %s: %s.", s.State, strings.Join(parts, "; "))
}

// thrashStatusPath is the stable file the later heal-status aggregator reads.
func thrashStatusPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "thrash_status"), nil
}

func writeThrashStatusFile(s *thrashSnapshot) {
	path, err := thrashStatusPath()
	if err != nil {
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func clearThrashStatusFile() {
	if path, err := thrashStatusPath(); err == nil {
		_ = os.Remove(path)
	}
}

// Accessors for the pressure_status writer (atomic reads only).
func thrashStateName() string {
	if s := globalThrashSnap.Load(); s != nil {
		return s.State
	}
	return ""
}

func thrashPSIFullForStatus() (float64, bool) {
	if s := globalThrashSnap.Load(); s != nil && s.PSIFull != nil {
		return *s.PSIFull, true
	}
	return 0, false
}

func thrashSwapIOForStatus() (float64, bool) {
	if s := globalThrashSnap.Load(); s != nil && s.SwapIOPS != nil {
		return *s.SwapIOPS, true
	}
	return 0, false
}

func thrashSummaryForStatus() string {
	if s := globalThrashSnap.Load(); s != nil {
		return s.Summary
	}
	return ""
}

// pressureSummaryOf renders the operator-facing summary for the
// pressure_status file: one sentence, human units, no decoding required
// (thrash states reuse the thrash snapshot's summary, which is written by
// the watchdog). The worst component names the driver in words.
func pressureSummaryOf(score float64, comps map[string]float64) string {
	if s := globalThrashSnap.Load(); s != nil && s.Summary != "" &&
		(s.State == "thrashing" || s.State == "critical") {
		return fmt.Sprintf("memory pressure %.2f — %s", score, s.Summary)
	}
	best, bestV := "", 0.0
	for k, v := range comps {
		if v > bestV {
			best, bestV = k, v
		}
	}
	snap := globalThrashSnap.Load()
	var facts []string
	if snap != nil {
		if snap.HeapFrac != nil {
			facts = append(facts, fmt.Sprintf("heap %.1fx its soft limit", *snap.HeapFrac))
		}
		if snap.RAMAvailMiB != nil {
			facts = append(facts, fmt.Sprintf("%s RAM free", fmtMiBHuman(*snap.RAMAvailMiB)))
		}
		if snap.SwapIOPS != nil && *snap.SwapIOPS > 0 {
			facts = append(facts, fmt.Sprintf("swapping %s", fmtSwapMBs(*snap.SwapIOPS)))
		}
	}
	joined := strings.Join(facts, "; ")
	if bestV < 0.5 {
		if joined == "" {
			return fmt.Sprintf("system calm (pressure %.2f)", score)
		}
		return fmt.Sprintf("system calm (pressure %.2f); %s", score, joined)
	}
	driver, prefix := map[string]string{
		"heap":    "the program's heap is over its soft limit",
		"mem":     "the machine is low on free RAM",
		"psi_mem": "tasks are stalling on memory",
		"psi_cpu": "tasks are stalling on CPU",
		"psi_io":  "tasks are stalling on disk IO",
		"goro":    "goroutine growth is high",
		"load":    "system load is high",
		"fd":      "file descriptors are running out",
	}[best], "system pressure"
	if driver == "" {
		driver, prefix = "pressure is elevated", "system pressure"
	} else if best == "heap" || best == "mem" || best == "psi_mem" {
		prefix = "memory pressure"
	}
	if joined != "" {
		return fmt.Sprintf("%s %.2f — %s (%s)", prefix, score, driver, joined)
	}
	return fmt.Sprintf("%s %.2f — %s", prefix, score, driver)
}

// ---------------------------------------------------------------------------
// Escalation
// ---------------------------------------------------------------------------

// thrashEscalation is the decision of one escalation attempt.
type thrashEscalation struct {
	Action   string // "restart" | "alert"
	Msg      string
	Reason   string // denial reason for alerts
	Cap      int
	Running  int
	Restarts int // restarts in the last 24h after this one
}

func thrashEscalationAlert(reason string) thrashEscalation {
	return thrashEscalation{Action: "alert", Reason: reason}
}

// thrashEscalate runs the full gate stack once. Pure aside from the cap file
// write, the ledger entry and env reads; the caller owns the log + exit.
func thrashEscalate(now time.Time, rt thrashRates, rd thrashRead, selfHeal bool, attr string, share float64, shareOK bool) thrashEscalation {
	if !resolveSelfHealEnabled(selfHeal) {
		return thrashEscalationAlert("self-heal is off (URNETWORK_SELF_HEAL / proxy_self_heal), so no automatic restart runs; the operator must act")
	}
	if os.Getenv(EnvHotSwap) == "1" {
		return thrashEscalationAlert("a hot-swap is in flight; not restarting")
	}
	switch attr {
	case "other":
		return thrashEscalationAlert(fmt.Sprintf("the swap belongs to another process: this provider holds %s of %s swapped (%.0f%%); never restarting someone else's thrash",
			fmtMiBHuman(rd.unitSwapMiB), fmtMiBHuman(rd.hostSwapUsedMiB), share*100))
	case "unknown":
		return thrashEscalationAlert("the swap cannot be attributed to this provider and no per-unit PSI is readable; not restarting blind")
	}
	if os.Getenv("INVOCATION_ID") == "" && os.Getenv("NOTIFY_SOCKET") == "" {
		return thrashEscalationAlert("not running under a service supervisor (systemd), so a self-exit would not be restarted; not restarting")
	}
	st := readThrashCapState()
	allowed, reason, n := thrashCapEscalationAllowed(st, now)
	if !allowed {
		return thrashEscalationAlert(reason)
	}
	running := runningProxyCountForPressure()
	cap := thrashCapForNextStart(running)
	if cap > 0 {
		if err := recordThrashEscalation(cap, now); err != nil {
			tlog("[proxy][thrash] warn: could not persist thrash cap: %v\n", err)
		}
	}
	stall := 0.0
	if v, ok := thrashStallFrac(rt); ok {
		stall = v
	}
	ledgerRecord(ledgerEntry{
		Actor:  "thrash",
		Action: "restart",
		From:   running,
		To:     cap,
		Mode:   "on",
		Reason: fmt.Sprintf("swap thrash: %.0f%% of wall time stalled on memory, swapping %s", stall*100, fmtSwapMBs(rt.swapInPS+rt.swapOutPS)),
	})
	return thrashEscalation{
		Action:   "restart",
		Msg:      thrashActionMsg(n+1, thrashMaxRestarts24h, running, cap),
		Cap:      cap,
		Running:  running,
		Restarts: n + 1,
	}
}

// ---------------------------------------------------------------------------
// Watchdog loop
// ---------------------------------------------------------------------------

// thrashSensorLine names the active and unavailable sources for the startup
// line — never log "monitoring" with zero signals.
func thrashSensorLine(rd thrashRead) (active, missing []string) {
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

// thrashDetailLine is the compact diagnostic line for the log.
func thrashDetailLine(rt thrashRates, rd thrashRead, attr string) string {
	stall := "?"
	if v, ok := thrashStallFrac(rt); ok {
		stall = fmt.Sprintf("%.0f%%", v*100)
	}
	swap := "?"
	if rt.swapOK {
		swap = fmtSwapMBs(rt.swapInPS + rt.swapOutPS)
	}
	return fmt.Sprintf("psi-mem full %s, swap %s, heap %.2f, ram %s, size-attr %s",
		stall, swap, rd.heapFrac, fmtMiBHuman(rd.ramAvailMiB), attr)
}

// runThrashWatchdog is the supervised loop. Sensing + status always run;
// escalation is gated (self-heal, supervision, attribution, cap, anti-loop).
func runThrashWatchdog(ctx context.Context, selfHealEnabled bool) {
	startedAt := thrashNowFn()
	tr := &thrashTracker{}
	m := &thrashMachine{state: thrashCalm, stateSince: startedAt}

	first := readThrashRead()
	active, missing := thrashSensorLine(first)
	switch {
	case len(active) == 0:
		importantLogf("⚠️ [memory] Thrash watch is INERT on this box: no memory-pressure signals are readable (no PSI, no swap counters). No thrash detection or response is possible here.\n")
	default:
		line := fmt.Sprintf("[proxy][thrash] monitor started, signals: %s", strings.Join(active, ", "))
		if len(missing) > 0 {
			line += fmt.Sprintf(" (unavailable: %s)", strings.Join(missing, ", "))
		}
		tlog("%s\n", line)
	}

	// A previous process left a cap behind: watch for the restart's outcome.
	priorEscalation := false
	if cap, ok := activeThrashCap(thrashNowFn()); ok && cap > 0 {
		priorEscalation = true
	}
	recoveredLogged := !priorEscalation
	clearedThisLife := false

	lastAction := ""
	if priorEscalation {
		lastAction = "restart (previous process)"
	}

	defer func() {
		thrashFreeze.Store(false)
		globalThrashSnap.Store(nil)
		clearThrashStatusFile()
	}()

	ticker := time.NewTicker(thrashSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := thrashNowFn()
		rd := readThrashRead()
		rt := tr.rates(rd, now)
		st := m.step(now, rt, rd)
		attr, share, shareOK := thrashAttribution(rd)

		// Freeze-growth rung: engaged from under-pressure on, released on calm.
		thrashFreeze.Store(m.state >= thrashUnderPressure)

		if st.changed {
			switch {
			case st.cur == thrashUnderPressure && st.prev < thrashUnderPressure:
				tlog("[proxy][thrash] memory pressure rising: %s\n", thrashDetailLine(rt, rd, attr))
			case st.cur >= thrashThrashing && st.prev < thrashThrashing:
				switch attr {
				case "unit":
					importantLogf("%s\n", thrashOnsetMsg(st.condDur, rt, rd, share, shareOK))
				case "other":
					importantLogf("%s\n", thrashOtherMsg(rd, share))
				default:
					importantLogf("%s\n", thrashUnknownMsg())
				}
			case st.cur == thrashCritical && st.prev == thrashThrashing:
				critLog("[thrash] still thrashing %s after onset (%s); restart capability: %v\n",
					roundDur(now.Sub(m.stateSince)), thrashDetailLine(rt, rd, attr), thrashRestartable())
			case st.cur == thrashCalm && st.prev >= thrashThrashing:
				importantLogf("%s\n", thrashClearedMsg("Thrash cleared", rd, rt))
				clearedThisLife = true
			case st.cur < thrashThrashing && st.prev >= thrashUnderPressure:
				tlog("[proxy][thrash] memory pressure settling: %s\n", thrashDetailLine(rt, rd, attr))
			}
		}

		// Early warning: heap over its limit while the machine is swapping.
		if !m.heapWarned && m.state < thrashThrashing && rd.heapOK && rd.heapFrac >= 1.0 &&
			rt.swapOK && (rt.swapInPS+rt.swapOutPS) > 0 {
			importantLogf("%s\n", thrashEarlyWarnMsg(rd))
			m.heapWarned = true
		}
		if m.state == thrashCalm {
			m.heapWarned = false
		}

		// Swap-nearly-full: OOM-imminent, distinct and loud, once per episode.
		if rd.hostSwapOK && rd.hostSwapTotalMiB > 0 {
			usedFrac := float64(rd.hostSwapUsedMiB) / float64(rd.hostSwapTotalMiB)
			if usedFrac >= thrashSwapWarnFrac && !m.swapCritWarned {
				critLog("[thrash] swap is %.0f%% full (%s of %s used) — the box is nearly out of memory entirely\n",
					usedFrac*100, fmtMiBHuman(rd.hostSwapUsedMiB), fmtMiBHuman(rd.hostSwapTotalMiB))
				m.swapCritWarned = true
			} else if usedFrac < 0.9 {
				m.swapCritWarned = false
			}
		}

		// Publish status every tick (lock-free: atomics + one atomic rename).
		restarts := len(thrashRestartsWithin(readThrashCapState(), now))
		snap := buildThrashSnapshot(m, rd, rt, attr, share, shareOK, restarts, lastAction, now)
		globalThrashSnap.Store(snap)
		writeThrashStatusFile(snap)

		// Escalation: throttled attempts while thrashing continues.
		if (m.state == thrashThrashing || m.state == thrashCritical) &&
			now.Sub(m.lastAttempt) >= thrashRetryInterval {
			m.lastAttempt = now
			esc := thrashEscalate(now, rt, rd, selfHealEnabled, attr, share, shareOK)
			switch esc.Action {
			case "restart":
				lastAction = fmt.Sprintf("restart %d/%d, cap %d -> %d", esc.Restarts, thrashMaxRestarts24h, esc.Running, esc.Cap)
				importantLogf("%s\n", esc.Msg)
				thrashExitFn(thrashExitCode)
				return // a test seam replaced thrashExitFn: stop the loop cleanly
			case "alert":
				if esc.Reason != lastAction {
					critLog("[thrash] detected but NOT restarting: %s\n", esc.Reason)
					ledgerRecord(ledgerEntry{Actor: "thrash", Action: "alert", Reason: esc.Reason})
					lastAction = esc.Reason
				}
			}
		}

		// The previous restart's outcome, reported once per process life.
		if !recoveredLogged && !clearedThisLife && m.state == thrashCalm &&
			now.Sub(startedAt) >= thrashRecoveryCheckAfter {
			importantLogf("%s\n", thrashClearedMsg("Thrash cleared by the restart", rd, rt))
			recoveredLogged = true
		}
	}
}

// thrashRestartable reports whether a supervised restart is possible at all
// (used by the critical-state log line).
func thrashRestartable() bool {
	return os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != ""
}
