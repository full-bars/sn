package miner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Tests for the miner's performance-based degraded reaper: the port of the v33
// provider's provider/degraded_reaper.go onto the miner's identity-keyed health
// registry and live reloader.
//
// The provider's own semantics are asserted there (keep-count rounding, timeout
// exclusion, minimum down time, only-cancellable selection); these cover the
// miner's seams, in particular the one behaviour the provider does not need —
// a reaped identity must not be re-added by the next reload.

// reapTestPool is a reloader seeded with one running slot per identity, with
// every cancel and every published running set recorded.
type reapTestPool struct {
	reloader *minerProxyReloader

	mu        sync.Mutex
	cancelled []string
	notified  [][]string
}

func newReapTestPool(t *testing.T, addresses ...string) *reapTestPool {
	t.Helper()
	pool := &reapTestPool{}
	pool.reloader = newMinerProxyReloader(
		func() ([]*connect.ProxySettings, error) { return nil, nil },
		func(settings *connect.ProxySettings) (context.CancelFunc, error) {
			key := settings.Key()
			return func() {
				pool.mu.Lock()
				pool.cancelled = append(pool.cancelled, key)
				pool.mu.Unlock()
			}, nil
		},
	)
	pool.reloader.onChange = func(keys []string) {
		pool.mu.Lock()
		pool.notified = append(pool.notified, append([]string(nil), keys...))
		pool.mu.Unlock()
	}
	for _, address := range addresses {
		settings := reloadTestSettings(address, "", "")
		pool.reloader.seed(settings, func() {
			pool.mu.Lock()
			pool.cancelled = append(pool.cancelled, settings.Key())
			pool.mu.Unlock()
		})
	}
	return pool
}

func (p *reapTestPool) cancelledKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cancelled...)
}

func (p *reapTestPool) lastNotified() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.notified) == 0 {
		return nil
	}
	return append([]string(nil), p.notified[len(p.notified)-1]...)
}

// newTestReaper builds a reaper over the pool with every seam pinned: a fixed
// clock, no outage, a static degraded snapshot, and no lifetime contribution.
func newTestReaper(pool *reapTestPool, now time.Time, entries []minerDegradedProxyEntry) *minerDegradedReaper {
	r := newMinerDegradedReaper(pool.reloader)
	r.now = func() time.Time { return now }
	r.frozen = func() bool { return false }
	r.degraded = func(time.Time) []minerDegradedProxyEntry { return entries }
	r.contribution = func(string) int64 { return 0 }
	r.stillDegraded = func(string) bool { return true }
	return r
}

func resetReapTestGlobals(t *testing.T) {
	t.Helper()
	resetReapedProxyHoldsForTest()
	globalProxyFailureHistory = &proxyFailureHistory{failures: map[string]int{}}
	t.Cleanup(func() {
		resetReapedProxyHoldsForTest()
		globalProxyFailureHistory = &proxyFailureHistory{failures: map[string]int{}}
	})
}

