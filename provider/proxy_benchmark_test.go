package provider

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
)

// TestResolveBenchmarkEndpoint is the regression for the CodeRabbit finding:
// resolveBenchmarkEndpoint manually trimmed the "wss://"/"ws://" scheme
// prefix instead of parsing the URL, so a saved connect_url containing a
// path (validateConnectUrl only requires a ws/wss scheme and a host, per
// network.go) left that path in the "cleaned" string, breaking
// net.SplitHostPort / producing a garbage port.
func TestResolveBenchmarkEndpoint(t *testing.T) {
	os.Unsetenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT")

	cases := []struct {
		name       string
		connectUrl string
		want       string
	}{
		{"no port, no path", "wss://connect.example.com", "connect.example.com:443"},
		{"explicit port", "wss://connect.example.com:8443", "connect.example.com:8443"},
		{"path, no port -- the regression case", "wss://connect.example.com/connect", "connect.example.com:443"},
		{"path and port", "wss://connect.example.com:8443/connect", "connect.example.com:8443"},
		{"ws scheme", "ws://connect.example.com", "connect.example.com:443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withTempHome(t)
			if err := writeNetworkConfig("https://api.example.com", c.connectUrl); err != nil {
				t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
			}
			got := resolveBenchmarkEndpoint()
			if got != c.want {
				t.Fatalf("resolveBenchmarkEndpoint() with connect_url=%q = %q, want %q", c.connectUrl, got, c.want)
			}
		})
	}
}

// TestResolveBenchmarkEndpointEnvOverride verifies the env var still wins
// over any saved network config.
func TestResolveBenchmarkEndpointEnvOverride(t *testing.T) {
	withTempHome(t)
	if err := writeNetworkConfig("https://api.example.com", "wss://connect.example.com/connect"); err != nil {
		t.Fatalf("writeNetworkConfig: unexpected error: %s", err)
	}
	os.Setenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT", "override.example.com:1234")
	defer os.Unsetenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT")

	got := resolveBenchmarkEndpoint()
	if got != "override.example.com:1234" {
		t.Fatalf("resolveBenchmarkEndpoint() = %q, want env override %q", got, "override.example.com:1234")
	}
}

// shortenProxyBenchmarkIntervals makes the probes tick quickly for a test and
// enables URNETWORK_PROXY_BENCHMARK.
func shortenProxyBenchmarkIntervals(t *testing.T) {
	t.Helper()
	oldTCP, oldSocks := proxyBenchmarkTCPInterval, proxyBenchmarkSocksInterval
	proxyBenchmarkTCPInterval = 5 * time.Millisecond
	proxyBenchmarkSocksInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		proxyBenchmarkTCPInterval, proxyBenchmarkSocksInterval = oldTCP, oldSocks
	})
	t.Setenv("URNETWORK_PROXY_BENCHMARK", "true")
	t.Setenv("URNETWORK_PROXY_BENCHMARK_ENDPOINT", "127.0.0.1:1")
}

// acceptAndClose starts a TCP listener that accepts and immediately closes
// every connection, standing in for the proxy's address.
func acceptAndClose(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String()
}

// TestStartProxyBenchmarksNilBandwidth: provideWithProxy used to pass a nil
// bandwidth, and the TCP probe's first successful dial then panicked on
// bw.LatencyNs.Store. startProxyBenchmarks must tolerate nil.
func TestStartProxyBenchmarksNilBandwidth(t *testing.T) {
	shortenProxyBenchmarkIntervals(t)
	addr := acceptAndClose(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startProxyBenchmarks(ctx, nil, &connect.ProxySettings{Network: "tcp", Address: addr})
	// Several probe ticks; a panic in the probe goroutine aborts the test binary.
	time.Sleep(200 * time.Millisecond)
}

// TestStartProxyBenchmarksStoresLatency checks the probe records latency on
// the proxy's real bandwidth entry.
func TestStartProxyBenchmarksStoresLatency(t *testing.T) {
	shortenProxyBenchmarkIntervals(t)
	addr := acceptAndClose(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bw := &bandwidth.ProxyBandwidth{}
	startProxyBenchmarks(ctx, bw, &connect.ProxySettings{Network: "tcp", Address: addr})
	deadline := time.Now().Add(2 * time.Second)
	for bw.LatencyNs.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("LatencyNs never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
