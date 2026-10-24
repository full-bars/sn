package miner

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The loop waits out the initial delay, then probes on every tick until the
// context ends.
func TestSmartDialerProbeLoopProbesAfterDelayThenRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSmartDialerProbes(ctx, 20*time.Millisecond, 10*time.Millisecond, nil, func(context.Context) int {
			calls.Add(1)
			return 0
		})
	}()

	time.Sleep(5 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("probed %d times inside the initial delay, want 0", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("probed %d times, want at least 3 (initial then repeating)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe loop did not stop when the context was canceled")
	}
}

// The loop stops and joins with the stop func, so a slot never closes the
// strategy a probe is still dialing with.
func TestSmartDialerProbeStopJoinsTheLoop(t *testing.T) {
	enableSmartDialerForTest(t)
	strategy := testSmartDialerStrategy(t)
	stop := startSmartDialerProbes(context.Background(), strategy, "http://127.0.0.1:1/", testSmartDialerProxyOwner("127.0.0.1:1080"), 3, "127.0.0.1:1080", true)
	// nothing probes inside the initial delay, so the stop has a full delay to
	// interrupt rather than a connect to wait out
	done := make(chan struct{})
	go func() {
		defer close(done)
		stop()
		stop() // idempotent
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe stop did not return")
	}
}

// Off is the default, and off must cost nothing: no probe goroutine, no timer,
// no summary ticker per identity. Counting the functions in the goroutine
// stacks is what proves the loop was never started; a goroutine count alone
// would not name it.
func TestSmartDialerProbeStartAllocatesNothingWhileOff(t *testing.T) {
	previous := connect.SetSmartDialer(false)
	defer connect.SetSmartDialer(previous)

	strategy := testSmartDialerStrategy(t)
	loopsBefore := strings.Count(goroutineStacks(), "miner.runSmartDialerProbes")
	summariesBefore := strings.Count(goroutineStacks(), "miner.runSmartDialerSummary")
	for i := 0; i < 3; i++ {
		stop := startSmartDialerProbes(context.Background(), strategy, "http://127.0.0.1:1/", "direct", i, "direct", false)
		stop()
		stop() // idempotent
	}
	if got := strings.Count(goroutineStacks(), "miner.runSmartDialerProbes"); got > loopsBefore {
		t.Fatalf("%d probe loops run while the smart dialer is off (was %d before the calls): an opted-out node must start none", got, loopsBefore)
	}
	if got := strings.Count(goroutineStacks(), "miner.runSmartDialerSummary"); got > summariesBefore {
		t.Fatalf("%d summary tickers run while the smart dialer is off (was %d before the calls)", got, summariesBefore)
	}
}

// The positive control for the test above: with the preference on, the loop
// does start, and stop joins it.
func TestSmartDialerProbeStartRunsWhileOn(t *testing.T) {
	enableSmartDialerForTest(t)
	strategy := testSmartDialerStrategy(t)
	stop := startSmartDialerProbes(context.Background(), strategy, "http://127.0.0.1:1/", "direct", 0, "direct", false)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(goroutineStacks(), "miner.runSmartDialerProbes") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(goroutineStacks(), "miner.runSmartDialerProbes") {
		t.Fatal("the smart dialer is on but no probe loop started")
	}
	stop()
	if strings.Contains(goroutineStacks(), "miner.runSmartDialerProbes") {
		t.Fatal("the probe loop still runs after stop")
	}
}

// testSmartDialerProxyOwner is the owner a proxy at addr would take. The real
// one is derived from the full proxy key, which a test does not need to model:
// what matters is that it comes from the identity, not the slot label.
func testSmartDialerProxyOwner(addr string) string {
	return smartDialerGatewayOwner(&connect.ProxySettings{Network: "tcp", Address: addr})
}

// Losing the preference stops the loop: off has to mean no wakeups, not a timer
// that fires every five minutes to run a probe that measures nothing.
func TestSmartDialerProbeLoopStopsWhenThePreferenceIsWithdrawn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var withdrawn atomic.Bool
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSmartDialerProbes(ctx, time.Millisecond, time.Millisecond, withdrawn.Load, func(context.Context) int {
			calls.Add(1)
			return 0
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("probed %d times, want the loop running", calls.Load())
	}

	withdrawn.Store(true)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe loop kept running after the preference was withdrawn")
	}
	// and it really stopped: no further rounds after the loop returned
	after := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != after {
		t.Fatalf("probed %d more times after the loop returned", got-after)
	}
}

