package miner

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// The error strings below are copied from a 500-proxy run's log, not invented.
func TestSocksReplyFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, socksNoReply},
		{"host unreachable", errors.New("socks connect tcp6 203.0.113.9:1080->connect-v6.bringyour.com:443: unknown error host unreachable"), socksRepHostUnreachable},
		{"ruleset", errors.New("socks connect tcp4 203.0.113.9:1080->connect-v4.bringyour.com:443: unknown error connection not allowed by ruleset"), socksRepNotAllowed},
		{"network unreachable", errors.New("socks connect tcp6 x:1->y:443: unknown error network unreachable"), socksRepNetworkUnreachable},
		{"address type", errors.New("socks connect tcp6 x:1->y:443: unknown error address type not supported"), socksRepAddrTypeNotSupported},
		{"general failure", errors.New("socks connect tcp6 x:1->y:443: unknown error general SOCKS server failure"), socksRepGeneralFailure},
		{"target refused", errors.New("socks connect tcp6 x:1->y:443: unknown error connection refused"), socksRepConnectionRefused},
		{"ttl", errors.New("socks connect tcp6 x:1->y:443: unknown error TTL expired"), socksRepTTLExpired},
		{"command", errors.New("socks connect tcp6 x:1->y:443: unknown error command not supported"), socksRepCommandNotSupported},
		{"wrapped", fmt.Errorf("probe: %w", errors.New("socks connect tcp6 x:1->y:443: unknown error host unreachable")), socksRepHostUnreachable},
		// Not proxy replies: none of these may be read as a reply code.
		{"dial to proxy refused", errors.New("socks connect tcp6 x:1->y:443: dial tcp 203.0.113.9:1080: connect: connection refused"), socksNoReply},
		{"unexpected eof", errors.New("socks connect tcp6 x:1->y:443: unexpected EOF"), socksNoReply},
		{"timeout", errors.New("socks connect tcp6 x:1->y:443: read tcp 1.2.3.4:5->203.0.113.9:1080: i/o timeout"), socksNoReply},
		{"unknown code text", errors.New("socks connect tcp6 x:1->y:443: unknown error unknown code: 9"), socksNoReply},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := socksReplyFromError(c.err); got != c.want {
				t.Fatalf("socksReplyFromError(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// End to end from the dial error as the logs print it to the verdict: this is
// the path the real probe takes, which classifyV6Probe alone does not cover.
func TestClassifyV6ProbeErrorFromLoggedText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want v6Verdict
	}{
		{"host unreachable is no", errors.New("socks connect tcp6 a:1->b:443: unknown error host unreachable"), v6No},
		{"address type is no", errors.New("socks connect tcp6 a:1->b:443: unknown error address type not supported"), v6No},
		{"unexpected eof is no", fmt.Errorf("socks connect tcp6 a:1->b:443: %w", io.ErrUnexpectedEOF), v6No},
		{"general failure is unknown", errors.New("socks connect tcp6 a:1->b:443: unknown error general SOCKS server failure"), v6Unknown},
		{"proxy itself refused is unknown", errors.New("socks connect tcp6 a:1->b:443: dial tcp 203.0.113.9:1080: connect: connection refused"), v6Unknown},
		{"target refused is unknown", errors.New("socks connect tcp6 a:1->b:443: unknown error connection refused"), v6Unknown},
		{"timeout is unknown", errors.New("socks connect tcp6 a:1->b:443: i/o timeout"), v6Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyV6ProbeError(c.err); got != c.want {
				t.Fatalf("classifyV6ProbeError(%v) = %s, want %s", c.err, got, c.want)
			}
		})
	}
}
