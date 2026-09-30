package provider

import (
	"github.com/urfoundation/sn/provider/bandwidth"
)

// registerProxyV2026 stubs for connect.RegisterProxy removed in v2026.
// Identity is the bare address here (unauthenticated proxies register
// address-as-key); credentialed shared-gateway callers register their real
// key through the reload engine.
func registerProxyV2026(idx int, addr string) { RegisterProxy(idx, addr, addr) }

// proxyBandwidthByKeyV2026 stubs for connect.ProxyBandwidthByKey removed in v2026.
func proxyBandwidthByKeyV2026(key string) *bandwidth.ProxyBandwidth {
	return ProxyBandwidthByKey(key)
}

// connect.ProxyHealthByAddress removed in v2026.
// DESIGN ADAPTATION: identity-keyed health snapshot (ProxyHealthByKey). For
// an unauthenticated proxy the key is the address, so address-keyed callers
// behave identically; credentialed shared-gateway callers must look up by key.
func proxyHealthByAddressV2026() map[string]ProxyHealthStatus { return ProxyHealthByKey() }

func unregisterProxyStub(idx interface{}) {
	if i, ok := idx.(int); ok {
		UnregisterProxy(i)
	}
}
