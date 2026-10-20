package miner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// This file ports the v33 provider's TLS-stage probe tests
// (proxy_probe_tls_test.go). Port adaptation: the probe target host is
// "127.0.0.1" (a literal IP, resolved without DNS) rather than
// api.beta-test.net, so the suite is hermetic; the leaf SANs are adjusted the
// same way via issueLeafForHost.

// testCA is a minimal, test-only CA+leaf pair.
type testCA struct {
	certDER []byte
	certPEM []byte
	key     *ecdsa.PrivateKey
	cert    *x509.Certificate
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{
		certDER: der,
		certPEM: pemEncodeCert(der),
		key:     key,
		cert:    cert,
	}
}

func (ca *testCA) issueLeaf(t *testing.T, sans []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	tlsCert, err := tls.X509KeyPair(pemEncodeCert(der), pemEncodeKey(keyBytes))
	if err != nil {
		t.Fatalf("build tls.Certificate: %v", err)
	}
	return tlsCert
}

func pemEncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemEncodeKey(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// issueLeafForHost issues a TLS leaf for either a DNS name or an IP literal
// (the probe's TLS config pins ServerName to the apiHost, and x509
// verification of an IP ServerName requires an IPAddresses SAN).
func issueLeafForHost(t *testing.T, ca *testCA, host string) tls.Certificate {
	t.Helper()
	if ip := net.ParseIP(host); ip != nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate leaf key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: "test-leaf"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			IPAddresses:  []net.IP{ip},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
		if err != nil {
			t.Fatalf("create leaf cert: %v", err)
		}
		keyBytes, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal leaf key: %v", err)
		}
		tlsCert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}))
		if err != nil {
			t.Fatalf("build tls.Certificate: %v", err)
		}
		return tlsCert
	}
	return ca.issueLeaf(t, []string{host})
}

// withProbeTLSRoot injects the test CA into the probe's TLS trust store for
// the duration of the test, and restores the production config afterwards.
func withProbeTLSRoot(t *testing.T, ca *testCA) {
	t.Helper()
	old := proxyProbeTLSClientConfig
	t.Cleanup(func() { proxyProbeTLSClientConfig = old })
	proxyProbeTLSClientConfig = func(serverName string) *tls.Config {
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		return &tls.Config{ServerName: serverName, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
}

// readSocks5Greeting reads a SOCKS5 greeting robustly: VER(1) NMETHODS(1)
// then NMETHODS method bytes. A credentialed probe offers
// {0x05, 0x02, 0x00, 0x02} (4 bytes); a no-auth probe offers {0x05, 0x01,
// 0x00} (3 bytes). Reading a fixed 3 bytes misaligns the subsequent CONNECT
// frame for credentialed lines - harmless when nothing follows CONNECT, but
// fatal once a TLS handshake reuses the same connection.
func readSocks5Greeting(c net.Conn) bool {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return false
	}
	if hdr[0] != 0x05 || hdr[1] == 0 {
		return false
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return false
	}
	return true
}

// listenSocks5TLSOnce starts a TCP listener that completes the SOCKS5
// greeting (no-auth) and answers every CONNECT with REP 0x00 (success),
// then serves a TLS handshake with the provided certificate on the same
// connection - simulating the tunnel to the "API" endpoint behind the
// proxy. If leaf is nil the connection is closed after CONNECT.
func listenSocks5TLSOnce(t *testing.T, leaf *tls.Certificate) (addr string, cleanup func()) {
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
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				connectFrame := make([]byte, 10)
				if _, err := io.ReadFull(c, connectFrame); err != nil {
					return
				}
				if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				if leaf == nil {
					return
				}
				tlsSrv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}})
				// A real MITM/transparent server completes the handshake;
				// the client decides whether the presented chain verifies.
				// The deadline keeps a probe that closes after CONNECT
				// without a ClientHello from blocking this goroutine for
				// the rest of the test binary run.
				tlsSrv.SetDeadline(time.Now().Add(2 * time.Second))
				tlsSrv.Handshake()
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// serveTLSAfterConnect wraps a SOCKS5 connection handler: after the handler
// has written a successful (REP 0x00) CONNECT reply, serve a TLS handshake
// on the same connection with leaf. If rep != 0x00 no TLS is served.
func serveTLSAfterConnect(rep byte, leaf *tls.Certificate, handle func(c net.Conn)) func(c net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		handle(c)
		if rep == 0x00 && leaf != nil {
			tlsSrv := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*leaf}})
			tlsSrv.SetDeadline(time.Now().Add(2 * time.Second))
			tlsSrv.Handshake()
		}
	}
}

