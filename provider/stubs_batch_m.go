package provider

import "sync"

// DESIGN ADAPTATION: Stubs for connect symbols removed in v2026 connect
// that are referenced by the ported Phase 2+3 collector and heartbeat code.

// MessagePoolSummary was connect.MessagePoolSummary() which returned
// per-pool-size bucket stats. v2026 removed the pool instrumentation.
// Returns nil so the [health][pool] line is skipped.
func messagePoolSummary() interface{ Len() int } { return nil }

// contractMetricsSnapshotPrev tracks the last cumulative (acquired, denied)
// totals handed out, so contractMetricsSnapshot can keep returning a
// since-last-call delta like the fork's swap-and-reset counters did.
var (
	contractMetricsSnapshotMu    sync.Mutex
	contractMetricsSnapshotPrevA int64
	contractMetricsSnapshotPrevD int64
)

// contractMetricsSnapshot was connect.ContractMetricsSnapshot(), which read
// atomics (contractsAcquired, contractsDenied, contractUtilSum) maintained
// deep in connect's transfer_contract_manager.go and swapped them to zero on
// each call. v2026 connect removed that instrumentation entirely.
//
// acquired/denied are real: this package's own globalContractMetrics
// registry (contract_metrics.go) already tracks per-proxy contract outcomes
// for the summary/bandwidth reports, so we diff its cumulative totals since
// the last call to reproduce the same delta-per-scrape behavior.
//
// utilSum (per-contract byte utilization) has no local equivalent — nothing
// in this codebase tracks contract byte utilization, since that lived
// entirely inside connect's contract manager. It stays 0 until that signal
// is ported; callers already guard divide-by-acquired, so this degrades to
// avg_util=0% rather than a crash.
func contractMetricsSnapshot() (acquired, denied, utilSum uint64) {
	a, d := globalContractMetrics.totals()

	contractMetricsSnapshotMu.Lock()
	deltaA := a - contractMetricsSnapshotPrevA
	deltaD := d - contractMetricsSnapshotPrevD
	contractMetricsSnapshotPrevA = a
	contractMetricsSnapshotPrevD = d
	contractMetricsSnapshotMu.Unlock()

	if deltaA < 0 {
		deltaA = 0
	}
	if deltaD < 0 {
		deltaD = 0
	}
	return uint64(deltaA), uint64(deltaD), 0
}

// activeProxyConnections returns the count of proxies currently reporting at
// least one active client. In the fork this was connect.ActiveProxyConnections(),
// an atomic counter maintained in connect's transport layer (transport.go)
// as transports were torn up/down; v2026 connect removed it. We derive the
// same "how many proxies are actively serving traffic" signal from the
// per-proxy client counts this package already tracks (proxy_health.go).
func activeProxyConnections() int64 {
	_, _, _, bw, _ := ProxyHealthSnapshot()
	var serving int64
	for _, b := range bw {
		if b.Clients.Load() > 0 {
			serving++
		}
	}
	return serving
}
