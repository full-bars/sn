package miner

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

type smartDialerFakeClock struct{ at atomic.Int64 }

func (c *smartDialerFakeClock) now() time.Time          { return time.Unix(0, c.at.Load()) }
func (c *smartDialerFakeClock) advance(d time.Duration) { c.at.Add(int64(d)) }

func testSmartDialerScheduler(clock *smartDialerFakeClock, quiet func(time.Duration) bool) *smartDialerScheduler {
	s := newSmartDialerScheduler(clock.now(), quiet)
	s.now = clock.now
	// a sleep that moves the fake clock instead of waiting
	s.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.advance(d)
		return nil
	}
	return s
}

func newSmartDialerTestClock() *smartDialerFakeClock {
	clock := &smartDialerFakeClock{}
	clock.at.Store(time.Now().UnixNano())
	return clock
}

// Nothing probes during the startup grace, even with auth idle: a hot restart
// reuses client logins, so the auth gate can sit idle while the pool dials in.
func TestSmartDialerSchedulerHoldsProbesForTheStartupGrace(t *testing.T) {
	clock := newSmartDialerTestClock()
	s := testSmartDialerScheduler(clock, func(time.Duration) bool { return true })
	if s.ready() {
		t.Fatal("ready during the startup grace")
	}
	clock.advance(smartDialerProbeStartupGrace)
	if !s.ready() {
		t.Fatal("not ready after the startup grace with auth idle")
	}
}

// A probe never starts while auth is busy and steps aside if auth begins while
// it was waiting for its slot or its token.
func TestSmartDialerSchedulerNeverStartsWhileAuthIsBusyAndCountsTheDeferral(t *testing.T) {
	clock := newSmartDialerTestClock()
	clock.advance(time.Hour)
	var busyChecks atomic.Int32
	// busy for the first three readiness checks, then idle
	quiet := func(d time.Duration) bool {
		if d != smartDialerProbeAuthQuiet {
			t.Errorf("quiet window = %v, want %v", d, smartDialerProbeAuthQuiet)
		}
		return 3 < busyChecks.Add(1)
	}
	s := newSmartDialerScheduler(clock.now().Add(-2*time.Hour), quiet)
	s.now = clock.now
	s.sleep = func(ctx context.Context, d time.Duration) error { clock.advance(d); return ctx.Err() }

	release, err := s.Gate(context.Background(), "1.2.3.4:1080", "proxy[1]")
	if err != nil {
		t.Fatalf("gate error: %v", err)
	}
	release()
	if busyChecks.Load() <= 3 {
		t.Fatalf("gate returned after %d readiness checks while auth was busy", busyChecks.Load())
	}
	if got := s.deferred.Load(); got != 1 {
		t.Fatalf("deferred = %d, want 1 (one gate call that had to wait)", got)
	}
}

