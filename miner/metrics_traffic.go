package miner

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// metrics_traffic.go emits the v33 connect handler's traffic families from
// the miner's own bandwidth registry (run.go installs it with
// controlRuntime.setBandwidthRegistry): the proxy pool composition, the
// per-proxy byte counters, the aggregate billable/total byte counters plus
// the live client count, and the settled cross-check
// (urnet_contract_used_bytes_total), read from the same entries as the
// billable aggregate so the two are comparable in one scrape.
//
// The registry is keyed by the hash of a proxy's IDENTITY key
// (run.go bwRegistry.Register(proxyIdentityBandwidthIndex(proxySettings)),
// and that index is addressHash(settings.Key())), so the per-proxy series
// recover the v33 identity-key label by hashing the same running slot key
// (controlIdentityKeyedBandwidth). Hashing only the address would miss every
// credentialed proxy: two accounts behind one gateway have different keys and
// different counter indices.

// writeTrafficMetrics emits the whole block. The pool composition is real
// even with an empty pool; the byte counters are omitted until a provide run
// has published its registry, so a missing series means "not counting yet".
func writeTrafficMetrics(b *strings.Builder) {
	// One proxy.state read per scrape: the composition feeds both
	// urnet_proxy_pool_size and the pool aggregate. The aggregate is passed the
	// same value, so the two can never disagree about a member, and the
	// pool-health mutex is never held across the read.
	comp := readProxyPoolComposition()
	// The composition metric keeps the registry's buckets (persisted grades
	// with the live overlay); the aggregate below judges the honest Counts.
	counts := comp.Registry
	fmt.Fprintf(b, "# HELP urnet_proxy_pool_size Proxy pool composition.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_pool_size gauge\n")
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"up\"} %d\n", counts.Up)
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"dead\"} %d\n", counts.Dead)
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"degraded\"} %d\n", counts.Degraded)
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"connecting\"} %d\n", counts.Connecting)
	// The aggregate that composition cannot express: whether the pool failed as
	// a unit or a few members did, and how low it got in the last hour.
	writeProxyPoolHealthMetrics(b, proxyPoolHealth(time.Now(), comp))

	reading := controlRuntime.trafficReading()
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
	if reading.OK {
		fmt.Fprintf(b, "# HELP urnet_billable_bytes_total Aggregate billable bytes for this provider.\n")
		fmt.Fprintf(b, "# TYPE urnet_billable_bytes_total counter\n")
		fmt.Fprintf(b, "urnet_billable_bytes_total{direction=\"in\"} %d\n", reading.BillableRx)
		fmt.Fprintf(b, "urnet_billable_bytes_total{direction=\"out\"} %d\n", reading.BillableTx)
		// The settled cross-check, from the same entries and in the same scrape
		// as the billable series above: the engine's per-contract accounting is
		// the number the platform settles on, so the difference between the two
		// families is the drift this node is carrying. Same direction labels and
		// the same in/out pairing as the provider's own dashboard
		// (provider/metrics_dashboard.go), so one panel reads both fleets the
		// same way. Emitted only when the billable series is: a settled figure
		// with nothing to compare it against is not a cross-check.
		fmt.Fprintf(b, "# HELP urnet_contract_used_bytes_total Bytes the platform contracts have used, from the engine's per-contract accounting, summed over the proxies this node counts (like urnet_billable_bytes_total, the totals continue across a restart): the quantity the platform settles on, kept as a cross-check against the counted billable bytes. in is receive-side contracts, out is send-side.\n")
		fmt.Fprintf(b, "# TYPE urnet_contract_used_bytes_total counter\n")
		fmt.Fprintf(b, "urnet_contract_used_bytes_total{direction=\"in\"} %d\n", reading.ContractUsedIngress)
		fmt.Fprintf(b, "urnet_contract_used_bytes_total{direction=\"out\"} %d\n", reading.ContractUsedEgress)
		fmt.Fprintf(b, "# HELP urnet_bytes_total Aggregate total bytes for this provider.\n")
		fmt.Fprintf(b, "# TYPE urnet_bytes_total counter\n")
		fmt.Fprintf(b, "urnet_bytes_total{direction=\"in\"} %d\n", reading.TotalRx)
		fmt.Fprintf(b, "urnet_bytes_total{direction=\"out\"} %d\n", reading.TotalTx)
		fmt.Fprintf(b, "# HELP urnet_clients_active Aggregate active clients across all proxies.\n")
		fmt.Fprintf(b, "# TYPE urnet_clients_active gauge\n")
		fmt.Fprintf(b, "urnet_clients_active %d\n", reading.Clients)
	}
}

// controlIdentityKeyedBandwidth maps the run's bandwidth registry (keyed by the
// identity index) onto the running slots' identity keys, the labels the v33
// per-proxy series use.
func controlIdentityKeyedBandwidth() map[string]*bandwidth.ProxyBandwidth {
	registry := controlRuntime.bandwidthSnapshot()
	if registry == nil {
		return nil
	}
	out := make(map[string]*bandwidth.ProxyBandwidth)
	for _, key := range controlRuntime.runningKeySnapshot() {
		if key == "" {
			continue
		}
		// The registry index is addressHash(settings.Key()), and a running
		// slot key IS settings.Key(). Hash the key, never the address
		// SplitProxyKey would return: for a credentialed proxy the registry
		// stored the counter under address+user, so an address-only lookup
		// finds nothing and the proxy silently loses its series.
		idx := int(addressHash(key) & 0x7fffffff)
		if bw, ok := registry[idx]; ok && bw != nil {
			out[key] = bw
		}
	}
	return out
}
