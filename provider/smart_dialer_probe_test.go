package provider

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// probeGoroutines counts the goroutines that belong to a smart dialer probe
// loop. A goroutine that has been created but not scheduled yet shows the
// closure it was started from, and one that is running shows the loop, so both
// frames count as one probe goroutine; inLoop says how many of them have
// actually entered the loop.
func probeGoroutines() (total int, inLoop int) {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, block := range strings.Split(string(buf), "\n\n") {
		started := strings.Contains(block, "startSmartDialerProbes.func1(")
		looping := strings.Contains(block, "runSmartDialerProbes(")
		if !started && !looping {
			continue
		}
		total += 1
		if looping {
			inLoop += 1
		}
	}
	return total, inLoop
}

// The defect this guards: with the preference off the probe loop is a no-op, so
// spawning it per proxy bought nothing and cost one goroutine plus its stack for
// every proxy on the node. Nothing may be started at all — proved from the
// goroutine stacks, not inferred from the preference being read.
func TestSmartDialerProbesStartNoGoroutineWhenOff(t *testing.T) {
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())

	// Let the strategy's own construction settle so the count below moves only
	// if a probe start adds something.
	time.Sleep(25 * time.Millisecond)
	if total, _ := probeGoroutines(); total != 0 {
		t.Fatalf("%d probe goroutines before any start", total)
	}
	before := runtime.NumGoroutine()

	// One call per identity, as provide() does for every proxy.
	const identities = 64
	for i := 0; i < identities; i += 1 {
		startSmartDialerProbes(ctx, strategy, "http://127.0.0.1:0/probe",
			smartDialerProbeOwner(fmt.Sprintf("10.0.0.%d:1080", i)), i, "10.0.0.1:1080", true)
	}

	// A started goroutine reaches the loop on its first turn; poll so a late
	// schedule cannot hide it, and fail the moment one appears.
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if total, _ := probeGoroutines(); total != 0 {
			t.Fatalf("smart dialer off, but %d probe goroutines were started for %d identities", total, identities)
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A loose bound: the stack scan above is the real assertion, but a run that
	// removed the gate would also show roughly one goroutine per identity here.
	if after := runtime.NumGoroutine(); before+8 <= after {
		t.Fatalf("goroutine count grew from %d to %d across %d starts with the smart dialer off", before, after, identities)
	}
}

