package miner

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listenerHost returns the host part of a listener's address.
func listenerHost(t *testing.T, ln net.Listener) string {
	t.Helper()
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("listener address %q: %v", ln.Addr().String(), err)
	}
	return host
}

// isUnspecifiedHost reports whether host is a wildcard bind (0.0.0.0 or ::).
// The kernel may report either spelling for a wildcard socket.
func isUnspecifiedHost(t *testing.T, host string) bool {
	t.Helper()
	addr, err := netip.ParseAddr(host)
	if err != nil {
		t.Fatalf("listener host %q: %v", host, err)
	}
	return addr.IsUnspecified()
}

// B10 (5526e8490): /metrics is on by default and auto mode binds loopback
// plus Tailscale, never a public interface. The half that had no test is the
// container sharing a publicly addressed network (--network host): it must
// drop the wide 0.0.0.0 bind rather than publish per-proxy series.
func TestMetricsDefaultNeverBindsPublic(t *testing.T) {
	withTempHome(t)
	stubMetricsAuto(t)
	withMetricsState(t)

	// A plain host: loopback only.
	ln, err := listenMetrics(0)
	if err != nil {
		t.Fatalf("auto listen: %v", err)
	}
	if host := listenerHost(t, ln); host != "127.0.0.1" {
		t.Fatalf("default /metrics bound %q, want 127.0.0.1", host)
	}
	ln.Close()

	// A bridged container has only private addresses, so only ports the
	// operator publishes are reachable and the wide bind stays.
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("write container marker: %v", err)
	}
	metricsContainerMarkers = []string{marker}
	containerAddrsFunc = func() []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("172.17.0.2")}
	}
	if publiclyAddressed() {
		t.Fatal("a bridged container's private address is not publicly addressed")
	}
	ln, err = listenMetrics(0)
	if err != nil {
		t.Fatalf("bridged container auto listen: %v", err)
	}
	if host := listenerHost(t, ln); !isUnspecifiedHost(t, host) {
		t.Fatalf("bridged container bound %q, want the wide wildcard bind", host)
	}
	ln.Close()

	// A container on the host network sees the host's public address.
	// Binding 0.0.0.0 there publishes /metrics on the public internet.
	containerAddrsFunc = func() []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("203.0.113.10")}
	}
	if !publiclyAddressed() {
		t.Fatal("TEST-NET-3 is global unicast and must count as publicly addressed")
	}
	ln, err = listenMetrics(0)
	if err != nil {
		t.Fatalf("public container auto listen: %v", err)
	}
	if host := listenerHost(t, ln); host != "127.0.0.1" {
		t.Fatalf("a publicly addressed container bound %q, want loopback only", host)
	}
	if multi, ok := ln.(*metricsMultiListener); !ok {
		t.Fatalf("a publicly addressed container must get the loopback plus Tailscale listener, got %T", ln)
	} else {
		for _, addr := range multi.Addrs() {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("served address %q: %v", addr, err)
			}
			if host == "0.0.0.0" || host == "::" {
				t.Fatalf("a publicly addressed container serves /metrics on the wildcard %q", addr)
			}
		}
	}
	ln.Close()

	// And the startup default, which is how a node that never set the key
	// reaches the listener, goes through the same guard.
	startDefaultMetrics(newControlState(), false)
	addrs := metricsServedAddrs()
	if len(addrs) == 0 {
		t.Fatal("the startup default did not start /metrics")
	}
	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("served address %q: %v", addr, err)
		}
		if host == "0.0.0.0" || host == "::" || strings.HasPrefix(host, "203.0.113.") {
			t.Fatalf("the startup default serves /metrics on a public interface: %v", addrs)
		}
	}
	if err := stopMetrics(); err != nil {
		t.Fatalf("stopMetrics: %v", err)
	}
}
