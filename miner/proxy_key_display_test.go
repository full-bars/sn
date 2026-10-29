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
