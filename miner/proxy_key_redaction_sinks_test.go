package miner

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// Every operator-facing sink that receives a proxy identity key is driven for
// real and its output checked for the key separator, the user and the password.

const (
	sinkTestUser     = "sppmr4vcnj"
	sinkTestPassword = "hunter2hunter2"
	sinkTestAddress  = "dc.decodo.com:10072"
)

func sinkTestSettings() *connect.ProxySettings {
	return reloadTestSettings(sinkTestAddress, sinkTestUser, sinkTestPassword)
}

func assertNoKeyLeak(t *testing.T, sink, text string, extra ...string) {
	t.Helper()
	for _, leak := range append([]string{"\x1f", sinkTestUser, sinkTestPassword}, extra...) {
		if strings.Contains(text, leak) {
			t.Errorf("%s leaks %q: %q", sink, leak, text)
		}
	}
}

// Sink: the duplicate-address refusal. The address is an operator-written
// field the legacy parser may keep malformed, userinfo included.
func TestAddressRefusalDoesNotPrintMalformedAddressOrOwner(t *testing.T) {
	for _, address := range []string{
		"socks5://alice:secret@host:1080/token",
		"alice:secret@host:1080",
		"host:1080;password=secret",
		sinkTestAddress,
	} {
		r := newMinerProxyReloader(nil, nil)
		first := reloadTestSettings(address, sinkTestUser, sinkTestPassword)
		second := reloadTestSettings(address, "othername77", "otherpass")
		if err := r.reserveAddress(first); err != nil {
			t.Fatal(err)
		}
		err := r.reserveAddress(second)
		if err == nil {
			t.Fatalf("%q: a second identity reserved the address", address)
		}
		assertNoKeyLeak(t, "reserveAddress refusal for "+address, err.Error(), "alice", "secret", "othername", "otherpass", "token")
	}
}
