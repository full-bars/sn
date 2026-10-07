//go:build linux

package provider

// Tests for the Phase 2a thrash watchdog: parse, rate math, state machine,
// attribution, cap ladder, messages and status serialization. All waits are
// time-driven with explicit clocks; no wall-clock assertions.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var thrashT0 = time.Unix(1_800_000_000, 0)

// ---------------------------------------------------------------------------
// Parsers: missing keys are unavailable, never 0
// ---------------------------------------------------------------------------

func TestParsePSITotalsMissingFullIsUnavailable(t *testing.T) {
	some, full, someOK, fullOK := parsePSITotals("some avg10=0.00 avg60=0.00 avg300=0.00 total=111\n")
	if !someOK || some != 111 {
		t.Fatalf("some: got (%d, %v), want (111, true)", some, someOK)
	}
	if fullOK || full != 0 {
		t.Fatalf("missing full line must be unavailable, got (%d, %v)", full, fullOK)
	}
	// full present but without a total= field is still unavailable.
	_, _, _, fullOK = parsePSITotals("some avg10=0.00 total=5\nfull avg10=0.00 avg60=0.00\n")
	if fullOK {
		t.Fatalf("full without total= must be unavailable")
	}
	_, full, _, fullOK = parsePSITotals("some avg10=0 total=5\nfull avg10=0 total=42\n")
	if !fullOK || full != 42 {
		t.Fatalf("full total: got (%d, %v), want (42, true)", full, fullOK)
	}
}

func TestParseVMStatSwapMissingCounterIsUnavailable(t *testing.T) {
	// Partial counters: ok=false (values may be partially filled; callers
	// must gate on ok and report the sensor unavailable, never a fake zero).
	if _, _, ok := parseVMStatSwap("pswpin 100\n"); ok {
		t.Fatalf("only pswpin present: want unavailable")
	}
	in, out, ok := parseVMStatSwap("pswpin 100\npswpout 7\n")
	if !ok || in != 100 || out != 7 {
		t.Fatalf("got (%d, %d, %v), want (100, 7, true)", in, out, ok)
	}
}

func TestParseMemoryStatCounter(t *testing.T) {
	content := "anon 100\npswpin 2\npswpout 0\nworkingset_refault_anon 55\n"
	if v, ok := parseMemoryStatCounter(content, "pswpin"); !ok || v != 2 {
		t.Fatalf("pswpin: got (%d, %v)", v, ok)
	}
	if _, ok := parseMemoryStatCounter(content, "pswpout_missing"); ok {
		t.Fatalf("missing key must be unavailable")
	}
	// The LA7 6.x case: swap keys entirely absent from memory.stat.
	if _, ok := parseMemoryStatCounter("anon 100\nfile 50\n", "pswpin"); ok {
		t.Fatalf("no pswpin key: must be unavailable")
	}
}

func TestParseMeminfoSwap(t *testing.T) {
	total, free, ok := parseMeminfoSwap("MemTotal:  2048000 kB\nSwapTotal: 4194304 kB\nSwapFree:  1048576 kB\n")
	if !ok || total != 4096 || free != 1024 {
		t.Fatalf("got (%d, %d, %v), want (4096, 1024, true)", total, free, ok)
	}
	if _, _, ok := parseMeminfoSwap("SwapTotal: 4194304 kB\n"); ok {
		t.Fatalf("missing SwapFree must be unavailable")
	}
}

// ---------------------------------------------------------------------------
// Rate math
// ---------------------------------------------------------------------------

func TestThrashCounterDeltaSkipsFirstAndReset(t *testing.T) {
	var d pressureCounterDelta
	if r := d.rate(100, thrashT0); r != 0 {
		t.Fatalf("first sample must be 0, got %v", r)
	}
	if r := d.rate(1100, thrashT0.Add(10*time.Second)); r != 100 {
		t.Fatalf("want 100/s, got %v", r)
	}
	// Counter moved backwards (reboot / cgroup recreation): clamp to 0.
	if r := d.rate(5, thrashT0.Add(20*time.Second)); r != 0 {
		t.Fatalf("reset must clamp to 0, got %v", r)
	}
	if r := d.rate(505, thrashT0.Add(30*time.Second)); r != 50 {
		t.Fatalf("after reset, want 50/s, got %v", r)
	}
}

