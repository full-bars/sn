package miner

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// listenSocks5Once starts a TCP listener that responds to every connection
// with a valid SOCKS5 greeting reply (version 5, "no auth" accepted),
// simulating a real SOCKS5 proxy. The caller is responsible for calling the
// returned cleanup.
func listenSocks5Once(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Liveness barrier: complete one full greeting exchange from the test
	// itself before returning. The accept loop may not be scheduled yet, and
	// a probe that arrives in that window can fail, flake every caller that
	// probes right after.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 3)
				if _, err := c.Read(buf); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00})
			}(conn)
		}
	}()
	probe, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatalf("liveness probe dial failed: %v", err)
	}
	if _, err := probe.Write([]byte{0x05, 0x00, 0x00}); err != nil {
		probe.Close()
		ln.Close()
		t.Fatalf("liveness probe write failed: %v", err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(probe, greeting); err != nil {
		probe.Close()
		ln.Close()
		t.Fatalf("liveness probe: accept loop did not reply to greeting: %v", err)
	}
	if greeting[0] != 0x05 || greeting[1] != 0x00 {
		probe.Close()
		ln.Close()
		t.Fatalf("liveness probe: unexpected greeting % x", greeting)
	}
	probe.Close()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenAcceptOnlyOnce starts a TCP listener that accepts connections but
// closes them immediately without writing anything - simulating an open
// port whose service isn't actually SOCKS5.
func listenAcceptOnlyOnce(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// closedPortAddr returns an address nothing is listening on, by opening and
// immediately closing a listener to grab a free port.
func closedPortAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestProbeProxySocks5_RealSocks5Server(t *testing.T) {
	addr, cleanup := listenSocks5Once(t)
	defer cleanup()

	if !probeProxySocks5(context.Background(), addr, time.Second) {
		t.Errorf("expected a real SOCKS5 server at %s to probe true", addr)
	}
}

func TestProbeProxySocks5_ClosedPort(t *testing.T) {
	addr := closedPortAddr(t)

	if probeProxySocks5(context.Background(), addr, time.Second) {
		t.Errorf("expected closed port %s to probe false", addr)
	}
}

// TestProbeProxySocks5_OpenPortNotSocks5 is a regression test for the gap a
// bare TCP probe leaves: a port that accepts a connection but isn't
// actually running SOCKS5 must be rejected, not treated as reachable.
func TestProbeProxySocks5_OpenPortNotSocks5(t *testing.T) {
	addr, cleanup := listenAcceptOnlyOnce(t)
	defer cleanup()

	if probeProxySocks5(context.Background(), addr, time.Second) {
		t.Errorf("expected open-but-non-SOCKS5 port %s to probe false", addr)
	}
}

// TestFilterReachableProxyURLLines_KeepsOnlyReachable is the core of the
// fix: free public proxy lists are mostly dead, so the merge step must drop
// unreachable (or non-SOCKS5) entries before they ever get an auth attempt.
func TestFilterReachableProxyURLLines_KeepsOnlyReachable(t *testing.T) {
	socks5Addr, cleanup := listenSocks5Once(t)
	defer cleanup()
	deadAddr := closedPortAddr(t)
	openNonSocks5Addr, cleanup2 := listenAcceptOnlyOnce(t)
	defer cleanup2()

	lines := []string{
		socks5Addr,
		deadAddr,
		openNonSocks5Addr,
		"not a valid line :::",
	}

	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), lines, "", 0)
	// With empty apiHost the probe skips the CONNECT stage, so SOCKS5
	// proxies end up in the socks5Only bucket.
	if len(apiOK) != 0 || len(socks5Only) != 1 || socks5Only[0] != socks5Addr {
		t.Fatalf("expected %q as socks5-only, got apiOK=%v socks5Only=%v", socks5Addr, apiOK, socks5Only)
	}
}

// TestProbeStartupCooldown_ExceedsSystemdRestartSec pins the safety
// invariant documented on probeStartupCooldown: it must stay strictly
// greater than the systemd unit's RestartSec (5s), or a crash-looping
// process could live long enough between restarts to reach the deferred
// fetch and re-trigger the probe-amplification loop.
func TestProbeStartupCooldown_ExceedsSystemdRestartSec(t *testing.T) {
	const systemdRestartSec = 5 * time.Second
	if probeStartupCooldown <= systemdRestartSec {
		t.Fatalf("probeStartupCooldown (%v) must be > systemd RestartSec (%v)", probeStartupCooldown, systemdRestartSec)
	}
}

func TestFilterReachableProxyURLLines_EmptyInput(t *testing.T) {
	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), nil, "", 0)
	if len(apiOK) != 0 || len(socks5Only) != 0 {
		t.Fatalf("expected empty result for empty input, got apiOK=%v socks5Only=%v", apiOK, socks5Only)
	}
}