// At most smartDialerProbeMaxInFlight connects run at once.
func TestSmartDialerSchedulerCapsInFlightProbes(t *testing.T) {
	clock := newSmartDialerTestClock()
	clock.advance(time.Hour)
	s := testSmartDialerScheduler(clock, func(time.Duration) bool { return true })
	s.start = clock.now().Add(-time.Hour)

	var releases []func()
	for i := 0; i < smartDialerProbeMaxInFlight; i++ {
		release, err := s.Gate(context.Background(), "", "proxy[x]")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Gate(ctx, "", "proxy[y]"); err == nil {
		t.Fatal("a third probe connect started while the cap was full")
	}
	releases[0]()
	release, err := s.Gate(context.Background(), "", "proxy[y]")
	if err != nil {
		t.Fatalf("no slot after a release: %v", err)
	}
	release()
	releases[1]()
}

// Connects start at most one per interval across the whole process.
func TestSmartDialerSchedulerSpacesConnectStarts(t *testing.T) {
	clock := newSmartDialerTestClock()
	clock.advance(time.Hour)
	s := testSmartDialerScheduler(clock, func(time.Duration) bool { return true })
	s.start = clock.now().Add(-time.Hour)

	first := clock.now()
	for i := 0; i < 5; i++ {
		release, err := s.Gate(context.Background(), "", "proxy[x]")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if elapsed := clock.now().Sub(first); elapsed < 4*smartDialerProbeInterval {
		t.Fatalf("5 connects started over %v, want at least %v", elapsed, 4*smartDialerProbeInterval)
	}
}

// A canceled context ends the wait with its error and frees everything.
func TestSmartDialerSchedulerGateStopsWithTheContext(t *testing.T) {
	clock := newSmartDialerTestClock()
	s := testSmartDialerScheduler(clock, func(time.Duration) bool { return false })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Gate(ctx, "", "proxy[x]"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Identities behind one shared host:port: the first owns the probing, the
// others are refused (and not penalised) until the hold passes.
func TestSmartDialerSchedulerLetsOneIdentityOwnASharedGateway(t *testing.T) {
	clock := newSmartDialerTestClock()
	clock.advance(time.Hour)
	s := testSmartDialerScheduler(clock, func(time.Duration) bool { return true })
	s.start = clock.now().Add(-time.Hour)

	ownerA, ownerB := "proxy:aaaa", "proxy:bbbb"
	release, err := s.Gate(context.Background(), "gw.example:1080", ownerA)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := s.Gate(context.Background(), "gw.example:1080", ownerB); !errors.Is(err, errSmartDialerGatewayShared) {
		t.Fatalf("second identity on the same gateway: err = %v, want errSmartDialerGatewayShared", err)
	}
	// the owner keeps probing
	release, err = s.Gate(context.Background(), "gw.example:1080", ownerA)
	if err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	release()
	// a different address is unaffected
	release, err = s.Gate(context.Background(), "other.example:1080", ownerB)
	if err != nil {
		t.Fatalf("different address refused: %v", err)
	}
	release()
	// after the hold the address is free
	clock.advance(smartDialerGatewayHold + time.Minute)
	release, err = s.Gate(context.Background(), "gw.example:1080", ownerB)
	if err != nil {
		t.Fatalf("gateway not released after the hold: %v", err)
	}
	release()
}

// The direct identity has no address to share, so it never takes the gateway
// hold and never blocks a proxy that is configured on a real one.
func TestSmartDialerSchedulerDirectIdentityClaimsNothing(t *testing.T) {
	clock := newSmartDialerTestClock()
	clock.advance(time.Hour)
	s := testSmartDialerScheduler(clock, func(time.Duration) bool { return true })
	s.start = clock.now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		release, err := s.Gate(context.Background(), "", smartDialerGatewayOwner(nil))
		if err != nil {
			t.Fatalf("direct gate %d: %v", i, err)
		}
		release()
	}
}

// A reload hands the same proxy a new slot label (proxy_reloader.go starts every
// reloaded slot past the configured range), so the gateway hold must follow the
// IDENTITY, not the label. Keyed by the label, the reloaded slot missed its own
// hold and was locked out of the address it already owned for the full two
// hours.
func TestSmartDialerGatewayHoldFollowsTheIdentityAcrossAReload(t *testing.T) {
	clock := newSmartDialerTestClock()
	clock.advance(time.Hour)
	s := testSmartDialerScheduler(clock, func(time.Duration) bool { return true })
	s.start = clock.now().Add(-time.Hour)

	// slot 5 authenticates and starts probing gw.example
	first := &connect.ProxySettings{Network: "tcp", Address: "gw.example:1080", Auth: &proxy.Auth{User: "session-1", Password: "pw"}}
	owner := smartDialerGatewayOwner(first)
	release, err := s.Gate(context.Background(), first.Address, owner)
	if err != nil {
		t.Fatalf("first identity refused: %v", err)
	}
	release()

	// the reload: same identity, rotated credentials, and a new slot index
	rotated := &connect.ProxySettings{Network: "tcp", Address: "gw.example:1080", Auth: &proxy.Auth{User: "session-1", Password: "rotated"}}
	if got := smartDialerGatewayOwner(rotated); got != owner {
		t.Fatalf("the owner changed across a credential rotation (%q -> %q): a reload would move the hold", got, owner)
	}
	release, err = s.Gate(context.Background(), rotated.Address, smartDialerGatewayOwner(rotated))
	if err != nil {
		t.Fatalf("the reloaded slot was locked out of its own gateway: %v", err)
	}
	release()

	// a genuinely different identity behind the same host:port is still held
	// off, which is the reason the hold exists at all
	other := &connect.ProxySettings{Network: "tcp", Address: "gw.example:1080", Auth: &proxy.Auth{User: "session-2"}}
	if got := smartDialerGatewayOwner(other); got == owner {
		t.Fatal("two identities behind one gateway share an owner, so the hold no longer separates them")
	}
	if _, err := s.Gate(context.Background(), other.Address, smartDialerGatewayOwner(other)); !errors.Is(err, errSmartDialerGatewayShared) {
		t.Fatalf("second identity on the same gateway: err = %v, want errSmartDialerGatewayShared", err)
	}

	// the owner never carries the credentials it is derived from
	if owner == "" || strings.Contains(owner, "session-1") || strings.Contains(owner, "pw") {
		t.Fatalf("owner %q carries the proxy key or its credentials", owner)
	}
}

// A window with no round at all must not eat the deferrals it accumulated: the
// gate calls did wait for auth, and the next window that renders has to account
// for them.
func TestSmartDialerSummaryKeepsDeferredAcrossAQuietWindow(t *testing.T) {
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
	globalSmartDialerSummary.record(time.Now(), 1, "10.0.0.1:1080", testProbeRound("", "normal",
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(50), Status: "ok"}))
	line := smartDialerSummaryWindow()
	if !strings.Contains(line, "deferred_for_auth=7") {
		t.Fatalf("summary %q lost the carried deferrals", line)
	}
	if got := globalSmartDialerScheduler.deferred.Load(); got != 0 {
		t.Fatalf("deferred = %d after the window rendered, want 0", got)
	}
}

func testProbeRound(leaderBefore string, leader string, results ...connect.ProbeTransportResult) connect.ProbeRound {
	return connect.ProbeRound{PreviousLeader: leaderBefore, Leader: leader, Results: results}
}

func probeMs(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// The per identity line names the identity by index and address, carries every
// transport's cost or outcome, and never carries credentials.
func TestFormatProbeRoundNamesTheIdentityAndEveryTransport(t *testing.T) {
	line := formatProbeRound(12, "203.0.113.7:1080", testProbeRound("fragment", "normal",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(320), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(140), Status: "ok"},
		connect.ProbeTransportResult{Transport: "reorder", Status: "blocked"},
	))
	for _, want := range []string{"proxy[12] (203.0.113.7:1080)", "fragment=320ms", "normal=140ms", "reorder=blocked", "leader=fragment->normal"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("line %q is not exactly one line", line)
	}
}

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
	if line := summary.record(now, 1, "10.0.0.1:1080", testProbeRound("", "fragment",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(100), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(60), Status: "ok"},
	)); line != "" {
		t.Fatalf("a first round printed a change line %q", line)
	}
	// round 2: the measurement moves the leader, and one path is dead
	change := summary.record(now, 1, "10.0.0.1:1080", testProbeRound("fragment", "normal",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(100), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(60), Status: "ok"},
		connect.ProbeTransportResult{Transport: "reorder", Status: "blocked"},
	))
	if !strings.Contains(change, "leader changed fragment -> normal") {
		t.Fatalf("change line %q missing the leader change", change)
	}
	// round 3: the dead path is probed again and fails; it stays out of the lead
	if line := summary.record(now, 2, "10.0.0.2:1080", testProbeRound("normal", "normal",
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(200), Status: "ok"},
		connect.ProbeTransportResult{Transport: "reorder", Status: "fail"},
	)); line != "" {
		t.Fatalf("an unchanged leader printed %q", line)
	}

	line := summary.flush(37)
	for _, want := range []string{
		"3 rounds over 2 proxies",
		"fragment median=100ms ok=2 fail=0 blocked=0",
		"normal median=60ms ok=3 fail=0 blocked=0",
		// a dead path is counted by its verdict, never as a success
		"reorder median=0ms ok=0 fail=1 blocked=1",
		"leader fragment=1 normal=2",
		"leader_changes=1",
		"deferred_for_auth=37",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "leader reorder") || strings.Contains(line, "slowest proxy[1] (10.0.0.1:1080) reorder") {
		t.Fatalf("the dead path was ranked: %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("summary %q is not one line", line)
	}
	if again := summary.flush(0); again != "" {
		t.Fatalf("the window did not reset: %q", again)
	}
}

