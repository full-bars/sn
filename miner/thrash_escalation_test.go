package miner

// Tests for the thrash escalation port (provider/thrash_watchdog.go
// "Escalation" + provider/thrash_cap.go). The relay's gate stack is driven with
// injected seams: a temp state directory, a fake clock, a scripted supervisor,
// and an exit seam that records the code instead of ending the test process.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// swapThrashStormSensor is the ladder's scripted path: calm, then a swap storm
// at +30s, +60s and +120s (severe PSI full for 90s takes it to thrashing).
func swapThrashStormSensor() *scriptedSwapThrashSensor {
	return &scriptedSwapThrashSensor{readings: []swapThrashRead{
		swapThrashCalmReading(),
		swapThrashStormReading(30 * time.Second),
		swapThrashStormReading(60 * time.Second),
		swapThrashStormReading(120 * time.Second),
	}}
}

// swapThrashEscalationWatch builds a watch whose escalation gates are all
// open: self-heal on, a systemd supervisor, a writable state dir, 500 running
// proxies, no hot-swap in flight, and an exit that records instead of exiting.
func swapThrashEscalationWatch(t *testing.T, sensor *scriptedSwapThrashSensor, clock *time.Time, logs *[]string, exits *[]int) (*swapThrashWatch, string) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	watch, path := swapThrashTestWatch(t, sensor, clock, logs)
	watch.selfHealFn = func() bool { return true }
	watch.supervisorKindFn = func() (string, bool) { return "systemd", true }
	watch.capDirWritableFn = func() error { return nil }
	watch.runningFn = func() int { return 500 }
	watch.hotSwapBusyFn = func() bool { return false }
	watch.exitFn = func(code int) { *exits = append(*exits, code) }
	return watch, path
}

// swapThrashStormRates is the rate view of swapThrashStormReading(120s): 45% of
// wall time stalled on memory, 800 pages/s of swap, 100 refaults/s.
func swapThrashStormRates() swapThrashRates {
	return swapThrashRates{
		fullFrac: 0.45, fullOK: true,
		someFrac: 0.80, someOK: true,
		swapInPS: 400, swapOutPS: 400, swapOK: true,
		refaultPS: 100, refaultOK: true,
	}
}

// The cap fires once for N thrash events inside the re-arm window, and not
// twice: the first escape is a restart and appends exactly one ring entry; the
// repeats are denied with the stable "rearm" code and change nothing.
func TestThrashEscalationFiresOncePerWindow(t *testing.T) {
	clock := swapThrashT0
	sensor := swapThrashStormSensor()
	var logs []string
	var exits []int
	watch, _ := swapThrashEscalationWatch(t, sensor, &clock, &logs, &exits)

	rt, rd := swapThrashStormRates(), swapThrashStormReading(120*time.Second)
	first := watch.escalate(clock, rt, rd, "unit", 0.94, true)
	if first.Action != "restart" {
		t.Fatalf("first thrash event: want restart, got %+v", first)
	}
	if first.Cap != 300 || first.Running != 500 || first.Restarts != 1 {
		t.Fatalf("restart decision: cap=%d running=%d restarts=%d, want 300/500/1", first.Cap, first.Running, first.Restarts)
	}
	if !strings.Contains(first.Msg, "restart 1 of max 3 per day") {
		t.Fatalf("restart message: %s", first.Msg)
	}

	// Two more thrash events inside the 30m re-arm window: denied, and no
	// second ring entry.
	for i := 0; i < 2; i++ {
		esc := watch.escalate(clock.Add(time.Minute), rt, rd, "unit", 0.94, true)
		if esc.Action != "alert" || esc.Code != "rearm" {
			t.Fatalf("repeat %d inside the window: want alert/rearm, got %+v", i, esc)
		}
		if !strings.Contains(esc.Reason, "next allowed in") {
			t.Fatalf("re-arm reason must say when the next restart is allowed: %s", esc.Reason)
		}
	}
	st := readThrashCapState()
	if len(st.Restarts) != 1 {
		t.Fatalf("N thrash events in the window must leave exactly one restart entry, got %v", st.Restarts)
	}
	if cap, ok := activeThrashCap(clock); !ok || cap != 300 {
		t.Fatalf("the escape must leave the 60%% start cap (300 of 500), got %d ok=%v", cap, ok)
	}

	// Past the first rung (30m) a new episode may restart again, and the
	// standing cap is never loosened.
	next := watch.escalate(clock.Add(31*time.Minute), rt, rd, "unit", 0.94, true)
	if next.Action != "restart" || next.Restarts != 2 || next.Cap != 300 {
		t.Fatalf("after the backoff: want restart #2 with cap 300, got %+v", next)
	}
}

