package urnettools

import (
	"strings"
	"testing"
)

func TestProxyIDLabelHidesTheUser(t *testing.T) {
	if got := proxyIDLabel("198.51.100.30:443\x1fcanary-user-7731"); got != "198.51.100.30:443 (user ***)" {
		t.Fatalf("label = %q", got)
	}
	if got := proxyIDLabel("[canary-user-7731:canary-pass-9042]:1080"); strings.Contains(got, "canary") {
		t.Fatalf("bracketed credentials leak: %q", got)
	}
	if got := proxyIDLabel("[fe80::1%eth0]:1080"); got != "<unparsable proxy address>" {
		t.Fatalf("zone address: %q", got)
	}
	if proxyIDLabel("direct") != "(direct)" {
		t.Fatal("direct label")
	}
}