// The aggregate line carries the numbers: counts, per transport median and
// outcomes, the slowest identity with its address.
func TestSummaryCarriesTheNumbersAndResets(t *testing.T) {
	summary := newSmartDialerSummary()
	now := time.Now()
	summary.record(now, 1, "10.0.0.1:1080", testProbeRound("", "fragment",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(100), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(150), Status: "ok"}))
	summary.record(now, 2, "10.0.0.2:1080", testProbeRound("fragment", "fragment",
		connect.ProbeTransportResult{Transport: "fragment", Latency: probeMs(300), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: probeMs(900), Status: "ok"}))
	line := summary.flush(0)
	if !strings.Contains(line, "slowest proxy[2] (10.0.0.2:1080) normal=900ms") {
		t.Fatalf("summary %q should name the slowest probe", line)
	}
	if !strings.Contains(line, "fragment median=300ms") {
		t.Fatalf("summary %q should carry the median", line)
	}
}

// A leader change prints once, rate limited; a round with no change prints
// nothing; the summary still counts every change.
func TestLeaderChangeLinesAreRateLimited(t *testing.T) {
	summary := newSmartDialerSummary()
	now := time.Now()
	if line := summary.record(now, 1, "10.0.0.1:1080", testProbeRound("", "fragment")); line != "" {
		t.Fatalf("first leader printed a change line %q", line)
	}
	if line := summary.record(now, 1, "10.0.0.1:1080", testProbeRound("fragment", "fragment")); line != "" {
		t.Fatalf("an unchanged leader printed %q", line)
	}
	printed := 0
	for i := 0; i < smartDialerLeaderLogBurst+10; i++ {
		if line := summary.record(now, i, "10.0.0.1:1080", testProbeRound("fragment", "normal")); line != "" {
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
	if line := summary.record(now.Add(2*time.Minute), 1, "10.0.0.1:1080", testProbeRound("fragment", "normal")); line == "" {
		t.Fatal("the rate limit did not reopen after a minute")
	}
}

// The preference is opt-in: the default is off. It is declared in exactly one
// place — the live-default table, which a state with no opinion resolves to and
// which clearing the key restores — and that is the value a restart applies.
// The key registries only say the socket accepts the key and what a write to it
// can do; they are not defaults, and turning them off would make the CLI reject
// the key instead of opting in.
func TestSmartDialerDefaultIsOff(t *testing.T) {
	if got := controlLiveDefaults["smart_dialer"]; got != "off" {
		t.Fatalf("controlLiveDefaults[smart_dialer] = %q, want off: smart dialer is opt-in only", got)
	}
	if !controlKeys["smart_dialer"] {
		t.Fatal("smart_dialer is no longer an accepted control key")
	}
	// A live "on" cannot keep its promise: the probe loop is per identity and
	// starts only after that identity authenticated, and a running slot never
	// re-authenticates. The key is restart-required so the socket reports what
	// it can really do.
	if controlLiveEffectKeys["smart_dialer"] {
		t.Fatal("smart_dialer is classified live, but a live enable never starts probes for running slots")
	}
	if !controlRestartEffectKeys["smart_dialer"] || controlKeyEffect("smart_dialer") != controlEffectRestart {
		t.Fatal("smart_dialer is not classified restart-required")
	}

	// A node that never opted in never turns it on, at startup or later.
	previous := connect.SetSmartDialer(true)
	defer connect.SetSmartDialer(previous)
	replaySmartDialerAtStartup(newControlState())
	if connect.SmartDialerEnabled() {
		t.Fatal("a state with no smart_dialer opinion left the preference on")
	}
	// ... and an explicit off is off.
	off := newControlState()
	off.setWithValue("smart_dialer", "off", SourceSocket)
	replaySmartDialerAtStartup(off)
	if connect.SmartDialerEnabled() {
		t.Fatal("a persisted smart_dialer=off was not applied at startup")
	}
}

// The control surface tells the truth about the key: a set is persisted and
// reported as needing a restart, and it does not touch the running process.
// Anything else is the miner claiming an effect it cannot deliver - a live "on"
// that only flips the preference leaves every already-authenticated slot
// unmeasured for the life of the process.
func TestSmartDialerControlKeyAppliesOnlyAtStartup(t *testing.T) {
	state := newAuditWiringState(t)
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "smart_dialer", Value: "on"})
	if !resp.OK {
		t.Fatalf("set smart_dialer on: %s", resp.Error)
	}
	if resp.Effect != controlEffectRestart || !resp.NeedsRestart {
		t.Fatalf("set smart_dialer on reported effect=%q needs_restart=%v, want %q/true", resp.Effect, resp.NeedsRestart, controlEffectRestart)
	}
	if connect.SmartDialerEnabled() {
		t.Fatal("a set on a running miner changed the preference: the key is restart-required, so the running process must keep the value it started with")
	}
	if value, ok := state.get("smart_dialer"); !ok || value != "on" {
		t.Fatalf("the set did not persist: %q, %v", value, ok)
	}
	// The restart is what applies it, and that is the replay.
	replaySmartDialerAtStartup(state)
	if !connect.SmartDialerEnabled() {
		t.Fatal("the persisted value was not applied at startup, so needs_restart is empty")
	}

	// Off is the same contract in the other direction.
	connect.SetSmartDialer(true)
	resp = handleControlRequest(state, controlRequest{Cmd: "set", Key: "smart_dialer", Value: "off"})
	if !resp.OK || !resp.NeedsRestart {
		t.Fatalf("set smart_dialer off = %+v, want ok and needs_restart", resp)
	}
	if !connect.SmartDialerEnabled() {
		t.Fatal("a set off on a running miner changed the preference, but the miner reports needs_restart")
	}
	replaySmartDialerAtStartup(state)
	if connect.SmartDialerEnabled() {
		t.Fatal("the persisted off was not applied at startup")
	}

	// Clearing is restart-required too, and keeps the off default for the
	// restart to apply.
	resp = handleControlRequest(state, controlRequest{Cmd: "clear", Key: "smart_dialer"})
	if !resp.OK || !resp.NeedsRestart || resp.Effect != controlEffectRestart {
		t.Fatalf("clear smart_dialer = %+v, want ok, restart effect and needs_restart", resp)
	}
	if value, ok := state.get("smart_dialer"); ok {
		t.Fatalf("clear kept the value %q", value)
	}
	replaySmartDialerAtStartup(state)
	if connect.SmartDialerEnabled() {
		t.Fatal("a cleared smart_dialer did not restart at the off default")
	}
}

