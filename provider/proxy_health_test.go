package provider

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// resetProxyHealthForTest clears global registry state between tests.
func resetProxyHealthForTest() {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	proxyHealthByIndex = map[int]*proxyHealth{}
	proxyHealthByKey = map[string]*proxyHealth{}
	proxyLifetimeRecovered = 0
	proxyLifetimeLost = 0
	proxyBaselineSet = false
}

func TestProxyHealthRegisterAndMark(t *testing.T) {
	resetProxyHealthForTest()

	RegisterProxy(0, "1.1.1.1:1081", "1.1.1.1:1081")
	RegisterProxy(1, "2.2.2.2:1081", "2.2.2.2:1081")
	if got := ProxyHealthCount(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}

	// idempotent: re-register keeps the entry
	RegisterProxy(0, "1.1.1.1:1081", "1.1.1.1:1081")
	if got := ProxyHealthCount(); got != 2 {
		t.Fatalf("count after re-register = %d, want 2", got)
	}

	markProxyUp(0)
	proxyHealthMu.Lock()
	up := proxyHealthByIndex[0].currentlyUp
	ever := proxyHealthByIndex[0].everUp
	proxyHealthMu.Unlock()
	if !up || !ever {
		t.Fatalf("after markProxyUp: currentlyUp=%v everUp=%v, want true,true", up, ever)
	}

	markProxyDown(0)
	proxyHealthMu.Lock()
	up = proxyHealthByIndex[0].currentlyUp
	ever = proxyHealthByIndex[0].everUp
	downStamped := !proxyHealthByIndex[0].downSince.IsZero()
	proxyHealthMu.Unlock()
	if up || !ever || !downStamped {
		t.Fatalf("after markProxyDown: up=%v ever=%v downStamped=%v, want false,true,true", up, ever, downStamped)
	}

	// mark on unknown index is a no-op (must not panic)
	markProxyUp(999)
	markProxyDown(999)
}

func TestProxyHealthSnapshot(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(2, "c:1", "c:1") // dead (never up)
	RegisterProxy(0, "a:1", "a:1") // will be up
	RegisterProxy(1, "b:1", "b:1") // will be degraded

	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1) // up then down -> degraded

	up, dead, degraded, _, connecting := ProxyHealthSnapshot()
	if up != 1 {
		t.Fatalf("up = %d, want 1", up)
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %v, want [] (RegisterProxy sets connecting=true)", dead)
	}
	if len(degraded) != 1 || degraded[0] != "proxy[1] (b:1)" {
		t.Fatalf("degraded = %v, want [proxy[1] (b:1)]", degraded)
	}
	if len(connecting) != 1 || connecting[0] != "proxy[2] (c:1)" {
		t.Fatalf("connecting = %v, want [proxy[2] (c:1)]", connecting)
	}

	// snapshot must NOT advance the baseline: lastSeenUp stays false everywhere
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	for idx, h := range proxyHealthByIndex {
		if h.lastSeenUp {
			t.Fatalf("snapshot advanced baseline for idx %d", idx)
		}
	}
}

func TestProxyHealthHeartbeatTransitions(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1")
	RegisterProxy(1, "b:1", "b:1") // becomes dead after up→down
	RegisterProxy(2, "c:1", "c:1") // connecting (RegisterProxy sets connecting=true)

	// First call establishes the baseline: no transitions, no dead (confirmDead=false).
	r := ProxyHealthHeartbeat(false)
	if len(r.Recovered) != 0 || len(r.NewlyDegraded) != 0 || len(r.NewlyDead) != 0 {
		t.Fatalf("first call should have no events, got %+v", r)
	}
	if r.LifetimeRecovered != 0 || r.LifetimeLost != 0 {
		t.Fatalf("first call lifetime counters should be 0, got %+v", r)
	}

	// Proxy 0 comes up -> recovered=1 (first-ever connect, after omitted).
	markProxyUp(0)
	r = ProxyHealthHeartbeat(false)
	if len(r.Recovered) != 1 || r.Recovered[0].Index != 0 {
		t.Fatalf("Recovered = %+v, want [idx 0]", r.Recovered)
	}
	if r.LifetimeRecovered != 1 {
		t.Fatalf("LifetimeRecovered = %d, want 1", r.LifetimeRecovered)
	}

	// Proxy 1 comes up then drops -> NewlyDegraded=1, lifetime_lost=1.
	markProxyUp(1)
	r = ProxyHealthHeartbeat(false)
	markProxyDown(1)
	r = ProxyHealthHeartbeat(false)
	if len(r.NewlyDegraded) != 1 || r.NewlyDegraded[0].Index != 1 {
		t.Fatalf("NewlyDegraded = %+v, want [idx 1]", r.NewlyDegraded)
	}
	if r.LifetimeLost != 1 || r.LifetimeRecovered != 2 {
		t.Fatalf("lifetime = (rec %d, lost %d), want (2,1)", r.LifetimeRecovered, r.LifetimeLost)
	}

	// confirmDead=true: proxy 1 (was up, now down, everUp=true) is degraded, not dead.
	// proxy 2 (connecting, never up) is not dead either (still connecting).
	// No proxy matches the dead criteria (everUp=false, connecting=false).
	r = ProxyHealthHeartbeat(true)
	if len(r.NewlyDead) != 0 {
		t.Fatalf("NewlyDead = %+v, want [] (no dead proxies)", r.NewlyDead)
	}
	if len(r.Dead) != 0 {
		t.Fatalf("Dead = %+v, want []", r.Dead)
	}
}

