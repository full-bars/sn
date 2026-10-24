package miner

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
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
	// The per-identity split is what the heartbeat deltas, so pin the entries
	// rather than only their sum.
	if got := s.billableByKey["198.51.100.7:443"]; got != 150 {
		t.Errorf("billable of 198.51.100.7 = %d, want 150 (100 rx + 50 tx)", got)
	}
	if got := s.billableByKey["198.51.100.8:443"]; got != 7 {
		t.Errorf("billable of 198.51.100.8 = %d, want 7 (0 rx + 7 tx)", got)
	}
	if got := s.billableByKey["198.51.100.7:443"] + s.billableByKey["198.51.100.8:443"]; got != 157 {
		t.Errorf("billable = %d, want 157", got)
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
			population:    2,
			proxiesUp:     proxiesUp,
			connecting:    connecting,
			clients:       clients,
			serving:       serving,
			billableByKey: map[string]uint64{"a": billable},
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
	empty := profitSample{population: 0, billableByKey: map[string]uint64{"a": 5000}}
	if got := h.tick(base, empty); got != nil {
		t.Fatalf("first empty tick emitted %v", got)
	}
	if got := h.tick(base.Add(15*time.Second), empty); got != nil {
		t.Fatalf("empty tick emitted %v, want no lines", got)
	}
}

// TestProfitHeartbeatDeltaIsPerIdentity pins the two artefacts a scalar sum
// produces when a proxy leaves and comes back.
//
// The bandwidth registry keeps one counter per identity INDEX for the life of
// the process, so an identity that is unpublished and later republished comes
// back with its byte history intact. Summing first loses that identity's bytes
// on the tick it departs (real traffic on the remaining identities is masked),
// and re-adds the whole history on the tick it returns (a phantom earning=yes
// burst). Deltas per identity have neither artefact: a departure contributes
// nothing, and a return is a fresh baseline.
func TestProfitHeartbeatDeltaIsPerIdentity(t *testing.T) {
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	h := newProfitHeartbeat(base)

	sample := func(billable map[string]uint64) profitSample {
		return profitSample{
			population:    2,
			proxiesUp:     2,
			clients:       1,
			serving:       1,
			billableByKey: billable,
		}
	}

	// Baseline: two identities with history behind them.
	if got := h.tick(base, sample(map[string]uint64{"a": 1000, "b": 500})); got != nil {
		t.Fatalf("baseline tick emitted %v", got)
	}

	// "b" departs while "a" moves 300 bytes. The summed total FALLS
	// (1000+300+500 -> 1300 is below the previous 1500), which a scalar delta
	// reads as zero and reports as no traffic. Per identity the 300 is visible.
	got := strings.Join(h.tick(base.Add(15*time.Second), sample(map[string]uint64{"a": 1300})), "")
	if !strings.Contains(got, "earning=yes") {
		t.Fatalf("a departure masked real traffic on the remaining identity: %q", got)
	}
	if !strings.Contains(got, "rate=20 B/s") {
		t.Fatalf("rate = %q, want the 300 bytes over 15s (20 B/s)", got)
	}

	// "b" returns with its counter intact (the registry reuses the entry). Its
	// 500 bytes were already counted before it left, so the tick is 0 — the
	// scalar sum would report a 500-byte phantom burst as earning=yes.
	got = strings.Join(h.tick(base.Add(30*time.Second), sample(map[string]uint64{"a": 1300, "b": 500})), "")
	if strings.Contains(got, "earning=yes") {
		t.Fatalf("a returning identity produced a phantom earning burst: %q", got)
	}
	if !strings.Contains(got, "rate=0 B/s") {
		t.Fatalf("returning identity was counted as traffic: %q", got)
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
	// The state dir is where the consumer reads; the health dir is set to a
	// DIFFERENT directory on purpose, so a writer that resolves the health dir
	// fails here instead of silently landing where idle-update never looks.
	t.Setenv("URNETWORK_STATE_DIR", dir)
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", filepath.Join(t.TempDir(), "health"))

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
	t.Setenv("URNETWORK_STATE_DIR", dir)

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

// TestBillableRateWriterCreatesStateDir pins that the writer makes its
// directory: URNETWORK_STATE_DIR may point somewhere the miner has not created,
// and a metric that silently never appears would put idle-update back on the
// assume-active path.
func TestBillableRateWriterCreatesStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("URNETWORK_STATE_DIR", dir)

	writeBillableRate(42)

	got, err := os.ReadFile(filepath.Join(dir, "billable_rate"))
	if err != nil {
		t.Fatalf("reading billable_rate from a directory that did not exist: %v", err)
	}
	if string(got) != "42\n" {
		t.Fatalf("billable_rate = %q, want %q", got, "42\n")
	}
	// The state dir holds the JWT and key material; creating it must not make
	// it world-readable.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir mode = %o, want 700", perm)
	}
}

