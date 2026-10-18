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

// TestRunProxyHealthLog_WritesOnStart pins that the cadence loop writes the
// files immediately (a fresh process must have them before the first tick) and
// returns on context cancellation.
func TestRunProxyHealthLog_WritesOnStart(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()

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