func TestUnregisterProxy_RemovesFromRegistry(t *testing.T) {
	resetProxyHealthForTest()

	RegisterProxy(99, "1.2.3.4:1080", "1.2.3.4:1080")
	if ProxyHealthCount() != 1 {
		t.Fatal("expected 1 proxy registered")
	}

	UnregisterProxy(99)
	if got := ProxyHealthCount(); got != 0 {
		t.Fatalf("expected 0 proxies after unregister, got %d", got)
	}
}

func TestUnregisterProxy_NoopIfNotRegistered(t *testing.T) {
	resetProxyHealthForTest()

	// Must not panic
	UnregisterProxy(42)
}

func TestProxyHealthHeartbeatFlappingCountsTwice(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1")
	ProxyHealthHeartbeat(false) // baseline

	markProxyUp(0)
	ProxyHealthHeartbeat(false) // recovered #1
	markProxyDown(0)
	ProxyHealthHeartbeat(false) // lost #1
	markProxyUp(0)
	r := ProxyHealthHeartbeat(false) // recovered #2

	if r.LifetimeRecovered != 2 {
		t.Fatalf("LifetimeRecovered = %d, want 2 (event semantics)", r.LifetimeRecovered)
	}
}

func TestDegradedProxies_EmptyWhenAllUp(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1")
	markProxyUp(0)

	dps := DegradedProxies()
	if len(dps) != 0 {
		t.Fatalf("DegradedProxies = %d, want 0 (all up)", len(dps))
	}
}

func TestDegradedProxies_IncludesDegradedOnly(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1") // up
	RegisterProxy(1, "b:1", "b:1") // degraded
	RegisterProxy(2, "c:1", "c:1") // connecting (never up, never down)

	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1)

	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].Address != "b:1" {
		t.Fatalf("degraded address = %q, want %q", dps[0].Address, "b:1")
	}
	if dps[0].DownFor <= 0 {
		t.Fatal("DownFor should be positive")
	}
}

func TestDegradedProxies_ExcludesConnectingAndDead(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1") // connecting (never up)
	RegisterProxy(1, "b:1", "b:1") // dead (never up, connecting=false)

	// Set proxy 1 to dead: it was registered with connecting=true,
	// set connecting=false without ever marking up
	proxyHealthMu.Lock()
	if h, ok := proxyHealthByIndex[1]; ok {
		h.connecting = false
	}
	proxyHealthMu.Unlock()

	dps := DegradedProxies()
	if len(dps) != 0 {
		t.Fatalf("DegradedProxies = %d, want 0 (no everUp proxies)", len(dps))
	}
}

func TestDegradedProxies_BandwidthPopulated(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1")
	markProxyUp(0)

	bw := RegisterProxyBandwidth(0)
	bw.TotalRx.Store(1000)
	bw.TotalTx.Store(2000)

	markProxyDown(0)

	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].TotalRxBytes != 1000 {
		t.Fatalf("TotalRxBytes = %d, want 1000", dps[0].TotalRxBytes)
	}
	if dps[0].TotalTxBytes != 2000 {
		t.Fatalf("TotalTxBytes = %d, want 2000", dps[0].TotalTxBytes)
	}
}

