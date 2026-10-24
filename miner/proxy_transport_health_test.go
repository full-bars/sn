package miner

// Tests for the transport half of proxy health (proxy_transport_health.go):
// the identity is up only while its platform transport has routes, a drop marks
// it down and starts its down clock, and none of that is confused with the auth
// path or the proxy-leg counter.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// fakeProviderTransport stands in for *sdk.DeviceLocal's provider connect read.
type fakeProviderTransport struct {
	mu        sync.Mutex
	connected bool
}

func (f *fakeProviderTransport) GetProviderConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeProviderTransport) set(connected bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = connected
}

func liveHealthEntryForTest(key string) proxyLiveHealthEntry {
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	return proxyLiveHealthRegistry.byKey[key]
}

func shortenProxyTransportDropConfirm(t *testing.T, d time.Duration) {
	t.Helper()
	prev := proxyTransportDropConfirm
	proxyTransportDropConfirm = d
	t.Cleanup(func() { proxyTransportDropConfirm = prev })
}

// A proxy is never up before its transport connects, is up once it has routes,
// and is down (with the down clock started) the moment that connection drops.
func TestProxyTransportHealth_UpOnConnectDownOnDrop(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	shortenProxyTransportDropConfirm(t, 0)

	const key = "203.0.113.10:1080"
	transport := &fakeProviderTransport{}
	stop := watchProxyTransportHealth(context.Background(), key, transport, time.Millisecond)
	t.Cleanup(stop)

	// Object construction is not a connection: nothing has been observed.
	time.Sleep(20 * time.Millisecond)
	if entry := liveHealthEntryForTest(key); entry.Health != "" {
		t.Fatalf("identity marked %q before its transport connected", entry.Health)
	}

	transport.set(true)
	waitFor(t, 2*time.Second, "identity up after connect", func() bool {
		return liveHealthEntryForTest(key).Health == "up"
	})

	transport.set(false)
	waitFor(t, 2*time.Second, "identity down after drop", func() bool {
		return liveHealthEntryForTest(key).Health != "up"
	})
	entry := liveHealthEntryForTest(key)
	if entry.Health != "recently_offline" {
		t.Fatalf("a dropped transport must read as recently_offline, got %q", entry.Health)
	}
	if entry.DownSince.IsZero() {
		t.Fatal("a dropped transport must start the identity's down clock")
	}
	if entry.TransportDrops != 1 {
		t.Fatalf("transport drops = %d, want 1", entry.TransportDrops)
	}
	if entry.AuthFailures != 0 {
		t.Fatalf("a transport drop was counted as an auth failure: %d", entry.AuthFailures)
	}
}

// The two failure kinds stay separate: an auth failure keeps counting as one
// after the transport comes up and after it drops, and the drop does not touch
// the auth count or the timeout-family label the quality reaper reads.
func TestProxyTransportHealth_DropIsNotAnAuthFailure(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	shortenProxyTransportDropConfirm(t, 0)

	settings := &connect.ProxySettings{Network: "tcp", Address: "203.0.113.11:1080"}
	key := settings.Key()

	// A deadline-cut attempt: an auth failure that is also timeout-family.
	noteProviderAuthFailure(settings, context.DeadlineExceeded)
	entry := liveHealthEntryForTest(key)
	if entry.AuthFailures != 1 {
		t.Fatalf("auth failures = %d, want 1", entry.AuthFailures)
	}
	if !entry.LastFailureTimeout {
		t.Fatal("a deadline-cut auth attempt must be recorded as timeout-family")
	}

	transport := &fakeProviderTransport{}
	stop := watchProxyTransportHealth(context.Background(), key, transport, time.Millisecond)
	t.Cleanup(stop)

	transport.set(true)
	waitFor(t, 2*time.Second, "identity up after connect", func() bool {
		return liveHealthEntryForTest(key).Health == "up"
	})
	if entry := liveHealthEntryForTest(key); entry.AuthFailures != 1 {
		t.Fatalf("a transport connect cleared the auth failure count: %d", entry.AuthFailures)
	}

	transport.set(false)
	waitFor(t, 2*time.Second, "identity down after drop", func() bool {
		return liveHealthEntryForTest(key).TransportDrops == 1
	})
	entry = liveHealthEntryForTest(key)
	if entry.Health == "up" {
		t.Fatal("a dropped transport left the identity up")
	}
	if entry.DownSince.IsZero() {
		t.Fatal("a dropped transport must start the identity's down clock")
	}
	if entry.AuthFailures != 1 {
		t.Fatalf("the auth failure count changed on a transport drop: %d, want 1", entry.AuthFailures)
	}
	if entry.LastFailureTimeout {
		t.Fatal("a transport drop is not a timeout-family failure; the reaper would skip it as merely slow")
	}
}

