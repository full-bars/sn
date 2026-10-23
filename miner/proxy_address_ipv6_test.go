package miner

import (
	"net"
	"testing"
)

// B9 (65fe949ba): a bracketed IPv6 host must come out of the real parser as
// one address. The provider's per-call pattern split [2001:db8::1]:1080 at its
// inner colons into address "[2001:db8:", user "1]", password "1080". The
// miner's parseProxyAddress (run.go) delegates to parseProxyEntry, so this
// pins the guarantee here instead of trusting the paste fixture.
func TestParseProxyAddressBracketedIPv6(t *testing.T) {
	for _, c := range []struct {
		in       string
		address  string
		user     string
		password string
	}{
		{"[::1]:1080", "[::1]:1080", "", ""},
		{"[2001:db8::1]:1080", "[2001:db8::1]:1080", "", ""},
		{"[2001:db8::1]:1080:bob:pw", "[2001:db8::1]:1080", "bob", "pw"},
		{"[::1]:9050::", "[::1]:9050", "", ""},
		{"[::ffff:1.2.3.4]:1080", "[::ffff:1.2.3.4]:1080", "", ""},
	} {
		t.Run(c.in, func(t *testing.T) {
			address, user, password := parseProxyAddress(c.in)
			if address != c.address || user != c.user || password != c.password {
				t.Fatalf(
					"parseProxyAddress(%q) = address %q user %q password %q, want %q %q %q",
					c.in, address, user, password, c.address, c.user, c.password,
				)
			}
			// The address the caller dials must be the bracketed host:port
			// form, or net.SplitHostPort rejects it with "too many colons".
			if _, _, err := net.SplitHostPort(address); err != nil {
				t.Fatalf("parseProxyAddress(%q) address %q is not dialable: %v", c.in, address, err)
			}
		})
	}

	// The exact shape the provider bug produced, called out separately so a
	// regression names the failure instead of only printing a table diff.
	address, user, password := parseProxyAddress("[2001:db8::1]:1080")
	if address != "[2001:db8::1]:1080" || user != "" || password != "" {
		t.Fatalf(
			"bracketed IPv6 host split at its inner colons: got address %q user %q password %q",
			address, user, password,
		)
	}
}

// B9: the paste path writes the canonical host:port:user:pass form, including
// the [...]::  empty-auth spelling, and the reload path parses that same text
// with parseProxyAddress. The fixture looking right is not enough; the output
// must survive the real parser.
func TestProxyPasteIPv6RoundTripsThroughParse(t *testing.T) {
	for _, c := range []struct {
		in       string
		address  string
		user     string
		password string
	}{
		{"[::1]:1080", "[::1]:1080", "", ""},
		{"[2001:db8::1]:1080", "[2001:db8::1]:1080", "", ""},
		{"[2001:db8::1]:1080:alice:s3cret", "[2001:db8::1]:1080", "alice", "s3cret"},
		{"alice:s3cret@[2001:db8::1]:1080", "[2001:db8::1]:1080", "alice", "s3cret"},
		{"socks5://[::1]:1080", "[::1]:1080", "", ""},
	} {
		t.Run(c.in, func(t *testing.T) {
			canonical := normalizeProxyLine(c.in)
			if canonical == "" {
				t.Fatalf("normalizeProxyLine(%q) rejected the line", c.in)
			}
			address, user, password := parseProxyAddress(canonical)
			if address != c.address || user != c.user || password != c.password {
				t.Fatalf(
					"parseProxyAddress(normalizeProxyLine(%q) = %q) = address %q user %q password %q, want %q %q %q",
					c.in, canonical, address, user, password, c.address, c.user, c.password,
				)
			}
			if _, _, err := net.SplitHostPort(address); err != nil {
				t.Fatalf("round-tripped address %q is not dialable: %v", address, err)
			}
		})
	}
}

// B9: no per-call pattern compile. parseProxyAddress runs for every proxy on
// every reload; a regexp.MustCompile inside it costs hundreds of allocations
// per entry, which is what 65fe949ba hoisted out. The budget is far below one
// compile and far above the measured 0 (parser) and 7 (normalizer) so it only
// trips on a reintroduced compile.
func TestProxyAddressParseDoesNotRecompile(t *testing.T) {
	if allocs := testing.AllocsPerRun(200, func() {
		parseProxyAddress("[2001:db8::1]:1080:alice:s3cret")
	}); allocs > 20 {
		t.Fatalf("parseProxyAddress allocates %.0f times per call; the address pattern is being recompiled", allocs)
	}
	// normalizeProxyLine is the one paste path that does use a pattern
	// (canonicalProxyRe, proxy_manager_paste.go). It must stay package level.
	if allocs := testing.AllocsPerRun(200, func() {
		normalizeProxyLine("[2001:db8::1]:1080:alice:s3cret")
	}); allocs > 20 {
		t.Fatalf("normalizeProxyLine allocates %.0f times per call; canonicalProxyRe is being recompiled", allocs)
	}
}
