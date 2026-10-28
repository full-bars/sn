package miner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// Tests for the fork-only health registry that replaces the P1(a) no-op
// seams: the live health the URL reaper reads, the trim overlay, the report
// builder and the health file writer.

// TestLiveProxyHealthSeam_ReflectsAuthOutcomes pins the wiring: a proxy that
// authenticated is "up" (and therefore shielded from reaper demotion), and one
// that failed authentication is not.
func TestLiveProxyHealthSeam_ReflectsAuthOutcomes(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()

	settings := &connect.ProxySettings{Network: "tcp", Address: "203.0.113.5:1080"}
	key := settings.Key()

	noteProviderAuthSuccess(settings)
	live := liveProxyHealthByKey()
	h, ok := live[key]
	if !ok || h.Health != "up" {
		t.Fatalf("a successful auth must register the proxy as up, got %+v", live)
	}
	// The reaper's live shield is "up" (see the isLive check in
	// runURLProxyReaperOnce), while proxyHeldOrConnecting holds a proxy that is
	// still coming up.
	if !proxyHeldOrConnecting(ProxyURLEntry{}, key, map[string]proxyHealthStatus{key: {Health: "connecting"}}) {
		t.Fatal("a connecting proxy must be held for the reaper")
	}

	noteProviderAuthFailure(settings, errors.New("auth failed"))
	live = liveProxyHealthByKey()
	if h := live[key]; h.Health == "up" {
		t.Fatalf("a failed auth must clear the up state, got %+v", h)
	}
	if proxyHeldOrConnecting(ProxyURLEntry{}, key, live) {
		t.Fatal("a proxy that just failed auth must not be held for the reaper")
	}
	noteLiveProxyHealthFailure(key)
	if h := liveProxyHealthByKey()[key]; h.Health != "recently_offline" {
		t.Fatalf("a failed auth must read as recently_offline, got %+v", h)
	}
}

// TestTrimSeesLiveHealth pins that the live registry reaches the trim ranking:
// an entry whose persisted health is unknown is shed first once the registry
// says it is dead.
func TestTrimSeesLiveHealth(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()

	proxies := map[string]ProxyEntry{
		"203.0.113.1:1080": {ID: 1},
		"203.0.113.2:1080": {ID: 2, Health: "up"},
	}
	noteLiveProxyHealth("203.0.113.1:1080", "dead", time.Now().Add(-time.Hour))

	if overlaid := overlayLiveProxyHealth(proxies); overlaid != 1 {
		t.Fatalf("expected exactly the unknown-health entry to be overlaid, got %d", overlaid)
	}
	if got := proxies["203.0.113.1:1080"].Health; got != "dead" {
		t.Fatalf("live dead health not overlaid, got %q", got)
	}

	gradeFor := func(string) (float64, bool) { return 0, false }
	shed := selectWorstRunningProxies(proxies, gradeFor, nil, []string{"203.0.113.1:1080", "203.0.113.2:1080"}, 1)
	if len(shed) != 1 || shed[0] != "203.0.113.1:1080" {
		t.Fatalf("the live-dead proxy must be shed first, got %v", shed)
	}
}

