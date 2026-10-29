package miner

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The factory's errors name the proxy. The stored address can be malformed
// with userinfo, so neither the refusal nor the dial failure may print it.
func TestH3FactoryErrorsDoNotPrintMalformedAddress(t *testing.T) {
	for _, address := range []string{
		"socks5://alice:secret@host:1080/token",
		"alice:secret@127.0.0.1:1",
	} {
		for name, verdict := range map[string]proxyUDPStatus{"refusal": proxyUDPNoRelay, "dial": proxyUDPUnknown} {
			settings := &connect.ProxySettings{Network: "tcp", Address: address}
			stubH3UDPVerdict(t, settings, verdict)
			pc, err := newH3PacketConnFactory(settings, nil, address)(context.Background())
			if err == nil {
				pc.Close()
				t.Fatalf("%s %q: the factory returned an endpoint", name, address)
			}
			for _, leak := range []string{"alice", "secret", "token"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("%s %q leaks %q: %v", name, address, leak, err)
				}
			}
		}
	}
}