// TestBillableRateLandsWhereIdleUpdateLooks pins the writer/reader contract of
// the two directory knobs. URNETWORK_PROXY_HEALTH_DIR relocates the health
// snapshots and nothing in internal/urnettools reads it; idle-update resolves
// <provider state dir>/billable_rate, where the state dir is URNETWORK_STATE_DIR
// when set and <home>/.urnetwork otherwise. So with the two variables pointing
// at different directories the rate must land in the state one — under the
// health dir it is invisible to the consumer, which then reads "no metric" as
// "active traffic" and waits out its whole 30m timeout on a silent node.
func TestBillableRateLandsWhereIdleUpdateLooks(t *testing.T) {
	stateDir := t.TempDir()
	healthDir := filepath.Join(t.TempDir(), "health")
	t.Setenv("URNETWORK_STATE_DIR", stateDir)
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", healthDir)

	path, ok := billableRatePath()
	if !ok {
		t.Fatal("billableRatePath reported no state dir with URNETWORK_STATE_DIR set")
	}
	// Exactly what idle-update opens: filepath.Join(StateDir, "billable_rate"),
	// with StateDir resolved from the provider's URNETWORK_STATE_DIR.
	want := filepath.Join(stateDir, "billable_rate")
	if path != want {
		t.Fatalf("billableRatePath = %q, want %q — the path idle-update polls", path, want)
	}

	writeBillableRate(4096)

	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("the file idle-update polls was not written: %v", err)
	}
	if string(got) != "4096\n" {
		t.Fatalf("billable_rate = %q, want %q", got, "4096\n")
	}
	if _, err := os.Stat(healthDir); !os.IsNotExist(err) {
		t.Fatalf("the writer touched the health dir %s (err=%v); the rate must not go there", healthDir, err)
	}
}

// TestProfitLineCarriesContractAccounting pins the contracts= / avg_util= group
// on the [profit] line: cumulative acquired and the open book's mean
// utilization, from the same snapshot the scrape publishes, with an undefined
// utilization rendered as n/a rather than a misleading 0%.
func TestProfitLineCarriesContractAccounting(t *testing.T) {
	line := profitLine(true, profitSample{
		population: 1, proxiesUp: 1, serving: 1, clients: 1,
		contractsKnown: true, contractsAcquired: 12, avgUtilPct: 37.5,
	}, 1, 1)
	if !strings.Contains(line, " contracts=12 avg_util=38%") {
		t.Errorf("line = %q, want contracts=12 avg_util=38%%", line)
	}

	undefined := profitLine(false, profitSample{
		contractsKnown: true, contractsAcquired: 3, avgUtilPct: math.NaN(),
	}, 0, 1)
	if !strings.Contains(undefined, "contracts=3 avg_util=n/a") {
		t.Errorf("undefined utilization rendered as a number: %q", undefined)
	}

	// No slot has registered a contract feed: the group is absent, not zero.
	if unknown := profitLine(false, profitSample{}, 0, 1); strings.Contains(unknown, "contracts=") {
		t.Errorf("unsourced contract accounting claimed a reading: %q", unknown)
	}
}

// TestReadProfitSampleCarriesContractAccounting pins the wiring: the sample the
// heartbeat renders picks up contractStatsSnapshot, so the line cannot drift
// from urnet_contracts_total.
func TestReadProfitSampleCarriesContractAccounting(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	device.deliver(t, false, contractOpenDetail(t, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", 1000, 500))

	s := readProfitSample()
	if !s.contractsKnown {
		t.Fatal("readProfitSample did not see a registered contract feed")
	}
	if s.contractsAcquired != 1 {
		t.Errorf("contracts acquired = %d, want 1", s.contractsAcquired)
	}
	if math.Abs(s.avgUtilPct-50) > 1e-9 {
		t.Errorf("avg util = %v, want 50", s.avgUtilPct)
	}
}
