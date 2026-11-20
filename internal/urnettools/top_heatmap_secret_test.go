package urnettools

import "testing"

func TestProxyDisplayAddressRefusesCredentialLookingHosts(t *testing.T) {
	for _, in := range []string{
		"canary-user-7731:canary-pass-9042:example.com:1080",
		"[canary-user-7731:canary-pass-9042]:1080",
	} {
		if got := proxyDisplayAddress(in); got != "?" {
			t.Errorf("proxyDisplayAddress(%q) = %q", in, got)
		}
	}
	for _, in := range []string{"example.com:1080", "[2001:db8::1]:1080"} {
		if got := proxyDisplayAddress(in); got != in {
			t.Errorf("proxyDisplayAddress(%q) = %q", in, got)
		}
	}
}
