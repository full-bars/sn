package miner

import (
	"testing"
	"time"
)

// A host that has never been measured is charged the conservative two posts,
// so an early pass can never overspend the operator's window.
func TestHostActivationPostsUnmeasuredIsConservative(t *testing.T) {
	testExtenderState(t)
	if got := hostActivationPosts(); got != 2 {
		t.Fatalf("unmeasured host charged %d posts, want 2", got)
	}
}

// A host measured without usable IPv6 posts once per pass, so it gets five
// passes in the operator's window instead of two.
func TestHostActivationPostsWithoutIPv6IsOne(t *testing.T) {
	testExtenderState(t)
	writeHostCapability(hostCapability{IPv6Usable: false, CheckedAtUnix: 1_800_000_000})
	if got := hostActivationPosts(); got != 1 {
		t.Fatalf("ipv4-only host charged %d posts, want 1", got)
	}
}

// A dual-stack host still posts twice, which is what the budget was built for.
func TestHostActivationPostsWithIPv6IsTwo(t *testing.T) {
	testExtenderState(t)
	writeHostCapability(hostCapability{IPv6Usable: true, CheckedAtUnix: 1_800_000_000})
	if got := hostActivationPosts(); got != 2 {
		t.Fatalf("dual-stack host charged %d posts, want 2", got)
	}
}

// The recheck cadence follows what the host has demonstrated: a host that has
// IPv6 is rechecked often, one that never had it is rechecked rarely, and one
// that lost it recently is treated as flapping.
func TestCapabilityRecheckInterval(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name string
		cap  hostCapability
		want time.Duration
	}{
		{"usable now", hostCapability{IPv6Usable: true, CheckedAtUnix: now.Unix()}, capabilityRecheckHadIPv6Recently},
		{"never had it", hostCapability{IPv6Usable: false}, capabilityRecheckNeverHadIPv6},
		{"lost it an hour ago", hostCapability{
			IPv6Usable:           false,
			EverUsable:           true,
			LastIPv6UsableAtUnix: now.Add(-time.Hour).Unix(),
		}, capabilityRecheckHadIPv6Recently},
		{"lost it a week ago", hostCapability{
			IPv6Usable:           false,
			EverUsable:           true,
			LastIPv6UsableAtUnix: now.Add(-7 * 24 * time.Hour).Unix(),
		}, capabilityRecheckHadIPv6LongAgo},
	}
	for _, c := range cases {
		if got := capabilityRecheckInterval(c.cap, now); got != c.want {
			t.Errorf("%s: interval = %s, want %s", c.name, got, c.want)
		}
	}
}

// The refusal backoff must never exceed the operator's own hourly window: a
// longer park keeps a healthy host out of the extender role for longer than
// the server ever would.
func TestRefusalBackoffCapsAtTheWindow(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	var last time.Duration
	for i := 0; i < 12; i++ {
		last = recordExtenderActivationRefused(now.Add(time.Duration(i)*time.Minute), extenderTestClientA)
		if last > extenderActivationWindow {
			t.Fatalf("refusal %d waited %s, longer than the %s window", i+1, last, extenderActivationWindow)
		}
	}
	if last != extenderActivationWindow {
		t.Fatalf("after many refusals the wait is %s, want the window %s", last, extenderActivationWindow)
	}
}

// A streak that goes quiet for a day starts over, so refusals spread across
// weeks cannot escalate today's into the maximum.
func TestRefusalStreakDecaysAfterADay(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < 5; i++ {
		recordExtenderActivationRefused(now, extenderTestClientA)
	}
	// A day and a half later, the streak must start from the first step.
	later := now.Add(36 * time.Hour)
	if wait := recordExtenderActivationRefused(later, extenderTestClientA); wait != extenderRefusalBackoff {
		t.Fatalf("after a quiet day the wait is %s, want the first step %s", wait, extenderRefusalBackoff)
	}
}
