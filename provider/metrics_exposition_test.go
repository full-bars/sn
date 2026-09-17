package provider

import (
	"net/http/httptest"
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
