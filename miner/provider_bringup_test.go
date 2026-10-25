package miner

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// providerBringUpLogBuffer captures the bring-up lines for one test.
type providerBringUpLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (self *providerBringUpLogBuffer) Write(raw []byte) (int, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.b.Write(raw)
}

func (self *providerBringUpLogBuffer) String() string {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.b.String()
}

func captureProviderBringUpLog(t *testing.T) *providerBringUpLogBuffer {
	t.Helper()
	old := proxyManagerLogWriter
	buffer := &providerBringUpLogBuffer{}
	proxyManagerLogWriter = buffer
	t.Cleanup(func() { proxyManagerLogWriter = old })
	return buffer
}

func TestProviderBringUpNarratesAuthOutcomes(t *testing.T) {
	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(3)
	tracker.start()
	defer tracker.shutdown()

	directID := connect.NewId()
	proxyID := connect.NewId()
	tracker.authReused(0, directID, "direct")
	tracker.authNew(1, proxyID, "proxy:10.0.0.1:1080")
	tracker.authFailed(2, "proxy:10.0.0.2:1080", errors.New("synthetic refused"), "in 15m")
	// A worker that fails after its auth line must not repeat the reason.
	tracker.workerFailed(2)

	text := buffer.String()
	for _, want := range []string{
		"providers: 0 running, 3 pending, 0 failed",
		"♻️ client_id: " + directID.String() + " (reused) provider=0 slot=direct",
		"✨ client_id: " + proxyID.String() + " (new) provider=1 slot=proxy:10.0.0.1:1080",
		"[auth] provider 2 failed: synthetic refused (slot proxy:10.0.0.2:1080, retry in 15m)",
		"providers: 2 running, 0 pending, 1 failed (client ids: 1 reused, 1 new; control plane reached: 0)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bring-up lines miss %q:\n%s", want, text)
		}
	}
	if count := strings.Count(text, "[auth] provider 2 failed"); count != 1 {
		t.Fatalf("the failed outcome was narrated %d times:\n%s", count, text)
	}
}

func TestProviderBringUpHeartbeatCadenceAndFinalTally(t *testing.T) {
	oldInterval := providerBringUpHeartbeatInterval
	providerBringUpHeartbeatInterval = 20 * time.Millisecond
	defer func() { providerBringUpHeartbeatInterval = oldInterval }()

	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(2)
	tracker.start()
	defer tracker.stop()

	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(buffer.String(), "providers:") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count := strings.Count(buffer.String(), "providers:"); count < 2 {
		t.Fatalf("heartbeat did not repeat: %d lines\n%s", count, buffer.String())
	}

	tracker.authReused(0, connect.NewId(), "direct")
	tracker.authFailed(1, "proxy:10.0.0.9:1080", errors.New("synthetic refused"), "on the next start")
	text := buffer.String()
	if !strings.Contains(text, "providers: 1 running, 0 pending, 1 failed") {
		t.Fatalf("final tally missing:\n%s", text)
	}
	// The heartbeat stops once nothing is pending: no further lines after the
	// final tally.
	resolved := strings.Count(text, "providers:")
	time.Sleep(80 * time.Millisecond)
	if count := strings.Count(buffer.String(), "providers:"); count != resolved {
		t.Fatalf("heartbeat continued after resolution: %d -> %d\n%s", resolved, count, buffer.String())
	}
}

func TestProviderBringUpStartStopIdempotent(t *testing.T) {
	captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(0)
	tracker.start()
	tracker.start()
	tracker.stop()
	tracker.stop()
	tracker.workerFailed(0)
	tracker.authReused(0, connect.NewId(), "direct")
	tracker.stop()
}

func TestProviderBringUpControlReachedUpdatesFinalTally(t *testing.T) {
	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(1)
	tracker.start()
	defer tracker.shutdown()

	tracker.authReused(0, connect.NewId(), "direct")
	initial := providerBringUpLine(1, 0, 0, 1, 0, 0)
	if !strings.Contains(buffer.String(), initial) {
		t.Fatalf("final tally missing %q:\n%s", initial, buffer.String())
	}

	tracker.controlReached(0, "direct", 10*time.Millisecond, "direct")
	want := providerBringUpLine(1, 0, 0, 1, 0, 1)
	if !strings.Contains(buffer.String(), want) {
		t.Fatalf("bring-up lines miss %q:\n%s", want, buffer.String())
	}

	tallies := strings.Count(buffer.String(), "providers:")
	tracker.controlReached(0, "direct", 10*time.Millisecond, "direct")
	if count := strings.Count(buffer.String(), "providers:"); count != tallies {
		t.Fatalf("second controlReached added another tally line: %d -> %d\n%s", tallies, count, buffer.String())
	}
}