// TestBuildProxyHealthReport_Transitions pins the report contents and the
// transition lists: an up proxy that goes degraded appears once in
// NewlyDegraded, and coming back appears in Recovered.
func TestBuildProxyHealthReport_Transitions(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	now := time.Date(2026, 6, 2, 16, 5, 11, 0, time.UTC)
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"203.0.113.1:1080": {ID: 1},
		"203.0.113.2:1080": {ID: 2},
	}}
	noteLiveProxyHealthUp("203.0.113.1:1080")
	noteLiveProxyHealth("203.0.113.2:1080", "dead", time.Time{})

	report := buildProxyHealthReport(state, now)
	if report.Up != 1 {
		t.Fatalf("up count = %d, want 1", report.Up)
	}
	if len(report.Dead) != 1 || !strings.Contains(report.Dead[0], "proxy[2]") {
		t.Fatalf("dead list = %v, want proxy[2] only", report.Dead)
	}
	if len(report.Bandwidth) != 0 {
		t.Fatalf("bandwidth must be empty until a counter source is wired, got %d", len(report.Bandwidth))
	}

	// proxy[1] goes down: one degraded transition, and the dead one stays dead.
	noteLiveProxyHealth("203.0.113.1:1080", "offline", now.Add(-time.Minute))
	report = buildProxyHealthReport(state, now)
	if len(report.NewlyDegraded) != 1 || report.NewlyDegraded[0].Index != 1 {
		t.Fatalf("expected one NewlyDegraded transition for proxy[1], got %+v", report.NewlyDegraded)
	}
	if len(report.NewlyDead) != 0 {
		t.Fatalf("no proxy is newly dead, got %+v", report.NewlyDead)
	}

	// proxy[1] comes back up: one recovery.
	noteLiveProxyHealthUp("203.0.113.1:1080")
	report = buildProxyHealthReport(state, now)
	if len(report.Recovered) != 1 || report.Recovered[0].Index != 1 {
		t.Fatalf("expected one Recovered transition for proxy[1], got %+v", report.Recovered)
	}
	if report.LifetimeRecovered == 0 || report.LifetimeLost == 0 {
		t.Fatalf("lifetime counters must accumulate, got recovered=%d lost=%d", report.LifetimeRecovered, report.LifetimeLost)
	}
}

// TestWriteProxyHealthSnapshot_WritesFilesAndState pins the cadence body: the
// v33 health files appear, and the overlaid health lands in proxy.state so the
// subcommands and a restart both see it.
func TestWriteProxyHealthSnapshot_WritesFilesAndState(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"203.0.113.7:1080": {ID: 7},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	noteLiveProxyHealthUp("203.0.113.7:1080")

	now := time.Date(2026, 6, 2, 16, 5, 11, 0, time.UTC)
	if !writeProxyHealthSnapshot(now) {
		t.Fatal("health snapshot must be written when the state directory resolves")
	}

	dir, ok := proxyHealthDir()
	if !ok {
		t.Fatal("health dir must resolve")
	}
	healthState, err := os.ReadFile(filepath.Join(dir, "proxy_health.state"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(healthState), "Up: 1") {
		t.Fatalf("health state must report the up proxy:\n%s", healthState)
	}
	if !strings.Contains(string(healthState), "Updated: 2026-06-02T16:05:11Z") {
		t.Fatalf("health state must carry the snapshot time:\n%s", healthState)
	}
	if _, err := os.Stat(filepath.Join(dir, "proxy_traffic.state")); err != nil {
		t.Fatalf("traffic state missing: %v", err)
	}

	persisted, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Proxies["203.0.113.7:1080"].Health; got != "up" {
		t.Fatalf("live health must be persisted into proxy.state, got %q", got)
	}
}

func TestProxyHealthAndManagerStateWritersShareTransaction(t *testing.T) {
	withTempHome(t)
	const key = "198.51.100.37:1080"
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{key: {ID: 7}}}); err != nil {
		t.Fatal(err)
	}
	noteLiveProxyHealthUp(key)
	healthRead := make(chan struct{})
	allowHealthWrite := make(chan struct{})
	proxyHealthBeforeStatePersist = func() {
		close(healthRead)
		<-allowHealthWrite
	}
	t.Cleanup(func() { proxyHealthBeforeStatePersist = nil })

	healthDone := make(chan struct{})
	go func() {
		writeProxyHealthSnapshot(time.Now())
		close(healthDone)
	}()
	<-healthRead

	writerAttempted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		close(writerAttempted)
		proxyStateMu.Lock()
		defer proxyStateMu.Unlock()
		state, err := readProxyState()
		if err != nil {
			writerDone <- err
			return
		}
		state.Source = "/tmp/operator-proxies.txt"
		writerDone <- writeProxyState(state)
	}()
	<-writerAttempted
	close(allowHealthWrite)
	<-healthDone
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}

	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Proxies[key]
	if state.Source != "/tmp/operator-proxies.txt" || entry.Health != "up" {
		t.Fatalf("concurrent state writers lost an update: source=%q health=%q", state.Source, entry.Health)
	}
}

