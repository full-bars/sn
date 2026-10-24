package miner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// metrics_persist_test.go covers the counter store's write policy: an idle
// miner must not rewrite it, a change must, an unusable file must be
// recreated, and a store older than a day must be refreshed. The clock and the
// store path are injected, so no test sleeps and none touches the real state
// directory.

// testMetricsCounterStore is a store over a temp path with a manual clock and
// a counted write.
type testMetricsCounterStore struct {
	store *metricsCounterStore
	path  string
	now   time.Time
	snap  minerMetricsCounterSnapshot
	// writes records the path of every write the store performed.
	writes []string
}

func newTestMetricsCounterStore(t *testing.T) *testMetricsCounterStore {
	t.Helper()
	h := &testMetricsCounterStore{
		now: time.Unix(1_700_000_000, 0).UTC(),
		snap: minerMetricsCounterSnapshot{
			Proxies: map[int]minerMetricsProxyCounters{},
		},
	}
	h.path = filepath.Join(t.TempDir(), "counters.json")
	h.store = newMetricsCounterStore(h.path)
	h.store.now = func() time.Time { return h.now }
	h.store.write = func(path string, data []byte, perm os.FileMode) error {
		h.writes = append(h.writes, path)
		return writeMetricsCounterFileAtomic(path, data, perm)
	}
	h.store.snapshot = func() minerMetricsCounterSnapshot {
		out := minerMetricsCounterSnapshot{
			Proxies: make(map[int]minerMetricsProxyCounters, len(h.snap.Proxies)),
			Control: h.snap.Control,
		}
		for index, counters := range h.snap.Proxies {
			// A live entry is restamped on every flush, as the real snapshot
			// does through flush's clock.
			counters.LastSeen = h.now.Unix()
			out.Proxies[index] = counters
		}
		return out
	}
	h.store.addControl = func(c minerMetricsControlCounters) {
		h.snap.Control.Acked += c.Acked
		h.snap.Control.Set += c.Set
		h.snap.Control.Get += c.Get
		h.snap.Control.Clear += c.Clear
	}
	return h
}

func (h *testMetricsCounterStore) advance(d time.Duration) {
	h.now = h.now.Add(d)
}

func (h *testMetricsCounterStore) writeCount() int {
	return len(h.writes)
}

func (h *testMetricsCounterStore) setProxy(index int, counters minerMetricsProxyCounters) {
	h.snap.Proxies[index] = counters
}

func (h *testMetricsCounterStore) readFile(t *testing.T) minerMetricsCountersFile {
	t.Helper()
	raw, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var file minerMetricsCountersFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse store: %v\n%s", err, raw)
	}
	return file
}

// An idle miner flushing twice performs exactly one write: the second flush is
// suppressed because nothing but the restamped fields changed.
func TestMetricsCounterFlushSuppressesUnchangedStore(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 100, TotalTx: 200, BillableRx: 10, BillableTx: 20})

	if err := h.store.flush(); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if got := h.writeCount(); got != 1 {
		t.Fatalf("first flush wrote %d times, want 1", got)
	}

	h.advance(45 * time.Second)
	if err := h.store.flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if got := h.writeCount(); got != 1 {
		t.Fatalf("an idle flush rewrote the unchanged store: %d writes, want 1", got)
	}
}

// A changed total is written, and the stored shape carries it.
func TestMetricsCounterFlushWritesChangedTotals(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 100, TotalTx: 200})
	if err := h.store.flush(); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	h.advance(45 * time.Second)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 130, TotalTx: 240})
	if err := h.store.flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if got := h.writeCount(); got != 2 {
		t.Fatalf("a changed total did not write: %d writes, want 2", got)
	}
	file := h.readFile(t)
	if got := file.Proxies[7].TotalRx; got != 130 {
		t.Fatalf("stored total_rx = %d, want 130", got)
	}
	if !file.SavedAt.Equal(h.now) {
		t.Fatalf("stored saved_at = %s, want %s", file.SavedAt, h.now)
	}

	// The control counters are part of the store, and a change to them is a
	// change to the store.
	h.advance(45 * time.Second)
	h.snap.Control.Acked = 3
	if err := h.store.flush(); err != nil {
		t.Fatalf("third flush: %v", err)
	}
	if got := h.writeCount(); got != 3 {
		t.Fatalf("a changed control counter did not write: %d writes, want 3", got)
	}
	if got := h.readFile(t).Control.Acked; got != 3 {
		t.Fatalf("stored control.acked = %d, want 3", got)
	}
}

