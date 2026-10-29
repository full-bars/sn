package miner

import (
	"strings"
	"testing"
)

// A malformed address that carries userinfo in percent-encoded form must not
// reach the log. The old host whitelist accepted '%', so the encoded string was
// treated as a plain host:port and printed verbatim, password included.
func TestRedactedProxyAddressMasksEncodedUserinfo(t *testing.T) {
	address := "socks5%3A%2F%2Falice%3Asecret%40host:1080"

	got := redactedProxyAddress(address)

	for _, leak := range []string{"secret", "alice", "socks5"} {
		if strings.Contains(got, leak) {
			t.Fatalf("redacted address leaks %q: %q", leak, got)
		}
	}
	if !strings.HasPrefix(got, "s") || !strings.HasSuffix(got, "0") {
		t.Fatalf("redacted address should keep first and last character: %q", got)
	}
	if strings.Count(got, "*") != len(address)-2 {
		t.Fatalf("redacted address should mask the middle with one * per character: %q", got)
	}
}

// A plain host:port carries no credentials and stays readable, with no masking
// for operators to decode.
func TestRedactedProxyAddressKeepsPlainHostPort(t *testing.T) {
	for _, address := range []string{"1.2.3.4:1080", "dc.decodo.com:10072", "[2001:db8::1]:1080", "direct"} {
		if got := redactedProxyAddress(address); got != address {
			t.Fatalf("redactedProxyAddress(%q) = %q, want it unchanged", address, got)
		}
	}
}

// Everything that is not a plain host:port renders as first character, a run of
// * for the middle, and the last character.
func TestMaskSecretShape(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"a":       "*",
		"ab":      "**",
		"abc":     "a*c",
		"alice":   "a***e",
		"hunter2": "h*****2",
	}
	for in, want := range cases {
		if got := maskSecret(in); got != want {
			t.Fatalf("maskSecret(%q) = %q, want %q", in, got, want)
		}
	}
}

// An IPv6 zone identifier carries '%', so it no longer qualifies as a plain
// address and is masked. The proxy still works; only the log line changes.
func TestRedactedProxyAddressMasksZoneIdentifier(t *testing.T) {
	address := "[fe80::1%eth0]:1080"
	got := redactedProxyAddress(address)
	if got == address {
		t.Fatalf("zone identifier address printed verbatim: %q", got)
	}
	if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "0") {
		t.Fatalf("masked zone address should keep first and last character: %q", got)
	}
}
