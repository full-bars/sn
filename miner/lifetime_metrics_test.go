package miner

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// withLifetimeStore points the process-wide lifetime store at one in-memory
// instance for the duration of a test.
func withLifetimeStore(t *testing.T) *lifetimeMetrics {
	t.Helper()
	prev := lifetimeStore
	store := loadLifetimeMetrics("")
	lifetimeStore = store
	t.Cleanup(func() { lifetimeStore = prev })
	return store
}

// The totals survive a restart, and the recorded start comes back exact.
func TestLifetimeStorePersistsAcrossRestart(t *testing.T) {
	withTempHome(t)
	path := lifetimeMetricsPath()
	if path == "" {
		t.Fatal("lifetimeMetricsPath must resolve inside the state directory")
	}
	store := loadLifetimeMetrics(path)
	store.Add(3, 5, 7, 11, 2, 1, 1024)
	store.Flush()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"pqe_opens": 3`) {
		t.Fatalf("persisted store = %s", string(b))
	}

	reloaded := loadLifetimeMetrics(path)
	pqe, clas, up, deny, rec, lost, bill := reloaded.Snapshot()
	if pqe != 3 || clas != 5 || up != 7 || deny != 11 || rec != 2 || lost != 1 || bill != 1024 {
		t.Fatalf("reloaded totals = %d %d %d %d %d %d %d", pqe, clas, up, deny, rec, lost, bill)
	}
	since, approx := reloaded.Started()
	if since.IsZero() {
		t.Fatal("the start must survive a restart")
	}
	if approx {
		t.Fatal("a store that recorded its start must not report an approximate one")
	}
}

// A store written before the start field existed keeps its totals but has no
// recorded start: the start is pinned and marked approximate, and written back
// even if no counter ever moves, so restarts stop re-pinning a later date.
func TestLifetimeLegacyStoreWithoutStartIsApproximateAndPinned(t *testing.T) {
	withTempHome(t)
	path := lifetimeMetricsPath()
	if err := os.WriteFile(path, []byte(`{"pqe_opens":9,"saved_at":"2026-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := loadLifetimeMetrics(path)
	if _, approx := store.Started(); !approx {
		t.Fatal("a store with no recorded start must report an approximate one")
	}
	if !store.MaybeFlush(time.Now()) {
		t.Fatal("the pinned start must be persisted on the first flush")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"since"`) {
		t.Fatalf("the pinned start was not written back: %s", string(b))
	}
	again := loadLifetimeMetrics(path)
	since, approx := again.Started()
	if since.IsZero() || !approx {
		t.Fatalf("reloaded start = %v approx=%v, want a pinned approximate start", since, approx)
	}
}

// A zero add is a no-op: no dirty flag, no file rewrite on an idle node.
func TestLifetimeZeroAddDoesNotRewriteTheStore(t *testing.T) {
	withTempHome(t)
	path := lifetimeMetricsPath()
	store := loadLifetimeMetrics(path)
	store.Add(0, 0, 0, 0, 0, 0, 0)
	if store.MaybeFlush(time.Now().Add(time.Hour)) {
		t.Fatal("a zero add must not mark the store dirty")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a zero add must not create the file, stat err = %v", err)
	}
}

func TestLifetimeMaybeFlushThrottles(t *testing.T) {
	withTempHome(t)
	path := lifetimeMetricsPath()
	store := loadLifetimeMetrics(path)
	now := time.Now()
	store.Add(1, 0, 0, 0, 0, 0, 0)
	if !store.MaybeFlush(now.Add(10 * time.Minute)) {
		t.Fatal("the first flush must write")
	}
	store.Add(1, 0, 0, 0, 0, 0, 0)
	if store.MaybeFlush(now.Add(11 * time.Minute)) {
		t.Fatal("a flush inside the throttle window must wait")
	}
	if !store.MaybeFlush(now.Add(16 * time.Minute)) {
		t.Fatal("the flush must write once the throttle window passes")
	}
}

// The scrape carries the persisted totals and the two start gauges.
func TestLifetimeMetricsExposeTotalsAndStartGauges(t *testing.T) {
	store := withLifetimeStore(t)
	store.Add(1, 2, 3, 4, 5, 6, 7)
	out := minerExtraMetrics()
	for _, want := range []string{
		`urnet_lifetime_sessions_total{encryption="pqe"} 1`,
		`urnet_lifetime_sessions_total{encryption="classical"} 2`,
		`urnet_lifetime_contracts_total{result="acquired"} 3`,
		`urnet_lifetime_contracts_total{result="denied"} 4`,
		`urnet_lifetime_proxies_total{event="recovered"} 5`,
		`urnet_lifetime_proxies_total{event="lost"} 6`,
		`urnet_lifetime_billable_bytes_total 7`,
		`urnet_lifetime_start_timestamp_seconds `,
		`urnet_lifetime_start_approximate 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("scrape is missing %q", want)
		}
	}
}

// The health report is the exclusive consumer of transition events, so it
// feeds the recovered/lost deltas exactly once per event.
func TestHealthTransitionsFeedTheLifetimeStore(t *testing.T) {
	store := withLifetimeStore(t)
	noteHealthReportForLifetime(proxyHealthReport{
		Recovered:     []proxyEvent{{Index: 1}, {Index: 2}},
		NewlyDegraded: []proxyEvent{{Index: 3}},
		NewlyDead:     []proxyEvent{{Index: 4}},
	})
	_, _, _, _, recovered, lost, _ := store.Snapshot()
	if recovered != 2 || lost != 2 {
		t.Fatalf("recovered=%d lost=%d, want 2 and 2", recovered, lost)
	}
}

// Billable bytes are counted per identity against a baseline, so a proxy that
// joins with existing counters does not add its whole history, a departing
// proxy's baseline survives a short absence, and a counter reset adds nothing.
func TestLifetimeBillableDeltasPerIdentity(t *testing.T) {
	flip := newLifetimeBillableFlip()
	now := time.Now()
	a := &bandwidth.ProxyBandwidth{}
	bw := map[string]*bandwidth.ProxyBandwidth{"a": a}

	if got := flip.fold(bw, now); got != 0 {
		t.Fatalf("the first sighting is a baseline, got %d", got)
	}
	a.BillableRx.Store(100)
	a.BillableTx.Store(50)
	if got := flip.fold(bw, now.Add(time.Minute)); got != 150 {
		t.Fatalf("increment = %d, want 150", got)
	}

	b := &bandwidth.ProxyBandwidth{}
	b.BillableRx.Store(1000)
	bw["b"] = b
	if got := flip.fold(bw, now.Add(2*time.Minute)); got != 0 {
		t.Fatalf("a joining identity's history must not count, got %d", got)
	}

	delete(bw, "a")
	flip.fold(bw, now.Add(3*time.Minute))
	bw["a"] = a
	a.BillableRx.Store(180)
	if got := flip.fold(bw, now.Add(4*time.Minute)); got != 80 {
		t.Fatalf("a returning identity must resume its baseline, got %d", got)
	}

	a.BillableRx.Store(10)
	if got := flip.fold(bw, now.Add(5*time.Minute)); got != 0 {
		t.Fatalf("a counter reset must add nothing, got %d", got)
	}
}
