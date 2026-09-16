package provider

// DESIGN ADAPTATION: These functions live in main.go which is the capstone
// file (6427 lines, 167 connect.* refs). They're stubbed here so the
// intermediate modules compile. Will be removed when main.go is ported.

import (
	"sync/atomic"
	"strings"

	"github.com/urnetwork/connect"
)

// parseProxyAddress splits a proxy address into address, user, password.
func parseProxyAddress(proxyAddress string) (address string, user string, password string) {
	if idx := strings.Index(proxyAddress, "@"); idx >= 0 {
		cred := proxyAddress[:idx]
		address = proxyAddress[idx+1:]
		if uidx := strings.Index(cred, ":"); uidx >= 0 {
			user = cred[:uidx]
			password = cred[uidx+1:]
		} else {
			user = cred
		}
		return
	}
	address = proxyAddress
	return
}

// removeAddressesFromFile removes specific addresses from a proxy config file.
func removeAddressesFromFile(path string, addresses []string) error {
	return nil
}

// readProxyConfig reads the persisted proxy configuration.
func readProxyConfig() *ProxyConfig {
	return &ProxyConfig{}
}

// writeProxyConfig persists the proxy configuration.
func writeProxyConfig(proxyConfig *ProxyConfig) {}

// ProxyConfig holds the persisted proxy list configuration.
type ProxyConfig struct {
	Servers map[string]string `json:"servers"`
	Proxies []string `json:"proxies"`
}

// directProxyKey is the map key for the direct (non-URL) proxy.
// readProxySettingsFromFile reads proxy settings from a file.
func readProxySettingsFromFile(path string) ([]*connect.ProxySettings, error) {
	return nil, nil
}

// readProxySettings reads the current proxy settings.
func readProxySettings() []*connect.ProxySettings {
	return nil
}

// mergeProxyURLCache merges URL-sourced proxies into the desired set.

// DefaultConnectUrl is the fallback connect URL.
const DefaultConnectUrl = "wss://connect.bringyour.com"

// registerProxyV2026 stubs for connect.RegisterProxy removed in v2026.
func registerProxyV2026(_ int, _ string) {}

// proxyBandwidthByAddressV2026 stubs for connect.ProxyBandwidthByAddress removed in v2026.
func proxyBandwidthByAddressV2026(_ string) *ProxyBandwidthV2026 { return &ProxyBandwidthV2026{} }

// ProxyBandwidthV2026 is a stub for the bandwidth info struct removed in v2026.
type ProxyBandwidthV2026 struct {
	Clients atomic.Int64
}

// proxyEarningsScore returns the earnings score for a proxy address.

// connect.ProxyHealthByAddress removed in v2026.
// DESIGN ADAPTATION: stub until health-by-address is reimplemented.
func proxyHealthByAddressV2026() map[string]struct{ Health string } { return map[string]struct{ Health string }{} }


func unregisterProxyStub(_ interface{}) {}
var proxyWarmupDone atomic.Bool