func TestThrashRatesDelayedTicker(t *testing.T) {
	// A swapped-out ticker can slip 90s+ between samples: the fraction must be
	// delta_total_us / delta_wall_us, not per-sample-count and not avg60.
	var tr thrashTracker
	rd := thrashRead{
		psiSomeTotal: 100_000_000, psiFullTotal: 50_000_000,
		psiSomeOK: true, psiFullOK: true, psiUnit: true,
		swapIn: 0, swapOut: 0, swapOK: true, swapUnit: true,
		refault: 0, refaultOK: true,
	}
	tr.rates(rd, thrashT0) // baseline

	rd.psiSomeTotal = 100_000_000 + 90_000_000 // 90s stalled over 90s wall
	rd.psiFullTotal = 50_000_000 + 45_000_000  // 45s of 90s
	rd.swapIn = 9000                           // 100 pages/s
	rd.refault = 900                           // 10 pages/s
	r := tr.rates(rd, thrashT0.Add(90*time.Second))

	if got := r.fullFrac; got < 0.49 || got > 0.51 {
		t.Fatalf("fullFrac over 90s slip: want ~0.5, got %v", got)
	}
	if got := r.someFrac; got < 0.99 || got > 1.01 {
		t.Fatalf("someFrac: want ~1.0, got %v", got)
	}
	if got := r.swapInPS; got < 99 || got > 101 {
		t.Fatalf("swapInPS: want ~100, got %v", got)
	}
	if got := r.refaultPS; got < 9 || got > 11 {
		t.Fatalf("refaultPS: want ~10, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// State machine
// ---------------------------------------------------------------------------

func mildRates(fullFrac float64, swapOK bool, swapPS float64) thrashRates {
	return thrashRates{fullFrac: fullFrac, fullOK: true, swapOK: swapOK, swapInPS: swapPS}
}

func TestThrashMachineMildCorroboratedTakesThreeMinutes(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	rt := mildRates(0.15, true, 300) // corroborated by 300 pages/s
	st := m.step(thrashT0, rt, thrashRead{})
	if !st.changed || st.cur != thrashUnderPressure {
		t.Fatalf("t0: want under-pressure, got %v (changed=%v)", st.cur, st.changed)
	}
	st = m.step(thrashT0.Add(2*time.Minute), rt, thrashRead{})
	if st.cur != thrashUnderPressure {
		t.Fatalf("t+2m: want still under-pressure, got %v", st.cur)
	}
	st = m.step(thrashT0.Add(3*time.Minute+time.Second), rt, thrashRead{})
	if st.cur != thrashThrashing {
		t.Fatalf("t+3m: want thrashing, got %v", st.cur)
	}
}

func TestThrashMachineSevereAloneEscalatesAtNinetySeconds(t *testing.T) {
	// The 51%-pool redegrade case: NO corroborators readable at all, PSI full
	// severe. Must escalate anyway — never AND with an unavailable signal.
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	rt := thrashRates{fullFrac: 0.30, fullOK: true}
	if st := m.step(thrashT0, rt, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("t0: want under-pressure, got %v", st.cur)
	}
	if st := m.step(thrashT0.Add(80*time.Second), rt, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("t+80s: want under-pressure, got %v", st.cur)
	}
	if st := m.step(thrashT0.Add(91*time.Second), rt, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("t+91s: want thrashing, got %v", st.cur)
	}
}

func TestThrashMachineMildUncorroboratedNeedsStricterBar(t *testing.T) {
	// 15% with nothing readable: below the stricter 20% PSI-only bar -> calm.
	m1 := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	if st := m1.step(thrashT0, thrashRates{fullFrac: 0.15, fullOK: true}, thrashRead{}); st.cur != thrashCalm {
		t.Fatalf("15%% uncorroborated: want calm, got %v", st.cur)
	}
	// 22% with nothing readable: above the stricter bar -> counts.
	m2 := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	if st := m2.step(thrashT0, thrashRates{fullFrac: 0.22, fullOK: true}, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("22%% uncorroborated: want under-pressure, got %v", st.cur)
	}
	// Corroborators readable but quiet -> 15% does NOT count (peace, not thrash).
	m3 := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	if st := m3.step(thrashT0, mildRates(0.15, true, 10), thrashRead{}); st.cur != thrashCalm {
		t.Fatalf("15%% corroborated-quiet: want calm, got %v", st.cur)
	}
}

func TestThrashMachineRelaxesOneStepPerCalmWindow(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	if st := m.step(thrashAt, severe, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("want thrashing, got %v", st.cur)
	}
	calm := thrashRates{}
	// Calm starts here.
	a := thrashAt.Add(time.Minute)
	if st := m.step(a, calm, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("calm just started: want still thrashing, got %v", st.cur)
	}
	b := a.Add(5*time.Minute + time.Second)
	if st := m.step(b, calm, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("first window: want under-pressure, got %v", st.cur)
	}
	if st := m.step(b.Add(time.Minute), calm, thrashRead{}); st.cur != thrashUnderPressure {
		t.Fatalf("between windows: want still under-pressure, got %v", st.cur)
	}
	// The window counts from the step that started the calm streak (b+1m).
	c := b.Add(6*time.Minute + 2*time.Second)
	if st := m.step(c, calm, thrashRead{}); st.cur != thrashCalm {
		t.Fatalf("second window: want calm, got %v", st.cur)
	}
}

func TestThrashMachineCriticalAfterFifteenMinutes(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	severe := thrashRates{fullFrac: 0.30, fullOK: true}
	m.step(thrashT0, severe, thrashRead{})
	thrashAt := thrashT0.Add(91 * time.Second)
	m.step(thrashAt, severe, thrashRead{})
	if st := m.step(thrashAt.Add(14*time.Minute), severe, thrashRead{}); st.cur != thrashThrashing {
		t.Fatalf("t+14m: want thrashing, got %v", st.cur)
	}
	if st := m.step(thrashAt.Add(15*time.Minute+time.Second), severe, thrashRead{}); st.cur != thrashCritical {
		t.Fatalf("t+15m: want critical, got %v", st.cur)
	}
}

// ---------------------------------------------------------------------------
// Attribution
// ---------------------------------------------------------------------------

func TestThrashAttribution(t *testing.T) {
	ours := thrashRead{unitSwapOK: true, unitSwapMiB: 3100, hostSwapOK: true, hostSwapTotalMiB: 3300, hostSwapUsedMiB: 3300}
	if attr, share, ok := thrashAttribution(ours); attr != "unit" || !ok || share < 0.9 {
		t.Fatalf("unit-dominant: got (%s, %v, %v)", attr, share, ok)
	}
	other := thrashRead{unitSwapOK: true, unitSwapMiB: 100, hostSwapOK: true, hostSwapTotalMiB: 3300, hostSwapUsedMiB: 3000}
	if attr, _, ok := thrashAttribution(other); attr != "other" || !ok {
		t.Fatalf("other-dominant: want other, got (%s, %v)", attr, ok)
	}
	unitOnly := thrashRead{unitSwapOK: true, unitSwapMiB: 500}
	if attr, _, shareOK := thrashAttribution(unitOnly); attr != "unit" || shareOK {
		t.Fatalf("unit-only: want (unit, share unknown), got (%s, %v)", attr, shareOK)
	}
	psiOnly := thrashRead{psiUnit: true, psiFullOK: true}
	if attr, _, _ := thrashAttribution(psiOnly); attr != "unit" {
		t.Fatalf("per-unit PSI: want unit, got %s", attr)
	}
	if attr, _, _ := thrashAttribution(thrashRead{}); attr != "unknown" {
		t.Fatalf("nothing readable: want unknown, got %s", attr)
	}
}

// ---------------------------------------------------------------------------
// Thrash cap: ladder, sizing, persistence, integration
// ---------------------------------------------------------------------------

func TestThrashCapEscalationLadder(t *testing.T) {
	withTempHome(t)
	base := thrashT0

	allowed, reason, n := thrashCapEscalationAllowed(thrashCapState{}, base)
	if !allowed || n != 0 {
		t.Fatalf("first escalation must be allowed, got (%v, %q, %d)", allowed, reason, n)
	}
	if err := recordThrashEscalation(300, base); err != nil {
		t.Fatalf("record: %v", err)
	}
	// 10 minutes later: inside the 30m re-arm window.
	if allowed, _, n := thrashCapEscalationAllowed(readThrashCapState(), base.Add(10*time.Minute)); allowed || n != 1 {
		t.Fatalf("within backoff must be denied, got (allowed=%v, n=%d)", allowed, n)
	}
	// Past 30m: allowed again (count now 1 -> next backoff 2h).
	second := base.Add(31 * time.Minute)
	if allowed, _, _ := thrashCapEscalationAllowed(readThrashCapState(), second); !allowed {
		t.Fatalf("past the first backoff must be allowed")
	}
	recordThrashEscalation(300, second)
	// 1h after the second: inside the 2h window.
	if allowed, _, _ := thrashCapEscalationAllowed(readThrashCapState(), second.Add(time.Hour)); allowed {
		t.Fatalf("within 2h backoff must be denied")
	}
	third := second.Add(2*time.Hour + time.Minute)
	if allowed, _, _ := thrashCapEscalationAllowed(readThrashCapState(), third); !allowed {
		t.Fatalf("past the 2h backoff must be allowed")
	}
	recordThrashEscalation(300, third)
	// Three in 24h: cap reached.
	if allowed, reason, n := thrashCapEscalationAllowed(readThrashCapState(), third.Add(7*time.Hour)); allowed || n != 3 {
		t.Fatalf("3/24h must deny (reason=%q, n=%d)", reason, n)
	}
	// Far enough out that the oldest fall out of the window: allowed again.
	later := base.Add(25 * time.Hour)
	if allowed, _, n := thrashCapEscalationAllowed(readThrashCapState(), later); !allowed {
		t.Fatalf("aged-out window must allow again (n=%d)", n)
	}
}

func TestThrashCapForNextStart(t *testing.T) {
	cases := []struct{ running, want int }{{500, 300}, {80, 48}, {1, 1}, {0, 0}}
	for _, c := range cases {
		if got := thrashCapForNextStart(c.running); got != c.want {
			t.Errorf("running=%d: got %d, want %d", c.running, got, c.want)
		}
	}
}

func TestActiveThrashCapExpiry(t *testing.T) {
	withTempHome(t)
	if err := recordThrashEscalation(300, thrashT0); err != nil {
		t.Fatalf("record: %v", err)
	}
	if cap, ok := activeThrashCap(thrashT0.Add(time.Hour)); !ok || cap != 300 {
		t.Fatalf("within hold: want (300, true), got (%d, %v)", cap, ok)
	}
	if _, ok := activeThrashCap(thrashT0.Add(25 * time.Hour)); ok {
		t.Fatalf("expired cap must be inactive")
	}
}

func TestEffectiveTrimCapIncludesThrashCap(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", "off")
	trimUnreadableReset()

	if err := recordThrashEscalation(300, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	cap, src, err := effectiveTrimCapSource()
	if err != nil || cap != 300 || src != trimCapThrash {
		t.Fatalf("thrash cap alone: got (%d, %q, %v)", cap, src, err)
	}
	// A tighter operator cap wins and is credited as the operator's.
	dir := filepath.Join(home, ".urnetwork")
	if err := os.WriteFile(filepath.Join(dir, "proxy_trim"), []byte("200\n"), 0600); err != nil {
		t.Fatalf("write proxy_trim: %v", err)
	}
	cap, src, err = effectiveTrimCapSource()
	if err != nil || cap != 200 || src != trimCapOperator {
		t.Fatalf("tighter operator cap: got (%d, %q, %v)", cap, src, err)
	}
	// A looser operator cap loses to the thrash cap.
	if err := os.WriteFile(filepath.Join(dir, "proxy_trim"), []byte("400\n"), 0600); err != nil {
		t.Fatalf("write proxy_trim: %v", err)
	}
	if cap, src, _ = effectiveTrimCapSource(); cap != 300 || src != trimCapThrash {
		t.Fatalf("looser operator cap: got (%d, %q)", cap, src)
	}
}

// ---------------------------------------------------------------------------
// Scoring fixes
// ---------------------------------------------------------------------------

func TestScoreExcludingCPU(t *testing.T) {
	comps := map[string]float64{"psi_cpu": 0.9, "mem": 0.3, "heap": 0.2}
	if got := scoreExcludingCPU(0.9, comps); got != 0.3 {
		t.Fatalf("want 0.3, got %v", got)
	}
	if got := scoreExcludingCPU(1.0, comps); got != 1.0 {
		t.Fatalf("emergency pin must carry: got %v", got)
	}
	if got := scoreExcludingCPU(0.1, map[string]float64{"psi_cpu": 0.1}); got != 0 {
		t.Fatalf("cpu-only: want 0, got %v", got)
	}
}

func TestCpuPressureComponent(t *testing.T) {
	// Multi-core keeps the shared ramp: (35-10)/50 = 0.5.
	if got := cpuPressureComponent(35, 4); got != 0.5 {
		t.Fatalf("multi-core: want 0.5, got %v", got)
	}
	// Single core uses the quiet ramp: (50-40)/50 = 0.2 (was 0.8 before).
	if got := cpuPressureComponent(50, 1); got < 0.19 || got > 0.21 {
		t.Fatalf("1-core: want ~0.2, got %v", got)
	}
	// Unknown cores behave like before (multi-core ramp).
	if got := cpuPressureComponent(35, 0); got != 0.5 {
		t.Fatalf("unknown cores: want 0.5, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Messages and serialization: human-readable, null-not-0
// ---------------------------------------------------------------------------

func TestThrashMessagesAreHumanReadable(t *testing.T) {
	rt := thrashRates{fullFrac: 0.51, fullOK: true, swapInPS: 3200, swapOutPS: 3200, swapOK: true}
	rd := thrashRead{unitSwapOK: true, unitSwapMiB: 3100, hostSwapOK: true, hostSwapTotalMiB: 3300, hostSwapUsedMiB: 3300, heapOK: true, heapFrac: 3.1, heapUsedMiB: 2100, heapLimitMiB: 680, ramAvailMiB: 82, ramAvailOK: true}

	msg := thrashOnsetMsg(3*time.Minute, rt, rd, 0.94, true)
	for _, want := range []string{"thrashing swap", "stalled on memory ~51%", "25 MB/s", "3.0 GB", "94% of all swap in use", "Heap is 3.1x"} {
		if !strings.Contains(msg, want) {
			t.Errorf("onset msg missing %q: %s", want, msg)
		}
	}
	other := thrashOtherMsg(rd, 0.04)
	if !strings.Contains(other, "another process") || !strings.Contains(other, "Not restarting") {
		t.Errorf("other-process msg: %s", other)
	}
	action := thrashActionMsg(1, 3, 500, 300)
	for _, want := range []string{"will not recover on its own", "restart 1 of max 3 per day", "cap reduced 500 -> 300"} {
		if !strings.Contains(action, want) {
			t.Errorf("action msg missing %q: %s", want, action)
		}
	}
	cleared := thrashClearedMsg("Thrash cleared by the restart", rd, rt)
	for _, want := range []string{"RAM free", "swap in use", "memory stalls 51%"} {
		if !strings.Contains(cleared, want) {
			t.Errorf("cleared msg missing %q: %s", want, cleared)
		}
	}
	warn := thrashEarlyWarnMsg(rd)
	if !strings.Contains(warn, "heap is 3.1x its size limit") || !strings.Contains(warn, "will start thrashing") {
		t.Errorf("early-warn msg: %s", warn)
	}
}

func TestThrashSnapshotNullsForUnavailable(t *testing.T) {
	m := &thrashMachine{state: thrashCalm, stateSince: thrashT0}
	snap := buildThrashSnapshot(m, thrashRead{}, thrashRates{}, "unknown", 0, false, 0, "", thrashT0)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"psi_mem_full":null`, `"swap_io_pps":null`, `"unit_swap_mib":null`, `"unit_swap_share":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("snapshot missing %s: %s", want, string(b))
		}
	}
	if strings.Contains(string(b), `"psi_mem_full":0`) {
		t.Errorf("psi_mem_full must never serialize as 0 when unavailable: %s", string(b))
	}
}

func TestThrashSensorLine(t *testing.T) {
	rd := thrashRead{
		psiSomeOK: true, psiFullOK: true, psiUnit: true,
		swapOK: true, swapUnit: true, refaultOK: true, pgscanOK: true,
		unitSwapOK: true, hostSwapOK: true, ramAvailOK: true, ramTotalOK: true, heapOK: true,
	}
	active, missing := thrashSensorLine(rd)
	if len(missing) != 0 {
		t.Fatalf("all-readable: unexpected missing %v", missing)
	}
	if !thrashHas(active, "psi-mem(unit)") || !thrashHas(active, "swap-activity(unit)") {
		t.Fatalf("active: %v", active)
	}
	active, missing = thrashSensorLine(thrashRead{})
	if len(active) != 0 || len(missing) == 0 {
		t.Fatalf("nothing-readable: want no active, some missing; got %v / %v", active, missing)
	}
}

func thrashHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