// A store that is present but unreadable must be recreated, not left broken
// and not treated as a reason to skip the write.
func TestMetricsCounterFlushRecreatesUnreadableStore(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		h := newTestMetricsCounterStore(t)
		h.setProxy(7, minerMetricsProxyCounters{TotalRx: 5})
		if err := os.WriteFile(h.path, []byte("{ this is not json"), 0o600); err != nil {
			t.Fatalf("seed corrupt store: %v", err)
		}
		if err := h.store.flush(); err != nil {
			t.Fatalf("flush over a corrupt store: %v", err)
		}
		if got := h.writeCount(); got != 1 {
			t.Fatalf("a corrupt store was not recreated: %d writes, want 1", got)
		}
		if got := h.readFile(t).Proxies[7].TotalRx; got != 5 {
			t.Fatalf("recreated store total_rx = %d, want 5", got)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		h := newTestMetricsCounterStore(t)
		h.setProxy(7, minerMetricsProxyCounters{TotalRx: 5})
		if err := h.store.flush(); err != nil {
			t.Fatalf("first flush: %v", err)
		}
		if err := os.Chmod(h.path, 0o000); err != nil {
			t.Fatalf("chmod store: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(h.path, 0o600) })
		if raw, err := os.ReadFile(h.path); err == nil && len(raw) > 0 {
			t.Skip("a 0000 file is still readable here (running as root)")
		}

		h.advance(45 * time.Second)
		if err := h.store.flush(); err != nil {
			t.Fatalf("flush over an unreadable store: %v", err)
		}
		if got := h.writeCount(); got != 2 {
			t.Fatalf("an unreadable store was not recreated: %d writes, want 2", got)
		}
	})
}

// An unchanged store is still refreshed once it is a day old, so saved_at stays
// a usable liveness signal, and not before.
func TestMetricsCounterFlushRefreshesDayOldStore(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 100})
	if err := h.store.flush(); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	h.advance(23 * time.Hour)
	if err := h.store.flush(); err != nil {
		t.Fatalf("flush a day short: %v", err)
	}
	if got := h.writeCount(); got != 1 {
		t.Fatalf("a store younger than a day was rewritten: %d writes, want 1", got)
	}

	h.advance(2 * time.Hour)
	if err := h.store.flush(); err != nil {
		t.Fatalf("flush past a day: %v", err)
	}
	if got := h.writeCount(); got != 2 {
		t.Fatalf("a day-old unchanged store was not refreshed: %d writes, want 2", got)
	}
	if !h.readFile(t).SavedAt.Equal(time.Unix(1_700_000_000+25*3600, 0).UTC()) {
		t.Fatalf("refreshed store saved_at = %s, want the flush clock", h.readFile(t).SavedAt)
	}
}

// A store another writer advanced past this process must not be lowered: the
// flush merges by max, which is what keeps a draining hotswap parent and a
// promoted child from pushing the file backwards.
func TestMetricsCounterFlushMergesHigherStoredTotals(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 100})
	if err := h.store.flush(); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	// Another process wrote a higher total, then this process's snapshot
	// (which restored the old value) flushes.
	file := h.readFile(t)
	file.Proxies[7] = minerMetricsProxyCounters{TotalRx: 900, LastSeen: h.now.Unix()}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(h.path, raw, 0o600); err != nil {
		t.Fatalf("rewrite store: %v", err)
	}

	h.advance(45 * time.Second)
	if err := h.store.flush(); err != nil {
		t.Fatalf("merge flush: %v", err)
	}
	if got := h.readFile(t).Proxies[7].TotalRx; got != 900 {
		t.Fatalf("flush lowered another writer's total: %d, want 900", got)
	}
}