// TestBuildProxyHealthReport_FillsTrafficFromLiveBandwidth pins the traffic
// table's source: the report carries the counters the provide loop published
// under the slot's identity key, so proxy_traffic.state names real bytes
// instead of rendering an empty table.
func TestBuildProxyHealthReport_FillsTrafficFromLiveBandwidth(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	settings := &connect.ProxySettings{Address: "203.0.113.9:1080"}
	key := settings.Key()
	bw := &bandwidth.ProxyBandwidth{}
	bw.TotalRx.Store(11_000)
	bw.TotalTx.Store(22_000)
	bw.BillableRx.Store(3_300)
	bw.BillableTx.Store(4_400)
	bw.Clients.Store(7)
	bw.AddSession("session-1", time.Now().Add(-90*time.Second))
	publishProxyEarnBandwidth(key, bw)

	// A credentialed identity reaches the same table under its address (v33
	// names the address, not the account, in the IP ADDRESS column).
	alice := &connect.ProxySettings{Address: "203.0.113.10:1080", Auth: &proxy.Auth{User: "alice"}}
	aliceBw := &bandwidth.ProxyBandwidth{}
	aliceBw.BillableTx.Store(5_500)
	publishProxyEarnBandwidth(alice.Key(), aliceBw)

	now := time.Now()
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		key:         {ID: 9, Health: "up"},
		alice.Key(): {ID: 10, Health: "up"},
	}}
	report := buildProxyHealthReport(state, now)

	row := report.Bandwidth["proxy[9] (203.0.113.9:1080)"]
	if row == nil {
		t.Fatalf("traffic report has no row for the live slot; keys = %v", report.Bandwidth)
	}
	if got := row.TotalRx.Load(); got != 11_000 {
		t.Errorf("total rx = %d, want 11000", got)
	}
	if got := row.TotalTx.Load(); got != 22_000 {
		t.Errorf("total tx = %d, want 22000", got)
	}
	if got := row.BillableRx.Load(); got != 3_300 {
		t.Errorf("billable rx = %d, want 3300", got)
	}
	if got := row.BillableTx.Load(); got != 4_400 {
		t.Errorf("billable tx = %d, want 4400", got)
	}
	if got := row.Clients.Load(); got != 7 {
		t.Errorf("clients = %d, want 7", got)
	}
	if age := row.MaxAge(); age < 90*time.Second || 2*time.Minute < age {
		t.Errorf("max age = %s, want the live 90s presence window", age)
	}

	aliceRow := report.Bandwidth["proxy[10] (203.0.113.10:1080)"]
	if aliceRow == nil {
		t.Fatalf("a credentialed live slot has no address-named row; keys = %v", report.Bandwidth)
	}
	if got := aliceRow.BillableTx.Load(); got != 5_500 {
		t.Errorf("credentialed billable tx = %d, want 5500", got)
	}

	rendered := formatTrafficStateFile(report, now)
	if !strings.Contains(rendered, "proxy[9]") || !strings.Contains(rendered, "203.0.113.9:1080") {
		t.Fatalf("traffic state file does not name the live proxy:\n%s", rendered)
	}
}