func TestDegradedProxies_NilBandwidth(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1")
	markProxyUp(0)
	markProxyDown(0)

	// Don't call RegisterProxyBandwidth — bw stays nil
	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].TotalRxBytes != 0 || dps[0].TotalTxBytes != 0 {
		t.Fatalf("expected zero bandwidth when bw is nil, got rx=%d tx=%d",
			dps[0].TotalRxBytes, dps[0].TotalTxBytes)
	}
}

func TestDegradedProxies_DownSinceStamped(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1")
	markProxyUp(0)
	markProxyDown(0)

	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].DownFor <= 0 {
		t.Fatal("DownFor should be positive after markProxyDown")
	}
}

func TestDegradedProxies_NotSorted(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(0, "a:1", "a:1") // will be degraded last
	RegisterProxy(1, "b:1", "b:1") // will be degraded first

	markProxyUp(0)
	markProxyUp(1)
	markProxyDown(1)

	time.Sleep(time.Millisecond)

	markProxyDown(0)

	dps := DegradedProxies()
	if len(dps) != 2 {
		t.Fatalf("DegradedProxies = %d, want 2", len(dps))
	}
	// Should NOT be sorted — order is from map iteration
	// Just verify both addresses are present
	addrs := map[string]bool{}
	for _, d := range dps {
		addrs[d.Address] = true
	}
	if !addrs["a:1"] || !addrs["b:1"] {
		t.Fatal("both degraded addresses should be present")
	}
}

func TestIsDegraded_TrueWhenDegraded(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(10, "degraded:1", "degraded:1")
	markProxyUp(10)
	markProxyDown(10)

	if !IsDegraded("degraded:1") {
		t.Fatal("expected degraded:1 to be reported as degraded")
	}
}

func TestIsDegraded_FalseWhenCurrentlyUp(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(11, "up:1", "up:1")
	markProxyUp(11)

	if IsDegraded("up:1") {
		t.Fatal("expected up:1 (currentlyUp) to not be reported as degraded")
	}
}

func TestIsDegraded_FalseWhenUnregistered(t *testing.T) {
	resetProxyHealthForTest()

	if IsDegraded("nonexistent:1") {
		t.Fatal("expected an unregistered address to not be reported as degraded")
	}
}

func TestIsDegraded_FalseWhenRecoveredAfterDown(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(12, "recovered:1", "recovered:1")
	markProxyUp(12)
	markProxyDown(12)

	if !IsDegraded("recovered:1") {
		t.Fatal("expected recovered:1 to be degraded before recovery")
	}

	markProxyUp(12)
	if IsDegraded("recovered:1") {
		t.Fatal("expected recovered:1 to no longer be degraded after reconnecting")
	}
}

func TestIsDegraded_FalseWhileRespawnConnecting(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(13, "respawn:1", "respawn:1")
	markProxyUp(13)
	markProxyDown(13)

	if !IsDegraded("respawn:1") {
		t.Fatal("expected respawn:1 to be degraded before respawn")
	}

	// Simulate hot-reload tearing down and respawning a fresh instance at
	// the same index/address: RegisterProxy sets connecting=true and reuses
	// the struct, leaving everUp/downSince stale until the new instance
	// reports its own first transition.
	RegisterProxy(13, "respawn:1", "respawn:1")

	if IsDegraded("respawn:1") {
		t.Fatal("expected respawn:1 to not be degraded while the respawned instance is still connecting")
	}
}

func TestDegradedProxiesExcludesConnecting(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(20, "degraded-conn:1", "degraded-conn:1")
	markProxyUp(20)
	markProxyDown(20)

	found := false
	for _, e := range DegradedProxies() {
		if e.Index == 20 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected index 20 to appear in DegradedProxies before respawn")
	}

	// Respawn at the same index: RegisterProxy sets connecting=true and reuses
	// the struct, so the stale everUp/downSince must not make it read degraded.
	RegisterProxy(20, "degraded-conn:1", "degraded-conn:1")
	for _, e := range DegradedProxies() {
		if e.Index == 20 {
			t.Fatal("expected index 20 to be excluded from DegradedProxies while connecting")
		}
	}
}

func TestProxyHealthByAddressReportsConnectingOnRespawn(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(21, "respawn-addr:2", "respawn-addr:2")
	markProxyUp(21)
	markProxyDown(21)

	status := ProxyHealthByAddress()
	if s, ok := status["respawn-addr:2"]; !ok || s.Health == "connecting" || s.Health == "up" || s.Health == "dead" {
		t.Fatalf("expected a degraded tier before respawn, got %q", s.Health)
	}

	RegisterProxy(21, "respawn-addr:2", "respawn-addr:2")
	status = ProxyHealthByAddress()
	if s, ok := status["respawn-addr:2"]; !ok || s.Health != "connecting" {
		t.Fatalf("expected connecting after respawn, got %q", s.Health)
	}
}

