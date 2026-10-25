package miner

import (
	"os"
	"strings"
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

// TestRunSetsDisableFamilyTransports checks the wiring, not the helper.
//
// The helper test above passes whether or not run.go ever calls it, which is the
// shape of the failure the READY signal had: the unit that owned the behaviour
// was correct and nothing checked that anything used it. A proxied slot would
// quietly rebuild the family group and this suite would stay green.
func TestRunSetsDisableFamilyTransports(t *testing.T) {
	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	assignment := "settings.DisablePlatformFamilyTransports = providerDisablesFamilyTransports(proxySettings)"
	if !strings.Contains(string(source), assignment) {
		t.Fatalf("miner/run.go no longer sets DisablePlatformFamilyTransports (%q); a proxied identity would rebuild the family group", assignment)
	}
}
