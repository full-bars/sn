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
	if got != "<unparsable proxy address>" {
		t.Fatalf("redacted address = %q", got)
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

// An IPv6 zone identifier carries '%', so it no longer qualifies as a plain
// address and is masked. The proxy still works; only the log line changes.
func TestRedactedProxyAddressMasksZoneIdentifier(t *testing.T) {
	address := "[fe80::1%eth0]:1080"
	got := redactedProxyAddress(address)
	if got == address {
		t.Fatalf("zone identifier address printed verbatim: %q", got)
	}
	if got != "<unparsable proxy address>" {
		t.Fatalf("redacted zone address = %q", got)
	}
}