// listenSocks5ApiOKTLS is listenSocks5Once (every CONNECT succeeds) plus a
// TLS server behind each connection - a faithful transparent-proxy fake for
// tests that must reach probeAPIReachable.
func listenSocks5ApiOKTLS(t *testing.T, leaf *tls.Certificate) (addr string, cleanup func()) {
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
			go serveTLSAfterConnect(0x00, leaf, func(c net.Conn) {
				if !readSocks5Greeting(c) {
					return
				}
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				connectFrame := make([]byte, 10)
				if _, err := io.ReadFull(c, connectFrame); err != nil {
					return
				}
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			})(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestProbeProxy_TLSVerifyRejectsMITM is the core hostile-proxy test: the
// fake answers SOCKS5 CONNECT with success (so the old two-stage probe
// would have admitted it), but presents a certificate that does NOT verify
// for the apiHost - the signature of a TLS-intercepting proxy. The probe
// must return probeTLSFailed, never probeAPIReachable.
func TestProbeProxy_TLSVerifyRejectsMITM(t *testing.T) {
	ca := newTestCA(t)
	// Leaf signed by a CA the probe does NOT trust (system roots), for a
	// hostname that is not the apiHost - an interceptor's self-issued cert.
	leaf := ca.issueLeaf(t, []string{"interceptor.example"})

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	res := probeProxy(context.Background(), addr, "", "", "127.0.0.1", 1)
	if res != probeTLSFailed {
		t.Fatalf("MITM proxy: result = %v, want probeTLSFailed (probeAPIReachable=%v, probeSocks5Only=%v, probeDead=%v)",
			res, probeAPIReachable, probeSocks5Only, probeDead)
	}
}

// TestProbeProxy_TLSVerifyAcceptsTransparent pins the good case: a
// transparent proxy relays the TLS handshake untouched, so the client sees
// the REAL apiHost certificate (here: a leaf issued by the injected test
// CA) and verification succeeds, giving probeAPIReachable.
func TestProbeProxy_TLSVerifyAcceptsTransparent(t *testing.T) {
	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	res := probeProxy(context.Background(), addr, "", "", "127.0.0.1", 1)
	if res != probeAPIReachable {
		t.Fatalf("transparent proxy: result = %v, want probeAPIReachable (tlsFailed=%v)", res, probeTLSFailed)
	}
}

// TestProbeProxy_TLSVerifySkipsWhenNoAPIHost pins the cheap-gate behavior:
// probeProxySocks5 calls probeProxy with an empty apiHost (auth-time gate);
// it must NOT attempt a TLS stage and must still return a socks5 verdict,
// not probeTLSFailed.
func TestProbeProxy_TLSVerifySkipsWhenNoAPIHost(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, []string{"api.example"})

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	res := probeProxy(context.Background(), addr, "", "", "", 0)
	if res != probeSocks5Only {
		t.Fatalf("empty apiHost: result = %v, want probeSocks5Only (tlsFailed=%v, apiReachable=%v, dead=%v)",
			res, probeTLSFailed, probeAPIReachable, probeDead)
	}
}

// TestProbeAndFilterProxyURLLines_TLSFailedNotAdmitted pins the admission
// flow: a line whose proxy passes CONNECT but fails TLS verification must
// NOT land in the apiOK bucket (which becomes the qualified pool), and must
// be surfaced via the socks5-only bucket so the reaper's retry-to-blacklist
// lifecycle can retire it.
func TestProbeAndFilterProxyURLLines_TLSFailedNotAdmitted(t *testing.T) {
	ca := newTestCA(t)
	leaf := ca.issueLeaf(t, []string{"interceptor.example"})

	addr, cleanup := listenSocks5TLSOnce(t, &leaf)
	defer cleanup()

	lines := []string{addr}
	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), lines, "127.0.0.1", 1)

	if len(apiOK) != 0 {
		t.Fatalf("MITM line admitted to apiOK: %v", apiOK)
	}
	if len(socks5Only) != 1 {
		t.Fatalf("MITM line not surfaced via socks5-only bucket for reaper lifecycle: apiOK=%v socks5Only=%v", apiOK, socks5Only)
	}
}