// With the preference on the behaviour is exactly what it was: one loop per
// identity, parked in the loop until its delay elapses, and gone once the
// identity's context is canceled.
func TestSmartDialerProbesStartLoopWhenOnAndStopCleanly(t *testing.T) {
	previous := connect.SetSmartDialer(true)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())

	if total, _ := probeGoroutines(); total != 0 {
		t.Fatalf("%d probe goroutines before any start", total)
	}

	const identities = 3
	for i := 0; i < identities; i += 1 {
		startSmartDialerProbes(ctx, strategy, "http://127.0.0.1:0/probe",
			smartDialerProbeOwner(fmt.Sprintf("10.0.0.%d:1080", i)), i, "10.0.0.1:1080", true)
	}

	// One goroutine per identity, and it must be inside the loop waiting out
	// the initial delay -- not merely created and forgotten.
	deadline := time.Now().Add(2 * time.Second)
	for {
		total, inLoop := probeGoroutines()
		if total == identities && inLoop == identities {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%d probe goroutines (%d inside the loop), want %d of each", total, inLoop, identities)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for {
		if total, _ := probeGoroutines(); total == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			total, inLoop := probeGoroutines()
			t.Fatalf("%d probe goroutines (%d inside the loop) still running after the context was canceled", total, inLoop)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The owner is the proxy identity, not a display string: two proxies behind one
// gateway differ, the direct identity is "direct", and the raw key (address plus
// session user) never appears in it.
func TestSmartDialerProbeOwnerIdentifiesTheProxyNotTheSlot(t *testing.T) {
	a := smartDialerProbeOwner("1.2.3.4:1080\u001fsession-a")
	b := smartDialerProbeOwner("1.2.3.4:1080\u001fsession-b")
	if a == b {
		t.Fatalf("two proxies share the owner %q, so one would be refused the shared gateway the other owns", a)
	}
	if a != smartDialerProbeOwner("1.2.3.4:1080\u001fsession-a") {
		t.Fatalf("the owner of one proxy is not stable across calls: %q", a)
	}
	if got := smartDialerProbeOwner("direct"); got != "direct" {
		t.Fatalf("direct identity owner = %q, want \"direct\"", got)
	}
	if got := smartDialerProbeOwner(""); got != "direct" {
		t.Fatalf("empty identity owner = %q, want \"direct\"", got)
	}
	if strings.Contains(a, "session-a") {
		t.Fatalf("owner %q carries the proxy's session user; the key is log-adjacent and must be hashed", a)
	}
}

// applyPersistedSmartDialer is the startup replay and the only writer of the
// runtime flag from control state, so a node that had the key persisted on
// keeps probing after a restart, and an unset or off key starts no loop.
func TestApplyPersistedSmartDialerAppliesBothDirections(t *testing.T) {
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	on := newControlState()
	if err := on.set("smart_dialer", "on"); err != nil {
		t.Fatal(err)
	}
	applyPersistedSmartDialer(on)
	if !connect.SmartDialerEnabled() {
		t.Fatal("a persisted smart_dialer=on was not applied at startup; the probe loop would never start")
	}

	// An unset key must put the flag at off, not leave the previous replay's
	// value in place.
	applyPersistedSmartDialer(newControlState())
	if connect.SmartDialerEnabled() {
		t.Fatal("an unset smart_dialer left the dialer enabled; the next start would probe for a preference nobody set")
	}

	off := newControlState()
	if err := off.set("smart_dialer", "off"); err != nil {
		t.Fatal(err)
	}
	connect.SetSmartDialer(true)
	applyPersistedSmartDialer(off)
	if connect.SmartDialerEnabled() {
		t.Fatal("a persisted smart_dialer=off was not applied at startup")
	}
}

// The startup tuning replay must reach the dialer through
// applyPersistedSmartDialer now that smart_dialer is not a live key, which is
// the path a restarting node takes.
func TestPersistedSmartDialerAppliesThroughTheStartupReplay(t *testing.T) {
	withTempHome(t)
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	state := newControlState()
	if err := state.set("smart_dialer", "on"); err != nil {
		t.Fatal(err)
	}
	applyPersistedRuntimeTuning(state)
	if !connect.SmartDialerEnabled() {
		t.Fatal("applyPersistedRuntimeTuning did not replay smart_dialer=on; a restarting node would stop probing")
	}
}

// The loop waits out the initial delay, then probes on every tick until the
// context ends.
func TestSmartDialerProbeLoopProbesAfterDelayThenRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSmartDialerProbes(ctx, 20*time.Millisecond, 10*time.Millisecond, func(context.Context) int {
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

// The probe starts only after the proxy authenticated, never before: a proxy
// that never gets through auth, or sits in slow retry, must not probe.
func TestSmartDialerProbeStartsAfterAuthSucceeds(t *testing.T) {
	source, err := os.ReadFile("provide.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	create := strings.Index(text, "connect.NewClientStrategy(proxyCtx, clientStrategySettings)")
	authFailed := strings.Index(text, "provideHandleAuthFailure(st, proxyCtx, proxySettings, isNative, isURLSourced, err)")
	start := strings.Index(text, "startSmartDialerProbes(proxyCtx, clientStrategy, st.apiUrl")
	if create < 0 || authFailed < 0 {
		t.Fatal("provideWithProxy no longer creates its strategy or handles auth failure this way; update this test")
	}
	if start < 0 {
		t.Fatal("provideWithProxy does not start the smart dialer probe")
	}
	if start < authFailed {
		t.Fatal("the smart dialer probe starts before the auth outcome is known; it must start only after auth succeeds")
	}
	if strings.Count(text, "startSmartDialerProbes(") != 1 {
		t.Fatal("expected exactly one probe start per proxy")
	}
}
