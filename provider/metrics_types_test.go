package provider

import (
	"regexp"
	"strings"
	"testing"
)

// TestProviderMetricsUseClassicTypes: the handler serves the classic
// Prometheus text format, which accepts only these types. One "info" line
// made Prometheus reject the whole scrape.
func TestProviderMetricsUseClassicTypes(t *testing.T) {
	allowed := map[string]bool{"counter": true, "gauge": true, "histogram": true, "summary": true, "untyped": true}
	typeLine := regexp.MustCompile(`^# TYPE (\S+) (\S+)$`)
	out := providerExtraMetrics()
	found := 0
	for _, line := range strings.Split(out, "\n") {
		m := typeLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		found++
		if !allowed[m[2]] {
			t.Errorf("metric %s declares type %q, which the classic text format rejects", m[1], m[2])
		}
	}
	if found == 0 {
		t.Fatal("no # TYPE lines found in provider metrics output")
	}
}

// TestUrnetInfoHasOnlyStableLabels: labels that change between scrapes
// (uptime, proxy count) create a new series every scrape.
func TestUrnetInfoHasOnlyStableLabels(t *testing.T) {
	out := providerExtraMetrics()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "urnet_info{") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("urnet_info sample not found")
	}
	if !regexp.MustCompile(`^urnet_info\{version="[^"]*"\} 1$`).MatchString(line) {
		t.Fatalf("urnet_info = %q, want only a version label and value 1", line)
	}
}
