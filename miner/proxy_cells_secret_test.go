package miner

import "testing"

func TestProxyCellSafeAddressRefusesCredentialLookingHosts(t *testing.T) {
	for _, in := range []string{
		"canary-user-7731:canary-pass-9042:example.com:1080",
		"[canary-user-7731:canary-pass-9042]:1080",
		"socks5://canary-user-7731:canary-pass-9042@[canary-user-7731:canary-pass-9042]:1080",
	} {
		if got := proxyCellSafeAddress(3, in); got != "?" {
			t.Errorf("proxyCellSafeAddress(%q) = %q", in, got)
		}
	}
	if got := proxyCellSafeAddress(3, "[2001:db8::1]:1080"); got != "[2001:db8::1]:1080" {
		t.Errorf("an IPv6 proxy address must stay readable, got %q", got)
	}
	if got := proxyCellSafeAddress(0, "x"); got != "direct (this host)" {
		t.Errorf("direct slot: %q", got)
	}
}
