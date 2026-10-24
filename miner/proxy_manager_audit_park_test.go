package miner

// Tests for proxy audit's act half (proxy_manager_audit_park.go): a park must
// really stop the reloader's slot and record itself, a second park of the same
// proxy must not cancel twice, a release must let the identity return, the
// client count must come from the live bandwidth counters, and the direct slot
// must never be parked.
//
// The reloader is the real one (proxy_reloader.go) with a fake spawn, so the
// tests exercise the same cancel path the provide run uses; only the slot's
// body is fake.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// auditParkPool is a reloader whose slots are recorded instead of served.
type auditParkPool struct {
	reloader *minerProxyReloader

	mu sync.Mutex
	// wanted is the desired set the reloader re-applies on a reload.
	wanted    []*connect.ProxySettings
	cancelled []string
	spawned   []string
}

func newAuditParkPool(t *testing.T, addresses ...string) *auditParkPool {
	t.Helper()
	pool := &auditParkPool{}
	pool.reloader = newMinerProxyReloader(
		func() ([]*connect.ProxySettings, error) {
			pool.mu.Lock()
			defer pool.mu.Unlock()
			return append([]*connect.ProxySettings(nil), pool.wanted...), nil
		},
		func(settings *connect.ProxySettings) (context.CancelFunc, error) {
			key := settings.Key()
			pool.mu.Lock()
			pool.spawned = append(pool.spawned, key)
			pool.mu.Unlock()
			return func() {
				pool.mu.Lock()
				pool.cancelled = append(pool.cancelled, key)
				pool.mu.Unlock()
			}, nil
		},
	)
	for _, address := range addresses {
		settings := reloadTestSettings(address, "", "")
		pool.wanted = append(pool.wanted, settings)
		pool.reloader.seed(settings, func() {
			pool.mu.Lock()
			pool.cancelled = append(pool.cancelled, settings.Key())
			pool.mu.Unlock()
		})
	}
	return pool
}

func (p *auditParkPool) cancelledKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cancelled...)
}

func (p *auditParkPool) spawnedKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.spawned...)
}

// publish installs the pool's reloader as the live one for the duration of a
// test, which is what the park seam resolves.
func (p *auditParkPool) publish(t *testing.T) {
	t.Helper()
	currentMinerProxyReloader.Store(p.reloader)
	t.Cleanup(func() { currentMinerProxyReloader.Store(nil) })
}

// newParkHarness builds the tick harness with the LIVE park seam wired to pool.
func newParkHarness(t *testing.T, pool *auditParkPool, addresses ...string) *auditHarness {
	t.Helper()
	resetReapTestGlobals(t)
	pool.publish(t)
	h := newAuditHarness(addresses...)
	h.act = true
	h.g.env.park = liveProxyAuditPark
	h.g.env.unpark = liveProxyAuditUnpark
	return h
}

// A park must cancel the slot the reloader owns, and record the park, its
// backoff and the hold that keeps the reloader from re-adding the identity.
func TestProxyAuditParkCancelsTheSlotAndRecordsIt(t *testing.T) {
	pool := newAuditParkPool(t, "a:1", "b:1")
	h := newParkHarness(t, pool, "a:1", "b:1")

	h.twoBadTicks("a:1")

	if got := pool.cancelledKeys(); !slices.Equal(got, []string{"a:1"}) {
		t.Fatalf("cancelled = %v, want [a:1]", got)
	}
	if got := pool.reloader.runningKeys(); !slices.Equal(got, []string{"b:1"}) {
		t.Fatalf("running = %v, want [b:1]", got)
	}
	if !h.g.st.isParked("a:1") {
		t.Fatal("the park must be recorded in the audit state")
	}
	// First ladder rung, no jitter at rand 0.5: six hours.
	if want := h.now.Add(6 * time.Hour); !h.g.st.parks["a:1"].until.Equal(want) {
		t.Fatalf("park until = %s, want %s", h.g.st.parks["a:1"].until, want)
	}
	// The backoff the reload path enforces, and the hold the desired-set build
	// filters on: both must cover the park, or the next reload puts the proxy
	// straight back.
	if h.hist.Eligible("a:1", h.now.Add(5*time.Hour)) {
		t.Fatal("the parked proxy is eligible inside its backoff")
	}
	if !h.hist.Eligible("a:1", h.now.Add(7*time.Hour)) {
		t.Fatal("the parked proxy is still backed off after its window")
	}
	if !reapedProxyHoldActive("a:1", h.now.Add(5*time.Hour)) {
		t.Fatal("the parked proxy is not held out of the desired set")
	}
	if reapedProxyHoldActive("a:1", h.now.Add(7*time.Hour)) {
		t.Fatal("the hold outlived the park window")
	}
	if !slices.Equal(h.parkedMarks, []string{"a:1"}) {
		t.Fatalf("parked marks = %v, want [a:1]", h.parkedMarks)
	}
	if h.logged("parked a:1") != 1 {
		t.Fatalf("expected one parked log line, got %v", h.logs)
	}
}

