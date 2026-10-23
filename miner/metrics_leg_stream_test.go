package miner

// The scrape is where an operator looks when a box carries no traffic. The leg
// families must be present even at zero, and the connect stream series must
// reach this scrape rather than only the relay's.

import (
	"strings"
	"testing"
)

func TestMetricsExposeLegAndStreamFamilies(t *testing.T) {
	body := minerExtraMetrics()
	for _, want := range []string{
		"urnet_proxy_leg_failures",
		"urnet_proxy_leg_shadow_flagged",
		"urnet_stream_open_requests_total",
		"urnet_p2p_transports_started_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape is missing %s", want)
		}
	}
}