func TestProxyHealthByAddressUpWinsOverConnecting(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(22, "up-wins:3", "up-wins:3")
	markProxyUp(22)

	// Re-register while still up: connecting=true, but currentlyUp must win.
	RegisterProxy(22, "up-wins:3", "up-wins:3")
	status := ProxyHealthByAddress()
	if s, ok := status["up-wins:3"]; !ok || s.Health != "up" {
		t.Fatalf("expected up to win over connecting, got %q", s.Health)
	}
}

func TestProxyHealthByAddress_Dead(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(30, "dead-addr:1", "dead-addr:1")

	// Force out of the connecting state without ever coming up, simulating
	// a proxy that gave up before its first successful connection.
	proxyHealthMu.Lock()
	proxyHealthByIndex[30].connecting = false
	proxyHealthMu.Unlock()

	status := ProxyHealthByAddress()
	if s, ok := status["dead-addr:1"]; !ok || s.Health != "dead" {
		t.Fatalf("expected dead, got %q", s.Health)
	}
}

func TestProxyHealthByAddress_ConnectingFresh(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(31, "connecting-addr:1", "connecting-addr:1")

	// Freshly registered, never up, never down: should read as connecting.
	status := ProxyHealthByAddress()
	if s, ok := status["connecting-addr:1"]; !ok || s.Health != "connecting" {
		t.Fatalf("expected connecting, got %q", s.Health)
	}
}

func TestProxyHealthByAddress_DegradedTier(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(32, "degraded-tier:1", "degraded-tier:1")
	markProxyUp(32)
	markProxyDown(32)

	status := ProxyHealthByAddress()
	s, ok := status["degraded-tier:1"]
	if !ok {
		t.Fatal("expected an entry for degraded-tier:1")
	}
	if s.Health != "recently_offline" {
		t.Fatalf("expected recently_offline tier, got %q", s.Health)
	}
}

func TestProxyHealthByAddress_AllHealthStatesTogether(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(40, "up:1", "up:1")
	RegisterProxy(41, "connecting:1", "connecting:1")
	RegisterProxy(42, "degraded:1", "degraded:1")
	RegisterProxy(43, "dead:1", "dead:1")

	markProxyUp(40)

	markProxyUp(42)
	markProxyDown(42)

	proxyHealthMu.Lock()
	proxyHealthByIndex[43].connecting = false
	proxyHealthMu.Unlock()

	status := ProxyHealthByAddress()
	want := map[string]string{
		"up:1":         "up",
		"connecting:1": "connecting",
		"degraded:1":   "recently_offline",
		"dead:1":       "dead",
	}
	for addr, expected := range want {
		s, ok := status[addr]
		if !ok {
			t.Fatalf("missing entry for %q", addr)
		}
		if s.Health != expected {
			t.Fatalf("health[%q] = %q, want %q", addr, s.Health, expected)
		}
	}
}

func TestDegradedProxies_MixedConnectingAndDegraded(t *testing.T) {
	resetProxyHealthForTest()

	// idx 50: genuinely degraded, no respawn involved.
	RegisterProxy(50, "genuine-degraded:1", "genuine-degraded:1")
	markProxyUp(50)
	markProxyDown(50)

	// idx 51: was degraded, then respawned at the same index/address -> must
	// be excluded despite the stale everUp/downSince it inherited.
	RegisterProxy(51, "respawned:1", "respawned:1")
	markProxyUp(51)
	markProxyDown(51)
	RegisterProxy(51, "respawned:1", "respawned:1")

	dps := DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1", len(dps))
	}
	if dps[0].Address != "genuine-degraded:1" {
		t.Fatalf("degraded address = %q, want %q", dps[0].Address, "genuine-degraded:1")
	}
}

