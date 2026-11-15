package miner

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/extender"
	"github.com/urnetwork/sdk"
)

// The traffic readings come in the canonical carrier order, one per carrier, so a
// reader never has to sort them or guess which are missing.
func TestExtenderCarrierBytesReadingsCanonicalOrder(t *testing.T) {
	extender.ResetProcessCarrierStats()
	t.Cleanup(extender.ResetProcessCarrierStats)
	got := extenderCarrierBytesReadings(time.Unix(1_800_000_000, 0))
	want := extender.ExtenderCarrierPorts()
	if len(got) != len(want) {
		t.Fatalf("%d readings, want %d", len(got), len(want))
	}
	for i, c := range want {
		if got[i].Carrier != c {
			t.Errorf("reading %d is %q, want %q", i, got[i].Carrier, c)
		}
		if got[i].IngressBytes != 0 || got[i].EgressBytes != 0 || got[i].LiveConns != 0 {
			t.Errorf("%s is not zero before any traffic: %+v", c, got[i])
		}
	}
}

// The control reply carries the relayed traffic beside the served and refused counts.
func TestExtenderControlReadingIncludesCarrierBytes(t *testing.T) {
	resetProviderExtenderObservation()
	t.Cleanup(resetProviderExtenderObservation)
	testExtenderState(t)
	listener := newProviderExtenderStatusListener(nil, 0, "")
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true, ActivatedV4: true})
	reading := providerExtenderControlLatest()
	if reading == nil {
		t.Fatal("no reading published")
	}
	if len(reading.CarrierBytes) != len(extender.ExtenderCarrierPorts()) {
		t.Fatalf("control reading has %d carrier_bytes rows: %+v", len(reading.CarrierBytes), reading.CarrierBytes)
	}
}

// The scrape names the carrier and the direction, and both families are present
// whenever the extender reading is known.
func TestExtenderMetricsCarrierBytes(t *testing.T) {
	withExtenderSeam(t, func() *providerExtenderObservation {
		return &providerExtenderObservation{Known: true, Enabled: true, Listening: true}
	})
	out := minerExtraMetrics()
	for _, want := range []string{
		"# HELP urnet_extender_carrier_bytes_total",
		"# TYPE urnet_extender_carrier_bytes_total counter",
		`urnet_extender_carrier_bytes_total{carrier="tcp-443",direction="in"}`,
		`urnet_extender_carrier_bytes_total{carrier="udp-4053",direction="out"}`,
		"# TYPE urnet_extender_carrier_connections gauge",
		`urnet_extender_carrier_connections{carrier="udp-53"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape is missing %q:\n%s", want, out)
		}
	}
}
