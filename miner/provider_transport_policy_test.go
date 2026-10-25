package miner

import (
	"testing"

	"github.com/urnetwork/connect"
)

// TestProviderDisablesFamilyTransports pins the slot policy: proxied identities
// get one family-agnostic transport, the direct identity keeps the group. The
// two cases are the whole rule, and getting it backwards costs either the
// direct slot's dual-stack record or every proxied slot two transports that can
// only fail.
func TestProviderDisablesFamilyTransports(t *testing.T) {
	if providerDisablesFamilyTransports(nil) {
		t.Fatal("the direct slot was given one transport; it has real routes for both pins")
	}
	for _, settings := range []*connect.ProxySettings{
		{Network: "tcp", Address: "198.51.100.1:1080"},
		{Network: "tcp", Address: "203.0.113.7:1080", Auth: nil},
	} {
		if !providerDisablesFamilyTransports(settings) {
			t.Fatalf("proxied slot %s kept the family group; a pinned dial cannot leave the proxy", settings.Address)
		}
	}
}