func TestDegradedProxies_RespawnThenRedown(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(52, "respawn-redown:1", "respawn-redown:1")
	markProxyUp(52)
	markProxyDown(52)

	// Respawn: connecting=true, must be excluded.
	RegisterProxy(52, "respawn-redown:1", "respawn-redown:1")
	dps := DegradedProxies()
	if len(dps) != 0 {
		t.Fatalf("DegradedProxies = %d, want 0 while connecting", len(dps))
	}

	// The new instance reports its own up->down transition: connecting is
	// cleared by markProxyUp/markProxyDown, so it should now count again.
	markProxyUp(52)
	markProxyDown(52)
	dps = DegradedProxies()
	if len(dps) != 1 {
		t.Fatalf("DegradedProxies = %d, want 1 after the respawned instance reports its own down", len(dps))
	}
	if dps[0].Address != "respawn-redown:1" {
		t.Fatalf("degraded address = %q, want %q", dps[0].Address, "respawn-redown:1")
	}
}

func TestConnectingStateExpiresToDegraded(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(23, "stale-conn:1", "stale-conn:1")
	markProxyUp(23)
	markProxyDown(23)
	RegisterProxy(23, "stale-conn:1", "stale-conn:1")

	// Fresh connecting: reported as connecting, not degraded.
	if s, ok := ProxyHealthByAddress()["stale-conn:1"]; !ok || s.Health != "connecting" {
		t.Fatalf("expected connecting while fresh, got %q", s.Health)
	}
	if IsDegraded("stale-conn:1") {
		t.Fatal("expected not degraded while connecting fresh")
	}

	// Backdate connectingSince past the stale threshold.
	proxyHealthMu.Lock()
	proxyHealthByIndex[23].connectingSince = time.Now().Add(-(connectingStaleAfter + time.Minute))
	proxyHealthMu.Unlock()

	// Stale connecting: falls back to a degraded tier.
	if s, ok := ProxyHealthByAddress()["stale-conn:1"]; !ok || s.Health != "recently_offline" {
		t.Fatalf("expected recently_offline once connecting stale, got %q", s.Health)
	}
	if !IsDegraded("stale-conn:1") {
		t.Fatal("expected degraded once connecting stale")
	}
}

func TestDegradedProxiesIncludesStaleConnecting(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(24, "stale-conn:2", "stale-conn:2")
	markProxyUp(24)
	markProxyDown(24)
	RegisterProxy(24, "stale-conn:2", "stale-conn:2")

	if len(DegradedProxies()) != 0 {
		t.Fatal("expected no degraded entries while connecting fresh")
	}

	proxyHealthMu.Lock()
	proxyHealthByIndex[24].connectingSince = time.Now().Add(-(connectingStaleAfter + time.Minute))
	proxyHealthMu.Unlock()

	found := false
	for _, e := range DegradedProxies() {
		if e.Index == 24 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected stale-connecting proxy to appear in DegradedProxies")
	}
}

func TestConnectingStateResetsOnUpAndDown(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(25, "reset-conn:3", "reset-conn:3")

	// Up clears connecting and connectingSince.
	markProxyUp(25)
	proxyHealthMu.Lock()
	h := proxyHealthByIndex[25]
	proxyHealthMu.Unlock()
	if h.connecting || !h.connectingSince.IsZero() {
		t.Fatal("expected connecting/connectingSince cleared after markProxyUp")
	}

	// Down also clears them.
	markProxyDown(25)
	proxyHealthMu.Lock()
	h = proxyHealthByIndex[25]
	proxyHealthMu.Unlock()
	if h.connecting || !h.connectingSince.IsZero() {
		t.Fatal("expected connecting/connectingSince cleared after markProxyDown")
	}
}

func TestNeverUpProxyReadsDeadAfterConnectingStale(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(60, "never-up:1", "never-up:1")

	// Fresh connecting: reads as connecting, not dead.
	if s, ok := ProxyHealthByAddress()["never-up:1"]; !ok || s.Health != "connecting" {
		t.Fatalf("expected connecting while fresh, got %q", s.Health)
	}

	proxyHealthMu.Lock()
	proxyHealthByIndex[60].connectingSince = time.Now().Add(-(connectingStaleAfter + time.Minute))
	proxyHealthMu.Unlock()

	if s, ok := ProxyHealthByAddress()["never-up:1"]; !ok || s.Health != "dead" {
		t.Fatalf("expected dead once connecting stale, got %q", s.Health)
	}
}

