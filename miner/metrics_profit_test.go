package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// Tests for metrics_profit.go: the port of the provider's [profit] line and the
// <state>/billable_rate writer. The rate writer is the machine contract —
// urnet-tools idle-update reads that file — so the tests pin the exact file
// content a given counter state produces, not just that a write happened.

func TestFormatRateMatchesProviderLadder(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0 B/s"},
		{999, "999 B/s"},
		{1000, "1.0 KB/s"},
		{1024, "1.0 KB/s"},
		{1_500_000, "1.5 MB/s"},
		{2_000_000_000, "2.0 GB/s"},
	}
	for _, c := range cases {
		if got := formatRate(c.in); got != c.want {
			t.Errorf("formatRate(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEarningReasonOrder(t *testing.T) {
	cases := []struct {
		name      string
		earning   bool
		proxiesUp int
		clients   int64
		warmup    bool
		want      string
	}{
		{"earning wins over everything", true, 0, 0, true, "-"},
		{"warmup before no_proxies", false, 0, 0, true, "warmup"},
		{"no proxies", false, 0, 0, false, "no_proxies"},
		{"idle with no clients", false, 4, 0, false, "idle"},
		{"clients but no bytes", false, 4, 2, false, "no_traffic"},
	}
	for _, c := range cases {
		if got := earningReason(c.earning, c.proxiesUp, c.clients, c.warmup); got != c.want {
			t.Errorf("%s: earningReason = %q, want %q", c.name, got, c.want)
		}
	}
}

// publishTestProxy registers one live counter set under an identity key.
func publishTestProxy(t *testing.T, key string, rx, tx, clients uint64) *bandwidth.ProxyBandwidth {
	t.Helper()
	bw := &bandwidth.ProxyBandwidth{}
	bw.BillableRx.Store(rx)
	bw.BillableTx.Store(tx)
	bw.Clients.Store(int64(clients))
	publishProxyEarnBandwidth(key, bw)
	return bw
}

// TestReadProfitSampleFromRegistries pins that the collector reads the miner's
// two live registries and classifies them the way the provider classifies its
// health snapshot: only "up" is up, a connecting identity is not up, and the
// byte/client aggregates come from the published counters.
func TestReadProfitSampleFromRegistries(t *testing.T) {
	resetProxyEarnBandwidthForTest()
	resetProxyLiveHealthRegistryForTest()
	t.Cleanup(func() {
		resetProxyEarnBandwidthForTest()
		resetProxyLiveHealthRegistryForTest()
	})

	publishTestProxy(t, "198.51.100.7:443", 100, 50, 2)
	publishTestProxy(t, "198.51.100.8:443", 0, 7, 0)
	noteLiveProxyHealth("198.51.100.7:443", "up", time.Time{})
	noteLiveProxyHealth("198.51.100.8:443", "connecting", time.Time{})

	s := readProfitSample()
	if s.population != 2 {
		t.Errorf("population = %d, want 2", s.population)
	}
	if s.proxiesUp != 1 {
		t.Errorf("proxiesUp = %d, want 1 (a connecting identity is not up)", s.proxiesUp)
	}
	if s.connecting != 1 {
		t.Errorf("connecting = %d, want 1", s.connecting)
	}
	if s.clients != 2 {
		t.Errorf("clients = %d, want 2", s.clients)
	}
	if s.serving != 1 {
		t.Errorf("serving = %d, want 1", s.serving)
	}
	if s.billable != 157 {
		t.Errorf("billable = %d, want 157", s.billable)
	}
}

// TestProfitHeartbeatFromKnownState drives the heartbeat through the earning
// edge, the stop edge, the idle throttle and the warmup reason, asserting the
// exact provider line shape at each step.
func TestProfitHeartbeatFromKnownState(t *testing.T) {
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	h := newProfitHeartbeat(base)

	sample := func(proxiesUp, connecting int, clients int64, serving int, billable uint64) profitSample {
		return profitSample{
			population: 2,
			proxiesUp:  proxiesUp,
			connecting: connecting,
			clients:    clients,
			serving:    serving,
			billable:   billable,
		}
	}

	// First sample only establishes the baseline: no line, like the provider.
	if got := h.tick(base, sample(2, 0, 3, 1, 1000)); got != nil {
		t.Fatalf("baseline tick emitted %v, want no lines", got)
	}

	// 15s later, 15360 billable bytes moved with 3 clients -> 1024 B/s earning.
	got := h.tick(base.Add(15*time.Second), sample(2, 0, 3, 1, 1000+15360))
	want := []string{
		"✈️ [traffic] started (clients=3)\n",
		"💰 [profit] earning=yes reason=- clients=3 rate=1.0 KB/s proxies_up=2 serving=1 idle=1\n",
	}
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Fatalf("earning tick lines:\n got %q\nwant %q", got, want)
	}

	// Traffic stops: the stop edge prints immediately, then the line says idle.
	got = h.tick(base.Add(30*time.Second), sample(2, 0, 0, 0, 1000+15360))
	want = []string{
		"🛬 [traffic] stopped (was=3 clients)\n",
		"[profit] earning=no reason=idle clients=0 rate=0 B/s proxies_up=2 serving=0 idle=2\n",
	}
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Fatalf("stop tick lines:\n got %q\nwant %q", got, want)
	}

	// Still idle 15s later: throttled, so a quiet node stays quiet.
	if got := h.tick(base.Add(45*time.Second), sample(2, 0, 0, 0, 1000+15360)); got != nil {
		t.Fatalf("throttled tick emitted %v, want no lines", got)
	}

	// After the 5m idle interval it prints again, and a pool still dialing is
	// reported as warmup rather than as no-demand.
	got = h.tick(base.Add(30*time.Second+profitIdleLogInterval), sample(1, profitWarmupConnecting, 0, 0, 1000+15360))
	want = []string{
		"[profit] earning=no reason=warmup clients=0 rate=0 B/s proxies_up=1 serving=0 idle=1\n",
	}
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Fatalf("warmup tick lines:\n got %q\nwant %q", got, want)
	}
}

