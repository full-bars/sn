package miner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the startup grading gate (proxy_manager_gate.go): the active probe
// loops (the paid/file grader, the URL fetch/reaper stage-1 cycles and the
// stale-reaper demotions) must not grade while the launched pool has no
// workers, and must start once the pool is up. The gate has no user-facing
// knob; every case here drives its private constants and seam directly.

// gateLogCapture is a concurrency-safe io.Writer for the gate's log lines: the
// gated loops log from their own goroutines while the test polls the capture.
type gateLogCapture struct {
	mu sync.Mutex
	b  strings.Builder
}

func (c *gateLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.Write(p)
}

func (c *gateLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// captureGateLogs routes tlog/importantLogf lines into a reader the test can
// poll while the gate's goroutines run.
func captureGateLogs(t *testing.T) *gateLogCapture {
	t.Helper()
	capture := &gateLogCapture{}
	prev := proxyManagerLogWriter
	proxyManagerLogWriter = capture
	t.Cleanup(func() { proxyManagerLogWriter = prev })
	return capture
}

// fakeGateClock returns a clock the test steers.
type fakeGateClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeGateClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeGateClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newFakeClockGate arms a gate whose clock and running count the test owns.
func newFakeClockGate(desired int, running *atomic.Int64) (*gradingGate, *fakeGateClock) {
	clock := &fakeGateClock{now: time.Now()}
	gate := newGradingGate(desired)
	gate.now = clock.get
	gate.runningCount = func() int { return int(running.Load()) }
	return gate, clock
}

// TestStartupGateOpensAtNinetyPercentRunning is the threshold case: 89.9% is
// still held, 90% opens with the running reason.
func TestStartupGateOpensAtNinetyPercentRunning(t *testing.T) {
	withTempHome(t)
	logs := captureGateLogs(t)

	var running atomic.Int64
	gate, _ := newFakeClockGate(1000, &running)
	gate.noteWorkerLaunches()

	running.Store(899)
	if gate.observe() {
		t.Fatal("899/1000 (89.9%) must stay held")
	}
	if !strings.Contains(logs.String(), "grading gate waiting: running=899/1000") {
		t.Fatalf("held gate must log the waiting line, got %q", logs.String())
	}

	running.Store(900)
	if !gate.observe() {
		t.Fatal("900/1000 (90%) must open")
	}
	if got := gate.openReason; got != "running=900/1000" {
		t.Fatalf("open reason = %q, want running=900/1000", got)
	}
	if !strings.Contains(logs.String(), "grading gate opened: running=900/1000") {
		t.Fatalf("opened line missing, got %q", logs.String())
	}
}

// TestStartupGatePlateauOpensWhenStragglersStopArriving is the plateau case: a
// non-zero running count that stops moving opens the gate after the launch
// floor, so the tail of a large pool never blocks grading.
func TestStartupGatePlateauOpensWhenStragglersStopArriving(t *testing.T) {
	withTempHome(t)
	logs := captureGateLogs(t)

	var running atomic.Int64
	gate, clock := newFakeClockGate(1000, &running)
	gate.noteWorkerLaunches()

	running.Store(12)
	if gate.observe() {
		t.Fatal("the first observation cannot open the gate")
	}

	clock.advance(29 * time.Second)
	if gate.observe() {
		t.Fatal("plateau must not fire inside the 30s launch floor")
	}

	clock.advance(2 * time.Second) // t+31s: floor met, running unchanged only 31s
	if gate.observe() {
		t.Fatal("plateau must not fire before the running count has been still for 60s")
	}

	clock.advance(30 * time.Second) // t+61s: unchanged 61s >= 60s
	if !gate.observe() {
		t.Fatal("plateau must open with 12/1000 stragglers")
	}
	if got := gate.openReason; got != "plateau, running=12/1000" {
		t.Fatalf("open reason = %q, want plateau, running=12/1000", got)
	}
	if !strings.Contains(logs.String(), "grading gate opened: plateau, running=12/1000") {
		t.Fatalf("opened line missing, got %q", logs.String())
	}
}

// TestStartupGatePlateauClockResetsWhenRunningMoves pins the other half of the
// plateau: a pool still bringing workers up is not a plateau, so a moving count
// restarts the 60s window.
func TestStartupGatePlateauClockResetsWhenRunningMoves(t *testing.T) {
	withTempHome(t)

	var running atomic.Int64
	gate, clock := newFakeClockGate(1000, &running)
	gate.noteWorkerLaunches()

	running.Store(12)
	gate.observe()

	clock.advance(50 * time.Second)
	running.Store(13) // the stragglers keep arriving
	if gate.observe() {
		t.Fatal("a running count that just moved must not read as a plateau")
	}

	clock.advance(50 * time.Second) // 50s since the move
	if gate.observe() {
		t.Fatal("plateau must not fire before 60s of a still count")
	}

	clock.advance(11 * time.Second) // 61s since the move
	if !gate.observe() {
		t.Fatal("plateau must open once the new count has been still for 60s")
	}
	if got := gate.openReason; got != "plateau, running=13/1000" {
		t.Fatalf("open reason = %q, want plateau, running=13/1000", got)
	}
}

// TestStartupGateDeadlineOpensWithZeroWorkers is the deadline case: a pool that
// never comes up (zero running) cannot hold the gate past 10 minutes from the
// first worker launch, and the exit is logged loudly with the counts.
func TestStartupGateDeadlineOpensWithZeroWorkers(t *testing.T) {
	stateDir := withTempHome(t)
	logs := captureGateLogs(t)

	var running atomic.Int64
	gate, clock := newFakeClockGate(1000, &running)
	gate.noteWorkerLaunches()

	clock.advance(9*time.Minute + 59*time.Second)
	if gate.observe() {
		t.Fatal("the gate must not open before the 10 minute deadline")
	}

	clock.advance(2 * time.Second)
	if !gate.observe() {
		t.Fatal("the deadline must open the gate with zero workers")
	}
	if got := gate.openReason; got != "deadline, running=0/1000" {
		t.Fatalf("open reason = %q, want deadline, running=0/1000", got)
	}
	line := "grading gate opened: deadline, running=0/1000"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("opened line missing from the ramlog, got %q", logs.String())
	}
	// "Loudly": the deadline exit is also in the disk events.log record.
	b, err := os.ReadFile(filepath.Join(stateDir, "events.log"))
	if err != nil {
		t.Fatalf("deadline line must reach events.log: %v", err)
	}
	if !strings.Contains(string(b), line) {
		t.Fatalf("events.log missing the deadline line: %q", string(b))
	}
}

