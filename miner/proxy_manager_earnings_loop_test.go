package miner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// Tests for the earnings recording loop, ported from the 3.23-fix v33
// provider's runEarningWindows feed (main.go): the tick that folds the live
// per-identity billable counters into the earn tracker and the earnings store.

// withFreshEarnTracker isolates globalPerProxyEarnTracker for one test: the
// tracker is a process-wide singleton, so a test that earns must not leak that
// state into the paid grader's and the audit's tests.
func withFreshEarnTracker(t *testing.T) {
	t.Helper()
	globalPerProxyEarnTracker.mu.Lock()
	prevLast := globalPerProxyEarnTracker.lastEarned
	prevCum := globalPerProxyEarnTracker.prevCum
	globalPerProxyEarnTracker.lastEarned = map[string]time.Time{}
	globalPerProxyEarnTracker.prevCum = map[string]uint64{}
	globalPerProxyEarnTracker.mu.Unlock()
	t.Cleanup(func() {
		globalPerProxyEarnTracker.mu.Lock()
		globalPerProxyEarnTracker.lastEarned = prevLast
		globalPerProxyEarnTracker.prevCum = prevCum
		globalPerProxyEarnTracker.mu.Unlock()
	})
}

// withEarnBandwidth isolates the published live counters for one test.
func withEarnBandwidth(t *testing.T) {
	t.Helper()
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)
}

// TestEarningsRecorderCreditsStoreAndTracker pins the loop's whole job: a
// second tick with a positive billable delta must mark the identity earning
// (the paid grader's earn-skip) and credit it in the persisted store.
func TestEarningsRecorderCreditsStoreAndTracker(t *testing.T) {
	withFreshEarnTracker(t)
	withEarnBandwidth(t)
	withGlobalEarningsStore(t)

	const key = "198.51.100.8:443"
	bw := &bandwidth.ProxyBandwidth{}
	publishProxyEarnBandwidth(key, bw)

	recorder := &proxyEarningsRecorder{}
	recorder.record(time.Now()) // baseline tick: no credit, not earning
	if globalPerProxyEarnTracker.EarnedSince(key, paidEarnWindow) {
		t.Fatal("the first tick must only establish the counter baseline")
	}
	if got := globalProxyEarningsStore.Score(key, time.Now()); got != 0 {
		t.Fatalf("first tick credited %v bytes, want 0", got)
	}

	bw.BillableRx.Store(1 << 20)
	recorder.record(time.Now())

	if !globalPerProxyEarnTracker.EarnedSince(key, paidEarnWindow) {
		t.Fatal("a positive billable delta must mark the identity earning")
	}
	if got := globalProxyEarningsStore.Score(key, time.Now()); got <= 0 {
		t.Fatalf("the earnings store credited %v bytes for an earning identity, want > 0", got)
	}
}

// TestEarningsRecorderPersistsThroughMaybeSave pins that the tick writes the
// store (v33 saves on the same cadence behind the 15m throttle): the earnings
// history has to survive a restart, which is the whole reason the store exists.
func TestEarningsRecorderPersistsThroughMaybeSave(t *testing.T) {
	dir := withTempHome(t)
	withFreshEarnTracker(t)
	withEarnBandwidth(t)

	const key = "198.51.100.9:443"
	bw := &bandwidth.ProxyBandwidth{}
	publishProxyEarnBandwidth(key, bw)

	recorder := &proxyEarningsRecorder{}
	recorder.record(time.Now())
	bw.BillableTx.Store(4096)
	recorder.record(time.Now())

	path := filepath.Join(dir, "proxy_earnings.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the earnings store was not persisted by the tick: %v", err)
	}
}

// TestEarningsRecorderWithdrawsDepartedIdentities pins the live-snapshot rule:
// a slot that exits stops being folded in, and the tracker prunes it so a
// proxy that comes back cannot inherit stale "earning" state (which would
// wrongly suppress its first paid probe). The store keeps the record.
func TestEarningsRecorderWithdrawsDepartedIdentities(t *testing.T) {
	withFreshEarnTracker(t)
	withEarnBandwidth(t)
	withGlobalEarningsStore(t)

	const key = "198.51.100.10:443"
	bw := &bandwidth.ProxyBandwidth{}

	publishProxyEarnBandwidth(key, bw)
	recorder := &proxyEarningsRecorder{}
	recorder.record(time.Now())
	bw.BillableRx.Store(2048)
	recorder.record(time.Now())
	if !globalPerProxyEarnTracker.EarnedSince(key, paidEarnWindow) {
		t.Fatal("the identity must be earning before it departs")
	}
	scored := globalProxyEarningsStore.Score(key, time.Now())

	unpublishProxyEarnBandwidth(key)
	recorder.record(time.Now())

	if globalPerProxyEarnTracker.EarnedSince(key, paidEarnWindow) {
		t.Fatal("a departed identity must be pruned from the trackers")
	}
	if got := globalProxyEarningsStore.Score(key, time.Now()); got < scored*0.99 {
		t.Fatalf("the earnings record must survive the departure: got %v, want ~%v (decay only)", got, scored)
	}
}

