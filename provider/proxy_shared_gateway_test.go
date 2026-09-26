package provider

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// A shared-gateway proxy provider hands one host:port to several accounts; the
// account (user) decides the backend IP. Everything that keys a proxy must
// therefore use the identity key (address, or address+user), never the bare
// address, or two accounts silently collapse into one entry.
//
// These tests use two ACCOUNTS ON ONE ADDRESS on purpose. A suite with only
// unique addresses cannot distinguish the two keying schemes and would pass
// either way — which is why the address-keyed consumers went unnoticed.

const sharedGateway = "gw.example:1080"

// TestEarnTrackerKeepsSiblingAccountsApart is the regression test for the earn
// tracker being fed the display snapshot. The display snapshot is keyed
// "proxy[N] (address)", which collapses both accounts onto one address entry,
// while EarnedSince is asked about an identity key — so the lookup could never
// match and the earn-skip optimization was dead for every credentialed proxy.
func TestEarnTrackerKeepsSiblingAccountsApart(t *testing.T) {
	tr := newPerProxyEarnTracker()

	keyA := sharedGateway + "\x1falice"
	keyB := sharedGateway + "\x1fbob"

	bwA := &bandwidth.ProxyBandwidth{}
	bwB := &bandwidth.ProxyBandwidth{}

	// Baseline: two accounts on one address, each with its own counter.
	tr.Update(map[string]*bandwidth.ProxyBandwidth{keyA: bwA, keyB: bwB})
	// A later tick where BOTH moved. Only a positive delta marks an earn.
	bwA.BillableRx.Store(200)
	bwB.BillableRx.Store(9)
	tr.Update(map[string]*bandwidth.ProxyBandwidth{keyA: bwA, keyB: bwB})

	if !tr.EarnedSince(keyA, time.Hour) {
		t.Fatalf("account A earned but EarnedSince(%q) said no", keyA)
	}
	if !tr.EarnedSince(keyB, time.Hour) {
		t.Fatalf("account B earned but EarnedSince(%q) said no: sibling accounts collapsed onto one address", keyB)
	}
}

// TestProxyBandwidthSnapshotByKeySeparatesAccounts pins the property the
// collector depends on: the identity-keyed bandwidth snapshot keeps sibling
// accounts apart, while the display snapshot collapses them. The collector
// feeds the earn tracker from the former; feeding it the latter is what made
// the earn-skip lookup dead for credentialed proxies.
func TestProxyBandwidthSnapshotByKeySeparatesAccounts(t *testing.T) {
	resetProxyHealthForTest()
	t.Cleanup(resetProxyHealthForTest)

	keyA := sharedGateway + "\x1falice"
	keyB := sharedGateway + "\x1fbob"
	RegisterProxy(0, sharedGateway, keyA)
	RegisterProxy(1, sharedGateway, keyB)
	// The identity-keyed snapshot only carries proxies that have bandwidth
	// registered, which is what the running provider has.
	RegisterProxyBandwidth(0)
	RegisterProxyBandwidth(1)

	byKey := ProxyBandwidthSnapshotByKey()
	if len(byKey) != 2 {
		t.Fatalf("identity-keyed snapshot has %d entries, want 2: sibling accounts at one address collapsed", len(byKey))
	}
	if _, ok := byKey[keyA]; !ok {
		t.Fatalf("identity-keyed snapshot is missing %q", keyA)
	}
	if _, ok := byKey[keyB]; !ok {
		t.Fatalf("identity-keyed snapshot is missing %q", keyB)
	}

	// The display snapshot is indexed by proxy ID and labels both with the same
	// address, so it cannot answer a per-account question.
	_, _, _, display, _ := ProxyHealthSnapshot()
	if len(display) != 2 {
		t.Fatalf("display snapshot has %d entries, want 2", len(display))
	}
	seen := map[string]int{}
	for label := range display {
		_, ip := parseProxyString(label)
		seen[ip]++
	}
	if seen[sharedGateway] != 2 {
		t.Fatalf("display snapshot labels = %v, want both accounts labelled with the same address", seen)
	}
}

// The degraded reaper looks proxies up in the cancel map, which is identity
// keyed. Keying it by Address instead made every credentialed degraded proxy
// invisible to the reaper, i.e. immune to being reaped.
func TestReaperSeesCredentialedProxyAtSharedAddress(t *testing.T) {
	var cancelMu sync.Mutex
	cancelled := false

	key := sharedGateway + "\x1falice"
	cancelMap := map[string]context.CancelFunc{
		key: func() { cancelled = true },
	}

	degraded := []DegradedProxyEntry{{
		Index:   0,
		Address: sharedGateway, // operator-facing dial target
		Key:     key,           // identity
	}}

	only := onlyCancellableProxies(degraded, cancelMap, &cancelMu)
	if len(only) != 1 {
		t.Fatalf("cancellable = %d, want 1: a credentialed proxy is invisible to the reaper", len(only))
	}

	reaped := reapProxies(only, cancelMap, &cancelMu, func(string) bool { return true })
	if reaped != 1 || !cancelled {
		t.Fatalf("reaped = %d cancelled = %v, want 1 and true", reaped, cancelled)
	}
	if _, still := cancelMap[key]; still {
		t.Fatalf("the reaped proxy is still in the cancel map")
	}
}

// Grade reporting files grades under the identity key. Looking one up by the
// address parsed out of the display label misses for every credentialed proxy,
// so the hub saw them all as ungraded.
func TestGradeReportResolvesIdentityForCredentialedProxy(t *testing.T) {
	resetProxyHealthForTest()
	t.Cleanup(resetProxyHealthForTest)

	key := sharedGateway + "\x1falice"
	RegisterProxy(0, sharedGateway, key)

	if got := ProxyKeyByIndex(0); got != key {
		t.Fatalf("ProxyKeyByIndex(0) = %q, want the identity key %q", got, key)
	}

	// The display label parses to the bare address, which is NOT the key the
	// grades are filed under. Any lookup built on that parse is a miss, which
	// is why the reporter must resolve the identity from the index instead.
	_, ip := parseProxyString("proxy[0] (" + sharedGateway + ")")
	if ip == key {
		t.Fatalf("the parse unexpectedly produced the identity key; the test would prove nothing")
	}
	if ip != sharedGateway {
		t.Fatalf("parse = %q, want the bare address %q", ip, sharedGateway)
	}
}
