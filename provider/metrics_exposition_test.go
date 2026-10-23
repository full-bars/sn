package provider

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderMetricsExpositionLints scrapes the handler exactly as a
// provider serves it (connect's families plus providerExtraMetrics) and runs
// the result through the text-format linter. An "info" type, a split family,
// a gauge named _total or a Go-only escape anywhere fails the whole scrape
// in Prometheus, so it fails here.
func TestProviderMetricsExpositionLints(t *testing.T) {

	withTempHome(t)
	stubSetExtraMetricsProvider(providerExtraMetrics)
	t.Cleanup(func() { stubSetExtraMetricsProvider(nil) })

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	prometheusHandlerStub().ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "# TYPE urnet_info gauge") {
		t.Fatalf("provider families missing from the scrape")
	}
	for _, err := range Lint(body) {
		t.Error(err)
	}
}

// A legacy store must report the caveat through the scrape too, not just in
// the log line: an exact 0 there would make the counters look datable.
func TestLifetimeStartMetricsApproximate(t *testing.T) {
	withTempHome(t)
	prev := lifetimeStore
	t.Cleanup(func() { lifetimeStore = prev })

	path := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(path, []byte(`{"pqe_opens":3,"saved_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lifetimeStore = loadLifetimeMetrics(path)

	body := providerExtraMetrics()
	if !strings.Contains(body, "urnet_lifetime_start_approximate 1") {
		t.Fatal("a legacy store must export approximate=1")
	}
	for _, err := range Lint(body) {
		t.Error(err)
	}
}