// TestProxyProbeTLSClientConfig_DefaultConfig pins the production seam's
// stock behavior (no test override installed): ServerName is pinned to the
// apiHost passed in, TLS 1.2 is the floor, and RootCAs is left nil so the
// system trust store is used.
func TestProxyProbeTLSClientConfig_DefaultConfig(t *testing.T) {
	cfg := proxyProbeTLSClientConfig("api.bringyour.com")
	if cfg.ServerName != "api.bringyour.com" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "api.bringyour.com")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want tls.VersionTLS12", cfg.MinVersion)
	}
	if cfg.RootCAs != nil {
		t.Errorf("RootCAs = %v, want nil (system pool)", cfg.RootCAs)
	}
}

// TestProbeProxy_TLSHandshakeFailsOnAbruptClose exercises a different
// failure mode through stage 3 than TestProbeProxy_TLSVerifyRejectsMITM:
// instead of presenting an untrusted certificate, the fake closes the
// connection the instant CONNECT succeeds, before any TLS bytes are
// exchanged (leaf=nil). A proxy that closes the tunnel this abruptly cannot
// be transparently relaying TLS either, so HandshakeContext errors out and
// the probe must still land on probeTLSFailed.
func TestProbeProxy_TLSHandshakeFailsOnAbruptClose(t *testing.T) {
	addr, cleanup := listenSocks5TLSOnce(t, nil)
	defer cleanup()

	res := probeProxy(context.Background(), addr, "", "", "127.0.0.1", 1)
	if res != probeTLSFailed {
		t.Fatalf("abrupt close after CONNECT: result = %v, want probeTLSFailed (apiReachable=%v, socks5Only=%v, dead=%v)",
			res, probeAPIReachable, probeSocks5Only, probeDead)
	}
}

// TestProbeAndFilterProxyURLLines_MixedResultsPartitionCorrectly drives
// probeAndFilterProxyURLLines across all four probeResult outcomes in one
// batch, pinning that: (1) only the genuinely transparent proxy lands in
// apiOK, (2) both the TLS-intercepting proxy and the plain socks5-only
// proxy are routed to the SAME retry bucket, and (3) the dead line is
// dropped from both buckets entirely.
func TestProbeAndFilterProxyURLLines_MixedResultsPartitionCorrectly(t *testing.T) {
	ca := newTestCA(t)
	trustedLeaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)

	apiOKAddr, apiOKCleanup := listenSocks5ApiOKTLS(t, &trustedLeaf)
	defer apiOKCleanup()

	// Signed by the trusted test CA, but the SAN doesn't cover "127.0.0.1"
	// (no IP SAN at all) - a MITM proxy reusing a plausible-looking cert
	// for the wrong host.
	mismatchedLeaf := ca.issueLeaf(t, []string{"interceptor.example"})
	tlsFailedAddr, tlsFailedCleanup := listenSocks5TLSOnce(t, &mismatchedLeaf)
	defer tlsFailedCleanup()

	socks5OnlyAddr, socks5OnlyCleanup := listenSocks5Once(t)
	defer socks5OnlyCleanup()

	deadAddr := closedPortAddr(t)

	lines := []string{apiOKAddr, tlsFailedAddr, socks5OnlyAddr, deadAddr}
	apiOK, socks5Only := probeAndFilterProxyURLLines(context.Background(), lines, "127.0.0.1", 1)

	if len(apiOK) != 1 || apiOK[0] != apiOKAddr {
		t.Fatalf("apiOK: got %v, want exactly [%s]", apiOK, apiOKAddr)
	}
	if len(socks5Only) != 2 || socks5Only[0] != tlsFailedAddr || socks5Only[1] != socks5OnlyAddr {
		t.Fatalf("socks5Only: got %v, want [%s, %s] (order preserved by line index)", socks5Only, tlsFailedAddr, socks5OnlyAddr)
	}
	for _, addr := range append(append([]string{}, apiOK...), socks5Only...) {
		if addr == deadAddr {
			t.Fatalf("dead address %s must not appear in either bucket", deadAddr)
		}
	}
}