// The daily ceiling: three escapes in 24h deny the fourth outright, whatever
// the spacing, and the standing cap stays in force.
func TestThrashEscalationDailyCeiling(t *testing.T) {
	clock := swapThrashT0
	sensor := swapThrashStormSensor()
	var logs []string
	var exits []int
	watch, _ := swapThrashEscalationWatch(t, sensor, &clock, &logs, &exits)

	for i := 1; i <= thrashMaxRestarts24h; i++ {
		if err := recordThrashEscalation(100+i, clock.Add(-time.Duration(i)*3*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	esc := watch.escalate(clock, swapThrashStormRates(), swapThrashStormReading(120*time.Second), "unit", 0.94, true)
	if esc.Action != "alert" || esc.Code != "cap-reached" {
		t.Fatalf("a spent ring must deny the restart, got %+v", esc)
	}
	if !strings.Contains(esc.Reason, "restart cap reached") {
		t.Fatalf("cap-reached reason: %s", esc.Reason)
	}
	if len(readThrashCapState().Restarts) != thrashMaxRestarts24h {
		t.Fatalf("a denied restart must not append to the ring: %v", readThrashCapState().Restarts)
	}
}

// Below the thrash state nothing is written and nothing exits: a box that is
// merely under pressure is not a box that cannot recover.
func TestWatchdogNoRestartBelowThrashState(t *testing.T) {
	clock := swapThrashT0
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{
		swapThrashCalmReading(),
		swapThrashStormReading(30 * time.Second),
		swapThrashStormReading(60 * time.Second),
	}}
	var logs []string
	var exits []int
	watch, path := swapThrashEscalationWatch(t, sensor, &clock, &logs, &exits)

	watch.sampleIfDue(clock)
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second} {
		clock = swapThrashT0.Add(at)
		watch.sampleIfDue(clock)
	}
	if watch.state() != swapThrashUnderPressure {
		t.Fatalf("the scripted ladder is only under pressure, got %s", watch.state())
	}
	if len(exits) != 0 {
		t.Fatalf("no restart may run below the thrash state, got exits %v", exits)
	}
	if st := readThrashCapState(); len(st.Restarts) != 0 || st.Cap != 0 {
		t.Fatalf("nothing may be recorded below the thrash state, got %+v", st)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the status file must still be published: %v", err)
	}
}

// The escalation's action is the provider's: exit 75 (EX_TEMPFAIL), the code
// the service manager and the miner image's start script restart on, and the
// status file is cleared before the exit so no reader sees a dead process's
// last reading.
func TestThrashEscalationExitCode(t *testing.T) {
	clock := swapThrashT0
	sensor := swapThrashStormSensor()
	var logs []string
	var exits []int
	watch, path := swapThrashEscalationWatch(t, sensor, &clock, &logs, &exits)

	watch.sampleIfDue(clock)
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		clock = swapThrashT0.Add(at)
		watch.sampleIfDue(clock)
	}
	if watch.state() != swapThrashThrashing {
		t.Fatalf("want thrashing, got %s", watch.state())
	}
	if len(exits) != 1 {
		t.Fatalf("one thrash episode must exit exactly once, got %v", exits)
	}
	if exits[0] != swapThrashExitCode {
		t.Fatalf("exit code = %d, want swapThrashExitCode", exits[0])
	}
	if swapThrashExitCode != 75 {
		t.Fatalf("the provider's exit code is 75 (EX_TEMPFAIL), got %d", swapThrashExitCode)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the status file must be cleared before the exit, stat err=%v", err)
	}
	if cap, ok := activeThrashCap(clock); !ok || cap != 300 {
		t.Fatalf("the exit must leave the lean start cap 300, got %d ok=%v", cap, ok)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Restarting the provider now") {
		t.Fatalf("the restart must be announced in plain language:\n%s", strings.Join(logs, "\n"))
	}
	// An injected exit stops the watch: no second escalation from a process
	// that was told to die.
	clock = swapThrashT0.Add(time.Hour)
	watch.sampleIfDue(clock)
	if len(exits) != 1 {
		t.Fatalf("the watch must stop after the exit, got %v", exits)
	}
}

