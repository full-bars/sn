package miner

import (
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// Tests for the billable-vs-settled exposition: the two families must carry
// exact values from a known counter state, use the same label conventions as
// the series beside them, and never step down when a slot is reaped.

// TestTrafficMetricsPublishBillableAndSettled pins one known registry state to
// the exact series /metrics serves, including the family registration every
// scraper requires.
func TestTrafficMetricsPublishBillableAndSettled(t *testing.T) {
	withTempHome(t)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)

	registry := bandwidth.NewRegistry()
	one := registry.Register(101)
	one.BillableRx.Store(1_000)
	one.BillableTx.Store(2_000)
	one.ContractUsedIngress.Store(900)
	one.ContractUsedEgress.Store(1_800)
	two := registry.Register(202)
	two.BillableRx.Store(11)
	two.BillableTx.Store(22)
	two.ContractUsedIngress.Store(10)
	two.ContractUsedEgress.Store(20)
	controlRuntime.setBandwidthRegistry(registry)
	controlRuntime.setRunningProxies(nil)

	var b strings.Builder
	writeTrafficMetrics(&b)
	out := b.String()

	for _, want := range []string{
		"# HELP urnet_billable_bytes_total ",
		"# TYPE urnet_billable_bytes_total counter\n",
		"urnet_billable_bytes_total{direction=\"in\"} 1011\n",
		"urnet_billable_bytes_total{direction=\"out\"} 2022\n",
		// The cross-check family: same direction values as the billable series
		// beside it (in/out, not ingress/egress), a counter by suffix and TYPE,
		// and its own HELP/TYPE so a scraper registers it on its own.
		"# HELP urnet_contract_used_bytes_total ",
		"# TYPE urnet_contract_used_bytes_total counter\n",
		"urnet_contract_used_bytes_total{direction=\"in\"} 910\n",
		"urnet_contract_used_bytes_total{direction=\"out\"} 1820\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// The whole block must still parse: a family split by an unrelated series,
	// a duplicate series or a bad label would fail the scrape as a whole.
	for _, err := range Lint(out) {
		t.Errorf("traffic block does not lint: %v", err)
	}
}

// TestTrafficMetricsKeepSettledBytesWhenASlotIsReaped pins the retention rule.
// A reaped slot loses its per-proxy series — it is no longer a running key —
// but the bytes it settled must stay in the aggregate, because a counter that
// steps down is read by Prometheus as a reset and reports most of the node's
// history as new traffic.
func TestTrafficMetricsKeepSettledBytesWhenASlotIsReaped(t *testing.T) {
	withTempHome(t)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)

	registry := bandwidth.NewRegistry()
	keep := registry.Register(101)
	keep.ContractUsedIngress.Store(500)
	keep.ContractUsedEgress.Store(600)
	gone := registry.Register(202)
	gone.ContractUsedIngress.Store(70)
	gone.ContractUsedEgress.Store(80)
	controlRuntime.setBandwidthRegistry(registry)
	controlRuntime.setRunningProxies([]string{"kept:1080", "reaped:1080"})

	before := controlRuntime.trafficReading()
	if !before.OK {
		t.Fatal("traffic reading not available with a registry installed")
	}
	if before.ContractUsedIngress != 570 || before.ContractUsedEgress != 680 {
		t.Fatalf("settled totals = %d/%d, want 570/680",
			before.ContractUsedIngress, before.ContractUsedEgress)
	}

	// The slot is reaped: it leaves the running set and its counters are no
	// longer reachable from the fleet's identity view. The registry entry —
	// the thing the aggregate sums — stays, which is what keeps the total
	// monotonic. Nothing in the miner unregisters an index.
	controlRuntime.setRunningProxies([]string{"kept:1080"})

	after := controlRuntime.trafficReading()
	if after.ContractUsedIngress != before.ContractUsedIngress ||
		after.ContractUsedEgress != before.ContractUsedEgress {
		t.Fatalf("a reaped slot reduced the cumulative total: %d/%d -> %d/%d",
			before.ContractUsedIngress, before.ContractUsedEgress,
			after.ContractUsedIngress, after.ContractUsedEgress)
	}
}

// TestMetricsStoreRetiresSettledBytesWithBillable pins the other half of the
// retention rule: when the store prunes an index, its contract bytes move into
// the retired accumulator with its billable bytes. Dropping them would make
// urnet_contract_used_bytes_total fall on every prune, and — worse for this
// cross-check — would reopen a drift against a billable total that kept them.
func TestMetricsStoreRetiresSettledBytesWithBillable(t *testing.T) {
	h := newTestMetricsCounterStore(t)

	old := h.now.Add(-60 * 24 * time.Hour).Unix()
	h.store.setBase(map[int]minerMetricsProxyCounters{
		1: {
			BillableRx:          900,
			BillableTx:          400,
			ContractUsedIngress: 800,
			ContractUsedEgress:  350,
			LastSeen:            old,
		},
	})
	h.snap.Proxies[2] = minerMetricsProxyCounters{BillableRx: 10, BillableTx: 5, ContractUsedIngress: 10, ContractUsedEgress: 5}

	if err := h.store.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	live := map[int]struct{}{2: {}}
	retired := h.store.retiredTotals(live)
	if retired.ContractUsedIngress != 800 || retired.ContractUsedEgress != 350 {
		t.Fatalf("pruned slot's settled bytes left the aggregate: %d/%d, want 800/350",
			retired.ContractUsedIngress, retired.ContractUsedEgress)
	}

	// A restart must not lose them either, or the two families would drift by
	// the whole pre-restart billable total.
	reloaded := newMetricsCounterStore(h.path)
	reloaded.now = h.store.now
	if err := reloaded.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := reloaded.retiredTotals(live); got.ContractUsedIngress != 800 || got.ContractUsedEgress != 350 {
		t.Fatalf("settled bytes not persisted: %d/%d, want 800/350",
			got.ContractUsedIngress, got.ContractUsedEgress)
	}
}

// TestProfitLineCarriesBillableAndSettled pins that a human reading the log
// sees the same comparison the scrape carries, and that a run with no counters
// says nothing rather than claiming zero bytes.
func TestProfitLineCarriesBillableAndSettled(t *testing.T) {
	withTempHome(t)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)

	registry := bandwidth.NewRegistry()
	one := registry.Register(101)
	one.BillableRx.Store(250)
	one.BillableTx.Store(150)
	one.ContractUsedIngress.Store(260)
	one.ContractUsedEgress.Store(240)
	controlRuntime.setBandwidthRegistry(registry)

	sample := readProfitSample()
	if !sample.totalsKnown {
		t.Fatal("totals not read from an installed registry")
	}
	if sample.billableTotal != 400 {
		t.Errorf("billableTotal = %d, want 400", sample.billableTotal)
	}
	if sample.settledTotal != 500 {
		t.Errorf("settledTotal = %d, want 500", sample.settledTotal)
	}
	line := profitLine(true, sample, 1_024, 1)
	if !strings.Contains(line, " billable=400 B settled=500 B") {
		t.Fatalf("profit line lost the cross-check: %q", line)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("profit line lost its newline: %q", line)
	}

	// No registry: the fields are omitted, so a missing pair means "not
	// counting yet" and never "zero bytes moved".
	offline := profitLine(false, profitSample{population: 1, proxiesUp: 1}, 0, 1)
	if strings.Contains(offline, "billable=") || strings.Contains(offline, "settled=") {
		t.Fatalf("profit line invented totals with no counters: %q", offline)
	}
}