func TestNewlyDeadFiresForNeverUpProxyAfterConnectingStale(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(61, "never-up-newly-dead:1", "never-up-newly-dead:1")

	// First heartbeat establishes the baseline; fresh connecting means no
	// NewlyDead yet.
	r := ProxyHealthHeartbeat(true)
	if len(r.NewlyDead) != 0 {
		t.Fatalf("expected no NewlyDead while connecting fresh, got %d", len(r.NewlyDead))
	}

	proxyHealthMu.Lock()
	proxyHealthByIndex[61].connectingSince = time.Now().Add(-(connectingStaleAfter + time.Minute))
	proxyHealthMu.Unlock()

	r = ProxyHealthHeartbeat(true)
	if len(r.NewlyDead) != 1 {
		t.Fatalf("expected 1 NewlyDead event, got %d", len(r.NewlyDead))
	}
	if r.NewlyDead[0].Index != 61 {
		t.Fatalf("NewlyDead index = %d, want 61", r.NewlyDead[0].Index)
	}

	// deadLogged pins the event: a later heartbeat must not re-emit it.
	r = ProxyHealthHeartbeat(true)
	if len(r.NewlyDead) != 0 {
		t.Fatalf("expected no repeated NewlyDead event, got %+v", r.NewlyDead)
	}
}

func TestRespawnedEverUpProxyNotDegradedWhileConnecting(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(62, "respawn-everup:1", "respawn-everup:1")
	markProxyUp(62)
	markProxyDown(62)
	RegisterProxy(62, "respawn-everup:1", "respawn-everup:1")

	// Previously up, now respawning: the inherited everUp must not classify it
	// as degraded while its fresh connection attempt is still running.
	r := ProxyHealthHeartbeat(true)
	want := "proxy[62] (respawn-everup:1)"
	for _, e := range r.Degraded {
		if e == want {
			t.Fatalf("respawned proxy reported degraded while connecting: %q", e)
		}
	}
	for _, e := range r.Dead {
		if e == want {
			t.Fatalf("respawned proxy reported dead while connecting: %q", e)
		}
	}
	if r.Up != 0 {
		t.Fatalf("Up = %d, want 0", r.Up)
	}

	// Same precedence applies to the snapshot: it must list the proxy as
	// connecting, not dead or degraded.
	_, dead, degraded, _, connecting := ProxyHealthSnapshot()
	for _, e := range dead {
		if e == want {
			t.Fatalf("snapshot listed respawning proxy as dead: %q", e)
		}
	}
	for _, e := range degraded {
		if e == want {
			t.Fatalf("snapshot listed respawning proxy as degraded: %q", e)
		}
	}
	found := false
	for _, e := range connecting {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("snapshot did not list respawning proxy as connecting: %v", connecting)
	}
}

func TestConnectingStaleAfterIsOneHourlyPulseCycle(t *testing.T) {
	if connectingStaleAfter != 65*time.Minute {
		t.Fatalf("connectingStaleAfter = %v, want 65m", connectingStaleAfter)
	}
}

func TestConnectingRemainsActiveBeyondOldFifteenMinuteBound(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(70, "beyond-old-bound:1", "beyond-old-bound:1")

	proxyHealthMu.Lock()
	proxyHealthByIndex[70].connectingSince = time.Now().Add(-20 * time.Minute)
	proxyHealthMu.Unlock()

	if s, ok := ProxyHealthByAddress()["beyond-old-bound:1"]; !ok || s.Health != "connecting" {
		t.Fatalf("expected connecting at 20m under the 65m bound, got %q", s.Health)
	}
	if IsDegraded("beyond-old-bound:1") {
		t.Fatal("expected not degraded at 20m under the 65m bound")
	}
	for _, e := range DegradedProxies() {
		if e.Index == 70 {
			t.Fatal("expected proxy not to appear in DegradedProxies at 20m under the 65m bound")
		}
	}
}

func TestConnectingActiveBoundary(t *testing.T) {
	now := time.Now()

	justBefore := &proxyHealth{connecting: true, connectingSince: now.Add(-(connectingStaleAfter - time.Second))}
	if !justBefore.connectingActive(now) {
		t.Fatal("expected connectingActive true just before the connectingStaleAfter boundary")
	}

	atBoundary := &proxyHealth{connecting: true, connectingSince: now.Add(-connectingStaleAfter)}
	if atBoundary.connectingActive(now) {
		t.Fatal("expected connectingActive false exactly at the connectingStaleAfter boundary")
	}

	justAfter := &proxyHealth{connecting: true, connectingSince: now.Add(-(connectingStaleAfter + time.Second))}
	if justAfter.connectingActive(now) {
		t.Fatal("expected connectingActive false just after the connectingStaleAfter boundary")
	}
}