// TestStartupGateDesiredZeroOpensImmediately: a direct-only or empty pool has
// no workers to wait for, so the gate is open as it is armed and wait never
// blocks.
func TestStartupGateDesiredZeroOpensImmediately(t *testing.T) {
	withTempHome(t)
	logs := captureGateLogs(t)

	gate := newGradingGate(0)
	if !gate.isOpen() {
		t.Fatal("desired == 0 must open immediately")
	}
	if got := gate.openReason; got != "running=0/0" {
		t.Fatalf("open reason = %q, want running=0/0", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !gate.wait(ctx) {
		t.Fatal("wait must return immediately for desired == 0")
	}
	if !strings.Contains(logs.String(), "grading gate opened: running=0/0") {
		t.Fatalf("opened line missing, got %q", logs.String())
	}
}

// TestStartupGateHoldsPaidGraderProbesUntilOpen drives the real paid/file
// grader loop through the real gate: while the gate is held no probe CONNECT
// leaves the box, the waiting line is logged, and the pass runs as the gate
// opens.
func TestStartupGateHoldsPaidGraderProbesUntilOpen(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	passed := tableProbePassCounter.Load()
	seedProbeDNSForAddress(t, addr, passed, passed+1)

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source: src,
		Proxies: map[string]ProxyEntry{
			identityKey(addr, "u"): {ID: 1, Health: "up", Source: "file"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	logs := captureGateLogs(t)
	oldInterval := gradingLoopInterval
	gradingLoopInterval = 25 * time.Millisecond
	t.Cleanup(func() { gradingLoopInterval = oldInterval })

	var running atomic.Int64
	gate := newGradingGate(1)
	gate.runningCount = func() int { return int(running.Load()) }

	ctx, cancel := context.WithCancel(context.Background())
	watcherDone := make(chan struct{})
	go func() { defer close(watcherDone); gate.run(ctx) }()
	graderDone := make(chan struct{})
	go func() { defer close(graderDone); runPaidProxyGrader(ctx, "1.2.3.4", 443, gate) }()
	stopLoops := func() {
		cancel()
		select {
		case <-graderDone:
		case <-time.After(5 * time.Second):
			t.Error("paid grader loop did not stop after cancellation")
		}
		select {
		case <-watcherDone:
		case <-time.After(5 * time.Second):
			t.Error("gate watcher did not stop after cancellation")
		}
	}

	// Several ticks elapse while the gate is held: nothing may probe.
	time.Sleep(300 * time.Millisecond)
	if n := connects.Load(); n != 0 {
		stopLoops()
		t.Fatalf("probed while the gate was held: %d CONNECTs", n)
	}
	if !strings.Contains(logs.String(), "grading gate waiting: running=0/1") {
		stopLoops()
		t.Fatalf("waiting line missing: %q", logs.String())
	}

	// The pool comes up; the tick parked in the gate runs its pass.
	running.Store(1)
	deadline := time.Now().Add(5 * time.Second)
	for connects.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	n := connects.Load()
	stopLoops()
	if n < 5 {
		t.Fatalf("grader did not probe after the gate opened: %d CONNECTs, want >= 5", n)
	}
	if !strings.Contains(logs.String(), "grading gate opened: running=1/1") {
		t.Fatalf("opened line missing: %q", logs.String())
	}
}

// TestStartupGateHoldsStaleReaperProbesUntilOpen is the URL-side counterpart:
// the reaper loop (stage-1 table probe cycle and the stale-reaper demotions)
// stays off the wire until the gate opens.
func TestStartupGateHoldsStaleReaperProbesUntilOpen(t *testing.T) {
	withTempHome(t)
	resetProbeConfigCache()
	writeReviewProbeOverride(t, map[string]any{"enabled": false, "sample_width": 4, "timeout_ms": 500})

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()

	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {ProbeOK: true, Graded: true, Score: 0.9, LastProbe: time.Now().Add(-24 * time.Hour)},
	}}
	if err := writeProxyURLState(state); err != nil {
		t.Fatal(err)
	}

	logs := captureGateLogs(t)
	oldInterval := gradingLoopInterval
	gradingLoopInterval = 25 * time.Millisecond
	t.Cleanup(func() { gradingLoopInterval = oldInterval })

	var running atomic.Int64
	gate := newGradingGate(1)
	gate.runningCount = func() int { return int(running.Load()) }

	ctx, cancel := context.WithCancel(context.Background())
	watcherDone := make(chan struct{})
	go func() { defer close(watcherDone); gate.run(ctx) }()
	reaperDone := make(chan struct{})
	go func() { defer close(reaperDone); runURLProxyReaper(ctx, "1.2.3.4", 443, gate) }()
	stopLoops := func() {
		cancel()
		select {
		case <-reaperDone:
		case <-time.After(5 * time.Second):
			t.Error("reaper loop did not stop after cancellation")
		}
		select {
		case <-watcherDone:
		case <-time.After(5 * time.Second):
			t.Error("gate watcher did not stop after cancellation")
		}
	}

	time.Sleep(300 * time.Millisecond)
	if n := connects.Load(); n != 0 {
		stopLoops()
		t.Fatalf("reaper probed while the gate was held: %d CONNECTs", n)
	}
	if !strings.Contains(logs.String(), "grading gate waiting: running=0/1") {
		stopLoops()
		t.Fatalf("waiting line missing: %q", logs.String())
	}

	running.Store(1)
	deadline := time.Now().Add(5 * time.Second)
	for connects.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	n := connects.Load()
	stopLoops()
	if n < 1 {
		t.Fatal("reaper did not probe after the gate opened")
	}
	if !strings.Contains(logs.String(), "grading gate opened: running=1/1") {
		t.Fatalf("opened line missing: %q", logs.String())
	}
}

// TestProxyManagerStartArmsTheGradingGateAndStopClearsIt pins the manager
// wiring: start() arms the gate with the launch count and registers it for the
// provide run's readiness hook; the hook reaches it; stop() clears it.
func TestProxyManagerStartArmsTheGradingGateAndStopClearsIt(t *testing.T) {
	home := withTempHome(t)
	src := filepath.Join(home, "proxies.txt")
	if err := os.WriteFile(src, []byte("4.4.4.4:1080:carol:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultProxyManagerConfig()
	cfg.ProxyFile = src
	m := newProxyManager(cfg)
	launch, _, err := m.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.gate == nil {
		t.Fatal("start must arm the startup grading gate")
	}
	if m.gate.desired != len(launch) || m.gate.desired != 1 {
		t.Fatalf("gate desired = %d, want the launch count %d", m.gate.desired, len(launch))
	}
	if processGradingGate.Load() != m.gate {
		t.Fatal("start must register the gate for the run's readiness hook")
	}
	noteProviderWorkerLaunches()
	if !m.gate.launched {
		t.Fatal("the run's readiness hook must reach the manager's gate")
	}
	m.stop()
	if processGradingGate.Load() != nil {
		t.Fatal("stop must clear the process gate")
	}
}

// TestProviderWorkerLaunchHookStartsTheDeadlineClock pins the run.go hook: the
// first noteProviderWorkerLaunches call starts the deadline clock, a repeat
// does not move it, and a process without a manager is a no-op.
func TestProviderWorkerLaunchHookStartsTheDeadlineClock(t *testing.T) {
	withTempHome(t)

	// No manager registered: the hook must not panic and must not create state.
	prev := processGradingGate.Load()
	processGradingGate.Store(nil)
	t.Cleanup(func() { processGradingGate.Store(prev) })
	noteProviderWorkerLaunches()

	var running atomic.Int64
	gate, clock := newFakeClockGate(1, &running)
	processGradingGate.Store(gate)
	noteProviderWorkerLaunches()
	if !gate.launched {
		t.Fatal("the readiness hook must start the gate's launch clock")
	}
	launchedAt := gate.launchedAt

	clock.advance(5 * time.Second)
	noteProviderWorkerLaunches()
	if !gate.launchedAt.Equal(launchedAt) {
		t.Fatal("a repeated hook call must not move the launch time")
	}
}
