//go:build ignore

package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestDashboardQueriesExportedMetrics: every urnet_* metric the shipped
// Grafana dashboards query must be declared by the /metrics emitters. A
// rename in the emitter otherwise leaves panels silently empty for every
// user of the monitoring bundle.
//
// The emitter source is scanned rather than a live scrape because some
// families (lifetime counters, proxy grades) are only written once their
// state exists, which a test process does not have.
func TestDashboardQueriesExportedMetrics(t *testing.T) {
	declared := map[string]bool{}
	typeLine := regexp.MustCompile(`# TYPE (urnet_[a-z0-9_]+) `)
	for _, src := range []string{"../metrics_prometheus.go", "metrics_provider.go"} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range typeLine.FindAllStringSubmatch(string(data), -1) {
			declared[m[1]] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no # TYPE declarations found in the emitters")
	}

	dashboards, err := filepath.Glob("../monitoring/grafana/dashboards/*.json")
	if err != nil || len(dashboards) == 0 {
		t.Fatalf("no dashboards found: %v", err)
	}
	metricRef := regexp.MustCompile(`\burnet_[a-z0-9_]+`)
	for _, path := range dashboards {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(data) {
			t.Errorf("%s is not valid JSON", path)
			continue
		}
		for _, name := range metricRef.FindAllString(string(data), -1) {
			if !declared[name] {
				t.Errorf("%s queries %s, which /metrics does not export", filepath.Base(path), name)
			}
		}
	}
}
