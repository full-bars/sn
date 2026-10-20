package provider

import (
	"strings"
	"testing"
)

// The connect module owns the stream and transport accounting; sn serves the
// whole /metrics payload from providerExtraMetrics, so the connect exposition
// must be appended there or those series never reach a scrape.
func TestProviderExtraMetricsIncludesConnectStreamSeries(t *testing.T) {
	out := providerExtraMetrics()
	for _, want := range []string{
		"urnet_stream_sequences_by_kind",
		"urnet_stream_open_requests_total",
		"urnet_stream_control_frames_total",
		"urnet_tls_handshakes_total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider scrape is missing the connect series %q", want)
		}
	}
	// a single family must not be split across the payload
	if got := strings.Count(out, "# TYPE urnet_stream_open_requests_total counter"); got != 1 {
		t.Fatalf("urnet_stream_open_requests_total declared %d times, want 1", got)
	}
}