func TestConnectingActiveFalseWhenNotConnecting(t *testing.T) {
	h := &proxyHealth{connecting: false, connectingSince: time.Now()}
	if h.connectingActive(time.Now()) {
		t.Fatal("expected connectingActive false when connecting is false, regardless of connectingSince")
	}
}

func TestConnectingActiveTrueWhenNeverStamped(t *testing.T) {
	h := &proxyHealth{connecting: true}
	if !h.connectingActive(time.Now()) {
		t.Fatal("expected connectingActive true when connectingSince has never been stamped")
	}
}

type testTimeoutErr struct{}

func (e testTimeoutErr) Error() string   { return "test network timeout" }
func (e testTimeoutErr) Timeout() bool   { return true }
func (e testTimeoutErr) Temporary() bool { return true }

func TestRecordProxyAuthFailure_AuthAndTimeout(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(80, "auth-fail:1", "auth-fail:1")

	// Standard error increments AuthFailures
	RecordProxyAuthFailure(80, errors.New("unauthorized: 401"))
	if count := ProxyAuthFailureCount(80); count != 1 {
		t.Fatalf("expected AuthFailures = 1, got %d", count)
	}

	// Context deadline timeout increments TimeoutFailures, not AuthFailures
	RecordProxyAuthFailure(80, context.DeadlineExceeded)
	if count := ProxyAuthFailureCount(80); count != 1 {
		t.Fatalf("expected AuthFailures to remain 1 on timeout, got %d", count)
	}

	// net.Error timeout also increments TimeoutFailures
	RecordProxyAuthFailure(80, testTimeoutErr{})
	if count := ProxyAuthFailureCount(80); count != 1 {
		t.Fatalf("expected AuthFailures to remain 1 on net timeout, got %d", count)
	}

	// Verify failure counters in status
	st := ProxyHealthByAddress()["auth-fail:1"]
	if st.AuthFailures != 1 {
		t.Fatalf("expected status AuthFailures = 1, got %d", st.AuthFailures)
	}
	if st.TimeoutFails != 2 {
		t.Fatalf("expected status TimeoutFails = 2, got %d", st.TimeoutFails)
	}

	// Unregistered index query returns 0
	if count := ProxyAuthFailureCount(999); count != 0 {
		t.Fatalf("expected 0 for unregistered proxy, got %d", count)
	}
}

func TestRecordProxyTransportDrop(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(81, "drop-proxy:1", "drop-proxy:1")

	RecordProxyTransportDrop(81, errors.New("connection reset by peer"))
	RecordProxyTransportDrop(81, errors.New("broken pipe"))

	st := ProxyHealthByAddress()["drop-proxy:1"]
	if st.TransportDrops != 2 {
		t.Fatalf("expected TransportDrops = 2, got %d", st.TransportDrops)
	}
}

func TestProxyEverUp(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(82, "everup-proxy:1", "everup-proxy:1")

	if ProxyEverUp(82) {
		t.Fatal("expected ProxyEverUp false before markProxyUp")
	}
	if ProxyEverUp(999) {
		t.Fatal("expected ProxyEverUp false for unknown index")
	}

	markProxyUp(82)
	if !ProxyEverUp(82) {
		t.Fatal("expected ProxyEverUp true after markProxyUp")
	}

	markProxyDown(82)
	if !ProxyEverUp(82) {
		t.Fatal("expected ProxyEverUp true even after markProxyDown")
	}
}

func TestProxyBandwidthByKey(t *testing.T) {
	resetProxyHealthForTest()
	RegisterProxy(83, "bw-addr:1", "bw-addr:1")
	bw := RegisterProxyBandwidth(83)
	bw.LatencyNs.Store(50_000_000)
	bw.SocksLatencyNs.Store(20_000_000)

	got := ProxyBandwidthByKey("bw-addr:1")
	if got == nil || got != bw {
		t.Fatalf("expected bw pointer for bw-addr:1, got %v", got)
	}

	if ProxyBandwidthByKey("unknown:1") != nil {
		t.Fatal("expected nil for unregistered address")
	}

	st := ProxyHealthByAddress()["bw-addr:1"]
	if st.LatencyMs != 50 {
		t.Fatalf("expected LatencyMs = 50, got %d", st.LatencyMs)
	}
	if st.SocksLatencyMs != 20 {
		t.Fatalf("expected SocksLatencyMs = 20, got %d", st.SocksLatencyMs)
	}
}

