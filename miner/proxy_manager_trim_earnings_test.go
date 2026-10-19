package miner

import (
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// Tests for the earnings half of the trim shed ranking, ported from the
// 3.23-fix v33 provider's proxy_trim_earnings_test.go and
// proxy_startup_trim_test.go.

// trimEarningsTestNow is the fixed instant every assertion here evaluates the
// decayed score at, so none of them depends on the wall clock.
var trimEarningsTestNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// publishTrimProxy publishes one identity-keyed counter with separate total and
// billable byte counts, the way a real provider sees them: most of the total is
// non-billable platform traffic.
func publishTrimProxy(t *testing.T, key string, total uint64, billable uint64) {
	t.Helper()
	bw := &bandwidth.ProxyBandwidth{}
	bw.TotalRx.Store(total)
	bw.TotalTx.Store(total)
	bw.BillableRx.Store(billable)
	publishProxyEarnBandwidth(key, bw)
}

// The shed rankings must read what a proxy EARNED. On a measured provider the
// total byte count was about 17x the billable count, so a proxy that moved a
// lot of non-billable traffic and earned nothing ranked as a protected earner,
// and a small real earner could be shed ahead of it.
func TestRunningProxyEarnings_UsesBillableNotTotalBytes(t *testing.T) {
	withEarnBandwidth(t)
	withGlobalEarningsStore(t)

	publishTrimProxy(t, "10.0.0.1:1080", 50<<30, 0) // busy, earned nothing
	publishTrimProxy(t, "10.0.0.2:1080", 1<<20, 1<<20)

	earnings := runningProxyEarningsAt(trimEarningsTestNow)
	if _, ok := earnings["10.0.0.1:1080"]; ok {
		t.Fatalf("a proxy with 50 GiB total and no billable bytes reads as earning %d", earnings["10.0.0.1:1080"])
	}
	if got := earnings["10.0.0.2:1080"]; got != 1<<20 {
		t.Fatalf("billable earner reads %d, want %d", got, 1<<20)
	}

	state := map[string]ProxyEntry{"10.0.0.1:1080": {Health: "up"}, "10.0.0.2:1080": {Health: "up"}}
	shed := selectWorstRunningProxies(state, nil, earnings, []string{"10.0.0.1:1080", "10.0.0.2:1080"}, 1)
	if len(shed) != 1 || shed[0] != "10.0.0.1:1080" {
		t.Fatalf("shed %v, want the busy non-earner and not the small real earner", shed)
	}
}

// A proxy that earned heavily before this process started, and has not moved a
// billable byte since, is still a proven earner: the decayed store score keeps
// it ahead of a proxy that never earned.
func TestRunningProxyEarnings_DecayedScoreProtectsAnIdleSinceRestartEarner(t *testing.T) {
	withEarnBandwidth(t)
	restore := withGlobalEarningsStore(t)
	defer restore()

	publishTrimProxy(t, "10.0.0.1:1080", 1<<30, 0) // earned last week, idle this run
	publishTrimProxy(t, "10.0.0.2:1080", 1<<30, 0) // never earned
	creditEarningsAt(globalProxyEarningsStore, "10.0.0.1:1080", 5<<30, trimEarningsTestNow.Add(-24*time.Hour))

	earnings := runningProxyEarningsAt(trimEarningsTestNow)
	if earnings["10.0.0.1:1080"] == 0 {
		t.Fatal("a proxy with a 5 GiB decayed earnings score reads as idle")
	}
	if got := earnings["10.0.0.2:1080"]; got != 0 {
		t.Fatalf("a proxy that never earned reads as %d", got)
	}
	state := map[string]ProxyEntry{"10.0.0.1:1080": {Health: "up"}, "10.0.0.2:1080": {Health: "up"}}
	// the earner sorts first by address, so only the earnings can save it
	shed := selectWorstRunningProxies(state, nil, earnings, []string{"10.0.0.1:1080", "10.0.0.2:1080"}, 1)
	if len(shed) != 1 || shed[0] != "10.0.0.2:1080" {
		t.Fatalf("shed %v, want the never-earner; the proven earner must be kept", shed)
	}
}

// The larger of this run's billable bytes and the decayed score wins, so
// neither signal can lower the other.
func TestRunningProxyEarnings_TakesTheLargerOfRunAndDecayedScore(t *testing.T) {
	withEarnBandwidth(t)
	restore := withGlobalEarningsStore(t)
	defer restore()

	publishTrimProxy(t, "10.0.0.1:1080", 100<<20, 40<<20)
	creditEarningsAt(globalProxyEarningsStore, "10.0.0.1:1080", 10<<20, trimEarningsTestNow)
	if got := runningProxyEarningsAt(trimEarningsTestNow)["10.0.0.1:1080"]; got != 40<<20 {
		t.Fatalf("earnings = %d, want the larger run billable %d", got, 40<<20)
	}

	publishTrimProxy(t, "10.0.0.2:1080", 100<<20, 1<<20)
	creditEarningsAt(globalProxyEarningsStore, "10.0.0.2:1080", 30<<20, trimEarningsTestNow)
	if got := runningProxyEarningsAt(trimEarningsTestNow)["10.0.0.2:1080"]; got != 30<<20 {
		t.Fatalf("earnings = %d, want the larger decayed score %d", got, 30<<20)
	}
}

// A proxy that is running but has not got a counter yet (published, still
// launching) must still carry its stored earnings. Building the map only from
// the live counters gave such a proxy zero, so on a tie it could be shed ahead
// of a proxy with a lower stored score.
func TestRunningProxyEarnings_ProxyWithoutACounterKeepsItsStoredScore(t *testing.T) {
	withEarnBandwidth(t)
	restore := withGlobalEarningsStore(t)
	defer restore()

	const launching = "10.0.0.1:1080"
	const weaker = "10.0.0.2:1080"

	// proven earner, published as running but with no counter
	controlRuntime.setRunningProxies([]string{launching, weaker})
	t.Cleanup(func() { controlRuntime.setRunningProxies(nil) })
	creditEarningsAt(globalProxyEarningsStore, launching, 50<<20, trimEarningsTestNow)

	// weaker earner with a counter
	publishTrimProxy(t, weaker, 100<<20, 1<<20)
	creditEarningsAt(globalProxyEarningsStore, weaker, 5<<20, trimEarningsTestNow)

	earnings := runningProxyEarningsAt(trimEarningsTestNow)
	if got := earnings[launching]; got != 50<<20 {
		t.Fatalf("launching proxy with a stored score reads %d, want %d", got, 50<<20)
	}

	state := map[string]ProxyEntry{launching: {Health: "up"}, weaker: {Health: "up"}}
	shed := selectWorstRunningProxies(state, nil, earnings, runningProxyAddresses(), 1)
	if len(shed) != 1 || shed[0] != weaker {
		t.Fatalf("shed %v, want the weaker earner and not the launching proven earner", shed)
	}
}

// runningProxyAddresses must report identity keys, so a credentialed proxy's
// earnings are visible under the key the ranking looks it up with.
func TestRunningProxyAddressesUsesIdentityKeys(t *testing.T) {
	withEarnBandwidth(t)
	controlRuntime.setRunningProxies(nil)
	t.Cleanup(func() { controlRuntime.setRunningProxies(nil) })

	key := (&connect.ProxySettings{Address: "10.0.0.9:1080", Auth: &proxy.Auth{User: "alice"}}).Key()
	publishProxyEarnBandwidth(key, &bandwidth.ProxyBandwidth{})

	keys := runningProxyAddresses()
	if len(keys) != 1 || keys[0] != key {
		t.Fatalf("runningProxyAddresses() = %v, want the identity key %q", keys, key)
	}
}

// TestStartupTrimSelection_ProvenEarnerIsNeverHeld pins the startup half of the
// earnings tiebreak: with the same health, an idle proxy is held before one
// that earned before this process started.
func TestStartupTrimSelection_ProvenEarnerIsNeverHeld(t *testing.T) {
	all := []*connect.ProxySettings{
		authSettings("a:1", "", ""),
		authSettings("b:1", "", ""),
		authSettings("c:1", "", ""),
		authSettings("d:1", "", ""),
		authSettings("e:1", "", ""),
	}
	allUp := map[string]ProxyEntry{}
	for _, a := range []string{"a:1", "b:1", "c:1", "d:1", "e:1"} {
		allUp[a] = ProxyEntry{Health: "up"}
	}
	earnings := func(k string) float64 {
		if k == "b:1" {
			return 5 << 30
		}
		return 0
	}

	launch, held := startupTrimSelection(all, 3, allUp, nil, earnings)
	// Idle ties break on address, so the first two idle addresses are held.
	if got, want := keysOfSettings(held), []string{"a:1", "c:1"}; !equalStrings(got, want) {
		t.Fatalf("held = %v, want %v (earner b:1 kept)", got, want)
	}
	if got, want := keysOfSettings(launch), []string{"b:1", "d:1", "e:1"}; !equalStrings(got, want) {
		t.Fatalf("launch = %v, want %v", got, want)
	}
}

// TestStartupTrimSelection_UnhealthyHeldBeforeEarners pins that earnings never
// outrank health: a dead proxy with a rich history is still held first.
func TestStartupTrimSelection_UnhealthyHeldBeforeEarners(t *testing.T) {
	all := []*connect.ProxySettings{
		authSettings("a:1", "", ""),
		authSettings("b:1", "", ""),
		authSettings("c:1", "", ""),
	}
	state := map[string]ProxyEntry{
		"a:1": {Health: "up"},
		"b:1": {Health: "dead"},
		"c:1": {Health: "offline"},
	}
	earnings := func(k string) float64 {
		if k == "b:1" { // dead but had earnings
			return 5 << 30
		}
		return 0
	}

	_, held := startupTrimSelection(all, 1, state, nil, earnings)
	if got, want := keysOfSettings(held), []string{"b:1", "c:1"}; !equalStrings(got, want) {
		t.Fatalf("held = %v, want the dead and offline proxies %v", got, want)
	}
}

func keysOfSettings(ps []*connect.ProxySettings) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Key())
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
