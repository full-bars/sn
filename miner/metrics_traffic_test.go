package miner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// TestControlIdentityKeyedBandwidthUsesTheIdentityIndex pins the key contract
// between the counter registry and the /metrics series: the provide loop
// registers a slot's counter under proxyIdentityBandwidthIndex(settings), which
// hashes settings.Key(); the reader must hash the same identity key. Hashing
// only the address (the old behavior) finds no counter for a credentialed
// proxy, so its series silently disappeared.
func TestControlIdentityKeyedBandwidthUsesTheIdentityIndex(t *testing.T) {
	withTempHome(t)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)

	plain := &connect.ProxySettings{Address: "203.0.113.10:1080"}
	alice := &connect.ProxySettings{Address: "198.51.100.30:443", Auth: &proxy.Auth{User: "alice"}}
	bob := &connect.ProxySettings{Address: "198.51.100.30:443", Auth: &proxy.Auth{User: "bob"}}

	registry := bandwidth.NewRegistry()
	plainBw := registry.Register(proxyIdentityBandwidthIndex(plain))
	plainBw.TotalRx.Store(1_000)
	aliceBw := registry.Register(proxyIdentityBandwidthIndex(alice))
	aliceBw.TotalRx.Store(2_000)
	aliceBw.BillableTx.Store(300)
	bobBw := registry.Register(proxyIdentityBandwidthIndex(bob))
	bobBw.TotalRx.Store(4_000)

	controlRuntime.setBandwidthRegistry(registry)
	controlRuntime.setRunningProxies([]string{plain.Key(), alice.Key(), bob.Key()})

	series := controlIdentityKeyedBandwidth()
	for _, settings := range []*connect.ProxySettings{plain, alice, bob} {
		key := settings.Key()
		if series[key] == nil {
			t.Fatalf("no counter for running slot %q; got keys %v", key, series)
		}
	}

	var b strings.Builder
	writeTrafficMetrics(&b)
	out := b.String()
	for _, settings := range []*connect.ProxySettings{plain, alice, bob} {
		key := settings.Key()
		label := prometheusLabelValue(key)
		if !strings.Contains(out, "urnet_proxy_bytes_total{proxy="+label+`,direction="in"}`) {
			t.Fatalf("no bytes series for running slot %q:\n%s", key, out)
		}
	}
	// The two accounts behind one gateway are distinct identities and must
	// remain distinct series, not one merged address series.
	aliceLabel := prometheusLabelValue(alice.Key())
	bobLabel := prometheusLabelValue(bob.Key())
	if aliceLabel == bobLabel || !strings.Contains(out, aliceLabel) || !strings.Contains(out, bobLabel) {
		t.Fatalf("the two accounts behind one gateway must have distinct series:\n%s", out)
	}
}

// TestTrafficMetricsReadPoolStateOnce pins the scrape path's I/O: the
// composition (urnet_proxy_pool_size) and the aggregate
// (urnet_proxy_pool_health) are built from one member read, not one each. The
// live-registry read is counted because it is the companion of the proxy.state
// read in proxyPoolMembers: a second composition read cannot happen without a
// second registry read.
func TestTrafficMetricsReadPoolStateOnce(t *testing.T) {
	withTempHome(t)
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)
	resetProxyLiveHealthRegistryForTest()
	resetProxyPoolHealthForTest()
	t.Cleanup(func() {
		resetProxyLiveHealthRegistryForTest()
		resetProxyPoolHealthForTest()
	})

	state, err := readProxyState()
	if err != nil {
		t.Fatalf("read proxy state: %v", err)
	}
	for i := 0; i < 4; i++ {
		state.Proxies[fmt.Sprintf("10.0.0.%d:443", i)] = ProxyEntry{ID: i, Health: "up"}
	}
	if err := writeProxyState(state); err != nil {
		t.Fatalf("write proxy state: %v", err)
	}

	reads := 0
	prev := liveProxyHealthByKey
	liveProxyHealthByKey = func() map[string]proxyHealthStatus {
		reads++
		return prev()
	}
	t.Cleanup(func() { liveProxyHealthByKey = prev })

	var b strings.Builder
	writeTrafficMetrics(&b)
	if reads != 1 {
		t.Fatalf("one scrape read the pool composition %d times, want 1:\n%s", reads, b.String())
	}
	if !strings.Contains(b.String(), `urnet_proxy_pool_size{status="up"} 4`) {
		t.Fatalf("scrape did not publish the composition it read:\n%s", b.String())
	}
}