// TestProfitHeartbeatSkipsWithNoRegisteredProxy pins the provider's
// ProxyHealthCount()==0 guard: with nothing registered there is no rate to
// report, so the tick is skipped instead of claiming idle.
func TestProfitHeartbeatSkipsWithNoRegisteredProxy(t *testing.T) {
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	h := newProfitHeartbeat(base)
	empty := profitSample{population: 0, billable: 5000}
	if got := h.tick(base, empty); got != nil {
		t.Fatalf("first empty tick emitted %v", got)
	}
	if got := h.tick(base.Add(15*time.Second), empty); got != nil {
		t.Fatalf("empty tick emitted %v, want no lines", got)
	}
}

// TestBillableRateTrackerFromKnownState pins the arithmetic the file carries:
// per-identity deltas over elapsed seconds, zero on the first sample, and zero
// rather than a wrap when a counter goes backwards.
func TestBillableRateTrackerFromKnownState(t *testing.T) {
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	a := &bandwidth.ProxyBandwidth{}
	b := &bandwidth.ProxyBandwidth{}
	a.BillableRx.Store(1000)
	b.BillableTx.Store(500)
	bw := map[string]*bandwidth.ProxyBandwidth{"a": a, "b": b}

	tr := newBillableRateTracker(base)
	if got := tr.rate(base, bw); got != 0 {
		t.Fatalf("first sample rate = %d, want 0 (no baseline yet)", got)
	}

	a.BillableRx.Store(3000) // +2000
	b.BillableTx.Store(1500) // +1000
	if got := tr.rate(base.Add(10*time.Second), bw); got != 300 {
		t.Fatalf("rate = %d, want 300 (3000 bytes / 10s)", got)
	}

	// Counter reset: a slot restarted and its counter is lower. Contribute
	// zero, never a wrapped enormous delta.
	a.BillableRx.Store(10)
	if got := tr.rate(base.Add(20*time.Second), bw); got != 0 {
		t.Fatalf("rate after counter reset = %d, want 0", got)
	}

	// An identity leaving the snapshot is dropped from the baseline, so its
	// return is not counted as one huge burst.
	delete(bw, "b")
	if got := tr.rate(base.Add(30*time.Second), bw); got != 0 {
		t.Fatalf("rate after identity departure = %d, want 0", got)
	}
}

// TestBillableRateCollectorPublishesRate is the end-to-end collector test: a
// known counter state in the live registry, run through the real tracker and
// the real file writer, must leave the number idle-update expects on disk.
func TestBillableRateCollectorPublishesRate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)

	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	publishTestProxy(t, "198.51.100.7:443", 1000, 0, 1)
	publishTestProxy(t, "198.51.100.8:443", 0, 500, 1)

	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tr := newBillableRateTracker(base)
	tr.rate(base, proxyEarnBandwidthSnapshot()) // baseline sample

	// Advance both proxies by 2000 and 1000 billable bytes respectively.
	publishTestProxy(t, "198.51.100.7:443", 3000, 0, 1)
	publishTestProxy(t, "198.51.100.8:443", 0, 1500, 1)

	rate := tr.rate(base.Add(10*time.Second), proxyEarnBandwidthSnapshot())
	if rate != 300 {
		t.Fatalf("rate = %d, want 300", rate)
	}
	writeBillableRate(rate)

	got, err := os.ReadFile(filepath.Join(dir, "billable_rate"))
	if err != nil {
		t.Fatalf("reading billable_rate: %v", err)
	}
	// The consumer parses a bare decimal with strconv.ParseUint after trimming
	// whitespace; the trailing newline is part of the provider's format.
	if string(got) != "300\n" {
		t.Fatalf("billable_rate = %q, want %q", string(got), "300\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "billable_rate.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind after atomic write: err=%v", err)
	}
}

// TestBillableRateZeroWhenPoolEmpty pins that a node with no live proxy still
// publishes a real 0. This is the difference the consumer depends on: 0 means
// "no traffic, safe to update", absent means "unknown, assume busy".
func TestBillableRateZeroWhenPoolEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)

	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tr := newBillableRateTracker(base)
	if got := tr.rate(base, proxyEarnBandwidthSnapshot()); got != 0 {
		t.Fatalf("empty-pool rate = %d, want 0", got)
	}
	writeBillableRate(0)

	got, err := os.ReadFile(filepath.Join(dir, "billable_rate"))
	if err != nil {
		t.Fatalf("reading billable_rate: %v", err)
	}
	if string(got) != "0\n" {
		t.Fatalf("billable_rate = %q, want %q", string(got), "0\n")
	}
}

// TestBillableRateWriterCreatesHealthDir pins that the writer makes its
// directory: URNETWORK_PROXY_HEALTH_DIR may point somewhere the miner has not
// created, and a metric that silently never appears would put idle-update back
// on the assume-active path.
func TestBillableRateWriterCreatesHealthDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "health")
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)

	writeBillableRate(42)

	got, err := os.ReadFile(filepath.Join(dir, "billable_rate"))
	if err != nil {
		t.Fatalf("reading billable_rate from a directory that did not exist: %v", err)
	}
	if string(got) != "42\n" {
		t.Fatalf("billable_rate = %q, want %q", string(got), "42\n")
	}
	// The state dir holds the JWT and key material; creating it must not make
	// it world-readable.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("health dir mode = %o, want 700", perm)
	}
}