// TestEarningsRecorderEmptySnapshotPrunesTracker pins the empty-health-set
// branch: with no live proxies every identity is pruned, so a re-added proxy
// cannot inherit stale earning state.
func TestEarningsRecorderEmptySnapshotPrunesTracker(t *testing.T) {
	withFreshEarnTracker(t)
	withEarnBandwidth(t)
	withGlobalEarningsStore(t)

	const key = "198.51.100.11:443"
	bw := &bandwidth.ProxyBandwidth{}
	publishProxyEarnBandwidth(key, bw)

	recorder := &proxyEarningsRecorder{}
	recorder.record(time.Now())
	bw.BillableRx.Store(1024)
	recorder.record(time.Now())
	if !globalPerProxyEarnTracker.EarnedSince(key, paidEarnWindow) {
		t.Fatal("the identity must be earning before the snapshot empties")
	}

	resetProxyEarnBandwidthForTest()
	recorder.record(time.Now())

	if globalPerProxyEarnTracker.EarnedSince(key, paidEarnWindow) {
		t.Fatal("an empty snapshot must prune the earning state")
	}
	if recorder.prevSet {
		t.Fatal("an empty snapshot must reset the aggregate window baseline")
	}
}

// TestEarningsRecorderKeysByIdentityNotAddress pins the shared-gateway rule:
// two accounts behind one address are two identities, and only the one that
// moved bytes is credited — neither the tracker nor the store may collapse
// them onto the bare address.
func TestEarningsRecorderKeysByIdentityNotAddress(t *testing.T) {
	withFreshEarnTracker(t)
	withEarnBandwidth(t)
	withGlobalEarningsStore(t)

	const address = "198.51.100.20:443"
	earning := (&connect.ProxySettings{Address: address, Auth: &proxy.Auth{User: "alice"}}).Key()
	quiet := (&connect.ProxySettings{Address: address, Auth: &proxy.Auth{User: "bob"}}).Key()
	bwEarning := &bandwidth.ProxyBandwidth{}
	bwQuiet := &bandwidth.ProxyBandwidth{}
	publishProxyEarnBandwidth(earning, bwEarning)
	publishProxyEarnBandwidth(quiet, bwQuiet)

	recorder := &proxyEarningsRecorder{}
	recorder.record(time.Now())
	bwEarning.BillableRx.Store(512)
	recorder.record(time.Now())

	if !globalPerProxyEarnTracker.EarnedSince(earning, paidEarnWindow) {
		t.Fatal("the earning identity must be marked earning")
	}
	if globalPerProxyEarnTracker.EarnedSince(quiet, paidEarnWindow) {
		t.Fatal("a sibling account at the same address must not be marked earning")
	}
	now := time.Now()
	if got := globalProxyEarningsStore.Score(earning, now); got <= 0 {
		t.Fatalf("the earning identity scored %v, want > 0", got)
	}
	if got := globalProxyEarningsStore.Score(quiet, now); got != 0 {
		t.Fatalf("the quiet identity at the same address scored %v, want 0", got)
	}
	// The bare address is not a store key: crediting it would recreate the
	// legacy bare-address entries the identity migration removes.
	if got := globalProxyEarningsStore.Score(address, now); got != 0 {
		t.Fatalf("the bare address scored %v, want 0 (identity keys only)", got)
	}
}

// TestProxyIdentityBandwidthIndexSeparatesSharedAddress pins the registry
// index rule the recorder depends on: the index is derived from the identity
// key, so two accounts at one address keep separate counters (a shared counter
// would credit each identity with the other's bytes), while one identity is
// stable and the direct slot stays index 0.
func TestProxyIdentityBandwidthIndexSeparatesSharedAddress(t *testing.T) {
	plain := &connect.ProxySettings{Address: "198.51.100.30:443"}
	alice := &connect.ProxySettings{Address: "198.51.100.30:443", Auth: &proxy.Auth{User: "alice"}}
	bob := &connect.ProxySettings{Address: "198.51.100.30:443", Auth: &proxy.Auth{User: "bob"}}

	if got := proxyIdentityBandwidthIndex(nil); got != 0 {
		t.Fatalf("the direct slot index = %d, want 0", got)
	}
	if proxyIdentityBandwidthIndex(plain) == proxyIdentityBandwidthIndex(alice) {
		t.Fatal("a credentialed proxy must not share the plain address's counter index")
	}
	if proxyIdentityBandwidthIndex(alice) == proxyIdentityBandwidthIndex(bob) {
		t.Fatal("two accounts at one address must not share a counter index")
	}
	if proxyIdentityBandwidthIndex(alice) != proxyIdentityBandwidthIndex(&connect.ProxySettings{Address: "198.51.100.30:443", Auth: &proxy.Auth{User: "alice"}}) {
		t.Fatal("one identity must hash to a stable index")
	}
}

// TestProxyEarnSumLastN pins the window arithmetic behind the [earn] line: a
// partial window sums what exists, and n larger than the slice is not a panic.
func TestProxyEarnSumLastN(t *testing.T) {
	deltas := []uint64{1, 2, 3, 4}
	if got := proxyEarnSumLastN(deltas, 2); got != 7 {
		t.Fatalf("sumLastN(4, 2) = %d, want 7", got)
	}
	if got := proxyEarnSumLastN(deltas, 10); got != 10 {
		t.Fatalf("sumLastN(4, 10) = %d, want the 10 in the slice", got)
	}
	if got := proxyEarnSumLastN(nil, 5); got != 0 {
		t.Fatalf("sumLastN(nil, 5) = %d, want 0", got)
	}
}