// Restoring seeds the per-proxy base and adds the control counters once; the
// aggregate's retired total covers exactly the indices this process does not
// run, so a seeded live proxy is never counted twice.
func TestMetricsCounterRestoreSeedsBaseAndControl(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 100, TotalTx: 200, BillableRx: 10, BillableTx: 20})
	h.setProxy(9, minerMetricsProxyCounters{TotalRx: 1, TotalTx: 2, BillableRx: 3, BillableTx: 4})
	h.snap.Control = minerMetricsControlCounters{Acked: 5, Set: 2, Get: 3, Clear: 1}
	if err := h.store.flush(); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	restored := newTestMetricsCounterStore(t)
	restored.path = h.path
	restored.store.path = h.path
	if err := restored.store.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	seed, ok := restored.store.proxySeed(7)
	if !ok {
		t.Fatal("restore did not seed proxy 7")
	}
	if seed.TotalRx != 100 || seed.TotalTx != 200 || seed.BillableRx != 10 || seed.BillableTx != 20 {
		t.Fatalf("seeded proxy 7 = %+v, want the stored totals", seed)
	}
	if got := restored.snap.Control; got != (minerMetricsControlCounters{Acked: 5, Set: 2, Get: 3, Clear: 1}) {
		t.Fatalf("restored control counters = %+v, want the stored counts", got)
	}

	retired := restored.store.retiredTotals(map[int]struct{}{7: {}})
	want := minerMetricsByteTotals{TotalRx: 1, TotalTx: 2, BillableRx: 3, BillableTx: 4}
	if retired != want {
		t.Fatalf("retired totals = %+v, want %+v (only the non-live index)", retired, want)
	}

	// Restoring twice must not double the process counters.
	if err := restored.store.restore(); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if got := restored.snap.Control.Acked; got != 5 {
		t.Fatalf("second restore doubled the control counters: acked = %d, want 5", got)
	}
}

// A store written by a newer build is left alone: rewriting it in this build's
// schema would drop fields this build does not know.
func TestMetricsCounterFlushLeavesNewerStoreAlone(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 1})
	raw, err := json.Marshal(minerMetricsCountersFile{Version: metricsCounterStoreVersion + 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(h.path, raw, 0o600); err != nil {
		t.Fatalf("seed newer store: %v", err)
	}
	err = h.store.flush()
	if !errors.Is(err, errMetricsCountersNewer) {
		t.Fatalf("flush over a newer store = %v, want errMetricsCountersNewer", err)
	}
	if got := h.writeCount(); got != 0 {
		t.Fatalf("flush rewrote a newer store: %d writes, want 0", got)
	}
}

// A HotSwap candidate registers its slots before the parent yields, so the
// restore that runs at takeover only fills the base and those live entries keep
// counting from zero. The catch-up raises them to the parent's stored totals,
// never lowers what this process already counted, and leaves a live index
// counted once in the aggregate.
func TestMetricsCounterCatchUpRaisesLiveProxies(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 100, TotalTx: 200, BillableRx: 10, BillableTx: 20})
	acked := controlCmdsAcked.Load()
	if acked < 0 {
		t.Fatalf("control counter is negative: %d", acked)
	}
	h.snap.Control = minerMetricsControlCounters{Acked: uint64(acked) + 11}
	if err := h.store.flush(); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	store := swapMetricsCounterStore(t, h.path)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)
	registry := bandwidth.NewRegistry()
	controlRuntime.setBandwidthRegistry(registry)
	// Registered while the base is empty, exactly as a takeover candidate
	// does: the seed finds nothing and the entry starts at zero.
	live := registry.Register(7)
	live.TotalRx.Add(5)
	live.BillableTx.Add(2)

	if err := store.catchUp(); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	// The catch-up ADDS the stored total to what this process already counted:
	// the slot started at zero at registration, so 5 bytes served before the
	// takeover are part of the series and must survive. Taking the max instead
	// discarded exactly those bytes.
	if got := live.TotalRx.Load(); got != 105 {
		t.Fatalf("caught-up total_rx = %d, want 105 (stored 100 plus the 5 counted before the takeover)", got)
	}
	if got := live.TotalTx.Load(); got != 200 {
		t.Fatalf("caught-up total_tx = %d, want the stored 200", got)
	}
	if got := live.BillableRx.Load(); got != 10 {
		t.Fatalf("caught-up billable_rx = %d, want the stored 10", got)
	}
	if got := live.BillableTx.Load(); got != 22 {
		t.Fatalf("caught-up billable_tx = %d, want 22 (stored 20 plus the 2 counted before the takeover)", got)
	}
	if got := controlCmdsAcked.Load(); got != acked+11 {
		t.Fatalf("caught-up control.acked = %d, want %d", got, acked+11)
	}

	// Index 7 is live, so the aggregate counts its entry and must not add the
	// same stored total again through the retired sum. The entry itself is
	// stored + own, which is why the total is 105 and not 100 or 110.
	if _, _, totalRx, _, _, ok := controlRuntime.trafficTotals(); !ok || totalRx != 105 {
		t.Fatalf("aggregate total_rx = %d (counted=%v), want 105 counted once", totalRx, ok)
	}

	// A catch-up after another writer's later flush raises again; one against
	// a store that is behind never lowers this process's counters.
	file := h.readFile(t)
	file.Proxies[7] = minerMetricsProxyCounters{TotalRx: 900, LastSeen: h.now.Unix()}
	file.Control = minerMetricsControlCounters{Acked: 1}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(h.path, raw, 0o600); err != nil {
		t.Fatalf("rewrite store: %v", err)
	}
	if err := store.catchUp(); err != nil {
		t.Fatalf("second catch-up: %v", err)
	}
	// Only the part of the store this process has not folded in is added: 900
	// stored, 100 already applied, so the live total moves by 800 to 905.
	if got := live.TotalRx.Load(); got != 905 {
		t.Fatalf("second catch-up total_rx = %d, want 905", got)
	}
	if got := live.TotalTx.Load(); got != 200 {
		t.Fatalf("a store without total_tx lowered the live one: %d, want 200", got)
	}
	if got := controlCmdsAcked.Load(); got != acked+11 {
		t.Fatalf("a lower stored control count lowered the live one: acked = %d, want %d", got, acked+11)
	}
}

