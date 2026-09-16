package provider

import "runtime/metrics"

// activeConnectionCount returns the current number of active connections.
// In the fork this was connect.ActiveConnectionCount(); v2026 connect
// removed this counter. Stub returns 0 until a local counter is wired.
func activeConnectionCount() int64 {
	return 0
}

// metricBytesToMiB converts a runtime/metrics Value to MiB. Checks Kind before
// dispatching to avoid panics; logs a warning and returns 0 for unrecognised kinds
// so a wrong metric name surfaces in logs rather than silently reading 0.
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
