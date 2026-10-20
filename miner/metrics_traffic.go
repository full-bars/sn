package miner

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
)

// metrics_traffic.go emits the v33 connect handler's traffic families from
// the miner's own bandwidth registry (run.go installs it with
// controlRuntime.setBandwidthRegistry): the proxy pool composition, the
// per-proxy byte counters, and the aggregate billable/total byte counters
// plus the live client count.
//
// The registry is keyed by the hash of a proxy's address (run.go
// bwRegistry.Register(int(addressHash(proxyAddr) & 0x7fffffff))), so the
// per-proxy series recover the v33 identity-key label by re-hashing the
// running slots' addresses (controlIdentityKeyedBandwidth).

// writeTrafficMetrics emits the whole block. The pool composition is real
// even with an empty pool; the byte counters are omitted until a provide run
// has published its registry, so a missing series means "not counting yet".
func writeTrafficMetrics(b *strings.Builder) {
	counts, _ := snapshotProxyCounts()
	fmt.Fprintf(b, "# HELP urnet_proxy_pool_size Proxy pool composition.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_pool_size gauge\n")
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"up\"} %d\n", counts.Up)
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"dead\"} %d\n", counts.Dead)
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"degraded\"} %d\n", counts.Degraded)
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"connecting\"} %d\n", counts.Connecting)

	billableRx, billableTx, totalRx, totalTx, clients, ok := controlRuntime.trafficTotals()
	perProxy := controlIdentityKeyedBandwidth()
	if len(perProxy) > 0 {
		// Each family is one contiguous block: HELP, TYPE, then its samples.
		// The text format requires that, and strict parsers reject samples of
		// one family interleaved with another's. Sorted keys keep the output
		// stable between scrapes.
		var proxyBytes, proxyBillable, proxyClients, proxyAge strings.Builder
		for _, key := range slices.Sorted(maps.Keys(perProxy)) {
			bw := perProxy[key]
			if bw == nil {
				continue
			}
			proxyID := prometheusLabelValue(key)
			fmt.Fprintf(&proxyBytes, "urnet_proxy_bytes_total{proxy=%s,direction=\"in\"} %d\n", proxyID, bw.TotalRx.Load())
			fmt.Fprintf(&proxyBytes, "urnet_proxy_bytes_total{proxy=%s,direction=\"out\"} %d\n", proxyID, bw.TotalTx.Load())
			fmt.Fprintf(&proxyBillable, "urnet_proxy_billable_bytes_total{proxy=%s,direction=\"in\"} %d\n", proxyID, bw.BillableRx.Load())
			fmt.Fprintf(&proxyBillable, "urnet_proxy_billable_bytes_total{proxy=%s,direction=\"out\"} %d\n", proxyID, bw.BillableTx.Load())
			fmt.Fprintf(&proxyClients, "urnet_proxy_clients{proxy=%s} %d\n", proxyID, bw.Clients.Load())
			fmt.Fprintf(&proxyAge, "urnet_proxy_session_age_seconds{proxy=%s} %g\n", proxyID, bw.MaxAge().Seconds())
		}
		fmt.Fprintf(b, "# HELP urnet_proxy_bytes_total Cumulative bytes transferred per proxy.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_bytes_total counter\n")
		b.WriteString(proxyBytes.String())
		fmt.Fprintf(b, "# HELP urnet_proxy_billable_bytes_total Cumulative billable bytes per proxy.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_billable_bytes_total counter\n")
		b.WriteString(proxyBillable.String())
		fmt.Fprintf(b, "# HELP urnet_proxy_clients Active clients per proxy.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_clients gauge\n")
		b.WriteString(proxyClients.String())
		fmt.Fprintf(b, "# HELP urnet_proxy_session_age_seconds How long the current client presence window has been active per proxy.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_session_age_seconds gauge\n")
		b.WriteString(proxyAge.String())
	}
	if ok {
		fmt.Fprintf(b, "# HELP urnet_billable_bytes_total Aggregate billable bytes for this provider.\n")
		fmt.Fprintf(b, "# TYPE urnet_billable_bytes_total counter\n")
		fmt.Fprintf(b, "urnet_billable_bytes_total{direction=\"in\"} %d\n", billableRx)
		fmt.Fprintf(b, "urnet_billable_bytes_total{direction=\"out\"} %d\n", billableTx)
		fmt.Fprintf(b, "# HELP urnet_bytes_total Aggregate total bytes for this provider.\n")
		fmt.Fprintf(b, "# TYPE urnet_bytes_total counter\n")
		fmt.Fprintf(b, "urnet_bytes_total{direction=\"in\"} %d\n", totalRx)
		fmt.Fprintf(b, "urnet_bytes_total{direction=\"out\"} %d\n", totalTx)
		fmt.Fprintf(b, "# HELP urnet_clients_active Aggregate active clients across all proxies.\n")
		fmt.Fprintf(b, "# TYPE urnet_clients_active gauge\n")
		fmt.Fprintf(b, "urnet_clients_active %d\n", clients)
	}
}

// controlIdentityKeyedBandwidth maps the run's bandwidth registry (keyed by
// address hash) back onto the running slots' identity keys, the labels the
// v33 per-proxy series use.
func controlIdentityKeyedBandwidth() map[string]*bandwidth.ProxyBandwidth {
	registry := controlRuntime.bandwidthSnapshot()
	if registry == nil {
		return nil
	}
	out := make(map[string]*bandwidth.ProxyBandwidth)
	for _, key := range controlRuntime.runningKeySnapshot() {
		addr, _ := connect.SplitProxyKey(key)
		if addr == "" {
			continue
		}
		idx := int(addressHash(addr) & 0x7fffffff)
		if bw, ok := registry[idx]; ok && bw != nil {
			out[key] = bw
		}
	}
	return out
}