// A probe result is only worth measuring if the preference it feeds survives a
// restart: a set on a running miner only persists the value (the key is
// restart-required), so the persisted value is replayed at startup.
func TestSmartDialerReplayAtStartupAppliesThePersistedValue(t *testing.T) {
	previous := connect.SetSmartDialer(false)
	defer connect.SetSmartDialer(previous)

	state := newControlState()
	state.setWithValue("smart_dialer", "on", SourceSocket)
	replaySmartDialerAtStartup(state)
	if !connect.SmartDialerEnabled() {
		t.Fatal("the persisted smart_dialer=on was not applied at startup")
	}

	cleared := newControlState()
	cleared.setWithValue("smart_dialer", "off", SourceSocket)
	replaySmartDialerAtStartup(cleared)
	if connect.SmartDialerEnabled() {
		t.Fatal("the persisted smart_dialer=off was not applied at startup")
	}

	// an absent value is the default (off), never a stale on
	connect.SetSmartDialer(true)
	replaySmartDialerAtStartup(newControlState())
	if connect.SmartDialerEnabled() {
		t.Fatal("a state with no smart_dialer opinion left the preference on")
	}
	// a nil state (offline tools, unit tests) must not panic
	replaySmartDialerAtStartup(nil)
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

// Auth admission: the quiet check reads only atomics (it can never contend
// with auth), is false while any attempt is waiting or running, and becomes
// true only after the quiet period.
func TestAdmissionGateQuietForTracksAuthActivity(t *testing.T) {
	limiter := newAuthRateLimiter(1000, 1000, 1000)
	gate := newProxyAdmissionGate(limiter)
	if !gate.QuietFor(time.Minute) {
		t.Fatal("a gate that never admitted anyone is quiet")
	}
	release, err := gate.Admit(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if gate.QuietFor(0) {
		t.Fatal("quiet while an attempt is in flight")
	}
	release()
	if gate.QuietFor(time.Hour) {
		t.Fatal("quiet for an hour right after a release")
	}
	if !gate.QuietFor(0) {
		t.Fatal("not quiet at zero window with nothing in flight")
	}
	// a wait that is canceled does not leave the gate busy
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := gate.Admit(ctx, 5); err == nil {
		r() // the dispatch loop won the race; the slot is returned
	}
	if gate.inAdmit.Load() != 0 {
		t.Fatalf("inAdmit = %d after a canceled or finished wait, want 0", gate.inAdmit.Load())
	}
}