// ---- kill-switch admission and the held/redacted source lines ----

// TestGradingKillSwitchAdmitsOnStage0: with stage-1 disabled (the P1(a)
// default), a SOCKS5+API-reachable proxy is qualified and a SOCKS5-only one is
// held for the reaper, with no fabricated grade on either.
func TestGradingKillSwitchAdmitsOnStage0(t *testing.T) {
	ca := newTestCA(t)
	trustedLeaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	apiOKAddr, cleanup := listenSocks5ApiOKTLS(t, &trustedLeaf)
	defer cleanup()
	greeted, cleanup2 := listenSocks5Once(t)
	defer cleanup2()

	cfg := defaultProxyTableProbeConfig()
	cfg.Enabled = false

	qualified := probeAndGradeProxyURLLines(context.Background(), []string{apiOKAddr}, "127.0.0.1", 1, cfg)
	g, ok := qualified[apiOKAddr]
	if !ok {
		t.Fatalf("stage-0-qualified line got no admission decision: %v", qualified)
	}
	if !g.Qualified || g.Decidable || g.Socks5Only {
		t.Fatalf("kill-switch admission must be Qualified, non-decidable, not socks5-only: %+v", g)
	}

	// A SOCKS5 greeting with no CONNECT target (empty apiHost) is socks5-only:
	// held for the reaper, never admitted, never graded.
	held := probeAndGradeProxyURLLines(context.Background(), []string{greeted}, "", 0, cfg)
	h, ok := held[greeted]
	if !ok {
		t.Fatalf("socks5-only line got no admission decision: %v", held)
	}
	if !h.Socks5Only || h.Qualified || h.Decidable {
		t.Fatalf("socks5-only line must be held, not admitted or graded: %+v", h)
	}
}

// TestFetchCycleDeadSourceLinesUseTheRedactedLabel ports the v33 guard that a
// source URL's token never reaches the cycle log through the all-dead path.
func TestFetchCycleDeadSourceLinesUseTheRedactedLabel(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("127.0.0.1:1:user:pass\n")) // nothing listens on port 1: dead
	}))
	defer srv.Close()

	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{srv.URL + "/dead.txt?token=SECRET"}, 0, "127.0.0.1", 1)
	})
	if !strings.Contains(out, "all unparseable or dead") {
		t.Fatalf("test setup did not reach the all-dead path:\n%s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("the cycle's log output leaks the URL's token:\n%s", out)
	}
}

// ---- stage-1 config width (v33 proxy_probe_width_test.go) ----

func TestProbeWidth_MaxWiderThanSample(t *testing.T) {
	cfg := proxyTableProbeConfig{SampleWidth: 12, MaxSampleWidth: 36}
	if got := cfg.probeWidth(); got != 36 {
		t.Fatalf("probeWidth() = %d, want 36 (MaxSampleWidth)", got)
	}
}

func TestProbeWidth_MaxAtOrBelowSample(t *testing.T) {
	cases := []struct {
		name           string
		sample, maxwid int
		want           int
	}{
		{"max equals sample", 12, 12, 12},
		{"max below sample", 12, 4, 12},
		{"max zero (disabled)", 12, 0, 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := proxyTableProbeConfig{SampleWidth: c.sample, MaxSampleWidth: c.maxwid}
			if got := cfg.probeWidth(); got != c.want {
				t.Fatalf("probeWidth() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestProbeWidth_DefaultConfig(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	if got := cfg.probeWidth(); got != cfg.MaxSampleWidth {
		t.Fatalf("probeWidth() on the default config = %d, want MaxSampleWidth %d", got, cfg.MaxSampleWidth)
	}
}
