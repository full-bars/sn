package provider

import (
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// TestPruneProxyHistoryStores_FallbackKeepsIdentityKeys: when the desired set
// cannot be read, the prune falls back to the live registry. That fallback
// used the health report's "proxy[N] (addr)" display keys, which match nothing
// in the identity-keyed stores, so one transient read error wiped every
// running proxy's failure history and proven flag.
func TestPruneProxyHistoryStores_FallbackKeepsIdentityKeys(t *testing.T) {
	home := withTempHome(t)
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	prevProven := globalProvenProxies
	globalProvenProxies = &provenProxySet{proven: map[string]bool{}}
	t.Cleanup(func() { globalProvenProxies = prevProven })

	// An unreadable configured source makes desiredAddressesForHistoryPruning error.
	missing := filepath.Join(home, "does-not-exist.txt")
	if err := writeProxyState(&ProxyState{Source: missing, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := desiredAddressesForHistoryPruning(); err == nil {
		t.Fatal("setup: expected desiredAddressesForHistoryPruning to fail")
	}

	live := (&connect.ProxySettings{Network: "tcp", Address: "10.0.0.1:1080", Auth: &proxy.Auth{User: "u1", Password: "p"}}).Key()
	gone := (&connect.ProxySettings{Network: "tcp", Address: "10.0.0.2:1080"}).Key()
	RegisterProxy(7, "10.0.0.1:1080", live)
	if RegisterProxyBandwidth(7) == nil {
		t.Fatal("setup: RegisterProxyBandwidth returned nil")
	}

	for _, k := range []string{live, gone} {
		globalProxyFailureHistory.RecordFailure(k)
		globalProvenProxies.MarkSucceeded(k)
	}

	pruneProxyHistoryStores()

	if globalProxyFailureHistory.FailureCount(live) != 1 {
		t.Fatal("running proxy's failure history was pruned by the fallback keep-set")
	}
	if !globalProvenProxies.HasSucceeded(live) {
		t.Fatal("running proxy's proven flag was pruned by the fallback keep-set")
	}
	if globalProxyFailureHistory.FailureCount(gone) != 0 || globalProvenProxies.HasSucceeded(gone) {
		t.Fatal("unregistered identity should still be pruned")
	}
}
