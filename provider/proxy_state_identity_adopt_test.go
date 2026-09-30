package provider

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestAdoptLegacyProxyState_ExistingIdentityEntryIsKept(t *testing.T) {
	const addr = "1.2.3.4:1080"
	desired := []*connect.ProxySettings{jwtRegressionSettings(addr, "alice")}
	key := desired[0].Key()
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		addr: {ID: 1},
		key:  {ID: 7},
	}}

	adoptLegacyProxyState(state, desired)

	if _, ok := state.Proxies[addr]; ok {
		t.Error("stale bare entry still present")
	}
	if got := state.Proxies[key]; got.ID != 7 {
		t.Fatalf("live identity entry overwritten: id=%d, want 7", got.ID)
	}
}