// Parking the same proxy twice must cancel its slot once: after the first park
// the reloader no longer owns it, so the second park has nothing to stop and
// must not record a second park or a second mark.
func TestProxyAuditParkTwiceDoesNotDoubleCancel(t *testing.T) {
	pool := newAuditParkPool(t, "a:1")
	h := newParkHarness(t, pool, "a:1")
	h.twoBadTicks("a:1")

	marks := len(h.parkedMarks)
	res := proxyAuditResult{Park: []proxyAuditPark{{Addr: "a:1"}}}
	h.g.park(h.now.Add(5*time.Minute), res, nil)

	if got := pool.cancelledKeys(); !slices.Equal(got, []string{"a:1"}) {
		t.Fatalf("cancelled = %v, want exactly one cancel of a:1", got)
	}
	if len(h.parkedMarks) != marks {
		t.Fatalf("a second park re-marked the proxy: %v", h.parkedMarks)
	}
	if got := h.logged("parked a:1"); got != 1 {
		t.Fatalf("a second park logged another park line: %v", h.logs)
	}
}

// A release must drop the hold and re-apply the desired set, so the identity
// comes back on the same tick instead of waiting for an unrelated trigger.
func TestProxyAuditUnparkLetsTheSlotReturn(t *testing.T) {
	pool := newAuditParkPool(t, "a:1")
	h := newParkHarness(t, pool, "a:1")

	h.twoBadTicks("a:1")
	if got := pool.reloader.runningKeys(); len(got) != 0 {
		t.Fatalf("running after park = %v, want none", got)
	}

	h.g.unpark([]string{"a:1"})

	if reapedProxyHoldActive("a:1", h.now.Add(time.Minute)) {
		t.Fatal("the hold survived the release")
	}
	if got := pool.spawnedKeys(); !slices.Equal(got, []string{"a:1"}) {
		t.Fatalf("spawned = %v, want [a:1] re-spawned by the release reload", got)
	}
	if got := pool.reloader.runningKeys(); !slices.Equal(got, []string{"a:1"}) {
		t.Fatalf("running after release = %v, want [a:1]", got)
	}
}

// Releasing an identity that was never held must not re-apply the desired set:
// that would churn the pool for nothing.
func TestProxyAuditUnparkWithoutAHoldDoesNotReload(t *testing.T) {
	pool := newAuditParkPool(t, "a:1")
	newParkHarness(t, pool, "a:1")

	liveProxyAuditUnpark([]string{"a:1"})

	if got := pool.spawnedKeys(); len(got) != 0 {
		t.Fatalf("released an unheld identity and reloaded anyway: %v", got)
	}
}

// The park seam re-checks eligibility under the reloader's own lock, at the
// moment of the cancel: a proxy that picked up clients (or went down) between
// the tick's snapshot and the act must not be cancelled for the state it was in.
// This is cancelSlots' half of the provider's reapProxies rule.
func TestProxyAuditParkRechecksEligibilityUnderTheReloaderLock(t *testing.T) {
	pool := newAuditParkPool(t, "a:1")
	pool.publish(t)

	sawLockHeld := false
	cancelled := pool.reloader.cancelSlots([]string{"a:1"}, func(string) bool {
		// TryLock fails iff the reloader's lock is held. A successful TryLock
		// takes the lock, so it must be given back before returning.
		if pool.reloader.mu.TryLock() {
			pool.reloader.mu.Unlock()
			return false
		}
		sawLockHeld = true
		return false
	}, nil)

	if cancelled != nil {
		t.Fatalf("cancelled = %v, want none: the re-check vetoed it", cancelled)
	}
	if !sawLockHeld {
		t.Fatal("the eligibility re-check must run with the reloader lock held")
	}
	if got := pool.cancelledKeys(); len(got) != 0 {
		t.Fatalf("a vetoed park cancelled %v", got)
	}
	if got := pool.reloader.runningKeys(); !slices.Equal(got, []string{"a:1"}) {
		t.Fatalf("running = %v, want [a:1] still served", got)
	}
}

