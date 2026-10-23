package miner

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// These tests cover the interim outage detector that stands in for v33's
// connect.OutageRecent: the freeze must trip only on a pool-wide run of
// failures, clear on the first success, and defer the per-proxy give-up
// verdict rather than erase it.

func TestProxyOutageFreezeRequiresTheFloorAndTheWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := newProxyOutageFreeze()
	f.now = func() time.Time { return now }
	f.lastSuccess = now

	// One failure, however old, is a bad proxy, not an outage.
	f.failure()
	now = now.Add(10 * time.Minute)
	if f.frozen() {
		t.Fatal("froze on a single failure; one bad proxy must not freeze the reaper")
	}

	for i := 1; i < proxyOutageFreezeDefaultMinFailures; i++ {
		f.failure()
	}
	if !f.frozen() {
		t.Fatal("did not freeze after the failure floor and a full window with no success")
	}
}

func TestProxyOutageFreezeClearsOnTheFirstSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := newProxyOutageFreeze()
	f.now = func() time.Time { return now }
	f.lastSuccess = now

	for i := 0; i < proxyOutageFreezeDefaultMinFailures; i++ {
		f.failure()
	}
	now = now.Add(10 * time.Minute)
	if !f.frozen() {
		t.Fatal("precondition: auth signal did not freeze")
	}

	f.success()
	if f.frozen() {
		t.Fatal("stayed frozen after a success; the platform is reachable again")
	}
}

// swapOutageTracker installs a frozen-or-not tracker for the duration of a test.
func swapOutageTracker(t *testing.T, frozen bool) {
	t.Helper()
	prev := proxyOutageTracker
	t.Cleanup(func() { proxyOutageTracker = prev })

	tr := newProxyOutageFreeze()
	if frozen {
		now := time.Unix(1_700_000_000, 0)
		tr.now = func() time.Time { return now }
		tr.lastSuccess = now
		for i := 0; i < proxyOutageFreezeDefaultMinFailures; i++ {
			tr.failure()
		}
		now = now.Add(10 * time.Minute)
		if !tr.frozen() {
			t.Fatal("precondition: auth signal did not freeze")
		}
	}
	proxyOutageTracker = tr
}

// During an outage the give-up ladder must not back off or evict a proxy for a
// failure that is not the proxy's fault.
func TestAuthFailureDuringAnOutageTakesTheSlowRetry(t *testing.T) {
	swapOutageTracker(t, true)

	settings := &connect.ProxySettings{Address: "203.0.113.9:1080"}
	key := settings.Key()
	globalProxyFailureHistory.Reset(key)
	t.Cleanup(func() { globalProxyFailureHistory.Reset(key) })

	var info providerAuthGiveUpInfo
	for i := 0; i < unprovenMaxAuthFailures; i++ {
		info = noteProviderAuthFailure(settings, errors.New("auth failed: backend unavailable"))
	}

	if info.evicted {
		t.Fatal("a proxy was evicted from the URL cache during a backend outage")
	}
	if info.giveUp != 0 {
		t.Fatalf("climbed the give-up ladder during an outage: %+v", info)
	}
	if info.retryIn <= 0 {
		t.Fatal("no slow retry was scheduled; the slot would retry immediately")
	}

	// The verdict is deferred, not erased: the attempt is still on the record,
	// so the proxy is judged on its next failure once the outage clears.
	if got := globalProxyFailureHistory.RecordFailure(key); got < unprovenMaxAuthFailures {
		t.Fatalf("failure history lost the attempt: got %d, want at least %d", got, unprovenMaxAuthFailures)
	}
}

// Outside an outage the ladder behaves exactly as before.
func TestAuthFailureOutsideAnOutageClimbsTheGiveUpLadder(t *testing.T) {
	swapOutageTracker(t, false)

	settings := &connect.ProxySettings{Address: "203.0.113.10:1080"}
	key := settings.Key()
	globalProxyFailureHistory.Reset(key)
	t.Cleanup(func() { globalProxyFailureHistory.Reset(key) })

	var info providerAuthGiveUpInfo
	for i := 0; i < unprovenMaxAuthFailures; i++ {
		info = noteProviderAuthFailure(settings, errors.New("auth failed: bad credentials"))
	}
	if info.giveUp == 0 {
		t.Fatal("a bad proxy did not climb the give-up ladder outside an outage")
	}
}

// The thresholds are defaults, not constants: a box can tune them without a
// rebuild, and a bad value falls back instead of disabling the freeze.
func TestProxyOutageFreezeThresholdsAreConfigurable(t *testing.T) {
	t.Setenv(proxyOutageFreezeWindowEnv, "90s")
	t.Setenv(proxyOutageFreezeMinFailuresEnv, "2")
	if got := proxyOutageFreezeWindow(); got != 90*time.Second {
		t.Fatalf("window = %v, want 90s", got)
	}
	if got := proxyOutageFreezeMinFailures(); got != 2 {
		t.Fatalf("floor = %d, want 2", got)
	}

	t.Setenv(proxyOutageFreezeWindowEnv, "not-a-duration")
	t.Setenv(proxyOutageFreezeMinFailuresEnv, "-4")
	if got := proxyOutageFreezeWindow(); got != proxyOutageFreezeDefaultWindow {
		t.Fatalf("unparseable window = %v, want the default %v", got, proxyOutageFreezeDefaultWindow)
	}
	if got := proxyOutageFreezeMinFailures(); got != proxyOutageFreezeDefaultMinFailures {
		t.Fatalf("invalid floor = %d, want the default %d", got, proxyOutageFreezeDefaultMinFailures)
	}
}
