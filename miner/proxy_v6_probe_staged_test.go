package miner

// The staged probe against a fake SOCKS5 server on net.Pipe: every row of the
// verdict table, the negatives included. No socket, no sleep: the only wait
// is the 200 ms deadline of the timeout row.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

const (
	testV4Host = "connect-v4.test.invalid"
	testV6Host = "connect-v6.test.invalid"
)

// fakeSocksBehaviour is what the fake proxy does for one target.
type fakeSocksBehaviour struct {
	// close the stream after reading the greeting, the credentials, or the
	// CONNECT request, instead of answering it
	closeAt socksStage
	// never answer the CONNECT (a black hole)
	silentAtReply bool
	// the CONNECT reply code
	reply byte
	// after a 0x00 reply: terminate TLS (true) or close the stream (false)
	serveTls bool
}

type fakeSocksServer struct {
	t            *testing.T
	requireAuth  bool
	refuseAuth   bool
	refuseDial   bool
	byHost       map[string]fakeSocksBehaviour
	certificate  tls.Certificate
	mu           sync.Mutex
	connectHosts []string
	connectAtyps []byte
	wg           sync.WaitGroup
}

func newTestCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func (self *fakeSocksServer) dial(ctx context.Context) (net.Conn, error) {
	if self.refuseDial {
		return nil, errors.New("dial tcp 198.51.100.9:1080: connect: connection refused")
	}
	client, server := net.Pipe()
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		defer server.Close()
		self.serve(server)
	}()
	return client, nil
}

func (self *fakeSocksServer) serve(conn net.Conn) {
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, greeting[1])); err != nil {
		return
	}
	// the behaviour is per target, but the target is only known later; a
	// close during the greeting or auth is set on every target alike
	anyBehaviour := func(stage socksStage) bool {
		for _, b := range self.byHost {
			if b.closeAt == stage {
				return true
			}
		}
		return false
	}
	if anyBehaviour(socksStageGreeting) {
		return
	}
	if self.requireAuth {
		conn.Write([]byte{socks5Version, 0x02})
		header := make([]byte, 2)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		user := make([]byte, header[1])
		io.ReadFull(conn, user)
		passLength := make([]byte, 1)
		io.ReadFull(conn, passLength)
		io.ReadFull(conn, make([]byte, passLength[0]))
		if anyBehaviour(socksStageAuth) {
			return
		}
		if self.refuseAuth {
			conn.Write([]byte{1, 1})
			return
		}
		conn.Write([]byte{1, 0})
	} else {
		conn.Write([]byte{socks5Version, 0x00})
	}

	request := make([]byte, 5)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	host := make([]byte, request[4])
	io.ReadFull(conn, host)
	io.ReadFull(conn, make([]byte, 2))
	self.mu.Lock()
	self.connectHosts = append(self.connectHosts, string(host))
	self.connectAtyps = append(self.connectAtyps, request[3])
	self.mu.Unlock()

	behaviour := self.byHost[string(host)]
	if behaviour.closeAt == socksStageConnectReply {
		return
	}
	if behaviour.silentAtReply {
		io.Copy(io.Discard, conn)
		return
	}
	reply := []byte{socks5Version, behaviour.reply, 0, socks5AtypIpv4, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(reply[8:], 1080)
	conn.Write(reply)
	if behaviour.reply != 0 || !behaviour.serveTls {
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{self.certificate}})
	tlsConn.Handshake()
	tlsConn.Close()
}

