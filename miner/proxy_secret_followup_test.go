package miner

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func TestRedactedProxyAddressRefusesCredentialLookingHosts(t *testing.T) {
	const bad = "<unparsable proxy address>"
	for _, in := range []string{
		"[canary-user-7731:canary-pass-9042]:1080",
		"canary-user-7731:canary-pass-9042:example.com:1080",
		"example.com:0",
		"[fe80::1%]:1080",
		"[fe80::1%eth0]:1080",
	} {
		if got := redactedProxyAddress(in); got != bad {
			t.Errorf("redactedProxyAddress(%q) = %q", in, got)
		}
	}
	for _, in := range []string{"example.com:1080", "203.0.113.7:1080", "[2001:db8::1]:1080", "direct"} {
		if got := redactedProxyAddress(in); got != in {
			t.Errorf("redactedProxyAddress(%q) = %q", in, got)
		}
	}
}

// The metrics label of a proxy identity must not carry its user, and two
// accounts behind one gateway must stay two series.
func TestProxyMetricLabelsHideUsersAndStayDistinct(t *testing.T) {
	alice := &connect.ProxySettings{Address: "198.51.100.30:443", Auth: &proxy.Auth{User: "canary-user-7731", Password: "canary-pass-9042"}}
	bob := &connect.ProxySettings{Address: "198.51.100.30:443", Auth: &proxy.Auth{User: "canary-user-8842", Password: "canary-pass-1177"}}
	plain := &connect.ProxySettings{Address: "203.0.113.9:1080"}
	keys := []string{alice.Key(), bob.Key(), plain.Key()}
	labels := proxyMetricLabels(keys)
	seen := map[string]bool{}
	for _, k := range keys {
		l := labels[k]
		if strings.Contains(l, "canary") || strings.Contains(l, "\x1f") {
			t.Errorf("label leaks: %q", l)
		}
		if seen[l] {
			t.Errorf("duplicate label %q", l)
		}
		seen[l] = true
	}
}