// A slot leaving (reload, reaper, shutdown) closes its device, which reads as
// disconnected. That is teardown, not a drop.
func TestProxyTransportHealth_TeardownIsNotADrop(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	shortenProxyTransportDropConfirm(t, 0)

	const key = "203.0.113.12:1080"
	ctx, cancel := context.WithCancel(context.Background())
	transport := &fakeProviderTransport{}
	stop := watchProxyTransportHealth(ctx, key, transport, time.Millisecond)

	transport.set(true)
	waitFor(t, 2*time.Second, "identity up after connect", func() bool {
		return liveHealthEntryForTest(key).Health == "up"
	})

	cancel()
	stop()
	transport.set(false)
	time.Sleep(30 * time.Millisecond)

	entry := liveHealthEntryForTest(key)
	if entry.TransportDrops != 0 {
		t.Fatalf("teardown was counted as a transport drop: %d", entry.TransportDrops)
	}
}

// A transport that never connects is an auth or reachability problem, which the
// auth path already records. It is not a drop.
func TestProxyTransportHealth_NeverConnectedIsNotADrop(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	shortenProxyTransportDropConfirm(t, 0)

	const key = "203.0.113.13:1080"
	transport := &fakeProviderTransport{}
	stop := watchProxyTransportHealth(context.Background(), key, transport, time.Millisecond)
	t.Cleanup(stop)

	time.Sleep(30 * time.Millisecond)
	entry := liveHealthEntryForTest(key)
	if entry.Health != "" || entry.TransportDrops != 0 || !entry.DownSince.IsZero() {
		t.Fatalf("a transport that never connected produced health %+v", entry)
	}
}

// A single disconnected read is not a drop: a transport migration installs a
// fresh generation that reads disconnected for a moment. The drop counts only
// once the disconnected reading has held for the confirm window.
func TestProxyTransportHealth_ConfirmsSustainedDrop(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	shortenProxyTransportDropConfirm(t, 150*time.Millisecond)

	const key = "203.0.113.14:1080"
	transport := &fakeProviderTransport{}
	stop := watchProxyTransportHealth(context.Background(), key, transport, 5*time.Millisecond)
	t.Cleanup(stop)

	transport.set(true)
	waitFor(t, 2*time.Second, "identity up after connect", func() bool {
		return liveHealthEntryForTest(key).Health == "up"
	})

	// A blip shorter than the confirm window: no drop, and the identity
	// recovers without ever being marked down.
	transport.set(false)
	time.Sleep(50 * time.Millisecond)
	transport.set(true)
	time.Sleep(50 * time.Millisecond)
	if entry := liveHealthEntryForTest(key); entry.TransportDrops != 0 || entry.Health != "up" {
		t.Fatalf("a transient disconnected read counted as a drop: %+v", entry)
	}

	// A disconnect that stays down does count.
	transport.set(false)
	waitFor(t, 2*time.Second, "drop confirmed", func() bool {
		return liveHealthEntryForTest(key).TransportDrops == 1
	})
	if entry := liveHealthEntryForTest(key); entry.Health == "up" {
		t.Fatal("a confirmed drop left the identity up")
	}
}

// The drop count reaches the state file the health report and the metrics read,
// and a persisted count is restored on the next start.
func TestProxyTransportHealth_DropsPersistThroughState(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)

	const key = "203.0.113.15:1080"
	noteLiveProxyHealthDrop(key)
	noteLiveProxyHealthDrop(key)

	proxies := map[string]ProxyEntry{key: {ID: 1}}
	if overlaid := overlayLiveProxyHealth(proxies); overlaid != 1 {
		t.Fatalf("expected the drop entry to be overlaid, got %d", overlaid)
	}
	entry := proxies[key]
	if entry.TransportDrops != 2 {
		t.Fatalf("persisted transport drops = %d, want 2", entry.TransportDrops)
	}
	if entry.Health == "" || entry.DownSince == "" {
		t.Fatalf("persisted health is incomplete: %+v", entry)
	}

	resetProxyHealthReportStateForTest()
	if seeded := seedLiveProxyHealthFromState(&ProxyState{Proxies: proxies}); seeded != 1 {
		t.Fatalf("seeded %d entries, want 1", seeded)
	}
	if got := liveHealthEntryForTest(key).TransportDrops; got != 2 {
		t.Fatalf("seeded transport drops = %d, want 2", got)
	}
}

