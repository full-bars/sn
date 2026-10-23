package miner

import "testing"

// While the platform is unreachable every proxy fails at the API stage through
// no fault of its own. Acting on those results demoted and blacklisted healthy
// proxies during the 2026-10-23 outage, so a cycle that never reached the API
// keeps its results out of the demotion path.
func TestNoProbeReachedTheAPI(t *testing.T) {
	apiDown := []probeResult{probeSocks5Only, probeTLSFailed}
	if !noProbeReachedTheAPI(apiDown) {
		t.Fatal("tunnels up with no API answer is a platform outage")
	}
	mixed := []probeResult{probeSocks5Only, probeAPIReachable}
	if noProbeReachedTheAPI(mixed) {
		t.Fatal("one reachable answer proves the platform is up")
	}
	dead := []probeResult{probeDead, probeDead}
	if noProbeReachedTheAPI(dead) {
		t.Fatal("tunnels that never came up are not evidence of a platform outage")
	}
	if noProbeReachedTheAPI(nil) {
		t.Fatal("no results is not an outage")
	}
}