// The provider's ceil rounding: the reaper keeps half of the DEGRADED set,
// rounded up, and never less than one.
func TestMinerDegradedReaperKeepCount(t *testing.T) {
	cases := []struct{ total, want int }{
		{0, 1},
		{1, 1},
		{2, 1},
		{3, 2},
		{4, 2},
		{5, 3},
		{9, 5},
		{10, 5},
		{100, 50},
	}
	for _, c := range cases {
		if got := degradedReaperKeepCount(c.total); got != c.want {
			t.Errorf("degradedReaperKeepCount(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}

// A proxy whose last failure was timeout-family was slow, not broken: the
// reaper must skip it, and it must skip a proxy that has not been down long
// enough. Both filters live in the selection so they cannot be forgotten by a
// caller that builds its own snapshot.
func TestMinerDegradedReaperSelectionExcludesTimeoutsAndFreshDowns(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	scored := []minerScoredDegradedProxy{
		// worst: down long enough, but cut short by the connect deadline
		{entry: minerDegradedProxyEntry{Key: "slow", DownFor: 2 * time.Hour, LastFailureTimeout: true}, score: 10},
		// next: plain failure, down long enough -> the only real candidate
		{entry: minerDegradedProxyEntry{Key: "bad", DownFor: 45 * time.Minute}, score: 20},
		// next: plain failure but down only 5m -> too fresh this pass
		{entry: minerDegradedProxyEntry{Key: "fresh", DownFor: 5 * time.Minute}, score: 30},
		{entry: minerDegradedProxyEntry{Key: "good", DownFor: 3 * time.Hour}, score: 900},
	}

	// keep 2 of 4 -> the two worst are in scope; only "bad" survives the filters.
	toReap := selectMinerProxiesToReap(scored, 2, degradedReaperMinDownTime)
	if len(toReap) != 1 || toReap[0].Key != "bad" {
		t.Fatalf("selectMinerProxiesToReap = %v, want exactly [bad]", toReap)
	}
	// Widening the scope to all four still only admits "bad": the timeout and
	// the fresh down are excluded on their own grounds, not by the cut.
	if toReap := selectMinerProxiesToReap(scored, 1, degradedReaperMinDownTime); len(toReap) != 1 || toReap[0].Key != "bad" {
		t.Fatalf("wider scope = %v, want exactly [bad]", toReap)
	}
	// With the timeout filter satisfied, the timeout entry is selected — the
	// exclusion above was the reason it was skipped.
	scored[0].entry.LastFailureTimeout = false
	if got := selectMinerProxiesToReap(scored, 1, degradedReaperMinDownTime); len(got) != 2 {
		t.Fatalf("selection with no timeout = %v, want both long-down entries", got)
	}

	// The provider's minimum down time on its own: nothing under
	// degradedReaperMinDownTime is ever selected, however it ranks.
	fresh := []minerScoredDegradedProxy{
		{entry: minerDegradedProxyEntry{Key: "fresh-a", DownFor: degradedReaperMinDownTime - time.Minute}, score: 1},
		{entry: minerDegradedProxyEntry{Key: "fresh-b", DownFor: degradedReaperMinDownTime - time.Second}, score: 2},
		{entry: minerDegradedProxyEntry{Key: "settled", DownFor: degradedReaperMinDownTime}, score: 3},
	}
	if got := selectMinerProxiesToReap(fresh, 1, degradedReaperMinDownTime); len(got) != 0 {
		t.Fatalf("a proxy down for less than the minimum was selected: %v", got)
	}
	// Exactly at the minimum counts as long enough.
	fresh[0].entry.DownFor = degradedReaperMinDownTime
	if got := selectMinerProxiesToReap(fresh, 2, degradedReaperMinDownTime); len(got) != 1 || got[0].Key != "fresh-a" {
		t.Fatalf("at the minimum down time selection = %v, want [fresh-a]", got)
	}
	_ = now
}

// The ranking is the provider's: ascending lifetime contribution, the worst
// first, keeping the best half.
func TestMinerDegradedReaperReapsWorstAndKeepsBest(t *testing.T) {
	resetReapTestGlobals(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	pool := newReapTestPool(t,
		"198.51.100.10:443", "198.51.100.11:443", "198.51.100.12:443", "198.51.100.13:443")
	entries := []minerDegradedProxyEntry{
		{Key: "198.51.100.10:443", Address: "198.51.100.10:443", DownFor: 40 * time.Minute, TotalRxBytes: 100, TotalTxBytes: 0},
		{Key: "198.51.100.11:443", Address: "198.51.100.11:443", DownFor: 40 * time.Minute, TotalRxBytes: 0, TotalTxBytes: 300},
		{Key: "198.51.100.12:443", Address: "198.51.100.12:443", DownFor: 40 * time.Minute, TotalRxBytes: 150, TotalTxBytes: 50},
		{Key: "198.51.100.13:443", Address: "198.51.100.13:443", DownFor: 40 * time.Minute, TotalRxBytes: 500, TotalTxBytes: 500},
	}
	reaper := newTestReaper(pool, base, entries)
	// A lifetime contribution is a tiebreak, ascending like the live bytes.
	reaper.contribution = func(key string) int64 {
		if key == "198.51.100.10:443" {
			return 1000 // lifts the worst live contributor above 198.51.100.12
		}
		return 0
	}

	result := reaper.cycle()
	if result.Reaped != 2 {
		t.Fatalf("reaped %d, want 2 (keep %d of %d): %+v", result.Reaped, result.Keep, result.Considered, result)
	}
	if result.Keep != 2 {
		t.Fatalf("keep = %d, want 2", result.Keep)
	}

	// Scores: .11=300, .12=200, .10=1100 (100 live + 1000 contribution), .13=1000.
	// Ascending: .12 (200), .11 (300), .13 (1000), .10 (1100) -> reap .12 and .11.
	wantReaped := []string{"198.51.100.11:443", "198.51.100.12:443"}
	gotReaped := pool.cancelledKeys()
	if len(gotReaped) != 2 || !containsIdentity(gotReaped, wantReaped[0]) || !containsIdentity(gotReaped, wantReaped[1]) {
		t.Fatalf("cancelled = %v, want %v", gotReaped, wantReaped)
	}
	running := pool.reloader.runningKeys()
	if len(running) != 2 || !containsIdentity(running, "198.51.100.10:443") || !containsIdentity(running, "198.51.100.13:443") {
		t.Fatalf("running = %v, want the two best contributors", running)
	}
	// The published set is what the OOM cap and the control surface read.
	if notified := pool.lastNotified(); len(notified) != 2 {
		t.Fatalf("notified running set = %v, want 2 identities", notified)
	}
}

// A degraded identity this run is not serving cannot be cancelled, so it must
// not be counted as cancellable, must not lower the keep count of the ones that
// can be, and must not be held out.
func TestMinerDegradedReaperOnlyCancellable(t *testing.T) {
	resetReapTestGlobals(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	// Only .11 and .12 are running; .10 and .13 are degraded in the registry but
	// not served by this run (stopped, or never started).
	pool := newReapTestPool(t, "198.51.100.11:443", "198.51.100.12:443")
	entries := []minerDegradedProxyEntry{
		{Key: "198.51.100.10:443", DownFor: time.Hour, TotalRxBytes: 1},
		{Key: "198.51.100.11:443", DownFor: time.Hour, TotalRxBytes: 10},
		{Key: "198.51.100.12:443", DownFor: time.Hour, TotalRxBytes: 20},
		{Key: "198.51.100.13:443", DownFor: time.Hour, TotalRxBytes: 30},
	}
	reaper := newTestReaper(pool, base, entries)

	result := reaper.cycle()
	if result.Considered != 4 || result.Cancellable != 2 {
		t.Fatalf("considered %d cancellable %d, want 4 and 2", result.Considered, result.Cancellable)
	}
	// keep = ceil(50% of 2) = 1, so exactly the worse running identity goes.
	if result.Reaped != 1 {
		t.Fatalf("reaped %d, want 1: %+v", result.Reaped, result)
	}
	if running := pool.reloader.runningKeys(); len(running) != 1 || running[0] != "198.51.100.12:443" {
		t.Fatalf("running = %v, want only 198.51.100.12:443", running)
	}
	if reapedProxyHoldActive("198.51.100.10:443", base.Add(time.Minute)) {
		t.Fatal("a degraded identity that is not running must not be held out")
	}
}

// A single cancellable candidate is never reaped: there is nothing to rank it
// against, and the pool would go to zero.
func TestMinerDegradedReaperSingleCandidate(t *testing.T) {
	resetReapTestGlobals(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	pool := newReapTestPool(t, "198.51.100.11:443")
	reaper := newTestReaper(pool, base, []minerDegradedProxyEntry{
		{Key: "198.51.100.11:443", DownFor: time.Hour, TotalRxBytes: 1},
		{Key: "198.51.100.12:443", DownFor: time.Hour, TotalRxBytes: 2},
	})
	result := reaper.cycle()
	if result.Reaped != 0 || len(pool.cancelledKeys()) != 0 {
		t.Fatalf("a single cancellable candidate was reaped: %+v", result)
	}
}

// Tied scores carry no ranking — the 2026-10-23 outage shape, where an
// unreachable upstream froze every proxy at the same contribution. Reaping on
// that would halve a healthy pool.
func TestMinerDegradedReaperNoRanking(t *testing.T) {
	resetReapTestGlobals(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	addresses := []string{"198.51.100.10:443", "198.51.100.11:443", "198.51.100.12:443", "198.51.100.13:443"}
	pool := newReapTestPool(t, addresses...)
	var entries []minerDegradedProxyEntry
	for _, address := range addresses {
		entries = append(entries, minerDegradedProxyEntry{Key: address, DownFor: time.Hour})
	}
	reaper := newTestReaper(pool, base, entries)

	result := reaper.cycle()
	if !result.NoRanking || result.Reaped != 0 {
		t.Fatalf("tied scores reaped: %+v", result)
	}
	if len(pool.cancelledKeys()) != 0 {
		t.Fatalf("cancelled = %v, want none", pool.cancelledKeys())
	}
}

// The miner's own outage detector holds the reaper: during a platform outage the
// traffic evidence is absent for reasons that are not the proxies'.
func TestMinerDegradedReaperFrozen(t *testing.T) {
	resetReapTestGlobals(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	pool := newReapTestPool(t, "198.51.100.10:443", "198.51.100.11:443", "198.51.100.12:443", "198.51.100.13:443")
	reaper := newTestReaper(pool, base, []minerDegradedProxyEntry{
		{Key: "198.51.100.10:443", DownFor: time.Hour, TotalRxBytes: 1},
		{Key: "198.51.100.11:443", DownFor: time.Hour, TotalRxBytes: 2},
		{Key: "198.51.100.12:443", DownFor: time.Hour, TotalRxBytes: 3},
		{Key: "198.51.100.13:443", DownFor: time.Hour, TotalRxBytes: 4},
	})
	reaper.frozen = func() bool { return true }

	result := reaper.cycle()
	if !result.Frozen || result.Reaped != 0 {
		t.Fatalf("frozen cycle = %+v, want no reap", result)
	}
	if len(pool.cancelledKeys()) != 0 {
		t.Fatalf("cancelled during a freeze: %v", pool.cancelledKeys())
	}
}

// The behaviour the provider does not need: the desired set still contains the
// identity the reaper cancelled, so without the hold the next reload would put
// it straight back. The hold must cover the whole minimum-down window and expire
// on its own.
func TestMinerDegradedReaperHoldKeepsReapedIdentityOutOfDesiredSet(t *testing.T) {
	resetReapTestGlobals(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	addresses := []string{"198.51.100.10:443", "198.51.100.11:443", "198.51.100.12:443", "198.51.100.13:443"}
	pool := newReapTestPool(t, addresses...)
	var entries []minerDegradedProxyEntry
	for i, address := range addresses {
		entries = append(entries, minerDegradedProxyEntry{
			Key:          address,
			DownFor:      time.Hour,
			TotalRxBytes: uint64(100 * (i + 1)),
		})
	}
	reaper := newTestReaper(pool, base, entries)
	result := reaper.cycle()
	if result.Reaped != 2 {
		t.Fatalf("reaped %d, want 2", result.Reaped)
	}

	// The desired set as the reloader would compute it: every configured
	// identity, including the two that were just reaped.
	desired := make([]*connect.ProxySettings, 0, len(addresses))
	for _, address := range addresses {
		desired = append(desired, reloadTestSettings(address, "", ""))
	}

	insideWindow := filterReapHeldProxySettings(desired, base.Add(time.Minute))
	if len(insideWindow) != 2 {
		t.Fatalf("desired inside the hold = %d identities, want 2", len(insideWindow))
	}
	for _, settings := range insideWindow {
		if settings.Key() == "198.51.100.10:443" || settings.Key() == "198.51.100.11:443" {
			t.Fatalf("a reaped identity %s survived the hold", settings.Key())
		}
	}
	// The backoff is recorded where the URL-sourced give-up gate reads it, so
	// the existing gate agrees with the hold.
	for _, key := range []string{"198.51.100.10:443", "198.51.100.11:443"} {
		if globalProxyFailureHistory.Eligible(key, base.Add(time.Minute)) {
			t.Fatalf("%s is eligible inside its backoff window", key)
		}
		if !globalProxyFailureHistory.Eligible(key, base.Add(degradedReaperMinDownTime)) {
			t.Fatalf("%s is still held after its window elapsed", key)
		}
	}

	// One second before the window closes it is still held; at the window it is
	// eligible again.
	if got := filterReapHeldProxySettings(desired, base.Add(degradedReaperMinDownTime-time.Second)); len(got) != 2 {
		t.Fatalf("desired just inside the window = %d identities, want 2", len(got))
	}
	if got := filterReapHeldProxySettings(desired, base.Add(degradedReaperMinDownTime)); len(got) != 4 {
		t.Fatalf("desired at the window = %d identities, want all 4", len(got))
	}

	// The reaper releases expiries itself and reloads, so a held identity comes
	// back without waiting for an unrelated trigger.
	resetReapedProxyHoldsForTest()
	expiringPool := newReapTestPool(t, "198.51.100.20:443", "198.51.100.21:443", "198.51.100.22:443", "198.51.100.23:443")
	expiringReaper := newTestReaper(expiringPool, base, []minerDegradedProxyEntry{
		{Key: "198.51.100.20:443", DownFor: time.Hour, TotalRxBytes: 1},
		{Key: "198.51.100.21:443", DownFor: time.Hour, TotalRxBytes: 2},
		{Key: "198.51.100.22:443", DownFor: time.Hour, TotalRxBytes: 3},
		{Key: "198.51.100.23:443", DownFor: time.Hour, TotalRxBytes: 4},
	})
	if res := expiringReaper.cycle(); res.Reaped != 2 {
		t.Fatalf("first cycle reaped %d, want 2", res.Reaped)
	}
	expiringReaper.now = func() time.Time { return base.Add(degradedReaperMinDownTime) }
	expiringReaper.degraded = func(time.Time) []minerDegradedProxyEntry { return nil }
	if res := expiringReaper.cycle(); res.Released != 2 {
		t.Fatalf("second cycle released %d holds, want 2", res.Released)
	}
}

// The reloader API the reaper cancels through: identity-keyed, idempotent for
// keys this run is not serving, and dead after stop().
func TestMinerProxyReloaderReap(t *testing.T) {
	pool := newReapTestPool(t, "198.51.100.10:443", "198.51.100.11:443")

	// An unknown identity is not the reaper's to cancel, and is not reported.
	if got := pool.reloader.reap([]string{"198.51.100.99:443"}); len(got) != 0 {
		t.Fatalf("reap of an unknown identity returned %v", got)
	}
	if running := pool.reloader.runningKeys(); len(running) != 2 {
		t.Fatalf("running = %v, want both slots untouched", running)
	}

	got := pool.reloader.reap([]string{"198.51.100.10:443", "198.51.100.99:443"})
	if len(got) != 1 || got[0] != "198.51.100.10:443" {
		t.Fatalf("reap returned %v, want [198.51.100.10:443]", got)
	}
	if running := pool.reloader.runningKeys(); len(running) != 1 || running[0] != "198.51.100.11:443" {
		t.Fatalf("running = %v, want only 198.51.100.11:443", running)
	}
	if cancelled := pool.cancelledKeys(); len(cancelled) != 1 {
		t.Fatalf("cancelled = %v, want exactly the reaped identity", cancelled)
	}
	if notified := pool.lastNotified(); len(notified) != 1 || notified[0] != "198.51.100.11:443" {
		t.Fatalf("notified = %v, want the remaining identity", notified)
	}

	// Reaping the same identity twice is a no-op, not a second cancel.
	if got := pool.reloader.reap([]string{"198.51.100.10:443"}); len(got) != 0 {
		t.Fatalf("second reap returned %v, want nothing", got)
	}

	// stop() is terminal: nothing to cancel afterwards.
	pool.reloader.stop()
	if got := pool.reloader.reap([]string{"198.51.100.11:443"}); len(got) != 0 {
		t.Fatalf("reap after stop returned %v, want nothing", got)
	}
}

// The health seam: the failure cause recorded by the auth path decides whether
// the identity becomes a quality candidate at all.
func TestMinerDegradedReaperHealthRegistryRecordsFailureCause(t *testing.T) {
	resetProxyLiveHealthRegistryForTest()
	resetReapedProxyHoldsForTest()
	t.Cleanup(resetProxyLiveHealthRegistryForTest)

	// A deadline-cut attempt is slow, not broken.
	noteLiveProxyHealthFailureCause("198.51.100.10:443", context.DeadlineExceeded)
	// A refusal is the proxy's own fault.
	noteLiveProxyHealthFailureCause("198.51.100.11:443", errors.New("connection refused by peer"))

	degraded := proxyLiveDegradedProxies(time.Now())
	if len(degraded) != 2 {
		t.Fatalf("degraded = %+v, want both failed identities", degraded)
	}
	byKey := map[string]minerDegradedProxyEntry{}
	for _, entry := range degraded {
		byKey[entry.Key] = entry
	}
	if !byKey["198.51.100.10:443"].LastFailureTimeout {
		t.Fatal("the deadline-cut identity was not recorded as timeout-family")
	}
	if byKey["198.51.100.11:443"].LastFailureTimeout {
		t.Fatal("a refused connection was recorded as timeout-family")
	}
	// A success clears the flag with the rest of the entry.
	noteLiveProxyHealthUp("198.51.100.10:443")
	if entry, ok := proxyLiveHealthRegistry.byKey["198.51.100.10:443"]; !ok || entry.LastFailureTimeout {
		t.Fatalf("success did not clear the timeout flag: %+v", entry)
	}
}

// The reaper's loop stops with the run: a cancelled context ends it, and a
// reaper without a reloader has nothing to cancel.
func TestMinerDegradedReaperRunStopsWithContext(t *testing.T) {
	pool := newReapTestPool(t)
	reaper := newTestReaper(pool, time.Now(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		reaper.run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper loop did not stop with its context")
	}

	// No reloader: returns immediately rather than panicking on a nil pool.
	(&minerDegradedReaper{}).run(context.Background())
}

// A released identity used to keep its old DownSince, so the next cycle ranked
// it as long-down and reaped it again: the hold only paused the loop. Release
// now resets the clock, and this pins that.
func TestMinerDegradedReaperReleaseResetsDownClock(t *testing.T) {
	proxyLiveHealthRegistry.Lock()
	proxyLiveHealthRegistry.byKey = map[string]proxyLiveHealthEntry{}
	proxyLiveHealthRegistry.Unlock()
	resetReapedProxyHoldsForTest()
	t.Cleanup(func() {
		proxyLiveHealthRegistry.Lock()
		proxyLiveHealthRegistry.byKey = map[string]proxyLiveHealthEntry{}
		proxyLiveHealthRegistry.Unlock()
		resetReapedProxyHoldsForTest()
	})

	key := "198.51.100.77:443:released"
	noteLiveProxyHealth(key, "offline", time.Now().Add(-2*time.Hour))

	proxyLiveHealthRegistry.Lock()
	before := proxyLiveHealthRegistry.byKey[key].DownSince
	proxyLiveHealthRegistry.Unlock()
	if before.IsZero() {
		t.Fatal("precondition: down clock was not recorded")
	}

	holdReapedProxy(key, time.Now().Add(-time.Minute))

	released := releaseExpiredReapedProxyHolds(time.Now())
	if len(released) != 1 || released[0] != key {
		t.Fatalf("released %v, want [%s]", released, key)
	}
	proxyLiveHealthRegistry.Lock()
	after := proxyLiveHealthRegistry.byKey[key].DownSince
	proxyLiveHealthRegistry.Unlock()
	if !after.IsZero() {
		t.Fatalf("down clock survived release (%v): the next cycle would reap it again", after)
	}
}
