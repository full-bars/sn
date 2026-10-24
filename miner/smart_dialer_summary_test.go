package miner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
	"github.com/urnetwork/connect"
)

// The summary is rendered per PATH now, not per identity: the operator needs to
// know which egress is failing, and how many identities sit behind it.
func TestFormatProbeRoundNamesThePathAndItsIdentities(t *testing.T) {
	line := formatProbeRound("203.0.113.7:1080", 12, 4, testProbeRound("fragment", "normal",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(320), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(140), Status: "ok"},
		connect.ProbeTransportResult{Transport: "reorder", Status: "blocked"},
	))
	for _, want := range []string{"203.0.113.7:1080", "12 identities, measured via proxy[4]", "fragment=320ms", "normal=140ms", "reorder=blocked", "leader=fragment->normal"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("line %q is not exactly one line", line)
	}
}

func testProbeRound(leaderBefore string, leader string, results ...connect.ProbeTransportResult) connect.ProbeRound {
	return connect.ProbeRound{PreviousLeader: leaderBefore, Leader: leader, Results: results}
}

func probeMs(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// A probe result changes what the schedule reports: the round's own leader is
// recorded, a change of leader is a change line, and the aggregate carries the
// new leader distribution. A dead path is reported by its verdict and is never
// counted as a working transport.
func TestSummaryReportsTheProbedOrderAndTheDeadPath(t *testing.T) {
	summary := newSmartDialerSummary()
	if line := summary.flush(0); line != "" {
		t.Fatalf("empty window rendered %q", line)
	}
	now := time.Now()
	// round 1: the first choice leads, and the alternative is measured
	if line := summary.record(now, "10.0.0.1:1080", 4, testProbeRound("", "fragment",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(100), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(60), Status: "ok"},
	)); line != "" {
		t.Fatalf("a first round printed a change line %q", line)
	}
	// round 2: the measurement moves the leader, and one transport is dead
	change := summary.record(now, "10.0.0.1:1080", 4, testProbeRound("fragment", "normal",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(100), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(60), Status: "ok"},
		connect.ProbeTransportResult{Transport: "reorder", Status: "blocked"},
	))
	if !strings.Contains(change, "leader changed fragment -> normal") {
		t.Fatalf("change line %q missing the leader change", change)
	}
	// round 3: another path is probed and fails
	if line := summary.record(now, "10.0.0.2:1080", 9, testProbeRound("normal", "normal",
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(200), Status: "ok"},
		connect.ProbeTransportResult{Transport: "reorder", Status: "fail"},
	)); line != "" {
		t.Fatalf("an unchanged leader printed %q", line)
	}

	line := summary.flush(37)
	for _, want := range []string{
		"3 rounds over 2 paths (13 identities)",
		"fragment median=100ms ok=2 fail=0 blocked=0",
		"normal median=60ms ok=3 fail=0 blocked=0",
		// a dead transport is counted by its verdict, never as a success
		"reorder median=0ms ok=0 fail=1 blocked=1",
		"leader fragment=1 normal=2",
		"leader_changes=1",
		"deferred_for_traffic=37",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "leader reorder") || strings.Contains(line, "slowest 10.0.0.1:1080 (4 identities) reorder") {
		t.Fatalf("the dead transport was ranked: %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("summary %q is not one line", line)
	}
	if again := summary.flush(0); again != "" {
		t.Fatalf("the window did not reset: %q", again)
	}
}

// The aggregate line carries the numbers: counts, per transport median and
// outcomes, the slowest path with its identity fan-out.
func TestSummaryCarriesTheNumbersAndResets(t *testing.T) {
	summary := newSmartDialerSummary()
	now := time.Now()
	summary.record(now, "10.0.0.1:1080", 2, testProbeRound("", "fragment",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(100), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(150), Status: "ok"}))
	summary.record(now, "10.0.0.2:1080", 7, testProbeRound("fragment", "fragment",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(300), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(900), Status: "ok"}))
	line := summary.flush(0)
	if !strings.Contains(line, "slowest 10.0.0.2:1080 (7 identities) normal=900ms") {
		t.Fatalf("summary %q should name the slowest probe", line)
	}
	if !strings.Contains(line, "fragment median=300ms") {
		t.Fatalf("summary %q should carry the median", line)
	}
	if !strings.Contains(line, "(9 identities)") {
		t.Fatalf("summary %q should total the identities behind the paths", line)
	}
}