// The delayed catch-up ends with the run instead of holding a timer goroutine
// past shutdown.
func TestMetricsCounterCatchUpAfterDrainReturnsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		catchUpMetricsCountersAfterDrain(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the delayed catch-up did not return on a cancelled run")
	}
}

// swapMetricsCounterStore points the process store at another path for one
// test, so restoring seeds the registry from a temp file and never from the
// real state directory.
func swapMetricsCounterStore(t *testing.T, path string) *metricsCounterStore {
	t.Helper()
	saved := metricsCounters
	store := newMetricsCounterStore(path)
	t.Cleanup(func() { metricsCounters = saved })
	metricsCounters = store
	return store
}

// A restored store seeds a returning proxy's registry entry and keeps a
// retired proxy in the aggregate, exactly once each: the seeded live entry
// replaces the retired contribution when that index comes back.
func TestMetricsCounterRestoreContinuesTheAggregate(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(7, minerMetricsProxyCounters{TotalRx: 100, TotalTx: 200, BillableRx: 10, BillableTx: 20})
	h.setProxy(9, minerMetricsProxyCounters{TotalRx: 1, TotalTx: 2, BillableRx: 3, BillableTx: 4})
	if err := h.store.flush(); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	store := swapMetricsCounterStore(t, h.path)
	if err := store.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)

	registry := bandwidth.NewRegistry()
	controlRuntime.setBandwidthRegistry(registry)
	if got := registry.Register(7).TotalRx.Load(); got != 100 {
		t.Fatalf("returning proxy 7 started at total_rx %d, want its stored 100", got)
	}

	// Index 9 has not registered in this process, so its stored totals still
	// count through the retired sum.
	billableRx, billableTx, totalRx, totalTx, _, ok := controlRuntime.trafficTotals()
	if !ok {
		t.Fatal("trafficTotals reported no counters after a restore")
	}
	if totalRx != 101 || totalTx != 202 || billableRx != 13 || billableTx != 24 {
		t.Fatalf("aggregate after restore = in %d/%d out %d/%d, want in 101/%d out 13/%d",
			totalRx, billableRx, totalTx, billableTx, 202, 24)
	}

	// When index 9 comes back its entry is seeded, and the aggregate must not
	// move: the retired contribution is replaced, not added to.
	registry.Register(9)
	billableRx, billableTx, totalRx, totalTx, _, _ = controlRuntime.trafficTotals()
	if totalRx != 101 || totalTx != 202 || billableRx != 13 || billableTx != 24 {
		t.Fatalf("aggregate after the second registration = in %d/%d out %d/%d, want in 101/%d out 13/%d",
			totalRx, billableRx, totalTx, billableTx, 202, 24)
	}
}

