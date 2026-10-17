package provider

import "testing"

func TestParseProxyAddress(t *testing.T) {
	for _, c := range []struct {
		in, address, user, password string
	}{
		{"1.2.3.4:1080", "1.2.3.4:1080", "", ""},
		{"1.2.3.4:1080:alice:s3cret", "1.2.3.4:1080", "alice", "s3cret"},
		{"host.example.com:8000:u:p", "host.example.com:8000", "u", "p"},
		{"1.2.3.4:1080::", "1.2.3.4:1080", "", ""},
		{"1.2.3.4:1080:onlyuser", "1.2.3.4:1080:onlyuser", "", ""},
		{"", "", "", ""},
		// IPv6 hosts in brackets
		{"[2001:db8::1]:1080", "[2001:db8::1]:1080", "", ""},
		{"[2001:db8::1]:1080:bob:pw", "[2001:db8::1]:1080", "bob", "pw"},
		{"[::1]:9050", "[::1]:9050", "", ""},
		{"[::1]:9050::", "[::1]:9050", "", ""},
		{"[2001:db8::1]:1080:u:p:extra", "[2001:db8::1]:1080:u:p:extra", "", ""},
		{"[2001:db8::1]:1080:onlyuser", "[2001:db8::1]:1080:onlyuser", "", ""},
		{"[2001:db8::1]", "[2001:db8::1]", "", ""},
		{"[2001:db8::1]:port", "[2001:db8::1]:port", "", ""},
		{"[unterminated:1080", "[unterminated:1080", "", ""},
	} {
		address, user, password := parseProxyAddress(c.in)
		if address != c.address || user != c.user || password != c.password {
			t.Errorf("parseProxyAddress(%q) = %q %q %q, want %q %q %q", c.in, address, user, password, c.address, c.user, c.password)
		}
	}
}

// The regexp is compiled once at package init, so a call allocates only what
// matching needs, not a compiled program.
func TestParseProxyAddressDoesNotRecompile(t *testing.T) {
	allocs := testing.AllocsPerRun(200, func() {
		parseProxyAddress("1.2.3.4:1080:alice:s3cret")
	})
	if allocs > 20 {
		t.Fatalf("parseProxyAddress allocates %.0f times per call; the pattern is being recompiled", allocs)
	}
}