// A leader change prints once, rate limited; a round with no change prints
// nothing; the summary still counts every change.
func TestLeaderChangeLinesAreRateLimited(t *testing.T) {
	summary := newSmartDialerSummary()
	now := time.Now()
	if line := summary.record(now, "10.0.0.1:1080", 1, testProbeRound("", "fragment")); line != "" {
		t.Fatalf("first leader printed a change line %q", line)
	}
	if line := summary.record(now, "10.0.0.1:1080", 1, testProbeRound("fragment", "fragment")); line != "" {
		t.Fatalf("an unchanged leader printed %q", line)
	}
	printed := 0
	for i := 0; i < smartDialerLeaderLogBurst+10; i++ {
		if line := summary.record(now, "10.0.0.1:1080", 1, testProbeRound("fragment", "normal")); line != "" {
			printed += 1
			if !strings.Contains(line, "leader changed fragment -> normal") {
				t.Fatalf("unexpected line %q", line)
			}
		}
	}
	if printed != smartDialerLeaderLogBurst {
		t.Fatalf("printed %d change lines in a minute, want the cap %d", printed, smartDialerLeaderLogBurst)
	}
	line := summary.flush(0)
	if !strings.Contains(line, "leader_changes=30") || !strings.Contains(line, "(10 change lines suppressed)") {
		t.Fatalf("summary %q should count every change and report the suppressed lines", line)
	}
	// the next minute prints again
	if line := summary.record(now.Add(2*time.Minute), "10.0.0.1:1080", 1, testProbeRound("fragment", "normal")); line == "" {
		t.Fatal("the rate limit did not reopen after a minute")
	}
}

// A window with no round at all must not eat the deferrals it accumulated: the
// gate calls did wait for traffic, and the next window that renders has to
// account for them.
func TestSmartDialerSummaryKeepsDeferredAcrossAQuietWindow(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	_ = scheduler
	globalSmartDialerSummary = newSmartDialerSummary()
	globalSmartDialerScheduler.deferred.Store(0)
	t.Cleanup(func() {
		globalSmartDialerSummary = newSmartDialerSummary()
		globalSmartDialerScheduler.deferred.Store(0)
	})

	globalSmartDialerScheduler.deferred.Add(7)
	if line := smartDialerSummaryWindow(); line != "" {
		t.Fatalf("a quiet window rendered %q", line)
	}
	if got := globalSmartDialerScheduler.deferred.Load(); got != 7 {
		t.Fatalf("deferred = %d after a quiet window, want the 7 kept for the next one", got)
	}

	// the next window has a round, and reports the carried deferrals with it
	globalSmartDialerSummary.record(time.Now(), "10.0.0.1:1080", 3, testProbeRound("", "normal",
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(50), Status: "ok"}))
	line := smartDialerSummaryWindow()
	if !strings.Contains(line, "deferred_for_traffic=7") {
		t.Fatalf("summary %q lost the carried deferrals", line)
	}
	if got := globalSmartDialerScheduler.deferred.Load(); got != 0 {
		t.Fatalf("deferred = %d after the window rendered, want 0", got)
	}
}

// Per window samples are bounded.
func TestSummarySamplesAreBounded(t *testing.T) {
	aggregate := &transportAggregate{}
	for i := 0; i < 5*smartDialerSampleCap; i++ {
		aggregate.add(probeMs(i))
	}
	if len(aggregate.samples) != smartDialerSampleCap {
		t.Fatalf("held %d samples, want the cap %d", len(aggregate.samples), smartDialerSampleCap)
	}
}

// QuietFor now reads the TRAFFIC clock, not the auth clock. Auth-idle for a
// two-minute window is unreachable on a node running thousands of identities,
// and traffic is the resource a probe actually competes for.
func TestAdmissionGateQuietForTracksTrafficNotAuth(t *testing.T) {
	limiter := newAuthRateLimiter(1000, 1000, 1000)
	gate := newProxyAdmissionGate(limiter)
	bw := &bandwidth.ProxyBandwidth{}

	if !gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, time.Minute) {
		t.Fatal("a counter that never carried a session is quiet")
	}
	bw.AddSession("client", time.Now())
	if gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, 0) {
		t.Fatal("quiet while a client session is open")
	}
	bw.RemoveSession("client")
	if gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, time.Hour) {
		t.Fatal("quiet for an hour right after the session ended")
	}
	if !gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, 0) {
		t.Fatal("not quiet at zero window with no session")
	}
	time.Sleep(20 * time.Millisecond)
	if !gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, 10*time.Millisecond) {
		t.Fatal("a counter idle past the window is not quiet")
	}
	// a path is as quiet as its busiest counter
	busy := &bandwidth.ProxyBandwidth{}
	busy.AddSession("client", time.Now())
	if gate.QuietFor([]*bandwidth.ProxyBandwidth{bw, busy}, 10*time.Millisecond) {
		t.Fatal("a path with one serving identity reads as quiet")
	}

	// auth activity miner-wide (inAdmit > 0) does not starve probes when the path has no client traffic
	if !gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, 0) {
		t.Fatal("quiet before any auth attempt")
	}
	release, err := gate.Admit(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if gate.inAdmit.Load() <= 0 {
		t.Fatal("expected inAdmit to be positive during admitted auth")
	}
	if !gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, 0) {
		t.Fatal("quiet while an auth attempt is in flight: auth activity must not starve probes")
	}
	// but actual client traffic on the path still keeps it from probing
	bwActive := &bandwidth.ProxyBandwidth{}
	bwActive.AddSession("client", time.Now())
	if gate.QuietFor([]*bandwidth.ProxyBandwidth{bwActive}, 0) {
		t.Fatal("path with active client session must not be quiet")
	}
	release()
	if !gate.QuietFor([]*bandwidth.ProxyBandwidth{bw}, 0) {
		t.Fatal("still busy after the auth attempt finished")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := gate.Admit(ctx, 5); err == nil {
		r() // the dispatch loop won the race; the slot is returned
	}
	if gate.inAdmit.Load() != 0 {
		t.Fatalf("inAdmit = %d after a canceled or finished wait, want 0", gate.inAdmit.Load())
	}
}

