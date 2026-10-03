package provider

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type fakeClock struct{ at atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.at.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.at.Add(int64(d)) }

func testScheduler(clock *fakeClock, quiet func(time.Duration) bool) *smartDialerScheduler {
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

// Nothing probes during the startup grace, even with auth idle: a hot restart
// reuses client logins, so the auth gate can sit idle while the pool dials in.
func TestSchedulerHoldsProbesForTheStartupGrace(t *testing.T) {
	clock := &fakeClock{}
	clock.at.Store(time.Now().UnixNano())
	s := testScheduler(clock, func(time.Duration) bool { return true })
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
func TestSchedulerNeverStartsWhileAuthIsBusyAndCountsTheDeferral(t *testing.T) {
	clock := &fakeClock{}
	clock.at.Store(time.Now().UnixNano())
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
func TestSchedulerCapsInFlightProbes(t *testing.T) {
	clock := &fakeClock{}
	clock.at.Store(time.Now().UnixNano())
	clock.advance(time.Hour)
	s := testScheduler(clock, func(time.Duration) bool { return true })
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
func TestSchedulerSpacesConnectStarts(t *testing.T) {
	clock := &fakeClock{}
	clock.at.Store(time.Now().UnixNano())
	clock.advance(time.Hour)
	s := testScheduler(clock, func(time.Duration) bool { return true })
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
func TestSchedulerGateStopsWithTheContext(t *testing.T) {
	clock := &fakeClock{}
	clock.at.Store(time.Now().UnixNano())
	s := testScheduler(clock, func(time.Duration) bool { return false })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Gate(ctx, "", "proxy[x]"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Proxies behind one shared host:port: the first owns the probing, the others
// are refused (and not penalised) until the hold passes.
func TestSchedulerLetsOneProxyOwnASharedGateway(t *testing.T) {
	clock := &fakeClock{}
	clock.at.Store(time.Now().UnixNano())
	clock.advance(time.Hour)
	s := testScheduler(clock, func(time.Duration) bool { return true })
	s.start = clock.now().Add(-time.Hour)

	release, err := s.Gate(context.Background(), "gw.example:1080", "proxy[1]")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := s.Gate(context.Background(), "gw.example:1080", "proxy[2]"); !errors.Is(err, errSmartDialerGatewayShared) {
		t.Fatalf("second proxy on the same gateway: err = %v, want errSmartDialerGatewayShared", err)
	}
	// the owner keeps probing
	release, err = s.Gate(context.Background(), "gw.example:1080", "proxy[1]")
	if err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	release()
	// a different address is unaffected
	release, err = s.Gate(context.Background(), "other.example:1080", "proxy[2]")
	if err != nil {
		t.Fatalf("different address refused: %v", err)
	}
	release()
	// after the hold the address is free
	clock.advance(smartDialerGatewayHold + time.Minute)
	release, err = s.Gate(context.Background(), "gw.example:1080", "proxy[2]")
	if err != nil {
		t.Fatalf("gateway not released after the hold: %v", err)
	}
	release()
}

func testRound(leaderBefore string, leader string, results ...connect.ProbeTransportResult) connect.ProbeRound {
	return connect.ProbeRound{PreviousLeader: leaderBefore, Leader: leader, Results: results}
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// The per proxy line names the proxy by index and address, carries every
// transport's cost or outcome, and never carries credentials.
func TestFormatProbeRoundNamesTheProxyAndEveryTransport(t *testing.T) {
	line := formatProbeRound(12, "203.0.113.7:1080", testRound("fragment", "normal",
		connect.ProbeTransportResult{Transport: "fragment", Latency: ms(320), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: ms(140), Status: "ok"},
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

// The aggregate line carries the numbers: counts, per transport median and
// outcomes, leader distribution, leader changes, the slowest proxy with its
// address, and the auth deferrals. It is empty when nothing was probed.
func TestSummaryCarriesTheNumbersAndResets(t *testing.T) {
	summary := newSmartDialerSummary()
	if line := summary.flush(0); line != "" {
		t.Fatalf("empty window rendered %q", line)
	}
	now := time.Now()
	summary.record(now, 1, "10.0.0.1:1080", testRound("", "fragment",
		connect.ProbeTransportResult{Transport: "fragment", Latency: ms(100), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: ms(150), Status: "ok"}))
	summary.record(now, 2, "10.0.0.2:1080", testRound("fragment", "normal",
		connect.ProbeTransportResult{Transport: "fragment", Latency: ms(300), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: ms(900), Status: "ok"},
		connect.ProbeTransportResult{Transport: "reorder", Status: "fail"}))
	summary.record(now, 3, "10.0.0.3:1080", testRound("normal", "normal",
		connect.ProbeTransportResult{Transport: "fragment", Latency: ms(500), Status: "ok"},
		connect.ProbeTransportResult{Transport: "normal", Latency: ms(200), Status: "ok"}))

	line := summary.flush(37)
	for _, want := range []string{
		"3 rounds over 3 proxies",
		"fragment median=300ms ok=3 fail=0 blocked=0",
		"normal median=200ms ok=3 fail=0 blocked=0",
		"reorder median=0ms ok=0 fail=1 blocked=0",
		"leader fragment=1 normal=2",
		"leader_changes=1",
		"slowest proxy[2] (10.0.0.2:1080) normal=900ms",
		"deferred_for_auth=37",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q missing %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("summary %q is not one line", line)
	}
	if again := summary.flush(0); again != "" {
		t.Fatalf("the window did not reset: %q", again)
	}
}

// A leader change prints once, rate limited; a round with no change prints
// nothing; the summary still counts every change.
func TestLeaderChangeLinesAreRateLimited(t *testing.T) {
	summary := newSmartDialerSummary()
	now := time.Now()
	if line := summary.record(now, 1, "10.0.0.1:1080", testRound("", "fragment")); line != "" {
		t.Fatalf("first leader printed a change line %q", line)
	}
	if line := summary.record(now, 1, "10.0.0.1:1080", testRound("fragment", "fragment")); line != "" {
		t.Fatalf("an unchanged leader printed %q", line)
	}
	printed := 0
	for i := 0; i < smartDialerLeaderLogBurst+10; i++ {
		if line := summary.record(now, i, "10.0.0.1:1080", testRound("fragment", "normal")); line != "" {
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
	if line := summary.record(now.Add(2*time.Minute), 1, "10.0.0.1:1080", testRound("fragment", "normal")); line == "" {
		t.Fatal("the rate limit did not reopen after a minute")
	}
}

// Per window samples are bounded.
func TestSummarySamplesAreBounded(t *testing.T) {
	aggregate := &transportAggregate{}
	for i := 0; i < 5*smartDialerSampleCap; i++ {
		aggregate.add(ms(i))
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