// Two credentialed identities can serve from the same gateway address, and the
// traffic row key is proxy[<entry id>] (<address>): the id is what keeps them
// apart. Keying the row by address alone would collapse the two rows into one,
// and looking the counters up by address alone would hand one account's bytes
// to the other. This pins both halves for the shared-gateway case.
func TestBuildProxyHealthReport_SharedGatewayCredentialedRowsStayDistinct(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	gateway := "198.51.100.7:1080"
	alice := &connect.ProxySettings{Address: gateway, Auth: &proxy.Auth{User: "alice"}}
	bob := &connect.ProxySettings{Address: gateway, Auth: &proxy.Auth{User: "bob"}}
	if alice.Key() == bob.Key() {
		t.Fatalf("two accounts behind one gateway must not share an identity key: %q", alice.Key())
	}

	aliceBw := &bandwidth.ProxyBandwidth{}
	aliceBw.TotalRx.Store(1_000)
	aliceBw.TotalTx.Store(2_000)
	aliceBw.BillableRx.Store(300)
	aliceBw.BillableTx.Store(400)
	publishProxyEarnBandwidth(alice.Key(), aliceBw)

	bobBw := &bandwidth.ProxyBandwidth{}
	bobBw.TotalRx.Store(10_000)
	bobBw.TotalTx.Store(20_000)
	bobBw.BillableRx.Store(3_000)
	bobBw.BillableTx.Store(4_000)
	bobBw.Clients.Store(2)
	publishProxyEarnBandwidth(bob.Key(), bobBw)

	now := time.Now()
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		alice.Key(): {ID: 10, Health: "up"},
		bob.Key():   {ID: 11, Health: "up"},
	}}
	report := buildProxyHealthReport(state, now)

	if len(report.Bandwidth) != 2 {
		t.Fatalf("two identities behind one gateway must make two rows, got %d: %v", len(report.Bandwidth), report.Bandwidth)
	}
	aliceRow := report.Bandwidth["proxy[10] (198.51.100.7:1080)"]
	bobRow := report.Bandwidth["proxy[11] (198.51.100.7:1080)"]
	if aliceRow == nil || bobRow == nil {
		t.Fatalf("row keys must carry the entry id and the shared address; keys = %v", report.Bandwidth)
	}
	if aliceRow == bobRow {
		t.Fatal("the two identities were handed the same row object")
	}

	for _, check := range []struct {
		name                                     string
		row                                      *proxyBandwidth
		totalRx, totalTx, billableRx, billableTx uint64
		clients                                  int64
	}{
		{"alice", aliceRow, 1_000, 2_000, 300, 400, 0},
		{"bob", bobRow, 10_000, 20_000, 3_000, 4_000, 2},
	} {
		if got := check.row.TotalRx.Load(); got != check.totalRx {
			t.Errorf("%s total rx = %d, want %d", check.name, got, check.totalRx)
		}
		if got := check.row.TotalTx.Load(); got != check.totalTx {
			t.Errorf("%s total tx = %d, want %d", check.name, got, check.totalTx)
		}
		if got := check.row.BillableRx.Load(); got != check.billableRx {
			t.Errorf("%s billable rx = %d, want %d", check.name, got, check.billableRx)
		}
		if got := check.row.BillableTx.Load(); got != check.billableTx {
			t.Errorf("%s billable tx = %d, want %d", check.name, got, check.billableTx)
		}
		if got := check.row.Clients.Load(); got != check.clients {
			t.Errorf("%s clients = %d, want %d", check.name, got, check.clients)
		}
	}

	// The rendered table must carry both rows, not one merged gateway row.
	rendered := formatTrafficStateFile(report, now)
	if got := strings.Count(rendered, gateway); got != 2 {
		t.Fatalf("traffic state file must show the shared gateway twice (one row per account), got %d:\n%s", got, rendered)
	}
}

// TestRunProxyHealthLog_WritesOnStart pins that the cadence loop writes the
// files immediately (a fresh process must have them before the first tick) and
// returns on context cancellation.
func TestRunProxyHealthLog_WritesOnStart(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	state := &ProxyState{Proxies: map[string]ProxyEntry{"203.0.113.8:1080": {ID: 8}}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	noteLiveProxyHealthUp("203.0.113.8:1080")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyHealthLog(ctx, time.Hour)
	}()
	dir, _ := proxyHealthDir()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "proxy_health.state")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("proxy_health.state was not written by the cadence loop start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
