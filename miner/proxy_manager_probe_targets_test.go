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

// TestProbeHostTableKeepsKnownDeadHostsOut: two hosts were removed because they
// could never pass a TLS probe on :443 - speedtest.tele2.net refuses the
// connection, and connectivity-check.ubuntu.com never completed the probe the
// table exists to run. Restoring either one brought a live provider's direct
// grade back to F, so the table must not carry them again.
func TestProbeHostTableKeepsKnownDeadHostsOut(t *testing.T) {
	knownDead := map[string]string{
		"speedtest.tele2.net":           "refuses :443",
		"connectivity-check.ubuntu.com": "never completes the :443 probe",
	}
	for _, host := range probeHostNames {
		if why, dead := knownDead[host]; dead {
			t.Errorf("probe host %q is a known never-pass host (%s); it must not be in the table", host, why)
		}
	}
}

// TestProbeHostTableStillHasHealthHosts: removing a reputation-class host must
// replace it, not shrink the table. The block sizes the probe samples from, so
// a silently smaller table changes every pass.
func TestProbeHostTableStillHasHealthHosts(t *testing.T) {
	// Exactly, not "at least": a table that silently shrinks changes every
	// pass that samples from it, and only an exact count notices. A deliberate
	// table change updates this number in the same commit.
	if len(probeHostNames) != 127 {
		t.Fatalf("probe host table has %d entries, want 127", len(probeHostNames))
	}
	// The two hosts that replaced the known-dead entries; both are infrastructure
	// endpoints that exist to be fetched and verify a TLS handshake on :443.
	for _, want := range []string{"www.iana.org", "www.debian.org"} {
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
