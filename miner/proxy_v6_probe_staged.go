package miner

// proxy_v6_probe_staged.go - one probe round that decides whether a SOCKS5
// proxy can carry this provider's IPv6 pin.
//
// classifyV6Probe (proxy_v6_probe.go) reads one dial error and cannot tell an
// IPv4-only proxy from a dead one: both can hang up. A definitive "no" needs a
// control. So one round makes two attempts through the SAME proxy, each with
// a hand-rolled SOCKS5 client (the pattern of socks5_udp.go) that records how
// far it got:
//
//  1. a v4 control: a CONNECT to the platform's v4 host, which proves the
//     proxy, its credentials and its route to the platform work right now;
//  2. the v6 attempt: a CONNECT to the v6 host, sent as a HOSTNAME exactly as
//     the pinned transport sends it (the name has only AAAA records, so the
//     proxy must reach it over IPv6), then a TLS handshake over that stream.
//
// The verdict (decideV6Probe) is:
//   - yes only when the TLS handshake over the v6 stream completed: the proxy
//     carried bytes both ways to the v6 host;
//   - no only when the v4 control completed AND the v6 attempt stopped at its
//     CONNECT reply with 0x02, 0x03, 0x04 or 0x08, or with the proxy hanging
//     up at the reply (the dominant IPv4-only behaviour on the LA7 pool);
//   - unknown otherwise: a timeout anywhere, a hang-up during the greeting or
//     authentication, a failed dial to the proxy, a failed v4 control, any
//     other reply, or a TLS failure after a successful CONNECT.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

// v6ProbeAttemptTimeout bounds each attempt of a round, from the proxy dial to
// the end of the TLS handshake.
const v6ProbeAttemptTimeout = 15 * time.Second

// socksStage is where an attempt stopped. Each failing stage names the step
// that did not complete; stageDone is a completed attempt.
type socksStage int

const (
	socksStageProxyDial socksStage = iota
	socksStageGreeting
	socksStageAuth
	socksStageConnectReply
	socksStageTls
	socksStageDone
)

func (s socksStage) String() string {
	switch s {
	case socksStageProxyDial:
		return "proxy-dial"
	case socksStageGreeting:
		return "greeting"
	case socksStageAuth:
		return "auth"
	case socksStageConnectReply:
		return "connect-reply"
	case socksStageTls:
		return "tls"
	case socksStageDone:
		return "done"
	default:
		return "unknown"
	}
}

// socksAttempt is how far one attempt got.
type socksAttempt struct {
	stage socksStage
	// the CONNECT reply code, socksNoReply when none was read
	reply int
	// the proxy closed or reset the stream at the stopping stage
	hangUp bool
	// the stopping stage ran out of time
	timeout bool
	err     error
}

func (a socksAttempt) String() string {
	if a.stage == socksStageDone {
		return "done"
	}
	reply := "none"
	if a.reply != socksNoReply {
		reply = fmt.Sprintf("0x%02x", a.reply)
	}
	return fmt.Sprintf("stopped=%s reply=%s hangup=%t timeout=%t", a.stage, reply, a.hangUp, a.timeout)
}

// v6ProbeTarget is one host the round connects to, named as the transport
// names it.
type v6ProbeTarget struct {
	host string
	port int
}

func (t v6ProbeTarget) String() string {
	return net.JoinHostPort(t.host, fmt.Sprint(t.port))
}

// v6ProbeRound is the evidence of one round and its verdict.
type v6ProbeRound struct {
	v4      socksAttempt
	v6      socksAttempt
	verdict v6Verdict
}

// decideV6Probe is the verdict table in the file comment.
func decideV6Probe(v4 socksAttempt, v6 socksAttempt) v6Verdict {
	if v6.stage == socksStageDone {
		return v6Yes
	}
	if v4.stage != socksStageDone {
		// nothing proves the proxy itself works right now
		return v6Unknown
	}
	if v6.stage != socksStageConnectReply || v6.timeout {
		return v6Unknown
	}
	switch v6.reply {
	case socksRepNotAllowed, socksRepNetworkUnreachable, socksRepHostUnreachable, socksRepAddrTypeNotSupported:
		return v6No
	case socksNoReply:
		if v6.hangUp {
			return v6No
		}
	}
	return v6Unknown
}

// v6ProbeSettings is what one round needs. dialProxy opens a stream to the
// proxy itself, on the PROXY's network (never "tcp6": the proxy's own address
// family has nothing to do with the target's).
type v6ProbeSettings struct {
	dialProxy func(ctx context.Context) (net.Conn, error)
	auth      *proxy.Auth
	v4Target  v6ProbeTarget
	v6Target  v6ProbeTarget
	// rootCAs verifies the v6 host's certificate; nil is the system pool
	rootCAs *x509.CertPool
	timeout time.Duration
}

// newV6ProbeProxyDialer dials the proxy the way connect does for this
// identity's transports (net.go, ProxySettings.NewDialContext): the network
// is the proxy's own, which proxy.SOCKS5 takes as its first argument.
func newV6ProbeProxyDialer(network string, address string) func(ctx context.Context) (net.Conn, error) {
	if network == "" {
		network = "tcp"
	}
	dialer := &net.Dialer{}
	return func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
}

