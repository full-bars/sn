package connectx

import (
	"github.com/urfoundation/sn/provider/bandwidth"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestPrometheusHandlerReturnsValidFormat(t *testing.T) {
	// This asserts the FULL metric set, including the per-proxy families, so it
	// must run with a pool snapshot registered. The omission path is covered
	// separately by TestPrometheusOmitsProxyPoolWhenSeamUnregistered.
	t.Cleanup(func() { proxyPoolSnapshotPtr.Store(nil) })
	SetProxyPoolSnapshot(func() (int, []string, []string, map[string]*bandwidth.ProxyBandwidth, []string) {
		return 0, nil, nil, map[string]*bandwidth.ProxyBandwidth{}, nil
	})
	handler := PrometheusHandler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	body := w.Body.String()

	// Must contain HELP and TYPE lines for each metric
	requiredMetrics := []string{
		"urnet_uptime_seconds",
		"urnet_proxy_pool_size",
		"urnet_proxy_billable_bytes_total",
		"urnet_proxy_session_age_seconds",
		"urnet_clients_active",
		"urnet_errors_total",
		"urnet_contracts_total",
		"urnet_gc_cycles_total",
		"urnet_mem_heap_bytes",
		"urnet_goroutines",
	}
	for _, m := range requiredMetrics {
		if !strings.Contains(body, m) {
			t.Errorf("missing metric %q in output", m)
		}
		// Each metric must have HELP and TYPE
		if !strings.Contains(body, "# HELP "+m) {
			t.Errorf("missing HELP for %q", m)
		}
		if !strings.Contains(body, "# TYPE "+m) {
			t.Errorf("missing TYPE for %q", m)
		}
	}
}

func TestPrometheusHandlerCounterIncrement(t *testing.T) {
	// Record an error and verify the counter increments
	before := globalProm.errorsTotal[ErrorTransport].Load()
	RecordError(ErrorTransport, "test error for prometheus")

	handler := PrometheusHandler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	after := globalProm.errorsTotal[ErrorTransport].Load()

	if after <= before {
		t.Errorf("error counter did not increment: before=%d after=%d", before, after)
	}

	if !strings.Contains(body, `urnet_errors_total{category="transport"}`) {
		t.Error("missing transport error metric in output")
	}
}

func TestPrometheusHandlerUptimePositive(t *testing.T) {
	handler := PrometheusHandler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	// uptime should be > 0
	if !strings.Contains(body, "urnet_uptime_seconds") {
		t.Error("missing uptime metric")
	}
}

func TestContractsAcquiredTotalTracksIncrements(t *testing.T) {
	before := ContractsAcquiredTotal()
	IncrContractAcquired()
	IncrContractAcquired()
	if got := ContractsAcquiredTotal(); got != before+2 {
		t.Fatalf("ContractsAcquiredTotal = %d, want %d", got, before+2)
	}
}

// TestPrometheusOmitsConnectionsActiveGauge pins the deliberate omission of
// urnet_connections_active on this engine. The counter it would report is
// incremented inside the engine's transport loop and is not reachable from
// outside the library, so the only value available here is 0. Emitting 0 would
// read as "no connections", which is a false signal rather than a missing one,
// so the gauge is omitted instead. If this test starts failing because the
// gauge reappeared, someone re-added it without the engine-side counter.
func TestPrometheusOmitsConnectionsActiveGauge(t *testing.T) {
	handler := PrometheusHandler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Body.String(); strings.Contains(got, "urnet_connections_active") {
		t.Errorf("urnet_connections_active must not be emitted on this engine; "+
			"the counter is engine-internal, so the gauge can only ever report a false zero.\noutput:\n%s", got)
	}
}

// TestEnhancedMetricsProvenance checks the rebuilt pool snapshot reports the
// full key shape callers expect, and that every value comes from a real
// upstream counter rather than a placeholder.
func TestEnhancedMetricsProvenance(t *testing.T) {
	m := EnhancedMetrics()
	for _, key := range []string{
		"hits", "misses", "returns", "active_buffers",
		"pooled_buffers", "gc_pauses", "size_distribution", "last_reset_time",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("EnhancedMetrics missing key %q", key)
		}
	}
	if _, ok := m["size_distribution"].(map[string]uint64); !ok {
		t.Errorf("size_distribution is %T, want map[string]uint64", m["size_distribution"])
	}
	if _, ok := m["gc_pauses"].([]string); !ok {
		t.Errorf("gc_pauses is %T, want []string", m["gc_pauses"])
	}
}

