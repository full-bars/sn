package miner

import (
	"testing"

	"github.com/urnetwork/connect"
)

func proxyAddInlineForTest(t *testing.T, entry string) {
	t.Helper()
	proxyAdd(parseArgsForTest(t, []string{"proxy", "add", entry, "-f"}))
}

func storedPasswordsByUser(settings []*connect.ProxySettings) map[string][]string {
	out := map[string][]string{}
	for _, s := range settings {
		if s.Auth != nil {
			out[s.Auth.User] = append(out[s.Auth.User], s.Auth.Password)
		}
	}
	return out
}

// Adding the same address and user with a new password replaces the stored
// entry. Both used to be stored and read back as two copies of one identity.
func TestProxyAddReplacesPasswordOfStoredIdentity(t *testing.T) {
	withTempHome(t)
	proxyAddInlineForTest(t, "198.51.100.60:1080:alice:old")
	proxyAddInlineForTest(t, "198.51.100.60:1080:alice:new")

	got := storedPasswordsByUser(readProxySettingsLegacy())
	if len(got["alice"]) != 1 || got["alice"][0] != "new" {
		t.Fatalf("alice's stored passwords = %v, want only [new]", got["alice"])
	}
}

// A different user at the same address is a different account and stays.
func TestProxyAddKeepsOtherAccountAtSharedAddress(t *testing.T) {
	withTempHome(t)
	proxyAddInlineForTest(t, "198.51.100.61:1080:alice:pw")
	proxyAddInlineForTest(t, "198.51.100.61:1080:bob:pw")
	proxyAddInlineForTest(t, "198.51.100.61:1080:alice:pw2")

	got := storedPasswordsByUser(readProxySettingsLegacy())
	if len(got["alice"]) != 1 || got["alice"][0] != "pw2" || len(got["bob"]) != 1 || got["bob"][0] != "pw" {
		t.Fatalf("stored accounts = %v, want alice=[pw2] bob=[pw]", got)
	}
}