// runV6ProbeRound runs the v4 control and the v6 attempt through the proxy
// and decides. The attempts run one after the other, never in parallel, so a
// round holds at most one connection to the proxy.
func runV6ProbeRound(ctx context.Context, settings v6ProbeSettings) v6ProbeRound {
	timeout := settings.timeout
	if timeout <= 0 {
		timeout = v6ProbeAttemptTimeout
	}
	attempt := func(target v6ProbeTarget, withTls bool) socksAttempt {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var tlsConfig *tls.Config
		if withTls {
			tlsConfig = &tls.Config{
				ServerName: target.host,
				RootCAs:    settings.rootCAs,
				MinVersion: tls.VersionTLS12,
			}
		}
		return runSocksAttempt(attemptCtx, settings.dialProxy, settings.auth, target, tlsConfig)
	}
	round := v6ProbeRound{}
	round.v4 = attempt(settings.v4Target, false)
	round.v6 = attempt(settings.v6Target, true)
	round.verdict = decideV6Probe(round.v4, round.v6)
	return round
}

// runSocksAttempt is one staged SOCKS5 CONNECT (RFC 1928, RFC 1929), followed
// by a TLS handshake when tlsConfig is set. The target is always sent as a
// domain name (ATYP 0x03), as golang.org/x/net/proxy sends a hostname.
func runSocksAttempt(
	ctx context.Context,
	dialProxy func(ctx context.Context) (net.Conn, error),
	auth *proxy.Auth,
	target v6ProbeTarget,
	tlsConfig *tls.Config,
) socksAttempt {
	result := socksAttempt{stage: socksStageProxyDial, reply: socksNoReply}
	fail := func(stage socksStage, err error) socksAttempt {
		result.stage = stage
		result.err = err
		result.hangUp = isSocksHangUp(err)
		result.timeout = isSocksTimeout(ctx, err)
		return result
	}

	conn, err := dialProxy(ctx)
	if err != nil {
		return fail(socksStageProxyDial, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	// a cancel ends a blocked read at once
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	methods := []byte{0x00}
	if auth != nil && auth.User != "" {
		methods = []byte{0x00, 0x02}
	}
	if _, err := conn.Write(append([]byte{socks5Version, byte(len(methods))}, methods...)); err != nil {
		return fail(socksStageGreeting, err)
	}
	choice := make([]byte, 2)
	if _, err := io.ReadFull(conn, choice); err != nil {
		return fail(socksStageGreeting, err)
	}
	if choice[0] != socks5Version {
		return fail(socksStageGreeting, fmt.Errorf("socks5: unexpected version %d", choice[0]))
	}
	switch choice[1] {
	case 0x00:
	case 0x02:
		if auth == nil || auth.User == "" || len(auth.User) > 255 || len(auth.Password) > 255 {
			return fail(socksStageAuth, errors.New("socks5: the proxy requires credentials this identity cannot send"))
		}
		request := []byte{1, byte(len(auth.User))}
		request = append(request, auth.User...)
		request = append(request, byte(len(auth.Password)))
		request = append(request, auth.Password...)
		if _, err := conn.Write(request); err != nil {
			return fail(socksStageAuth, err)
		}
		status := make([]byte, 2)
		if _, err := io.ReadFull(conn, status); err != nil {
			return fail(socksStageAuth, err)
		}
		if status[1] != 0 {
			return fail(socksStageAuth, errors.New("socks5: authentication refused"))
		}
	default:
		return fail(socksStageGreeting, fmt.Errorf("socks5: no acceptable method (0x%02x)", choice[1]))
	}

	if len(target.host) > 255 {
		return fail(socksStageConnectReply, errors.New("socks5: target host too long"))
	}
	request := []byte{socks5Version, 0x01, 0x00, 0x03, byte(len(target.host))}
	request = append(request, target.host...)
	request = binary.BigEndian.AppendUint16(request, uint16(target.port))
	if _, err := conn.Write(request); err != nil {
		// the proxy closed before it read the request: not a hang-up AT the
		// reply, which is the only hang-up that counts as evidence
		attempt := fail(socksStageConnectReply, err)
		attempt.hangUp = false
		return attempt
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fail(socksStageConnectReply, err)
	}
	if header[0] != socks5Version {
		return fail(socksStageConnectReply, fmt.Errorf("socks5: unexpected version %d", header[0]))
	}
	result.reply = int(header[1])
	if header[1] != socksRepSucceeded {
		return fail(socksStageConnectReply, fmt.Errorf("socks5: connect reply 0x%02x", header[1]))
	}
	var boundLength int
	switch header[3] {
	case socks5AtypIpv4:
		boundLength = 4
	case socks5AtypIpv6:
		boundLength = 16
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return fail(socksStageConnectReply, err)
		}
		boundLength = int(length[0])
	default:
		return fail(socksStageConnectReply, fmt.Errorf("socks5: unknown bound address type %d", header[3]))
	}
	if _, err := io.ReadFull(conn, make([]byte, boundLength+2)); err != nil {
		return fail(socksStageConnectReply, err)
	}

	if tlsConfig != nil {
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fail(socksStageTls, err)
		}
	}
	result.stage = socksStageDone
	return result
}

// isSocksHangUp is a stream the proxy closed or reset.
func isSocksHangUp(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrClosedPipe)
}

// isSocksTimeout is a stage that ran out of time, or a round that was
// canceled: neither says anything about the proxy's families.
func isSocksTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