func TestProviderAuthRetryToken(t *testing.T) {
	cases := []struct {
		name string
		info providerAuthGiveUpInfo
		want string
	}{
		{"below the ceiling", providerAuthGiveUpInfo{}, "on the next start"},
		{"backoff recorded", providerAuthGiveUpInfo{giveUp: 1, retryIn: 15 * time.Minute}, "in 15m"},
		{"evicted", providerAuthGiveUpInfo{giveUp: 4, evicted: true}, "never (evicted from the URL cache)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := providerAuthRetryToken(testCase.info); got != testCase.want {
				t.Fatalf("retry token = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestControlEvidenceIsCurrentNotEver pins the figure every "up" surface
// prints: proxies a live slot holds control-plane evidence for right now. It
// rises on a slot's first dial, falls when that slot exits, and counts a proxy
// once however many slot indexes hold it (a reload hands the same proxy a new
// index while the old slot drains). The monotonic count survives only as
// reached_ever.
func TestControlEvidenceIsCurrentNotEver(t *testing.T) {
	resetControlReached()
	t.Cleanup(resetControlReached)

	const proxy = "198.51.100.1:1080"
	if got := honestProxiesUp(); got != 0 {
		t.Fatalf("fresh process reports up=%d, want 0", got)
	}

	// The direct slot has no proxy identity: it is not a proxy and must not
	// pad a proxies figure.
	direct := newControlEvidence("")
	direct.reached()
	if got := honestProxiesUp(); got != 0 {
		t.Fatalf("direct slot counted as a proxy: up=%d, want 0", got)
	}

	// Slot index 3 reaches, twice (the dial callback is once per launch, but
	// the hold must not depend on that).
	first := newControlEvidence(proxy)
	first.reached()
	first.reached()
	if got := honestProxiesUp(); got != 1 {
		t.Fatalf("after one slot reached twice, up=%d, want 1", got)
	}

	// A reload launches index 7 for the same proxy while index 3 drains.
	reload := newControlEvidence(proxy)
	reload.reached()
	if got := honestProxiesUp(); got != 1 {
		t.Fatalf("two slot indexes for one proxy: up=%d, want 1 (counting per index would say 2)", got)
	}

	// The old slot exits: the new one still holds evidence, so the proxy is
	// still up.
	first.lost()
	if got := honestProxiesUp(); got != 1 {
		t.Fatalf("after the drained slot exited, up=%d, want 1 (the reloaded slot still holds it)", got)
	}

	// The reloaded slot exits too: nothing vouches for the proxy any more.
	reload.lost()
	if got := honestProxiesUp(); got != 0 {
		t.Fatalf("after every slot for the proxy exited, up=%d, want 0 (a monotonic count would say 1)", got)
	}

	// lost is idempotent and never drives the count negative, and a dial
	// callback that lands after the slot exited does not resurrect it.
	reload.lost()
	first.lost()
	reload.reached()
	if got := honestProxiesUp(); got != 0 {
		t.Fatalf("after a repeated lost and a late reached, up=%d, want 0", got)
	}
	if got := proxiesReachedEver(); got != 1 {
		t.Fatalf("reached_ever=%d, want 1 (one proxy reached at some point)", got)
	}
}

// TestControlEvidenceUpIsNotATransportDrop pins the other half of the rule: a
// slot can stay alive while proxy_transport_health.go watches its transport
// drop, and a proxy the live registry has seen go down is not up however
// recently its slot dialed.
func TestControlEvidenceUpIsNotATransportDrop(t *testing.T) {
	resetControlReached()
	resetProxyLiveHealthRegistryForTest()
	t.Cleanup(func() {
		resetControlReached()
		resetProxyLiveHealthRegistryForTest()
	})

	const proxy = "198.51.100.2:1080"
	hold := newControlEvidence(proxy)
	defer hold.lost()
	hold.reached()
	noteLiveProxyHealth(proxy, "up", time.Time{})
	if got := honestProxiesUp(); got != 1 {
		t.Fatalf("evidenced proxy graded up: up=%d, want 1", got)
	}
	noteLiveProxyHealthDrop(proxy)
	if got := honestProxiesUp(); got != 0 {
		t.Fatalf("after a watched transport drop, up=%d, want 0", got)
	}
}

// TestControlEvidenceConcurrentSlots runs many slot holds over a few proxies
// at once. Under -race this is the data-race check; in any mode the count must
// stay within [0, live holds] throughout and land on zero once every slot is
// gone.
func TestControlEvidenceConcurrentSlots(t *testing.T) {
	resetControlReached()
	t.Cleanup(resetControlReached)

	const slots = 64
	var wg sync.WaitGroup
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hold := newControlEvidence(fmt.Sprintf("198.51.100.%d:1080", i%4))
			hold.reached()
			if got := honestProxiesUp(); got < 0 || got > 4 {
				t.Errorf("up=%d during churn, want within [0, 4]", got)
			}
			hold.lost()
			hold.reached()
			hold.lost()
		}(i)
	}
	wg.Wait()
	if got := honestProxiesUp(); got != 0 {
		t.Fatalf("after every slot exited, up=%d, want 0", got)
	}
	if got := proxiesReachedEver(); got != 4 {
		t.Fatalf("reached_ever=%d, want 4", got)
	}
}

// TestAuthLogoutWithdrawsControlEvidence pins the auth-loss hook: a rejected
// credential withdraws the slot's evidence at once, without waiting for the
// cancel to unwind the slot.
func TestAuthLogoutWithdrawsControlEvidence(t *testing.T) {
	resetControlReached()
	t.Cleanup(resetControlReached)

	hold := newControlEvidence("198.51.100.3:1080")
	hold.reached()
	if got := honestProxiesUp(); got != 1 {
		t.Fatalf("before logout, up=%d, want 1", got)
	}
	dir := t.TempDir()
	cancelled := false
	callbacks := &providerAuthenticationCallbacks{
		clientJwtPath:  filepath.Join(dir, "client.jwt"),
		networkJwtPath: filepath.Join(dir, "network.jwt"),
		cancel:         func() { cancelled = true },
		rejected:       hold.lost,
	}
	callbacks.AuthLogout()
	if !cancelled {
		t.Fatal("AuthLogout did not cancel")
	}
	if got := honestProxiesUp(); got != 0 {
		t.Fatalf("after the platform rejected the credential, up=%d, want 0", got)
	}
}
