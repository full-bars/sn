package provider

// DESIGN ADAPTATION: metricBytesToMiB was in fork's main.go:1299.
// Real implementation ported from fork.

import (
	"runtime/metrics"
)

// activeConnectionCount returns the current number of active connections.
// Stub returns 0; real implementation needs connect.ActiveProxyConnections()
// which was removed in v2026.
func activeConnectionCount() int64 { return 0 }

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