// A park that names a proxy this run is not serving cancels nothing and records
// nothing: the audit may only claim what it actually stopped.
func TestProxyAuditParkSkipsAnIdentityTheReloaderDoesNotOwn(t *testing.T) {
	pool := newAuditParkPool(t, "a:1")
	h := newParkHarness(t, pool, "a:1", "b:1")

	done := make(chan string, 1)
	cancelled := pool.reloader.cancelSlots([]string{"b:1"}, func(string) bool { return true }, func(key string) {
		done <- key
	})

	if cancelled != nil {
		t.Fatalf("cancelled = %v, want none for a slot the reloader does not own", cancelled)
	}
	select {
	case key := <-done:
		t.Fatalf("recorded a park for %s that was never cancelled", key)
	default:
	}
	if got := pool.cancelledKeys(); len(got) != 0 {
		t.Fatalf("cancelled = %v, want none", got)
	}
	if h.g.st.isParked("b:1") {
		t.Fatal("a park that cancelled nothing was recorded")
	}
}

// The client count is the live bandwidth registry's per-identity counter — the
// audit's only idle signal. "Known and zero" (a running slot with no sessions)
// is idle; "no entry" is unknown and vetoes. The count comes from recorded
// sessions, not from bytes moved.
func TestProxyAuditClientsCountsRecordedTraffic(t *testing.T) {
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	if n, known := proxyAuditClients("a:1"); known || n != 0 {
		t.Fatalf("an unpublished identity reported %d/%v, want unknown", n, known)
	}

	key := identityKey("a:1", "u")
	bw := &bandwidth.ProxyBandwidth{}
	bw.TotalRx.Store(4096)
	bw.TotalTx.Store(2048)
	bw.Clients.Store(2)
	publishProxyEarnBandwidth(key, bw)

	if n, known := proxyAuditClients(key); !known || n != 2 {
		t.Fatalf("clients = %d/%v, want 2/known", n, known)
	}
	// Bytes without sessions are not clients: an identity whose slot has moved
	// traffic but holds no session is idle and therefore park-eligible.
	bw.Clients.Store(0)
	if n, known := proxyAuditClients(key); !known || n != 0 {
		t.Fatalf("clients = %d/%v, want 0/known", n, known)
	}
	// The bare address is a different identity: credentials are part of it, so
	// a credentialed proxy must not read address-keyed counters.
	if n, known := proxyAuditClients("a:1"); known || n != 0 {
		t.Fatalf("the bare address reported %d/%v, want unknown", n, known)
	}
	// The direct slot is not a proxy and publishes nothing.
	if n, known := proxyAuditClients(directProxyKey); known || n != 0 {
		t.Fatalf("the direct slot reported %d/%v, want unknown", n, known)
	}

	// Through the audit's own veto path: a live session vetoes the park, and no
	// counters at all also vetoes it.
	g := &proxyAuditor{env: proxyAuditEnv{
		health: func() map[string]proxyHealthStatus {
			return map[string]proxyHealthStatus{key: {Health: "up"}}
		},
		clients:        proxyAuditClients,
		earnedRecently: func(string) bool { return false },
	}}
	bw.Clients.Store(2)
	if g.stillEligible(key) {
		t.Fatal("an identity with live sessions must not be park-eligible")
	}
	bw.Clients.Store(0)
	if !g.stillEligible(key) {
		t.Fatal("an idle identity with known counters must be park-eligible")
	}
	unpublishProxyEarnBandwidth(key)
	if g.stillEligible(key) {
		t.Fatal("an identity with no live counters must not be park-eligible (unknown is not idle)")
	}
}

// The direct slot is not a proxy: it has no paid entry and no client counters,
// and a real run never hands it to the reloader. A corrupt paid set that names
// it must still leave it alone — so the reloader here is seeded with a "direct"
// slot on purpose, which makes the audit's own filter the thing under test
// rather than the reloader's running-set check.
func TestProxyAuditNeverParksTheDirectSlot(t *testing.T) {
	pool := newAuditParkPool(t, directProxyKey, "a:1")
	h := newParkHarness(t, pool, directProxyKey, "a:1")

	// Both look parkable to the engine: up, idle, and two bad grades.
	for _, addr := range []string{directProxyKey, "a:1"} {
		h.grade(addr, 0.1, h.now.Add(-7*time.Hour))
	}
	h.g.runOnce()
	h.now = h.now.Add(6 * time.Hour)
	for _, addr := range []string{directProxyKey, "a:1"} {
		h.grade(addr, 0.1, h.now.Add(-time.Hour))
	}
	h.g.runOnce()

	if got := pool.cancelledKeys(); !slices.Equal(got, []string{"a:1"}) {
		t.Fatalf("cancelled = %v, want only [a:1]", got)
	}
	if h.g.st.isParked(directProxyKey) {
		t.Fatal("the direct slot was recorded as parked")
	}
	if !slices.Equal(h.parkedMarks, []string{"a:1"}) {
		t.Fatalf("parked marks = %v, want [a:1]", h.parkedMarks)
	}
	if h.logged("parked direct") != 0 {
		t.Fatalf("the direct slot was logged as parked: %v", h.logs)
	}
}
