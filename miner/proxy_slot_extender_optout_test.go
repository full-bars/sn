package miner

import (
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// Only the direct slot keeps the extender network client and the gossip node;
// every proxied slot opts out.
func TestSlotClientStrategySettingsExtenderOptOut(t *testing.T) {
	registry := bandwidth.NewRegistry()

	direct, _, _ := slotClientStrategySettings(nil, registry, nil)
	if direct.DisableExtenderNetwork {
		t.Fatal("the direct slot opted out of the extender network")
	}

	proxied, _, _ := slotClientStrategySettings(&connect.ProxySettings{Address: "127.0.0.1:1080"}, registry, nil)
	if !proxied.DisableExtenderNetwork {
		t.Fatal("a proxied slot kept the extender network")
	}
}
