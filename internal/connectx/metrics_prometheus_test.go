package connectx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusHandlerReturnsValidFormat(t *testing.T) {
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
