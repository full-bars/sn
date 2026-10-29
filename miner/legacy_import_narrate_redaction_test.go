package miner

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// The narration names a slot by address and obfuscated user. A malformed stored
// address carries userinfo, so it must come out as the placeholder.
func TestLegacyImportNarrateDisplayRedactsMalformedAddress(t *testing.T) {
	for _, auth := range []*proxy.Auth{nil, {User: "bob12345", Password: "pw"}} {
		settings := &connect.ProxySettings{Network: "socks5", Address: "socks5://alice:secret@host:1080/token", Auth: auth}
		text := legacyImportNarrateDisplay(settings)
		for _, leak := range []string{"alice", "secret", "token"} {
			if strings.Contains(text, leak) {
				t.Errorf("narration leaks %q: %q", leak, text)
			}
		}
	}
}
