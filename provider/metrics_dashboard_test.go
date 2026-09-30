package provider

import (
	"strings"
	"testing"

	"github.com/urfoundation/sn/provider/bandwidth"
)

func dashboardTestBandwidth(rx, tx, billRx, billTx uint64, clients int64) *bandwidth.ProxyBandwidth {
	bw := &bandwidth.ProxyBandwidth{}
	bw.TotalRx.Store(rx)
	bw.TotalTx.Store(tx)
	bw.BillableRx.Store(billRx)
	bw.BillableTx.Store(billTx)
	bw.Clients.Store(clients)
	return bw
}

// The families the shipped Grafana dashboards and alert rules query must
// actually be exported. They were declared only by a stale copy of the fork's
// emitter that sn never linked, so every panel on them was empty.
func TestWriteDashboardMetricsExportsTheQueriedFamilies(t *testing.T) {
	var b strings.Builder
	writeDashboardMetricsFrom(&b, dashboardMetricsInput{
		Up:         3,
		Dead:       []string{"proxy[4] 10.0.0.4:1080"},
		Degraded:   []string{"proxy[5] 10.0.0.5:1080", "proxy[6] 10.0.0.6:1080"},
		Connecting: []string{"proxy[7] 10.0.0.7:1080"},
		Bandwidth: map[string]*bandwidth.ProxyBandwidth{
			"proxy[2] 10.0.0.2:1080": dashboardTestBandwidth(100, 200, 10, 20, 2),
			"proxy[1] 10.0.0.1:1080": dashboardTestBandwidth(1000, 2000, 30, 40, 5),
		},
		ContractsAcquired: 12,
		ContractsDenied:   3,
		Errors:            map[string]uint64{"transport": 7, "auth": 2},
		Goroutines:        4321,
	})
	out := b.String()

	want := []string{
		`urnet_proxy_pool_size{status="up"} 3`,
		`urnet_proxy_pool_size{status="dead"} 1`,
		`urnet_proxy_pool_size{status="degraded"} 2`,
		`urnet_proxy_pool_size{status="connecting"} 1`,
		`urnet_proxy_billable_bytes_total{proxy="proxy[1] 10.0.0.1:1080",direction="in"} 30`,
		`urnet_proxy_billable_bytes_total{proxy="proxy[1] 10.0.0.1:1080",direction="out"} 40`,
		`urnet_proxy_billable_bytes_total{proxy="proxy[2] 10.0.0.2:1080",direction="in"} 10`,
		`urnet_billable_bytes_total{direction="in"} 40`,
		`urnet_billable_bytes_total{direction="out"} 60`,
		`urnet_bytes_total{direction="in"} 1100`,
		`urnet_bytes_total{direction="out"} 2200`,
		`urnet_clients_active 7`,
		`urnet_contracts_total{result="acquired"} 12`,
		`urnet_contracts_total{result="denied"} 3`,
		`urnet_errors_total{category="auth"} 2`,
		`urnet_errors_total{category="transport"} 7`,
		`urnet_goroutines 4321`,
	}
	for _, line := range want {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing sample %q in:\n%s", line, out)
		}
	}
	for _, fam := range []string{
		"urnet_proxy_pool_size", "urnet_proxy_billable_bytes_total", "urnet_billable_bytes_total",
		"urnet_bytes_total", "urnet_clients_active", "urnet_contracts_total", "urnet_errors_total", "urnet_goroutines",
	} {
		if n := strings.Count(out, "# TYPE "+fam+" "); n != 1 {
			t.Errorf("family %s declared %d times, want exactly once", fam, n)
		}
	}
	// per-proxy samples are sorted, so the output is stable between scrapes
	if strings.Index(out, `proxy[1] 10.0.0.1:1080",direction="in"`) > strings.Index(out, `proxy[2] 10.0.0.2:1080",direction="in"`) {
		t.Error("per-proxy samples must be sorted by proxy")
	}
}

// A count nothing produces is left out, never exported as 0, so a missing
// series means unknown and an alert on it cannot be silenced by a fake zero.
func TestWriteDashboardMetricsLeavesOutWhatHasNoSource(t *testing.T) {
	var b strings.Builder
	writeDashboardMetricsFrom(&b, dashboardMetricsInput{Up: 0, Goroutines: 10})
	out := b.String()
	if strings.Contains(out, "urnet_errors_total") {
		t.Errorf("no error counts were recorded, so the family must be omitted:\n%s", out)
	}
	// with no proxies registered the sums are real zeros and stay exported
	if !strings.Contains(out, "urnet_clients_active 0\n") || !strings.Contains(out, `urnet_bytes_total{direction="in"} 0`) {
		t.Errorf("aggregates over an empty registry are real zeros and must be exported:\n%s", out)
	}
	if strings.Contains(out, "urnet_proxy_billable_bytes_total{") {
		t.Errorf("no proxy is registered, so no per-proxy sample may appear:\n%s", out)
	}
}

// Every family in the output must be one contiguous block (HELP, TYPE, then its
// samples); strict parsers reject samples of one family interleaved with another.
func TestWriteDashboardMetricsFamiliesAreContiguous(t *testing.T) {
	var b strings.Builder
	writeDashboardMetricsFrom(&b, dashboardMetricsInput{
		Up: 1,
		Bandwidth: map[string]*bandwidth.ProxyBandwidth{
			"proxy[1] a:1": dashboardTestBandwidth(1, 2, 3, 4, 1),
		},
		Errors: map[string]uint64{"x": 1},
	})
	seen := map[string]bool{}
	current := ""
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var fam string
		switch {
		case strings.HasPrefix(line, "# HELP "):
			fam = strings.Fields(line)[2]
		case strings.HasPrefix(line, "# TYPE "):
			fam = strings.Fields(line)[2]
		default:
			fam = line[:strings.IndexAny(line, "{ ")]
		}
		if fam != current {
			if seen[fam] {
				t.Fatalf("family %s reappears after another family: %q", fam, line)
			}
			seen[fam] = true
			current = fam
		}
	}
}

// The live scrape must include the families: a helper nothing calls would leave
// the dashboards exactly as empty as before.
func TestProviderExtraMetricsIncludesTheDashboardFamilies(t *testing.T) {
	out := providerExtraMetrics()
	for _, fam := range []string{
		"urnet_proxy_pool_size", "urnet_billable_bytes_total", "urnet_bytes_total",
		"urnet_clients_active", "urnet_contracts_total", "urnet_goroutines",
	} {
		if !strings.Contains(out, "# TYPE "+fam+" ") {
			t.Errorf("/metrics does not export %s", fam)
		}
	}
}