// The control key's whole point is the live toggle, so the classification must
// say so, and the tables must not disagree about it.
func TestSmartDialerControlKeyIsLiveAndDisjoint(t *testing.T) {
	if !controlKeys["smart_dialer"] {
		t.Fatal("smart_dialer is no longer an accepted control key")
	}
	if !controlLiveEffectKeys["smart_dialer"] {
		t.Fatal("smart_dialer is not classified live: the scheduler made the live case possible")
	}
	if controlRestartEffectKeys["smart_dialer"] {
		t.Fatal("smart_dialer is still restart-required somewhere: the two tables disagree")
	}
	if got := controlKeyEffect("smart_dialer"); got != controlEffectLive {
		t.Fatalf("controlKeyEffect(smart_dialer) = %q, want %q", got, controlEffectLive)
	}
	if controlNeedsRestart("smart_dialer") {
		t.Fatal("smart_dialer still reports needs_restart")
	}
	if got := controlLiveDefaults["smart_dialer"]; got != "off" {
		t.Fatalf("controlLiveDefaults[smart_dialer] = %q, want off: it stays opt-in", got)
	}
	// a key can only be one thing, or the reported effect is whichever table
	// happens to be consulted first
	for key := range controlLiveEffectKeys {
		if controlRestartEffectKeys[key] {
			t.Errorf("%s is both live and restart-required", key)
		}
		if controlNoEffectKeys[key] {
			t.Errorf("%s is both live and no-effect", key)
		}
	}
	for key := range controlRestartEffectKeys {
		if controlNoEffectKeys[key] {
			t.Errorf("%s is both restart-required and no-effect", key)
		}
	}
}

// The startup replay is what keeps a restart from losing the value, and with it
// the scheduler: a persisted on must leave a running scheduler, an absent or
// off value must leave none.
func TestSmartDialerReplayAtStartupAppliesThePersistedValue(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	state := newControlState()
	state.setWithValue("smart_dialer", "on", SourceSocket)
	replaySmartDialerAtStartup(state)
	if !connect.SmartDialerEnabled() {
		t.Fatal("the persisted smart_dialer=on was not applied at startup")
	}
	if !scheduler.Running() {
		t.Fatal("a persisted smart_dialer=on left the scheduler stopped: nothing would probe")
	}

	off := newControlState()
	off.setWithValue("smart_dialer", "off", SourceSocket)
	replaySmartDialerAtStartup(off)
	if connect.SmartDialerEnabled() {
		t.Fatal("the persisted smart_dialer=off was not applied at startup")
	}

	// an absent value is the default (off), never a stale on, and it must not
	// start a scheduler
	connect.SetSmartDialer(true)
	scheduler.Stop()
	replaySmartDialerAtStartup(newControlState())
	if connect.SmartDialerEnabled() {
		t.Fatal("a state with no smart_dialer opinion left the preference on")
	}
	if scheduler.Running() {
		t.Fatal("a state with no smart_dialer opinion started the scheduler")
	}
	// a nil state (offline tools, unit tests) must not panic
	replaySmartDialerAtStartup(nil)
}

// A re-enable after a stop starts exactly one dispatcher, however many times
// the start is asked for: the control socket, the startup replay and a slot's
// registration can all reach it.
func TestSmartDialerSchedulerStartsOnce(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	var wg sync.WaitGroup
	started := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- scheduler.Start()
		}()
	}
	wg.Wait()
	close(started)
	count := 0
	for ok := range started {
		if ok {
			count += 1
		}
	}
	if count != 1 {
		t.Fatalf("%d concurrent starts reported a start, want exactly 1", count)
	}
	if got := strings.Count(goroutineStacks(), "miner.(*smartDialerScheduler).run"); got != 1 {
		t.Fatalf("%d scheduler goroutines run after 8 starts, want 1", got)
	}
	scheduler.Stop()
	waitForNoSchedulerGoroutine(t)
}
