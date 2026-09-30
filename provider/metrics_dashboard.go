package provider

import (
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// The families below are the ones the shipped Grafana dashboards and alert
// rules (monitoring/) query. sn serves /metrics from providerExtraMetrics
// alone, and these used to be declared only by a stale copy of the fork's
// emitter that sn never linked, so every panel and alert that read them was
// silently empty.
//
// A count nothing produces is left out rather than exported as 0, so a missing
// series means unknown. Sums over the proxy registry are real (an empty
// registry really has zero clients and zero bytes) and stay exported.

// dashboardMetricsInput is everything writeDashboardMetricsFrom renders. The
// live scrape fills it from the health registry, the contract registry, the
// persisted error counts and the runtime; tests fill it directly.
type dashboardMetricsInput struct {
	Up                int
	Dead              []string
	Degraded          []string
	Connecting        []string
	Bandwidth         map[string]*bandwidth.ProxyBandwidth
	ContractsAcquired int64
	ContractsDenied   int64
	// Errors is nil or empty when nothing has been recorded.
	Errors     map[string]uint64
	Goroutines int
}

// writeDashboardMetrics renders the families from the live sources.
func writeDashboardMetrics(b *strings.Builder) {
	up, dead, degraded, bwMap, connecting := ProxyHealthSnapshot()
	acquired, denied := globalContractMetrics.totals()
	writeDashboardMetricsFrom(b, dashboardMetricsInput{
		Up:                up,
		Dead:              dead,
		Degraded:          degraded,
		Connecting:        connecting,
		Bandwidth:         bwMap,
		ContractsAcquired: acquired,
		ContractsDenied:   denied,
		Errors:            snapshotErrors(),
		Goroutines:        runtime.NumGoroutine(),
	})
}

// writeDashboardMetricsFrom renders each family as one contiguous block: HELP,
// TYPE, then its samples. The text format requires that, and strict parsers
// reject samples of one family interleaved with another's. Keys are sorted so
// the output is stable between scrapes.
func writeDashboardMetricsFrom(b *strings.Builder, in dashboardMetricsInput) {
	fmt.Fprintf(b, "# HELP urnet_proxy_pool_size Proxy pool composition.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_pool_size gauge\n")
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"up\"} %d\n", in.Up)
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"dead\"} %d\n", len(in.Dead))
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"degraded\"} %d\n", len(in.Degraded))
	fmt.Fprintf(b, "urnet_proxy_pool_size{status=\"connecting\"} %d\n", len(in.Connecting))

	var perProxy strings.Builder
	var billRx, billTx, rx, tx uint64
	var clients int64
	for _, key := range slices.Sorted(maps.Keys(in.Bandwidth)) {
		bw := in.Bandwidth[key]
		if bw == nil {
			continue
		}
		pRx, pTx := bw.TotalRx.Load(), bw.TotalTx.Load()
		pBillRx, pBillTx := bw.BillableRx.Load(), bw.BillableTx.Load()
		label := prometheusLabelValue(key)
		fmt.Fprintf(&perProxy, "urnet_proxy_billable_bytes_total{proxy=%s,direction=\"in\"} %d\n", label, pBillRx)
		fmt.Fprintf(&perProxy, "urnet_proxy_billable_bytes_total{proxy=%s,direction=\"out\"} %d\n", label, pBillTx)
		rx += pRx
		tx += pTx
		billRx += pBillRx
		billTx += pBillTx
		clients += bw.Clients.Load()
	}
	if perProxy.Len() > 0 {
		fmt.Fprintf(b, "# HELP urnet_proxy_billable_bytes_total Cumulative billable bytes per proxy.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_billable_bytes_total counter\n")
		b.WriteString(perProxy.String())
	}
	fmt.Fprintf(b, "# HELP urnet_billable_bytes_total Aggregate billable bytes for this provider.\n")
	fmt.Fprintf(b, "# TYPE urnet_billable_bytes_total counter\n")
	fmt.Fprintf(b, "urnet_billable_bytes_total{direction=\"in\"} %d\n", billRx)
	fmt.Fprintf(b, "urnet_billable_bytes_total{direction=\"out\"} %d\n", billTx)
	fmt.Fprintf(b, "# HELP urnet_bytes_total Aggregate total bytes for this provider.\n")
	fmt.Fprintf(b, "# TYPE urnet_bytes_total counter\n")
	fmt.Fprintf(b, "urnet_bytes_total{direction=\"in\"} %d\n", rx)
	fmt.Fprintf(b, "urnet_bytes_total{direction=\"out\"} %d\n", tx)
	fmt.Fprintf(b, "# HELP urnet_clients_active Aggregate active clients across all proxies.\n")
	fmt.Fprintf(b, "# TYPE urnet_clients_active gauge\n")
	fmt.Fprintf(b, "urnet_clients_active %d\n", clients)

	if len(in.Errors) > 0 {
		fmt.Fprintf(b, "# HELP urnet_errors_total Cumulative transport errors by category.\n")
		fmt.Fprintf(b, "# TYPE urnet_errors_total counter\n")
		for _, cat := range slices.Sorted(maps.Keys(in.Errors)) {
			fmt.Fprintf(b, "urnet_errors_total{category=%s} %d\n", prometheusLabelValue(cat), in.Errors[cat])
		}
	}

	fmt.Fprintf(b, "# HELP urnet_contracts_total Cumulative contract outcomes.\n")
	fmt.Fprintf(b, "# TYPE urnet_contracts_total counter\n")
	fmt.Fprintf(b, "urnet_contracts_total{result=\"acquired\"} %d\n", in.ContractsAcquired)
	fmt.Fprintf(b, "urnet_contracts_total{result=\"denied\"} %d\n", in.ContractsDenied)

	fmt.Fprintf(b, "# HELP urnet_goroutines Number of goroutines.\n")
	fmt.Fprintf(b, "# TYPE urnet_goroutines gauge\n")
	fmt.Fprintf(b, "urnet_goroutines %d\n", in.Goroutines)
}
