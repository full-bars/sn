package provider

// DESIGN ADAPTATION: Stubs for metrics-related connect symbols removed
// in v2026. Bridges between the fork's connect metrics hooks and the
// provider's own metrics_listen.go endpoint.

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
// Ported from connect's metrics_prometheus.go (connect.PrometheusLabelValue),
// which v2026 removed. Escapes backslash, double-quote, and newlines per the
// Prometheus text format spec, and scrubs invalid UTF-8 so a malformed label
// (e.g. a garbled proxy address) can't corrupt the exposition output.
func prometheusLabelValue(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
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

// prometheusHandlerStub serves providerExtraMetrics(), which is now the
// complete Prometheus text-format payload (the fork's connect.PrometheusHandler
// wrote the base metrics and appended the provider's extra lines; v2026 removed
// that hook, so providerExtraMetrics absorbed the whole payload — see
// metrics_provider.go). Returning nil here left the http.Server falling back
// to http.DefaultServeMux, which serves nothing on /metrics.
func prometheusHandlerStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, providerExtraMetrics())
	})
}

// getDohFailureCountStub returns the provider-level DOH failure count.
// The fork's connect.GetDohFailureCount tracked DNS-over-HTTPS failures
// inside the connect package. v2026 removed this counter, and the provider's
// inert DoH cache (which held its own local counter) was removed. With no
// DoH subsystem active this has no live source, so it reports 0.
func getDohFailureCountStub() int64 {
	return 0
}

// PQETotalCounts holds PQE/classical session counts for metrics.
// The fork's pqeTotalCounts() summed connect.PQECounts (transfer_encrypt.go's
// EncryptionSessionManager.PQECounts(), backed by pqe_tracker.go) across every
// live encryptionManagers entry. v2026 connect's EncryptionSessionManager
// (transfer_encrypt.go:3065) carries no PQE/classical session tracker at
// all — the capability was removed upstream, not just renamed — so there is
// no live data this package can read.
//
// When Measured is false the remaining fields are zero and must NOT be
// interpreted as "no sessions" — they mean "not tracked in v2026".
// Callers (Prometheus, health heartbeat) should suppress or label the
// metric as unavailable rather than emitting zero.
type PQETotalCounts struct {
	ActivePQE, ActiveClas       int
	PQELifetime, ClasLifetime   int
	PQEHour, PQEDay, PQEWeek    int
	ClasHour, ClasDay, ClasWeek int
	Measured                    bool // false = upstream removed PQE accounting
}

func pqeTotalCounts() PQETotalCounts {
	// Measured: false — v2026 connect has no per-session PQE tracker.
	// All numeric fields remain zero. Callers must check Measured before
	// interpreting these as actual counts.
	return PQETotalCounts{}
}
