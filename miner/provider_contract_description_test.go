package miner

// A contract line has to name the path it ran on, or a contract that never
// establishes cannot be traced to a proxy. The miner has no proxy index, so the
// slot index and the address carry that meaning, and the direct slot says so.

import "testing"

func TestContractProxyDescriptionNamesThePath(t *testing.T) {
	cases := []struct {
		name      string
		slot      uint64
		proxyAddr string
		expected  string
	}{
		{"direct slot", 0, "", "proxy[0] (direct)"},
		{"proxy slot", 3, "1.2.3.4:8080", "proxy[3] (1.2.3.4:8080)"},
		{"credentialed proxy is described by address", 1, "proxy.example:1080", "proxy[1] (proxy.example:1080)"},
	}
	for _, c := range cases {
		if got := contractProxyDescription(c.slot, c.proxyAddr); got != c.expected {
			t.Errorf("%s: %q, expected %q", c.name, got, c.expected)
		}
	}
}