// The summary ticker lives exactly as long as some probe loop does: it starts
// with the first, survives one loop stopping while another still probes, and is
// joined by the last. A miner that stopped probing carries no five-minute wakeup
// and no goroutine no teardown can reach.
func TestSmartDialerProbeStopStopsTheSummaryTicker(t *testing.T) {
	enableSmartDialerForTest(t)
	first := startSmartDialerProbes(context.Background(), testSmartDialerStrategy(t), "http://127.0.0.1:1/", "direct", 0, "direct", false)
	second := startSmartDialerProbes(context.Background(), testSmartDialerStrategy(t), "http://127.0.0.1:1/", testSmartDialerProxyOwner("127.0.0.1:1080"), 1, "127.0.0.1:1080", true)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(goroutineStacks(), "miner.runSmartDialerSummary") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(goroutineStacks(), "miner.runSmartDialerSummary") {
		t.Fatal("the probe loops started but no summary ticker did")
	}

	// one identity ending must not close the window the others report in
	first()
	if !strings.Contains(goroutineStacks(), "miner.runSmartDialerSummary") {
		t.Fatal("the summary ticker stopped while a probe loop was still running")
	}

	second()
	waitForNoGoroutineFrame(t, "miner.runSmartDialerSummary")
}

// waitForNoGoroutineFrame waits until no goroutine's stack carries frame. The
// stop funcs join their goroutine, but the frame survives the join until the
// goroutine has actually returned to the scheduler, so a single check races.
func waitForNoGoroutineFrame(t *testing.T, frame string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for strings.Contains(goroutineStacks(), frame) {
		if deadline.Before(time.Now()) {
			t.Fatalf("a goroutine is still running %s after the last probe loop stopped", frame)
		}
		time.Sleep(time.Millisecond)
	}
}

// goroutineStacks returns every goroutine's stack. A frame names the miner
// function the goroutine is in, which is how these tests tell a started loop
// from an allocated-nothing one.
func goroutineStacks() string {
	buf := make([]byte, 4<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// The probe starts only after the identity authenticated, never before: an
// identity that never gets through auth, or sits in slow retry, must not probe.
// It runs on the slot's own strategy (the one its wallet and renewal dial with),
// so the measurement and the dial are the same dialer set.
func TestSmartDialerProbeStartsAfterAuthOnTheSlotStrategy(t *testing.T) {
	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	created := strings.Index(text, "clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)")
	authSucceeded := strings.Index(text, "noteProviderAuthSuccess(proxySettings)")
	start := strings.Index(text, "stopSmartDialerProbes := startSmartDialerProbes(proxyCtx, clientStrategy, self.apiUrl")
	if created < 0 || authSucceeded < 0 {
		t.Fatal("the slot no longer creates its strategy or records auth success this way; update this test")
	}
	if start < 0 {
		t.Fatal("the slot does not start the smart dialer probe")
	}
	if start < authSucceeded {
		t.Fatal("the smart dialer probe starts before the auth outcome is known; it must start only after auth succeeds")
	}
	if created > start {
		t.Fatal("the probe does not run on the slot's own strategy")
	}
	if strings.Count(text, "startSmartDialerProbes(") != 1 {
		t.Fatal("expected exactly one probe start per identity")
	}
	// the strategy's close is deferred before the probe's stop, so the stop
	// (last registered) runs first and the loop is joined before the close
	if strings.Index(text, "defer clientStrategy.Close()") > strings.Index(text, "defer stopSmartDialerProbes()") {
		t.Fatal("the probe loop is not joined before the strategy is closed")
	}
}

// testSmartDialerStrategy gives a strategy with the ordinary dialer set and no
// extender directory, so probing it dials the url directly.
func testSmartDialerStrategy(t *testing.T) *connect.ClientStrategy {
	t.Helper()
	settings := connect.DefaultClientStrategySettings()
	settings.RequestTimeout = 5 * time.Second
	strategy := connect.NewClientStrategy(context.Background(), settings)
	t.Cleanup(strategy.Close)
	return strategy
}

func enableSmartDialerForTest(t *testing.T) {
	t.Helper()
	previous := connect.SetSmartDialer(true)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })
}