// The drain window is the reason the catch-up adds instead of taking a max:
// the parent keeps serving while the candidate accepts, so the stored total
// already contains the parent's drain bytes and the candidate's own counters
// were counted from zero at registration. The continuation is the sum, and a
// max drops whichever side is smaller.
func TestMetricsCounterCatchUpKeepsBothSidesOfTheDrainWindow(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(9, minerMetricsProxyCounters{TotalRx: 1050, TotalTx: 1050})
	if err := h.store.flush(); err != nil {
		t.Fatalf("parent drain flush: %v", err)
	}

	store := swapMetricsCounterStore(t, h.path)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)
	registry := bandwidth.NewRegistry()
	controlRuntime.setBandwidthRegistry(registry)

	// The candidate served 150 bytes of its own before the catch-up ran.
	live := registry.Register(9)
	live.TotalRx.Add(150)
	live.TotalTx.Add(150)

	if err := store.catchUp(); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if got := live.TotalRx.Load(); got != 1200 {
		t.Fatalf("total_rx after the drain window = %d, want 1200 (1050 stored plus 150 served)", got)
	}

	// A second catch-up over the same store must not add the same bytes again.
	if err := store.catchUp(); err != nil {
		t.Fatalf("second catch up: %v", err)
	}
	if got := live.TotalRx.Load(); got != 1200 {
		t.Fatalf("second catch-up double counted: total_rx = %d, want 1200", got)
	}
}

// A flush rewrites the store, and with it the base, from this process's own
// live totals. If the catch-up then reads that base as "the other writer's
// value", the bytes this process served are added a second time. The catch-up
// must read the foreign baseline, which only ever mirrors the file.
func TestMetricsCounterCatchUpAfterOwnFlushDoesNotDoubleCount(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(11, minerMetricsProxyCounters{TotalRx: 1000})
	if err := h.store.flush(); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	store := swapMetricsCounterStore(t, h.path)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)
	registry := bandwidth.NewRegistry()
	controlRuntime.setBandwidthRegistry(registry)

	live := registry.Register(11)
	if err := store.catchUp(); err != nil {
		t.Fatalf("first catch-up: %v", err)
	}
	if got := live.TotalRx.Load(); got != 1000 {
		t.Fatalf("after first catch-up total_rx = %d, want 1000", got)
	}

	// The candidate serves while the store is rewritten with its own totals.
	live.TotalRx.Add(300)
	if err := store.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := store.catchUp(); err != nil {
		t.Fatalf("second catch-up: %v", err)
	}
	if got := live.TotalRx.Load(); got != 1300 {
		t.Fatalf("after own flush total_rx = %d, want 1300: the catch-up added this process's own bytes", got)
	}
}

// A slot registered after a restore starts at the seeded total rather than at
// zero, so the next catch-up must not add that same total again.
func TestMetricsCounterCatchUpAfterSeededRegistrationDoesNotDoubleCount(t *testing.T) {
	h := newTestMetricsCounterStore(t)
	h.setProxy(9, minerMetricsProxyCounters{TotalRx: 1000, TotalTx: 500})
	if err := h.store.flush(); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	store := swapMetricsCounterStore(t, h.path)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)
	registry := bandwidth.NewRegistry()
	controlRuntime.setBandwidthRegistry(registry)

	if err := store.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Exactly what the registry seed function does for a proxy that registers
	// after the restore: the slot starts at the stored totals.
	seed, ok := store.proxySeed(9)
	if !ok {
		t.Fatalf("proxySeed(9) found no stored totals")
	}
	live := registry.Register(9)
	// The registry's seed function already applied the stored totals at
	// registration; adding them again here was the test's own bug.
	_ = seed

	if err := store.catchUp(); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if got := live.TotalRx.Load(); got != 1000 {
		t.Fatalf("seeded slot total_rx = %d, want 1000", got)
	}
	if got := live.TotalTx.Load(); got != 500 {
		t.Fatalf("seeded slot total_tx = %d, want 500", got)
	}
}

