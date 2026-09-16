package provider

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// metricsProviderLog formats timestamped log output for the metrics provider.
func metricsProviderLog(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("2006-01-02T15:04:05.000")+" [metrics-prov] "+format, args...)
}

// prometheusLabelValue wraps a string for use as a Prometheus label value.
// In the fork this was connect.PrometheusLabelValue; v2026 connect removed it.
// The only transformation is quoting special characters.
//
// DESIGN ADAPTATION: connect.PrometheusLabelValue was removed in v2026.
// The function only quoted label values containing special characters.
// We replicate that behavior locally.
func prometheusLabelValue(s string) string {
	if strings.ContainsAny(s, `"\`) {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return `"` + s + `"`
	}
	return `"` + s + `"`
}

// stubMetricsProvider is a no-op extra metrics provider.
// The fork called connect.SetExtraMetricsProvider(providerExtraMetrics);
// v2026 connect removed this hook. We call providerExtraMetrics directly
// from the control socket metrics handler instead.
//
// DESIGN ADAPTATION: The fork's SetExtraMetricsProvider let the provider
// inject custom Prometheus lines into connect's default /metrics handler.
// v2026 removed this hook. The provider now serves its own /metrics endpoint
// via metrics_listen.go, so this bridge is no longer needed.
func stubMetricsProvider() string {
	return providerExtraMetrics()
}

// stubSetPersistentErrorFunc is a no-op.
// The fork's connect.SetPersistentErrorFunc wired error categorization.
// v2026 removed this; we handle persistent errors locally via
// the persistentErrors type in this package.
//
// DESIGN ADAPTATION: connect.SetPersistentErrorFunc was removed in v2026.
// The error categorization is now handled entirely within the provider
// package via IncrPersistentError and snapshotErrors.
func stubSetPersistentErrorFunc(_ func(string)) {}

// initMetricsStubs wires the local metric bridges.
// Called once at provider startup.
func initMetricsStubs() {
	// In the fork, this would call:
	//   connect.SetExtraMetricsProvider(providerExtraMetrics)
	//   connect.SetPersistentErrorFunc(func(cat string) { IncrPersistentError(cat) })
	// v2026 removed both hooks, so we just call providerExtraMetrics directly
	// from the control socket handler.
	metricsProviderLog("metrics stubs initialized (v2026 adaptation: direct provider metrics)\n")
}

// stubSetExtraMetricsProvider accepts the extra metrics provider function but
// does nothing — v2026 connect removed the hook. The provider serves its own
// /metrics endpoint directly via metrics_listen.go.
//
// DESIGN ADAPTATION: connect.SetExtraMetricsProvider was removed in v2026.
func stubSetExtraMetricsProvider(_ func() string) {}

// prometheusHandlerStub returns nil — the fork used connect.PrometheusHandler()
// which v2026 removed. The provider now serves its own /metrics endpoint
// via the metricsListen multi-listener.
//
// DESIGN ADAPTATION: connect.PrometheusHandler was removed in v2026.
// The provider serves /metrics directly via metrics_listen.go.
func prometheusHandlerStub() http.Handler {
	return nil
}

// getDohFailureCountStub returns 0.
// The fork's connect.GetDohFailureCount tracked DNS-over-HTTPS failures
// inside the connect package. v2026 removed this counter.
//
// DESIGN ADAPTATION: connect.GetDohFailureCount was removed in v2026.
// The provider tracks its own DOH failures locally via doh_cache.go.
func getDohFailureCountStub() int64 {
	return 0
}

// PQETotalCounts holds PQE/classical session counts for metrics.
// The fork's pqeTotalCounts() was from pqe_tracker.go.
// This is a minimal stub until that module is ported.
//
// DESIGN ADAPTATION: pqe_tracker.go hasn't been ported yet.
// Returns zero values so metrics_provider.go compiles.
type PQETotalCounts struct {
	ActivePQE, ActiveClas                     int
	PQELifetime, ClasLifetime                 int
	PQEHour, PQEDay, PQEWeek                  int
	ClasHour, ClasDay, ClasWeek               int
}

func pqeTotalCounts() PQETotalCounts {
	return PQETotalCounts{}
}