// The gates that refuse an exit without touching the ring.
func TestThrashEscalationRefusals(t *testing.T) {
	rt, rd := swapThrashStormRates(), swapThrashStormReading(120*time.Second)
	for _, tc := range []struct {
		name    string
		mutate  func(w *swapThrashWatch)
		attr    string
		code    string
		message string
	}{
		{
			name:    "self-heal off",
			mutate:  func(w *swapThrashWatch) { w.selfHealFn = func() bool { return false } },
			attr:    "unit",
			code:    "self-heal-off",
			message: "self-heal is off",
		},
		{
			name:    "hot swap in flight",
			mutate:  func(w *swapThrashWatch) { w.hotSwapBusyFn = func() bool { return true } },
			attr:    "unit",
			code:    "hotswap",
			message: "hot-swap is draining",
		},
		{
			name:    "another process's swap",
			mutate:  func(w *swapThrashWatch) {},
			attr:    "other",
			code:    "attributed-other",
			message: "never restarting someone else's thrash",
		},
		{
			name:    "unattributable swap",
			mutate:  func(w *swapThrashWatch) {},
			attr:    "unknown",
			code:    "attributed-unknown",
			message: "not restarting blind",
		},
		{
			name:    "no supervisor",
			mutate:  func(w *swapThrashWatch) { w.supervisorKindFn = func() (string, bool) { return "none", false } },
			attr:    "unit",
			code:    "no-supervisor",
			message: "would not be restarted",
		},
		{
			name: "container state dir not writable",
			mutate: func(w *swapThrashWatch) {
				w.supervisorKindFn = func() (string, bool) { return "docker", true }
				w.capDirWritableFn = func() error { return os.ErrPermission }
			},
			attr:    "unit",
			code:    "persist-failed",
			message: "is not writable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := swapThrashT0
			sensor := swapThrashStormSensor()
			var logs []string
			var exits []int
			watch, _ := swapThrashEscalationWatch(t, sensor, &clock, &logs, &exits)
			tc.mutate(watch)

			esc := watch.escalate(clock, rt, rd, tc.attr, 0.94, tc.attr == "unit")
			if esc.Action != "alert" || esc.Code != tc.code {
				t.Fatalf("want alert/%s, got %+v", tc.code, esc)
			}
			if !strings.Contains(esc.Reason, tc.message) {
				t.Fatalf("reason %q must contain %q", esc.Reason, tc.message)
			}
			if st := readThrashCapState(); len(st.Restarts) != 0 || st.Cap != 0 {
				t.Fatalf("a refused restart must leave no state, got %+v", st)
			}
		})
	}
}