func TestFormatProxyErrorEntry(t *testing.T) {
	failures := &ProxyFailureCounters{}
	failures.AuthFailures.Store(2)
	failures.TimeoutFailures.Store(3)
	failures.TransportDrops.Store(1)

	entry := formatProxyErrorEntry(5, "proxy.local:1080", failures, "tls handshake timeout", time.Time{})
	wantPrefix := "proxy[5] (proxy.local:1080) [auth:2, timeout:3, drops:1, last_err:\"tls handshake timeout\"]"
	if entry != wantPrefix {
		t.Fatalf("entry = %q, want %q", entry, wantPrefix)
	}

	// No failures
	emptyFailures := &ProxyFailureCounters{}
	emptyEntry := formatProxyErrorEntry(5, "proxy.local:1080", emptyFailures, "", time.Time{})
	if emptyEntry != "proxy[5] (proxy.local:1080)" {
		t.Fatalf("emptyEntry = %q, want %q", emptyEntry, "proxy[5] (proxy.local:1080)")
	}
}

func TestDegradedTierFromDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{1 * time.Hour, "recently_offline"},
		{23 * time.Hour, "recently_offline"},
		{24 * time.Hour, "offline"},
		{71 * time.Hour, "offline"},
		{72 * time.Hour, "long_offline"},
		{6 * 24 * time.Hour, "long_offline"},
		{7 * 24 * time.Hour, "inactive"},
		{14 * 24 * time.Hour, "inactive"},
	}

	for _, tc := range tests {
		got := degradedTierFromDuration(tc.d)
		if got != tc.want {
			t.Errorf("degradedTierFromDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestConcurrentAccess_Race(t *testing.T) {
	resetProxyHealthForTest()

	const numProxies = 20
	for i := 0; i < numProxies; i++ {
		addr := net.JoinHostPort("10.0.0.1", "8080")
		RegisterProxy(i, addr, addr)
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Goroutine: Up/down flapping
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			idx := workerID % numProxies
			for ctx.Err() == nil {
				MarkProxyUp(idx)
				MarkProxyDown(idx)
			}
		}(i)
	}

	// Goroutine: Recording failures
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			idx := workerID % numProxies
			err := errors.New("auth fail")
			for ctx.Err() == nil {
				RecordProxyAuthFailure(idx, err)
				RecordProxyTransportDrop(idx, err)
			}
		}(i)
	}

	// Goroutine: Heartbeat and Snapshots
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = ProxyHealthHeartbeat(false)
				_, _, _, _, _ = ProxyHealthSnapshot()
				_ = ProxyHealthByAddress()
				_ = DegradedProxies()
			}
		}()
	}

	wg.Wait()
}

func TestUnregisterProxyStaleGeneration(t *testing.T) {
	resetProxyHealthForTest()

	// Register a proxy and capture its generation.
	gen1 := RegisterProxy(0, "1.1.1.1:1080", "1.1.1.1:1080")
	if ProxyHealthCount() != 1 {
		t.Fatalf("expected 1 proxy after first register, got %d", ProxyHealthCount())
	}

	// Re-register at the same index with a new address (triggers new generation).
	gen2 := RegisterProxy(0, "2.2.2.2:1080", "2.2.2.2:1080")
	if gen2 == gen1 {
		t.Fatalf("expected different generation on re-register, got gen1=%d gen2=%d", gen1, gen2)
	}

	// Unregister with the OLD generation — must be a no-op.
	UnregisterProxySafe(0, gen1)

	// Proxy must still exist.
	if got := ProxyHealthCount(); got != 1 {
		t.Fatalf("proxy should still exist after stale unregister, count = %d, want 1", got)
	}
	// Address must be the new one.
	status := ProxyHealthByAddress()
	if _, ok := status["2.2.2.2:1080"]; !ok {
		t.Fatal("expected new address 2.2.2.2:1080 to still be registered")
	}
}

func TestUnregisterProxyFreshGeneration(t *testing.T) {
	resetProxyHealthForTest()

	// Register a proxy and capture its generation.
	gen := RegisterProxy(0, "3.3.3.3:1080", "3.3.3.3:1080")
	if ProxyHealthCount() != 1 {
		t.Fatalf("expected 1 proxy, got %d", ProxyHealthCount())
	}

	// Unregister with the CURRENT generation — should succeed.
	UnregisterProxySafe(0, gen)

	if got := ProxyHealthCount(); got != 0 {
		t.Fatalf("proxy should be removed after fresh unregister, count = %d, want 0", got)
	}
}
