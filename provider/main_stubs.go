package provider

// DESIGN ADAPTATION: These functions live in main.go which is the capstone
// file (6427 lines, 167 connect.* refs). They're stubbed here so the
// intermediate modules compile. Will be removed when main.go is ported.

import (
	"sync/atomic"

	"github.com/docopt/docopt-go"
	"github.com/urfoundation/sn/provider/bandwidth"
)

// directProxyKey is the map key for the direct (non-URL) proxy.

// mergeProxyURLCache merges URL-sourced proxies into the desired set.

// DefaultConnectUrl is the fallback connect URL.
const DefaultConnectUrl = "wss://connect.bringyour.com"

// registerProxyV2026 stubs for connect.RegisterProxy removed in v2026.
func registerProxyV2026(idx int, addr string) { RegisterProxy(idx, addr) }

// proxyBandwidthByAddressV2026 stubs for connect.ProxyBandwidthByAddress removed in v2026.
func proxyBandwidthByAddressV2026(addr string) *bandwidth.ProxyBandwidth { return ProxyBandwidthByAddress(addr) }

// ProxyBandwidthV2026 is a stub for the bandwidth info struct removed in v2026.

// proxyEarningsScore returns the earnings score for a proxy address.

// connect.ProxyHealthByAddress removed in v2026.
// DESIGN ADAPTATION: stub until health-by-address is reimplemented.
func proxyHealthByAddressV2026() map[string]ProxyHealthStatus { return ProxyHealthByAddress() }

func unregisterProxyStub(idx interface{}) { if i, ok := idx.(int); ok { UnregisterProxy(i) } }
var proxyWarmupDone atomic.Bool

// VersionStamp is an alternative version marker embedded as program data.
var VersionStamp string

// Ensure connect import is used.
var _ = docopt.Opts(nil)