func TestV6ProbeRoundVerdictTable(t *testing.T) {
	certificate, roots := newTestCertificate(t, testV6Host)
	ok := fakeSocksBehaviour{serveTls: true}
	type row struct {
		name        string
		v4          fakeSocksBehaviour
		v6          fakeSocksBehaviour
		requireAuth bool
		refuseAuth  bool
		refuseDial  bool
		timeout     time.Duration
		want        v6Verdict
		wantV6Stage socksStage
	}
	rows := []row{
		{name: "tls over v6 completed", v4: ok, v6: ok, want: v6Yes, wantV6Stage: socksStageDone},
		{name: "tls over v6 completed with auth", v4: ok, v6: ok, requireAuth: true, want: v6Yes, wantV6Stage: socksStageDone},
		{name: "yes needs no v4 control", v4: fakeSocksBehaviour{reply: socksRepGeneralFailure}, v6: ok, want: v6Yes, wantV6Stage: socksStageDone},
		{name: "reply 0x02 not allowed", v4: ok, v6: fakeSocksBehaviour{reply: socksRepNotAllowed}, want: v6No, wantV6Stage: socksStageConnectReply},
		{name: "reply 0x03 network unreachable", v4: ok, v6: fakeSocksBehaviour{reply: socksRepNetworkUnreachable}, want: v6No, wantV6Stage: socksStageConnectReply},
		{name: "reply 0x04 host unreachable", v4: ok, v6: fakeSocksBehaviour{reply: socksRepHostUnreachable}, want: v6No, wantV6Stage: socksStageConnectReply},
		{name: "reply 0x08 address type", v4: ok, v6: fakeSocksBehaviour{reply: socksRepAddrTypeNotSupported}, want: v6No, wantV6Stage: socksStageConnectReply},
		{name: "hang-up at the reply", v4: ok, v6: fakeSocksBehaviour{closeAt: socksStageConnectReply}, want: v6No, wantV6Stage: socksStageConnectReply},
		{name: "reply 0x01 general failure", v4: ok, v6: fakeSocksBehaviour{reply: socksRepGeneralFailure}, want: v6Unknown, wantV6Stage: socksStageConnectReply},
		{name: "reply 0x05 refused", v4: ok, v6: fakeSocksBehaviour{reply: socksRepConnectionRefused}, want: v6Unknown, wantV6Stage: socksStageConnectReply},
		{name: "reply 0x06 ttl", v4: ok, v6: fakeSocksBehaviour{reply: socksRepTTLExpired}, want: v6Unknown, wantV6Stage: socksStageConnectReply},
		{name: "reply 0x07 command", v4: ok, v6: fakeSocksBehaviour{reply: socksRepCommandNotSupported}, want: v6Unknown, wantV6Stage: socksStageConnectReply},
		{name: "tls fails after connect", v4: ok, v6: fakeSocksBehaviour{}, want: v6Unknown, wantV6Stage: socksStageTls},
		{name: "timeout at the reply", v4: ok, v6: fakeSocksBehaviour{silentAtReply: true}, timeout: 200 * time.Millisecond, want: v6Unknown, wantV6Stage: socksStageConnectReply},
		{name: "hang-up during greeting", v4: fakeSocksBehaviour{closeAt: socksStageGreeting}, v6: fakeSocksBehaviour{closeAt: socksStageGreeting}, want: v6Unknown, wantV6Stage: socksStageGreeting},
		{name: "hang-up during auth", v4: fakeSocksBehaviour{closeAt: socksStageAuth}, v6: fakeSocksBehaviour{closeAt: socksStageAuth}, requireAuth: true, want: v6Unknown, wantV6Stage: socksStageAuth},
		{name: "auth refused", v4: ok, v6: ok, requireAuth: true, refuseAuth: true, want: v6Unknown, wantV6Stage: socksStageAuth},
		{name: "proxy dial fails", v4: ok, v6: ok, refuseDial: true, want: v6Unknown, wantV6Stage: socksStageProxyDial},
		{name: "v4 control failed, v6 0x04", v4: fakeSocksBehaviour{reply: socksRepGeneralFailure}, v6: fakeSocksBehaviour{reply: socksRepHostUnreachable}, want: v6Unknown, wantV6Stage: socksStageConnectReply},
		{name: "v4 control hung up, v6 hung up", v4: fakeSocksBehaviour{closeAt: socksStageConnectReply}, v6: fakeSocksBehaviour{closeAt: socksStageConnectReply}, want: v6Unknown, wantV6Stage: socksStageConnectReply},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			server := &fakeSocksServer{
				t:           t,
				requireAuth: r.requireAuth,
				refuseAuth:  r.refuseAuth,
				refuseDial:  r.refuseDial,
				byHost:      map[string]fakeSocksBehaviour{testV4Host: r.v4, testV6Host: r.v6},
				certificate: certificate,
			}
			var auth *proxy.Auth
			if r.requireAuth {
				auth = &proxy.Auth{User: "identity-7", Password: "secret"}
			}
			round := runV6ProbeRound(context.Background(), v6ProbeSettings{
				dialProxy: server.dial,
				auth:      auth,
				v4Target:  v6ProbeTarget{host: testV4Host, port: 443},
				v6Target:  v6ProbeTarget{host: testV6Host, port: 443},
				rootCAs:   roots,
				timeout:   r.timeout,
			})
			server.wg.Wait()
			if round.verdict != r.want {
				t.Fatalf("verdict = %s, want %s (v4 %s, v6 %s)", round.verdict, r.want, round.v4, round.v6)
			}
			if round.v6.stage != r.wantV6Stage {
				t.Fatalf("v6 stopped at %s, want %s (%v)", round.v6.stage, r.wantV6Stage, round.v6.err)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			for i, atyp := range server.connectAtyps {
				if atyp != 0x03 {
					t.Fatalf("CONNECT %d sent address type 0x%02x, want a hostname (0x03)", i, atyp)
				}
			}
			if len(server.connectHosts) == 2 && (server.connectHosts[0] != testV4Host || server.connectHosts[1] != testV6Host) {
				t.Fatalf("CONNECT targets %v, want the v4 control then the v6 host", server.connectHosts)
			}
		})
	}
}

// The pure table, for the combinations a server cannot easily produce.
func TestDecideV6ProbeNegatives(t *testing.T) {
	done := socksAttempt{stage: socksStageDone, reply: socksNoReply}
	for _, c := range []struct {
		name string
		v4   socksAttempt
		v6   socksAttempt
		want v6Verdict
	}{
		{"a timeout that looks like a hang-up is unknown", done, socksAttempt{stage: socksStageConnectReply, reply: socksNoReply, hangUp: true, timeout: true}, v6Unknown},
		{"a reply without the v4 control is unknown", socksAttempt{stage: socksStageProxyDial, reply: socksNoReply}, socksAttempt{stage: socksStageConnectReply, reply: socksRepHostUnreachable}, v6Unknown},
		{"a tls hang-up is unknown", done, socksAttempt{stage: socksStageTls, reply: socksRepSucceeded, hangUp: true}, v6Unknown},
		{"a greeting hang-up is unknown", done, socksAttempt{stage: socksStageGreeting, reply: socksNoReply, hangUp: true}, v6Unknown},
		{"an auth hang-up is unknown", done, socksAttempt{stage: socksStageAuth, reply: socksNoReply, hangUp: true}, v6Unknown},
		{"a request write failure is unknown", done, socksAttempt{stage: socksStageConnectReply, reply: socksNoReply}, v6Unknown},
	} {
		if got := decideV6Probe(c.v4, c.v6); got != c.want {
			t.Fatalf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// The proxy is dialed on its own network, never on the target's family.
func TestV6ProbeDialsTheProxyNetwork(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- struct{}{}
			conn.Close()
		}
	}()
	conn, err := newV6ProbeProxyDialer("tcp", listener.Addr().String())(context.Background())
	if err != nil {
		t.Fatalf("dial the v4 proxy on its own network: %v", err)
	}
	conn.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the proxy saw no connection")
	}
}
