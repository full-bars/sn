package miner

import (
	"strings"
	"testing"
)

// TestProbeHostTableKeepsReputationClassOut pins the policy the table's own
// header states: the reputation class is deliberately ABSENT, not merely
// unsampled, because automated probing of exactly those sites is the traffic
// pattern that gets a provider's egress ip listed. Two of them (reddit,
// stackoverflow) had silently replaced health hosts, so the table needs a test
// and not only a comment.
func TestProbeHostTableKeepsReputationClassOut(t *testing.T) {
	forbidden := []string{"akamai", "reddit", "epic", "stackoverflow", "reuters", "etsy", "ecosia", "canva"}
	for _, host := range probeHostNames {
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(host), bad) {
				t.Errorf("probe host %q is reputation-class (%q); the table must not probe it at all", host, bad)
			}
		}
	}
}

// TestProbeHostTableStillHasHealthHosts: removing a reputation-class host must
// replace it, not shrink the table. The block sizes the probe samples from, so
// a silently smaller table changes every pass.
func TestProbeHostTableStillHasHealthHosts(t *testing.T) {
	if len(probeHostNames) < 100 {
		t.Fatalf("probe host table has %d entries, want the full table (>= 100)", len(probeHostNames))
	}
	// The two hosts the reputation-class entries displaced; both are ordinary
	// reachable health endpoints.
	for _, want := range []string{"connectivity-check.ubuntu.com", "speedtest.tele2.net"} {
		found := false
		for _, host := range probeHostNames {
			if host == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("health host %q missing from the probe table", want)
		}
	}
}
