package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetricsScrapeTarget(t *testing.T) {
	noMarker := filepath.Join(t.TempDir(), "absent")
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := metricsContainerMarker
	t.Cleanup(func() { metricsContainerMarker = orig })

	cases := []struct {
		name       string
		addrs      []string
		container  bool
		wantTarget string
		wantHint   string
	}{
		{"tailscale wins over loopback", []string{"127.0.0.1:9100", "100.64.0.10:9100"}, false, "100.64.0.10:9100", ""},
		{"explicit private address", []string{"10.0.0.5:9101"}, false, "10.0.0.5:9101", ""},
		{"loopback only", []string{"127.0.0.1:9100"}, false, "", "urnet-tools metrics listen"},
		{"ipv6 loopback only", []string{"[::1]:9100"}, false, "", "Install Tailscale"},
		{"wildcard in container", []string{"0.0.0.0:9100"}, true, "", "-p <host Tailscale IP>:9100:9100"},
		{"wildcard on bare metal", []string{"0.0.0.0:9102"}, false, "", "port 9102"},
	}
	for _, c := range cases {
		metricsContainerMarker = noMarker
		if c.container {
			metricsContainerMarker = marker
		}
		target, hint := metricsScrapeTarget(c.addrs)
		if target != c.wantTarget {
			t.Errorf("%s: target = %q, want %q", c.name, target, c.wantTarget)
		}
		if !strings.Contains(hint, c.wantHint) {
			t.Errorf("%s: hint = %q, want it to mention %q", c.name, hint, c.wantHint)
		}
	}
}

func TestPrintMetricsStatus(t *testing.T) {
	var b strings.Builder
	printMetricsStatus(&b, controlResponse{OK: true})
	if !strings.Contains(b.String(), "Metrics: off") || !strings.Contains(b.String(), "urnet-tools metrics on") {
		t.Errorf("off status = %q", b.String())
	}

	b.Reset()
	printMetricsStatus(&b, controlResponse{OK: true, MetricsAddrs: []string{"127.0.0.1:9100", "100.64.0.10:9100"}})
	out := b.String()
	for _, want := range []string{"Metrics: on", "http://127.0.0.1:9100/metrics", "http://100.64.0.10:9100/metrics", "Prometheus target: 100.64.0.10:9100"} {
		if !strings.Contains(out, want) {
			t.Errorf("on status missing %q:\n%s", want, out)
		}
	}

	// A provider older than metrics_addrs: on, but nothing to show.
	b.Reset()
	printMetricsStatus(&b, controlResponse{OK: true, Settings: map[string]SettingInfo{"metrics": {Value: "on"}}})
	if !strings.Contains(b.String(), "reported no listen address") {
		t.Errorf("old-provider status = %q", b.String())
	}
}

func TestMetricsListenKeyAndValidation(t *testing.T) {
	for _, key := range []string{"metrics-listen", "metrics_listen", "METRICS-LISTEN"} {
		if c, ok := canonicalControlKey(key); !ok || c != "metrics_listen" {
			t.Errorf("canonicalControlKey(%q) = %q, %v", key, c, ok)
		}
	}
	for _, v := range []string{"auto", "off", "100.64.0.10:9100", "0.0.0.0:9100", "[::1]:9100"} {
		if err := validateControlValue("metrics_listen", v); err != nil {
			t.Errorf("validateControlValue(metrics_listen, %q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "on", "localhost:9100", "9100", "1.2.3.4", "1.2.3.4:0"} {
		if validateControlValue("metrics_listen", v) == nil {
			t.Errorf("validateControlValue(metrics_listen, %q) accepted a bad value", v)
		}
	}
}