// TestPrometheusOmitsProxyPoolWhenSeamUnregistered pins the deliberate omission
// of the proxy-pool families when no provider has registered the snapshot seam.
// Without this, an unwired build prints urnet_proxy_pool_size{status="up"} 0 on
// every scrape and a scraper concludes the pool is empty, which is a fabricated
// measurement rather than a missing one (#1995).
func TestPrometheusOmitsProxyPoolWhenSeamUnregistered(t *testing.T) {
	proxyPoolSnapshotPtr.Store(nil) // no provider registered

	body := scrapeMetrics(t)
	for _, family := range []string{
		"urnet_proxy_pool_size",
		"urnet_proxy_billable_bytes_total",
		"urnet_proxy_session_age_seconds",
	} {
		if strings.Contains(body, family) {
			t.Errorf("%s must be omitted when no provider registered the pool snapshot; "+
				"a zero there reads as an empty pool.\noutput:\n%s", family, body)
		}
	}
	// The families that do NOT depend on the seam must still be present, so this
	// test cannot pass just because the handler emitted nothing at all.
	for _, family := range []string{"urnet_uptime_seconds", "urnet_goroutines"} {
		if !strings.Contains(body, family) {
			t.Errorf("%s must still be emitted; the seam guards only the proxy-pool families", family)
		}
	}
}

// TestPrometheusEmitsProxyPoolWhenSeamRegistered is the other half: with a
// provider registered, the pool families come back. Together the two tests pin
// that the omission tracks registration and is not a permanent regression.
func TestPrometheusEmitsProxyPoolWhenSeamRegistered(t *testing.T) {
	t.Cleanup(func() { proxyPoolSnapshotPtr.Store(nil) })
	SetProxyPoolSnapshot(func() (int, []string, []string, map[string]*bandwidth.ProxyBandwidth, []string) {
		return 3, []string{"proxy[1] (a:1)"}, nil, map[string]*bandwidth.ProxyBandwidth{}, nil
	})

	body := scrapeMetrics(t)
	if !strings.Contains(body, "urnet_proxy_pool_size") {
		t.Fatalf("urnet_proxy_pool_size must be emitted once a provider registers the snapshot.\noutput:\n%s", body)
	}
	for _, want := range []string{
		`urnet_proxy_pool_size{status="up"} 3`,
		`urnet_proxy_pool_size{status="dead"} 1`,
		`urnet_proxy_pool_size{status="degraded"} 0`,
		`urnet_proxy_pool_size{status="connecting"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in output:\n%s", want, body)
		}
	}
}

// TestProxyPoolSnapshotSeamIsRaceSafe exercises the seam under -race: the write
// happens at provider startup while scrapes read it, so the storage must be
// atomic rather than a plain package var.
func TestProxyPoolSnapshotSeamIsRaceSafe(t *testing.T) {
	t.Cleanup(func() { proxyPoolSnapshotPtr.Store(nil) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				SetProxyPoolSnapshot(func() (int, []string, []string, map[string]*bandwidth.ProxyBandwidth, []string) {
					return 1, nil, nil, nil, nil
				})
				_, _, _, _, _, _ = proxyPoolSnapshot()
			}
		}()
	}
	wg.Wait()
}

// scrapeMetrics renders the exposition exactly as an HTTP scrape would.
func scrapeMetrics(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	PrometheusHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	return w.Body.String()
}