// A probe round measures every transport it can and reports the order it
// produced: the round names a leader, and every transport that answered is
// recorded with its cost. This is the seam the miner depends on — the ranking
// itself is the connect library's.
func TestProbeRoundMeasuresEveryTransportAndReportsTheOrder(t *testing.T) {
	enableSmartDialerForTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	strategy := testSmartDialerStrategy(t)
	var round connect.ProbeRound
	rounds := 0
	attempted := strategy.ProbeDialersWith(context.Background(), server.URL, connect.SmartDialerProbeOptions{
		OnRound: func(r connect.ProbeRound) {
			round = r
			rounds += 1
		},
	})
	if attempted == 0 {
		t.Fatal("the round attempted no transport")
	}
	if rounds != 1 {
		t.Fatalf("rounds = %d, want 1", rounds)
	}
	if len(round.Results) != attempted {
		t.Fatalf("round reported %d results for %d attempts", len(round.Results), attempted)
	}
	okTransports := 0
	for _, result := range round.Results {
		if result.Transport == "" {
			t.Fatalf("a result carried no transport name: %+v", result)
		}
		if result.Status == "ok" {
			okTransports += 1
		}
	}
	if okTransports == 0 {
		t.Fatalf("no transport was measured ok against a live server: %+v", round.Results)
	}
	if round.Leader == "" {
		t.Fatalf("the round produced no leader: %+v", round.Results)
	}

	// the schedule is stable when nothing changes: nothing is due, so the next
	// call does no work at all
	var second connect.ProbeRound
	again := strategy.ProbeDialersWith(context.Background(), server.URL, connect.SmartDialerProbeOptions{
		OnRound: func(r connect.ProbeRound) { second = r },
	})
	if again != 0 {
		t.Fatalf("a second round attempted %d transports with nothing due", again)
	}
	if second.Results != nil {
		t.Fatalf("a round with nothing due reported results: %+v", second.Results)
	}
}

// A dead path is deprioritised: the round that finds it dead records the
// failures, produces no leader, and the transports stop being probed until
// their own penalty window passes, so a dead proxy never keeps paying the
// round's cost on every tick.
func TestDeadTransportsStopBeingProbed(t *testing.T) {
	enableSmartDialerForTest(t)
	// a loopback port with nothing listening: every dial is refused at once
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadUrl := "http://" + listener.Addr().String() + "/"
	listener.Close()

	strategy := testSmartDialerStrategy(t)
	var round connect.ProbeRound
	rounds := 0
	attempted := strategy.ProbeDialersWith(context.Background(), deadUrl, connect.SmartDialerProbeOptions{
		OnRound: func(r connect.ProbeRound) {
			round = r
			rounds += 1
		},
	})
	if attempted == 0 {
		t.Fatal("the round attempted no transport")
	}
	if rounds != 1 {
		t.Fatalf("rounds = %d, want 1", rounds)
	}
	if round.Leader != "" {
		t.Fatalf("a round where every transport is dead named a leader %q", round.Leader)
	}
	for _, result := range round.Results {
		if result.Status == "ok" {
			t.Fatalf("%s was reported ok against a dead endpoint", result.Transport)
		}
	}

	// the second round is the last one that pays the cost: every transport is
	// marked blocked and is not dialed again
	var blocked connect.ProbeRound
	blockedRounds := 0
	strategy.ProbeDialersWith(context.Background(), deadUrl, connect.SmartDialerProbeOptions{
		OnRound: func(r connect.ProbeRound) {
			blocked = r
			blockedRounds += 1
		},
	})
	if blockedRounds != 1 {
		t.Fatalf("rounds = %d, want 1 (the round that marks the transports blocked)", blockedRounds)
	}
	for _, result := range blocked.Results {
		if result.Status != "blocked" {
			t.Fatalf("%s = %q after repeated failure, want blocked: %+v", result.Transport, result.Status, blocked.Results)
		}
	}

	// the dead transports are now not probed at all: no connects, no round
	again := strategy.ProbeDialersWith(context.Background(), deadUrl, connect.SmartDialerProbeOptions{
		OnRound: func(r connect.ProbeRound) {
			t.Fatalf("a round ran while every transport was blocked: %+v", r.Results)
		},
	})
	if again != 0 {
		t.Fatalf("the dead transports were probed again: %d attempts while every transport was blocked", again)
	}
}

// Everything is a no-op while the smart dialer is off: the miner runs with the
// control key at its default, and nothing may dial for a preference nobody
// asked for.
func TestProbesDoNothingWhileTheSmartDialerIsOff(t *testing.T) {
	previous := connect.SetSmartDialer(false)
	defer connect.SetSmartDialer(previous)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	strategy := testSmartDialerStrategy(t)
	if attempted := strategy.ProbeDialersWith(context.Background(), server.URL, connect.SmartDialerProbeOptions{}); attempted != 0 {
		t.Fatalf("probed %d transports while the smart dialer was off", attempted)
	}
}
