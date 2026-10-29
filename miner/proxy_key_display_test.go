package miner

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// The slot-stop log lines print a proxy identity key. The key joins the user
// to the address with a control separator and carries the user in full, so
// printing it raw both exposes the username and puts an unprintable byte in
// the operator log. Everything operator-facing goes through proxyKeyDisplay.
func TestProxyKeyDisplayRedactsUser(t *testing.T) {
	settings := &connect.ProxySettings{
		Address: "dc.decodo.com:10072",
		Auth:    &proxy.Auth{User: "sppmr4vcnj", Password: "hunter2hunter2"},
	}
	key := settings.Key()

	got := proxyKeyDisplay(key)

	if strings.Contains(got, "sppmr4vcnj") {
		t.Fatalf("display leaks the username: %q", got)
	}
	if strings.ContainsRune(got, '\x1f') {
		t.Fatalf("display leaks the key separator: %q", got)
	}
	if strings.Contains(got, "hunter2hunter2") {
		t.Fatalf("display leaks the password: %q", got)
	}
	want := "dc.decodo.com:10072 (user sp***nj)"
	if got != want {
		t.Fatalf("display = %q, want %q", got, want)
	}
}

// A proxy without credentials must still render as a bare address, so the
// display does not grow a useless "(user ...)" tail.
func TestProxyKeyDisplayWithoutAuth(t *testing.T) {
	settings := &connect.ProxySettings{Address: "1.2.3.4:1080"}
	got := proxyKeyDisplay(settings.Key())
	if got != "1.2.3.4:1080" {
		t.Fatalf("display = %q, want %q", got, "1.2.3.4:1080")
	}
}

// Short usernames are fully hidden rather than half-revealed.
func TestObfuscateUserShortName(t *testing.T) {
	if got := obfuscateUser("ab1"); got != "***" {
		t.Fatalf("short user = %q, want %q", got, "***")
	}
	if got := obfuscateUser("abcdef"); got != "ab***ef" {
		t.Fatalf("six char user = %q, want %q", got, "ab***ef")
	}
}

// The legacy config parser can keep a malformed URI as the address. Its
// userinfo and path must not reach the operator log, with or without a
// separate auth user.
func TestProxyKeyDisplayRejectsEmbeddedUserinfo(t *testing.T) {
	bad := []string{
		"socks5://alice:secret@host:1080/token",
		"alice:secret@host:1080",
		"host:1080/token",
		"not an address",
		"",
	}
	for _, address := range bad {
		for _, auth := range []*proxy.Auth{nil, {User: "sppmr4vcnj", Password: "pw"}} {
			key := (&connect.ProxySettings{Address: address, Auth: auth}).Key()
			got := proxyKeyDisplay(key)
			for _, leak := range []string{"alice", "secret", "token", "sppmr4vcnj", "\x1f"} {
				if strings.Contains(got, leak) {
					t.Fatalf("display of %q leaks %q: %q", address, leak, got)
				}
			}
		}
	}
	if got := proxyKeyDisplay("[2001:db8::1]:1080"); got != "[2001:db8::1]:1080" {
		t.Fatalf("ipv6 host:port = %q", got)
	}
	if got := proxyKeyDisplay("direct"); got != "direct" {
		t.Fatalf("direct sentinel = %q", got)
	}
}
