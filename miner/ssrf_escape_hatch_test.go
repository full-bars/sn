package miner

import (
	"net"
	"testing"
)

// setSSRFFlags sets both SSRF switches for a test and restores them after.
func setSSRFFlags(t *testing.T, loopback, private bool) {
	t.Helper()
	prevL, prevP := ssrfAllowLoopback.Load(), ssrfAllowPrivate.Load()
	ssrfAllowLoopback.Store(loopback)
	ssrfAllowPrivate.Store(private)
	t.Cleanup(func() {
		ssrfAllowLoopback.Store(prevL)
		ssrfAllowPrivate.Store(prevP)
	})
}

func TestSSRFEscapeHatchPredicate(t *testing.T) {
	cases := []struct {
		name       string
		ip         string
		blockedOff bool // ssrfAllowPrivate=false
		blockedOn  bool // ssrfAllowPrivate=true
	}{
		{"loopback v4", "127.0.0.1", true, false},
		{"loopback v4 other", "127.8.9.10", true, false},
		{"loopback v6", "::1", true, false},
		{"10/8", "10.0.0.1", true, false},
		{"172.16/12 low", "172.16.0.1", true, false},
		{"docker bridge", "172.17.0.1", true, false},
		{"172.16/12 high", "172.31.255.255", true, false},
		{"192.168/16", "192.168.1.1", true, false},
		{"ULA fc", "fc00::1", true, false},
		{"ULA fd", "fd12:3456::1", true, false},
		{"link-local v4", "169.254.1.1", true, true},
		{"metadata", "169.254.169.254", true, true},
		{"link-local v6", "fe80::1", true, true},
		{"multicast v4", "224.0.0.1", true, true},
		{"multicast v6", "ff02::1", true, true},
		{"link-local multicast v4", "224.0.0.251", true, true},
		{"unspecified v4", "0.0.0.0", true, true},
		{"unspecified v6", "::", true, true},
		{"public v4", "8.8.8.8", false, false},
		{"public v6", "2001:4860:4860::8888", false, false},
		{"just below 172.16", "172.15.255.255", false, false},
		{"just above 172.31", "172.32.0.1", false, false},
		// IPv4-mapped IPv6 must classify like the plain IPv4 form.
		{"mapped 10/8", "::ffff:10.0.0.1", true, false},
		{"mapped loopback", "::ffff:127.0.0.1", true, false},
		{"mapped metadata", "::ffff:169.254.169.254", true, true},
		{"mapped public", "::ffff:8.8.8.8", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("bad test ip %q", c.ip)
			}
			setSSRFFlags(t, false, false)
			if got := isBlockedSourceIP(ip); got != c.blockedOff {
				t.Errorf("flag off: isBlockedSourceIP(%s)=%v want %v", c.ip, got, c.blockedOff)
			}
			setSSRFFlags(t, false, true)
			if got := isBlockedSourceIP(ip); got != c.blockedOn {
				t.Errorf("flag on: isBlockedSourceIP(%s)=%v want %v", c.ip, got, c.blockedOn)
			}
		})
	}
}

func TestSSRFEscapeHatchNilIPAlwaysBlocked(t *testing.T) {
	setSSRFFlags(t, true, true)
	if !isBlockedSourceIP(nil) {
		t.Fatal("nil ip must stay blocked with every switch on")
	}
}

// The unit-test loopback switch must not open private ranges.
func TestSSRFLoopbackSwitchDoesNotAllowPrivate(t *testing.T) {
	setSSRFFlags(t, true, false)
	if isBlockedSourceIP(net.ParseIP("127.0.0.1")) {
		t.Error("loopback switch should allow loopback")
	}
	for _, s := range []string{"10.0.0.1", "172.17.0.1", "192.168.0.1", "fd00::1"} {
		if !isBlockedSourceIP(net.ParseIP(s)) {
			t.Errorf("loopback switch must not allow %s", s)
		}
	}
}

func TestPrivateSourcesEnabledExactOne(t *testing.T) {
	for v, want := range map[string]bool{
		"1": true, "": false, "0": false, "true": false, "TRUE": false,
		"yes": false, " 1": false, "1 ": false, "01": false, "11": false,
	} {
		if got := privateSourcesEnabled(v); got != want {
			t.Errorf("privateSourcesEnabled(%q)=%v want %v", v, got, want)
		}
	}
}

// The dial-time guard checks the literal IP before any connect, so these need
// no network: refused with the flag off, and link-local stays refused with it on.
func TestSSRFDialContextLiteralIP(t *testing.T) {
	setSSRFFlags(t, false, false)
	for _, addr := range []string{"10.0.0.1:80", "127.0.0.1:80", "[::1]:80", "169.254.169.254:80", "[::ffff:10.0.0.1]:80"} {
		if _, err := ssrfDialContext(t.Context(), "tcp", addr); err == nil {
			t.Errorf("flag off: dial %s was not refused", addr)
		}
	}
	setSSRFFlags(t, false, true)
	for _, addr := range []string{"169.254.169.254:80", "[fe80::1]:80", "[::ffff:169.254.169.254]:80", "0.0.0.0:80"} {
		if _, err := ssrfDialContext(t.Context(), "tcp", addr); err == nil {
			t.Errorf("flag on: dial %s was not refused", addr)
		}
	}
}
