package provider

// Stubs for connect symbols removed in v2026 connect that are referenced
// by the ported Phase 2+3 collector and heartbeat code.

// MessagePoolSummary was connect.MessagePoolSummary() which returned
// per-pool-size bucket stats. v2026 removed the pool instrumentation.
// Returns nil so the [health][pool] line is skipped.
func messagePoolSummary() interface{ Len() int } { return nil }

// ContractMetricsSnapshot was connect.ContractMetricsSnapshot() returning
// (acquired, denied, utilSum uint64). v2026 removed the counter.
// Returns zero values.
func contractMetricsSnapshot() (acquired, denied, utilSum uint64) {
	return 0, 0, 0
}

// activeProxyConnections returns the current number of active proxy connections.
// In the fork this was connect.ActiveProxyConnections(); v2026 connect
// removed this counter. Stub returns 0.
func activeProxyConnections() int64 {
	return 0
}
