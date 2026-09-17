package provider

import (
	"github.com/urfoundation/sn/provider/bandwidth"
)

// registerProxyV2026 stubs for connect.RegisterProxy removed in v2026.
func registerProxyV2026(idx int, addr string) { RegisterProxy(idx, addr) }

// proxyBandwidthByAddressV2026 stubs for connect.ProxyBandwidthByAddress removed in v2026.
func proxyBandwidthByAddressV2026(addr string) *bandwidth.ProxyBandwidth {
	return ProxyBandwidthByAddress(addr)
}

// connect.ProxyHealthByAddress removed in v2026.
// DESIGN ADAPTATION: stub until health-by-address is reimplemented.
func proxyHealthByAddressV2026() map[string]ProxyHealthStatus { return ProxyHealthByAddress() }

func unregisterProxyStub(idx interface{}) {
	if i, ok := idx.(int); ok {
		UnregisterProxy(i)
	}
}