// writeProxyTransportDropMetric is the operator-visible half: the drop count is
// its own series, not folded into the auth-failure or proxy-leg gauges.
func TestProxyTransportDropMetric_IsItsOwnSeries(t *testing.T) {
	var b strings.Builder
	writeProxyTransportDropMetric(&b, 3)
	out := b.String()
	for _, want := range []string{
		"# TYPE urnet_proxy_transport_drops gauge",
		"urnet_proxy_transport_drops 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metric output missing %q:\n%s", want, out)
		}
	}
}

// A last-resort guard: the watcher must never count the auth path's error as a
// drop. (The counter is a different field; this pins that the drop path only
// moves TransportDrops.)
func TestProxyTransportHealth_DropCounterIsIndependent(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)

	const key = "203.0.113.16:1080"
	noteLiveProxyHealthFailureCause(key, errors.New("auth rejected"))
	noteLiveProxyLegFailure(key)
	before := liveHealthEntryForTest(key)

	noteLiveProxyHealthDrop(key)
	after := liveHealthEntryForTest(key)

	if after.TransportDrops != before.TransportDrops+1 {
		t.Fatalf("transport drops = %d, want %d", after.TransportDrops, before.TransportDrops+1)
	}
	if after.AuthFailures != before.AuthFailures {
		t.Fatalf("a transport drop moved the auth failure count: %d -> %d", before.AuthFailures, after.AuthFailures)
	}
	if after.ProxyLegFailures != before.ProxyLegFailures {
		t.Fatalf("a transport drop moved the proxy-leg count: %d -> %d", before.ProxyLegFailures, after.ProxyLegFailures)
	}
}

// The drop is evidence, and recovery must not erase it: the carrier returning
// clears the down clock but leaves the count, and the next loss is a second
// drop. This is the half of the migration boundary the seam can see -- a
// reconnect -- so it is what pins that a real loss still counts.
func TestProxyTransportHealth_ReconnectAfterDropKeepsTheEvidence(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	shortenProxyTransportDropConfirm(t, 0)

	const key = "203.0.113.17:1080"
	transport := &fakeProviderTransport{}
	stop := watchProxyTransportHealth(context.Background(), key, transport, time.Millisecond)
	t.Cleanup(stop)

	transport.set(true)
	waitFor(t, 2*time.Second, "identity up after connect", func() bool {
		return liveHealthEntryForTest(key).Health == "up"
	})

	transport.set(false)
	waitFor(t, 2*time.Second, "first drop confirmed", func() bool {
		return liveHealthEntryForTest(key).TransportDrops == 1
	})
	if entry := liveHealthEntryForTest(key); entry.Health != "recently_offline" || entry.DownSince.IsZero() {
		t.Fatalf("a drop must be recently_offline with a down clock, got %+v", entry)
	}

	transport.set(true)
	waitFor(t, 2*time.Second, "identity up after reconnect", func() bool {
		entry := liveHealthEntryForTest(key)
		return entry.Health == "up" && entry.DownSince.IsZero()
	})
	if entry := liveHealthEntryForTest(key); entry.TransportDrops != 1 {
		t.Fatalf("the reconnect erased the drop it proves: %d", entry.TransportDrops)
	}

	transport.set(false)
	waitFor(t, 2*time.Second, "second drop confirmed", func() bool {
		return liveHealthEntryForTest(key).TransportDrops == 2
	})
}

// A confirmed drop is one event: a carrier that stays down is not a new loss on
// every sample, or a dead relay's count would grow forever and the metric would
// mean nothing.
func TestProxyTransportHealth_ConfirmedDropIsCountedOnce(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	shortenProxyTransportDropConfirm(t, 0)

	const key = "203.0.113.18:1080"
	transport := &fakeProviderTransport{}
	stop := watchProxyTransportHealth(context.Background(), key, transport, time.Millisecond)
	t.Cleanup(stop)

	transport.set(true)
	waitFor(t, 2*time.Second, "identity up after connect", func() bool {
		return liveHealthEntryForTest(key).Health == "up"
	})

	transport.set(false)
	waitFor(t, 2*time.Second, "drop confirmed", func() bool {
		return liveHealthEntryForTest(key).TransportDrops == 1
	})

	// Many ticks with the transport still down: still one drop.
	time.Sleep(30 * time.Millisecond)
	if entry := liveHealthEntryForTest(key); entry.TransportDrops != 1 {
		t.Fatalf("a continuously down carrier counted %d drops, want 1", entry.TransportDrops)
	}
}