// The cap binding: the escape's cap feeds effectiveTrimCapSource as the
// automatic cap, under its own source name, and never loosens the OOM cap.
func TestThrashCapBindsAsTheAutomaticTrimCap(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	now := time.Now()
	if err := recordThrashEscalation(300, now); err != nil {
		t.Fatal(err)
	}
	cap, source := automaticTrimCapSource()
	if cap != 300 || source != trimCapThrash {
		t.Fatalf("automatic cap = %d/%q, want 300/%q", cap, source, trimCapThrash)
	}
	// A tighter OOM cap wins, and a looser one does not mask the thrash cap.
	if err := oomWriteJSON(mustOOMCapPath(t), oomCapState{Cap: 200, SinceUnix: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "on")
	if cap, source := automaticTrimCapSource(); cap != 200 || source != trimCapOOM {
		t.Fatalf("tighter OOM cap must bind: got %d/%q", cap, source)
	}
	if err := oomWriteJSON(mustOOMCapPath(t), oomCapState{Cap: 900, SinceUnix: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if cap, source := automaticTrimCapSource(); cap != 300 || source != trimCapThrash {
		t.Fatalf("a looser OOM cap must not mask the thrash cap: got %d/%q", cap, source)
	}
}

func mustOOMCapPath(t *testing.T) string {
	t.Helper()
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir + "/oom_cap.json"
}

// stubHealthyThrashBox pins both health seams to a calm, roomy box so the
// staged-relaxation assertions exercise the logic under test rather than host
// memory or the live watchdog. The originals are restored by t.Cleanup. A test
// that needs a sick box stubs thrashWatchdogSnapshotFn again on top of this.
func stubHealthyThrashBox(t *testing.T) {
	t.Helper()
	origSnapFn := thrashWatchdogSnapshotFn
	origHeadroomFn := thrashHeadroomFn
	t.Cleanup(func() {
		thrashWatchdogSnapshotFn = origSnapFn
		thrashHeadroomFn = origHeadroomFn
	})
	thrashWatchdogSnapshotFn = func() *swapThrashSnapshot {
		return &swapThrashSnapshot{State: "calm"}
	}
	// 300 MiB available against a 200 MiB low threshold: above the 1.25x gate.
	thrashHeadroomFn = func() (int64, int64) {
		return 300, 200
	}
}

// Pure sizing and window helpers, including health-based expiry and staged
// relaxation (+20% per window until target capacity is reached).
func TestThrashCapSizesAndExpires(t *testing.T) {
	stubHealthyThrashBox(t)
	if got := thrashCapForNextStart(500); got != 300 {
		t.Fatalf("cap for 500 running: got %d, want 300", got)
	}
	if got := thrashCapForNextStart(1); got != 1 {
		t.Fatalf("the cap never falls below one proxy, got %d", got)
	}
	if got := thrashCapForNextStart(0); got != 0 {
		t.Fatalf("nothing running means no cap, got %d", got)
	}

	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	now := time.Now()
	if err := recordThrashEscalation(300, now); err != nil {
		t.Fatal(err)
	}
	// Before 24h: cap holds.
	if cap, ok := activeThrashCap(now.Add(thrashCapHold - time.Minute)); !ok || cap != 300 {
		t.Fatalf("the cap holds for 24h, got %d ok=%v", cap, ok)
	}

	// At 24h mark, if the box is still sick (e.g. watchdog thrashing), the cap
	// must NOT expire on a clock: it stays in force.
	origSnapFn := thrashWatchdogSnapshotFn
	t.Cleanup(func() { thrashWatchdogSnapshotFn = origSnapFn })
	thrashWatchdogSnapshotFn = func() *swapThrashSnapshot {
		return &swapThrashSnapshot{State: "thrashing"}
	}
	tSick := now.Add(thrashCapHold + time.Minute)
	if cap, ok := activeThrashCap(tSick); !ok || cap != 300 {
		t.Fatalf("sick box at 24h must not relax or expire: got %d ok=%v, want 300/true", cap, ok)
	}
	thrashWatchdogSnapshotFn = origSnapFn

	// When healthy, the cap relaxes in STAGES (+20%): stage 1 goes from 300 to 360,
	// NOT releasing the full pre-cut load at once.
	t1 := now.Add(thrashCapHold + time.Minute)
	if cap, ok := activeThrashCap(t1); !ok || cap != 360 {
		t.Fatalf("stage 1 relaxation: got %d ok=%v, want 360/true", cap, ok)
	}

	// Inside the next hold window, stage 1 holds.
	if cap, ok := activeThrashCap(t1.Add(time.Hour)); !ok || cap != 360 {
		t.Fatalf("stage 1 hold: got %d ok=%v, want 360/true", cap, ok)
	}

	// Stage 2 (after 24h quiet at stage 1): relaxes from 360 to 432.
	t2 := t1.Add(thrashCapHold + time.Minute)
	if cap, ok := activeThrashCap(t2); !ok || cap != 432 {
		t.Fatalf("stage 2 relaxation: got %d ok=%v, want 432/true", cap, ok)
	}

	// Stage 3 (after 24h quiet at stage 2): 432 * 1.2 = 518 >= 500 (target).
	// Target reached: cap cleared (stops applying on demonstrated recovery).
	t3 := t2.Add(thrashCapHold + time.Minute)
	if cap, ok := activeThrashCap(t3); ok || cap != 0 {
		t.Fatalf("stage 3 target reached: got %d ok=%v, want 0/false", cap, ok)
	}

	// A later escalation re-arms it.
	if err := recordThrashEscalation(250, t3.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if cap, ok := activeThrashCap(t3.Add(2 * time.Minute)); !ok || cap != 250 {
		t.Fatalf("a new escalation must re-arm the cap, got %d ok=%v", cap, ok)
	}
}

// Low memory headroom prevents relaxation even if the box has been quiet for 24h.
func TestThrashCapHeadroomGatesRelaxation(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	now := time.Now()
	if err := recordThrashEscalation(300, now); err != nil {
		t.Fatal(err)
	}

	origHeadroomFn := thrashHeadroomFn
	t.Cleanup(func() { thrashHeadroomFn = origHeadroomFn })

	// Free RAM is 100 MiB, below the 200 MiB threshold -> low headroom.
	thrashHeadroomFn = func() (int64, int64) {
		return 100, 200
	}

	tCheck := now.Add(thrashCapHold + time.Minute)
	if cap, ok := activeThrashCap(tCheck); !ok || cap != 300 {
		t.Fatalf("low headroom must prevent relaxation: got %d ok=%v, want 300/true", cap, ok)
	}

	// Headroom recovers: 300 MiB available (well above 200 * 1.25 = 250 MiB).
	thrashHeadroomFn = func() (int64, int64) {
		return 300, 200
	}
	if cap, ok := activeThrashCap(tCheck); !ok || cap != 360 {
		t.Fatalf("healthy headroom allows staged relaxation: got %d ok=%v, want 360/true", cap, ok)
	}
}

// Staged relaxation towards an operator trim of 1500 (the incident scenario):
// A box that thrashed at ~605 and was cut to 363 does NOT jump to 1500 on expiry.
// It relaxes in stages (363 -> 435 -> 522), preventing several hundred slots in one launch.
func TestThrashCapRelaxesInStagesTowardsOperatorTrim(t *testing.T) {
	stubHealthyThrashBox(t)
	stateDir := shortStateDir(t)
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	// Write operator trim of 1500.
	if err := os.WriteFile(filepath.Join(stateDir, "proxy_trim"), []byte("1500\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := recordThrashEscalation(363, now); err != nil {
		t.Fatal(err)
	}

	// 24h later: box is healthy.
	t1 := now.Add(thrashCapHold + time.Minute)
	if cap, ok := activeThrashCap(t1); !ok || cap != 435 {
		t.Fatalf("stage 1 relaxation towards 1500: got %d ok=%v, want 435/true", cap, ok)
	}

	// Next stage 24h later: relaxes from 435 to 522, NOT releasing 1500 at once.
	t2 := t1.Add(thrashCapHold + time.Minute)
	if cap, ok := activeThrashCap(t2); !ok || cap != 522 {
		t.Fatalf("stage 2 relaxation towards 1500: got %d ok=%v, want 522/true", cap, ok)
	}
}

// seedRecoveredThrashCap stores a 300 cap whose escalation is older than the
// hold window, on a healthy box. The next mutating check would relax it to 360,
// so the stall and escalation tests below can observe the cap as stored only
// because their paths read it without relaxing.
func seedRecoveredThrashCap(t *testing.T) {
	t.Helper()
	stubHealthyThrashBox(t)
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	prevRunning := oomCapRunningProxies.Load()
	t.Cleanup(func() { oomCapRunningProxies.Store(prevRunning) })
	oomCapRunningProxies.Store(500)
	if err := recordThrashEscalation(300, time.Now().Add(-(thrashCapHold + time.Minute))); err != nil {
		t.Fatal(err)
	}
}

// A stall reads the stored cap through standingThrashCap, so it neither relaxes
// nor re-persists it, even on a healthy box past the hold window.
func TestStandingThrashCapHoldsThroughAStall(t *testing.T) {
	seedRecoveredThrashCap(t)
	recordLivenessStall(critLog)
	if got := readThrashCapState().Cap; got != 300 {
		t.Fatalf("a stall relaxed the stored cap: got %d, want 300", got)
	}
}

// An escalation reads the stored cap through standingThrashCapLocked, so the
// tighter of the stored 300 and the new 400 is kept, not a relaxed 360.
func TestStandingThrashCapHoldsThroughAnEscalation(t *testing.T) {
	seedRecoveredThrashCap(t)
	if err := recordThrashEscalation(400, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := readThrashCapState().Cap; got != 300 {
		t.Fatalf("an escalation relaxed the stored cap: got %d, want 300", got)
	}
}

// The marker write runs immediately before the exit, so it is bounded by the
// same timeout as the anti-loop record: a state directory hung in swap I/O must
// not keep the exit from being taken, or the supervised miner never restarts. A
// timeout is returned for the caller to log, and the exit proceeds.
func TestRequestThrashExitBoundedTimesOut(t *testing.T) {
	prevWrite, prevTimeout := thrashExitMarkerWriteFn, swapThrashPersistTimeout
	t.Cleanup(func() { thrashExitMarkerWriteFn, swapThrashPersistTimeout = prevWrite, prevTimeout })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	thrashExitMarkerWriteFn = func() error { <-release; return nil }
	swapThrashPersistTimeout = 50 * time.Millisecond

	start := time.Now()
	err := requestThrashExitBounded()
	if err == nil {
		t.Fatal("a marker write that never returns must time out")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the marker write was not bounded: took %v", took)
	}

	thrashExitMarkerWriteFn = func() error { return nil }
	if err := requestThrashExitBounded(); err != nil {
		t.Fatalf("a healthy marker write must pass: %v", err)
	}
}