// A prune removes an index from base. Its totals must move into the retired
// accumulator first, or urnet_*_total steps DOWN when a proxy ages out and
// Prometheus reads the decrease as a counter reset.
func TestMetricsCounterTotalsStayMonotonicWhenAPruneDropsAnIndex(t *testing.T) {
	h := newTestMetricsCounterStore(t)

	old := h.now.Add(-60 * 24 * time.Hour).Unix()
	h.store.setBase(map[int]minerMetricsProxyCounters{
		1: {TotalRx: 1000, TotalTx: 500, BillableRx: 900, BillableTx: 400, LastSeen: old},
	})
	h.snap.Proxies[2] = minerMetricsProxyCounters{TotalRx: 10, TotalTx: 5, BillableRx: 10, BillableTx: 5}

	if err := h.store.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	live := map[int]struct{}{2: {}}
	if got := h.store.retiredTotals(live); got.TotalRx != 1000 {
		t.Fatalf("pruned index left the aggregate: TotalRx = %d, want 1000", got.TotalRx)
	}

	// A second flush must not add the same retired totals again.
	if err := h.store.flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if got := h.store.retiredTotals(live); got.TotalRx != 1000 {
		t.Fatalf("retired totals counted twice: TotalRx = %d, want 1000", got.TotalRx)
	}

	// And they survive a reload, or a restart would drop the history.
	reloaded := newMetricsCounterStore(h.path)
	reloaded.now = h.store.now
	if err := reloaded.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := reloaded.retiredTotals(live); got.TotalRx != 1000 {
		t.Fatalf("retired totals not persisted: TotalRx = %d, want 1000", got.TotalRx)
	}
}

// A slot registered while catchUp reads the registry keeps its seed record.
// Losing it makes the next catch-up add that stored total a second time.
func TestCatchUpKeepsASlotRegisteredWhileItReadsTheRegistry(t *testing.T) {
	h := newTestMetricsCounterStore(t)

	file := minerMetricsCountersFile{
		Version: metricsCounterStoreVersion,
		SavedAt: h.now,
		Proxies: map[int]minerMetricsProxyCounters{1: {TotalRx: 100}},
	}
	blob, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := writeMetricsCounterFileAtomic(h.path, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	prev := catchUpLiveSnapshot
	t.Cleanup(func() { catchUpLiveSnapshot = prev })
	catchUpLiveSnapshot = func() map[int]*bandwidth.ProxyBandwidth {
		// Register published this slot and recorded its seed while catchUp was
		// reading the registry.
		h.store.mu.Lock()
		h.store.applied[1] = minerMetricsProxyCounters{TotalRx: 100}
		h.store.mu.Unlock()
		return map[int]*bandwidth.ProxyBandwidth{}
	}

	if err := h.store.catchUp(); err != nil {
		t.Fatalf("catchUp: %v", err)
	}

	h.store.mu.Lock()
	got := h.store.applied[1]
	h.store.mu.Unlock()
	if got.TotalRx != 100 {
		t.Fatalf("seed record lost by catch-up: %+v", got)
	}
}

// TestSuperviseLoopRestartsMetricsCounterFlush covers the supervised form of
// the periodic writer: a panic in one flush must restart the loop rather than
// ending counter persistence for the rest of the process, and stopFlush must
// still return promptly instead of waiting out a restart backoff.
func TestSuperviseLoopRestartsMetricsCounterFlush(t *testing.T) {
	const name = "metrics_counter_flush"
	resetLoopStatus(name)

	var writes atomic.Int32
	store := newMetricsCounterStore(filepath.Join(t.TempDir(), "counters.json"))
	store.snapshot = func() minerMetricsCounterSnapshot { return minerMetricsCounterSnapshot{} }
	store.write = func(path string, data []byte, perm os.FileMode) error {
		if writes.Add(1) == 1 {
			panic("flush boom")
		}
		return nil
	}
	store.startFlush(2 * time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for loopStatusOf(name).Restarts < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the supervisor never restarted the panicking counter flush")
		}
		time.Sleep(time.Millisecond)
	}

	// The restarted writer must keep flushing, not just have been counted.
	deadline = time.Now().Add(5 * time.Second)
	for writes.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the restarted writer stopped writing after %d flush(es)", writes.Load())
		}
		time.Sleep(time.Millisecond)
	}

	stopped := make(chan struct{})
	go func() {
		store.stopFlush()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopFlush did not return after the writer was restarted by the supervisor")
	}
}
