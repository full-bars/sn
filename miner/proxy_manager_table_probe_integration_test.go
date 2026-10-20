package miner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Ported from the 3.23-fix v33 provider's
// proxy_table_probe_integration_test.go (read-only source candidate/v33-ops).
//
// Port adaptation: v33 resolves api.bringyour.com and skips when the test box
// cannot, which makes the suite network-dependent. Here the probed API
// endpoint is the loopback literal 127.0.0.1 (a literal IP goes through
// resolveAPIProbeAddr without a DNS lookup) with a test leaf issued for it and
// the test CA injected, and the stage-1 table targets are seeded in the probe
// DNS cache. The config override file lives in the miner's state directory
// (URNETWORK_STATE_DIR), not in $HOME/.urnetwork.

// listenSocks5Smart starts a TCP listener that answers the SOCKS5 greeting
// and replies to CONNECT with REP 0x00 when the destination IP is in the
// ok map, REP 0x05 otherwise. Used to build a proxy that passes the API
// CONNECT (stage 0) but fails most table targets (stage 1) - the
// below-bar admission case.
func listenSocks5Smart(t *testing.T, okIPs map[string]bool) (addr string, cleanup func()) {
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
			go func(c net.Conn) {
				defer c.Close()
				if !readSocks5Greeting(c) {
					return
				}
				c.Write([]byte{0x05, 0x00})
				frame := make([]byte, 10)
				if _, err := io.ReadFull(c, frame); err != nil {
					return
				}
				// ATYP=1 (v4): bytes 4-7 are the destination IP
				rep := byte(0x05)
				if frame[3] == 0x01 && okIPs[net.IP(frame[4:8]).String()] {
					rep = 0x00
				}
				resp := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
				c.Write(resp)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenSocks5ConnectOnceTLS is listenSocks5ConnectOnce (every CONNECT
// answered with rep) plus a TLS server behind a successful CONNECT - the fake
// the full stage-0 + stage-1 path needs. Ported from v33
// proxy_probe_tls_test.go.
func listenSocks5ConnectOnceTLS(t *testing.T, rep byte, leaf *tls.Certificate) (addr string, cleanup func()) {
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
			go serveTLSAfterConnect(rep, leaf, func(c net.Conn) {
				if !readSocks5Greeting(c) {
					return
				}
				c.Write([]byte{0x05, 0x00}) // greeting: no auth
				connectFrame := make([]byte, 10)
				if _, err := io.ReadFull(c, connectFrame); err != nil {
					return
				}
				resp := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
				c.Write(resp)
			})(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// listenSocks5SmartTLS is listenSocks5Smart plus a TLS server behind an
// accepted CONNECT. Ported from v33 proxy_probe_tls_test.go.
func listenSocks5SmartTLS(t *testing.T, okIPs map[string]bool, leaf *tls.Certificate) (addr string, cleanup func()) {
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
			go func(c net.Conn) {
				defer c.Close()
				if !readSocks5Greeting(c) {
					return
				}
				c.Write([]byte{0x05, 0x00})
				frame := make([]byte, 10)
				if _, err := io.ReadFull(c, frame); err != nil {
					return
				}
				rep := byte(0x05)
				if frame[3] == 0x01 && okIPs[net.IP(frame[4:8]).String()] {
					rep = 0x00
				}
				resp := []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
				c.Write(resp)
				if rep == 0x00 && leaf != nil {
					tlsSrv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}})
					tlsSrv.SetDeadline(time.Now().Add(2 * time.Second))
					tlsSrv.Handshake()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestProbeAndGradeProxyURLLines_QualifiedGetsGrade: a proxy that answers
// every CONNECT with success is qualified and carries its score/failed.
func TestProbeAndGradeProxyURLLines_QualifiedGetsGrade(t *testing.T) {
	seedAllProbeDNS(t)
	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	addr, cleanup := listenSocks5ConnectOnceTLS(t, 0x00, &leaf)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	lines := []string{addr}
	grades := probeAndGradeProxyURLLines(context.Background(), lines, "127.0.0.1", 443, cfg)

	g, ok := grades[addr]
	if !ok {
		t.Fatalf("expected a grade for %s, got %v", addr, grades)
	}
	if !g.Qualified {
		t.Fatalf("expected qualified, got %+v", g)
	}
	if g.Score != 1.0 {
		t.Fatalf("expected score 1.0, got %v", g.Score)
	}
	if len(g.Failed) != 0 {
		t.Fatalf("expected no failed targets, got %v", g.Failed)
	}
}

// TestProbeAndGradeProxyURLLines_DeadDropped: a dead address gets no grade
// entry at all (dropped before cache).
func TestProbeAndGradeProxyURLLines_DeadDropped(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = 200 * time.Millisecond

	dead := closedPortAddr(t)
	grades := probeAndGradeProxyURLLines(context.Background(), []string{dead}, "127.0.0.1", 443, cfg)
	if _, ok := grades[dead]; ok {
		t.Fatalf("expected dead address to be dropped, got grade %+v", grades[dead])
	}
}

// TestProbeAndGradeProxyURLLines_Socks5OnlyFlagged: an address that speaks
// SOCKS5 but refuses the API CONNECT is flagged Socks5Only, never admitted.
func TestProbeAndGradeProxyURLLines_Socks5OnlyFlagged(t *testing.T) {
	addr, cleanup := listenSocks5ConnectOnce(t, 0x05) // every CONNECT refused
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	grades := probeAndGradeProxyURLLines(context.Background(), []string{addr}, "127.0.0.1", 443, cfg)
	g, ok := grades[addr]
	if !ok {
		t.Fatalf("expected a grade for %s", addr)
	}
	if !g.Socks5Only {
		t.Fatalf("expected Socks5Only flag, got %+v", g)
	}
	if g.Qualified {
		t.Fatalf("socks5-only must never be qualified, got %+v", g)
	}
}

// TestProbeAndGradeProxyURLLines_BelowBarNotQualified: a proxy that passes
// the API CONNECT but fails nearly all table targets is not qualified -
// it carries a score below the bar and no auth admission.
func TestProbeAndGradeProxyURLLines_BelowBarNotQualified(t *testing.T) {
	seedAllProbeDNS(t)
	// The API CONNECT destination is the loopback literal the fake
	// accepts; every table target (the sampled hosts) resolves to the
	// seeded address and is refused.
	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	addr, cleanup := listenSocks5SmartTLS(t, map[string]bool{"127.0.0.1": true}, &leaf)
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 4
	cfg.TargetTimeout = time.Second

	grades := probeAndGradeProxyURLLines(context.Background(), []string{addr}, "127.0.0.1", 443, cfg)
	g, ok := grades[addr]
	if !ok {
		t.Fatalf("expected a grade for %s", addr)
	}
	if g.Qualified {
		t.Fatalf("below-bar proxy must not be qualified, got %+v", g)
	}
	if g.Score >= cfg.PassBar {
		t.Fatalf("expected score below bar %v, got %v", cfg.PassBar, g.Score)
	}
	if g.Socks5Only {
		t.Fatalf("below-bar is not socks5-only (it passed the API CONNECT), got %+v", g)
	}
}

// TestResolveProxyTableProbeConfig_OverrideFile: an operator-written
// <state>/proxy_probe.json changes the effective config; a malformed file
// falls back to defaults.
func TestResolveProxyTableProbeConfig_OverrideFile(t *testing.T) {
	withTempHome(t)
	path, err := proxyProbeOverridePath()
	if err != nil {
		t.Fatal(err)
	}

	good := map[string]any{"sample_width": 20, "timeout_ms": 2000, "pass_bar": 0.5, "preferred_bar": 0.85}
	b, _ := json.Marshal(good)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	resetProbeConfigCache()
	cfg := resolveProxyTableProbeConfig()
	if cfg.SampleWidth != 20 || cfg.TargetTimeout != 2*time.Second || cfg.PassBar != 0.5 || cfg.PreferredBar != 0.85 {
		t.Fatalf("override not applied: %+v", cfg)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	resetProbeConfigCache()
	cfg = resolveProxyTableProbeConfig()
	def := defaultProxyTableProbeConfig()
	if cfg.SampleWidth != def.SampleWidth || cfg.PassBar != def.PassBar {
		t.Fatalf("malformed override must fall back to defaults, got %+v", cfg)
	}
}

// TestFetchAndMergeProxyURLs_GradedPersistence: the fetch pipeline persists
// the stage-1 score/failed into proxy_url.json and only admits qualified
// proxies with ProbeOK=true.
func TestFetchAndMergeProxyURLs_GradedPersistence(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)
	seedAllProbeDNS(t)

	// One proxy that passes everything (qualified), one dead (dropped).
	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	goodAddr, cleanup := listenSocks5ConnectOnceTLS(t, 0x00, &leaf)
	defer cleanup()
	deadAddr := closedPortAddr(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(goodAddr + "\n" + deadAddr + "\n"))
	}))
	defer srv.Close()

	// Point the probe config at a small fast sample so the test stays quick.
	writeReviewProbeOverride(t, map[string]any{"sample_width": 4, "timeout_ms": 500})

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "127.0.0.1", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	good, ok := state.Cache[goodAddr]
	if !ok {
		t.Fatalf("expected qualified proxy in cache, got %v", state.Cache)
	}
	if !good.ProbeOK {
		t.Errorf("qualified proxy must have ProbeOK=true, got %+v", good)
	}
	if good.Score < 0.6 {
		t.Errorf("expected score >= pass bar, got %v", good.Score)
	}
	if _, ok := state.Cache[deadAddr]; ok {
		t.Errorf("dead proxy must not be cached, got %v", state.Cache[deadAddr])
	}
}

// TestFetchAndMergeProxyURLs_AllBelowBarNothingAdmitted: when every fetched
// proxy is below the stage-1 bar, nothing gets ProbeOK=true - the auth-time
// gate then blocks them all.
func TestFetchAndMergeProxyURLs_AllBelowBarNothingAdmitted(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)
	seedAllProbeDNS(t)

	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	smartAddr, cleanup := listenSocks5SmartTLS(t, map[string]bool{"127.0.0.1": true}, &leaf)
	defer cleanup()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(smartAddr + "\n"))
	}))
	defer srv.Close()

	writeReviewProbeOverride(t, map[string]any{"sample_width": 4, "timeout_ms": 500})

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "127.0.0.1", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := state.Cache[smartAddr]
	if !ok {
		t.Fatalf("below-bar entry should still be cached for the reaper, got %v", state.Cache)
	}
	if entry.ProbeOK {
		t.Errorf("below-bar proxy must not be ProbeOK, got %+v", entry)
	}
	if entry.Score >= 0.6 {
		t.Errorf("expected below-bar score, got %v", entry.Score)
	}
}
