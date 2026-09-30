package provider

import (
	"testing"

	"github.com/urnetwork/connect"

	"golang.org/x/net/proxy"

	"github.com/urfoundation/sn/internal/connectx"
)

// The reload path keys its per-proxy maps with connectx.ProxyKey, while
// provide.go and the rest of the package call ProxySettings.Key() on the
// pinned engine. If those two ever produced different strings for the same
// proxy, a proxy registered by reload would be looked up under a key that is
// not in the table, and getProxyIndex would return 0: every such proxy would
// log as proxy[0] and share one health slot.
//
// They are the same function today, but nothing but this test holds them
// together. The pinned engine's Key() and connectx.ProxyKey are separate
// implementations in separate modules, so either can drift on its own.
func TestIdentityKeyAgreesWithEngineKey(t *testing.T) {
	cases := []struct {
		name string
		auth *proxy.Auth
	}{
		{"no auth at all", nil},
		{"auth with empty user", &proxy.Auth{}},
		{"auth with empty user and a password", &proxy.Auth{Password: "secret"}},
		{"user set", &proxy.Auth{User: "alice", Password: "p"}},
		{"user set, no password", &proxy.Auth{User: "bob"}},
	}
	const addr = "dc.decodo.com:8001"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: c.auth}
			engine := s.Key()
			port := connectx.ProxyKey(s.Address, s.Auth)
			if engine != port {
				t.Fatalf("keys differ for %s: engine %q, connectx %q; reload would register "+
					"under one and provide.go would look up the other, so the proxy reads as index 0",
					c.name, engine, port)
			}
		})
	}
}

// Two accounts on one shared gateway must be two identities, not one slot. The
// account, not the address, decides which backend you land on, so a gateway
// with alice and bob is genuinely two proxies.
func TestTwoUsersOnOneAddressAreDistinctIdentities(t *testing.T) {
	const addr = "dc.decodo.com:8001"
	alice := &connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: "alice", Password: "p1"}}
	bob := &connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: "bob", Password: "p2"}}

	aliceKey, bobKey := alice.Key(), bob.Key()
	if aliceKey == bobKey {
		t.Fatalf("two users on %s collapsed to one identity %q; they are different proxies", addr, aliceKey)
	}
	// A password rotation is NOT a new proxy: same account, new secret, same
	// identity, so the proxy keeps its health history and earnings.
	aliceRotated := &connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: "alice", Password: "p2"}}
	if got := aliceRotated.Key(); got != aliceKey {
		t.Fatalf("a password rotation changed the identity from %q to %q; it must not, or the "+
			"proxy loses its ID, health history and earnings on every credential refresh", aliceKey, got)
	}

	// Registering both through the real launch path must yield two distinct
	// indexes, each of which resolves back to its own key. RegisterProxy is
	// what populates the reverse index map, so going through it here is what
	// proves the forward and reverse tables agree rather than just the map.
	setProxyIndex(aliceKey, 9001)
	setProxyIndex(bobKey, 9002)
	RegisterProxy(9001, addr, aliceKey)
	RegisterProxy(9002, addr, bobKey)
	t.Cleanup(func() {
		UnregisterProxy(9001)
		UnregisterProxy(9002)
		deleteProxyIndex(aliceKey)
		deleteProxyIndex(bobKey)
	})
	if got := getProxyIndex(aliceKey); got != 9001 {
		t.Errorf("getProxyIndex(alice) = %d, want 9001", got)
	}
	if got := getProxyIndex(bobKey); got != 9002 {
		t.Errorf("getProxyIndex(bob) = %d, want 9002", got)
	}
	if a, b := ProxyKeyByIndex(9001), ProxyKeyByIndex(9002); a != aliceKey || b != bobKey {
		t.Errorf("reverse lookup crossed: index 9001 -> %q, 9002 -> %q (want %q, %q)", a, b, aliceKey, bobKey)
	}
	// An unregistered key must NOT silently read as some other proxy's slot:
	// getProxyIndex returns -1, not 0, because index 0 is the direct transport.
	if got := getProxyIndex("never-registered"); got != -1 {
		t.Errorf("an unregistered key returned index %d, want -1 (0 is the direct "+
			"transport, so a miss that reads as 0 would log a proxy as the direct one)", got)
	}
}
