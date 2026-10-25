package miner

import (
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

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

// A proxied slot runs one family-agnostic platform transport and no extender
// role; the direct slot keeps the family group and the role.
func TestProviderSlotRoleSettingsSingleTransportForProxiedSlots(t *testing.T) {
	if sdk.DefaultDeviceLocalSettings().DisablePlatformFamilyTransports {
		t.Fatal("the sdk default disables the family group, so the slot setting proves nothing")
	}

	direct := ProviderDeviceSettings([32]byte{})
	applyProviderSlotRoleSettings(direct, nil)
	if direct.DisablePlatformFamilyTransports {
		t.Fatal("the direct slot lost the family transport group")
	}
	if !direct.ProvideExtenderEnabled {
		t.Fatal("the direct slot lost the extender role")
	}

	proxied := ProviderDeviceSettings([32]byte{})
	applyProviderSlotRoleSettings(proxied, &connect.ProxySettings{Address: "127.0.0.1:1080"})
	if !proxied.DisablePlatformFamilyTransports {
		t.Fatal("a proxied slot kept the family transport group")
	}
	if proxied.ProvideExtenderEnabled {
		t.Fatal("a proxied slot holds the extender role")
	}
}
