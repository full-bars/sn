package provider

// DESIGN ADAPTATION: metricBytesToMiB was in fork's main.go:1299.
// Real implementation ported from fork.

import (
	"runtime/metrics"
)

// activeConnectionCount returns the current number of active client
// connections across all registered proxies. The fork sourced this from
// connect.ActiveConnectionCount(), an atomic counter incremented deep in
// connect's IP-layer code (ip.go); v2026 connect exposes no equivalent.
// Instead we sum the per-proxy client-session counts that this package
// already tracks in proxy_health.go/bandwidth for the [health] report and
// bandwidth_reporter.go, which is the same signal at proxy granularity.
func activeConnectionCount() int64 {
	_, _, _, bw, _ := ProxyHealthSnapshot()
	var total int64
	for _, b := range bw {
		total += b.Clients.Load()
	}
	return total
}

// metricBytesToMiB converts a runtime/metrics value to MiB.
// Real implementation ported from fork main.go:1299.
func metricBytesToMiB(name string, v metrics.Value) uint64 {
	switch v.Kind() {
	case metrics.KindUint64:
		return v.Uint64() / 1024 / 1024
	case metrics.KindFloat64:
		return uint64(v.Float64()) / 1024 / 1024
	default:
		tlog("[health] warning: metric %q has unreadable kind %v — check metric name\n", name, v.Kind())
		return 0
	}
}
