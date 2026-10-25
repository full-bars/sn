package miner

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestProviderDisablesFamilyTransports pins the policy table. The group is
// dropped only on positive evidence that the proxy cannot carry IPv6, so a
// proxy that might is never demoted on a guess.
func TestProviderDisablesFamilyTransports(t *testing.T) {
	proxied := &connect.ProxySettings{Network: "tcp", Address: "198.51.100.1:1080"}
	for _, c := range []struct {
		name    string
		proxy   *connect.ProxySettings
		verdict v6Verdict
		want    bool
	}{
		{"direct slot keeps the group", nil, v6No, false},
		{"no verdict keeps the group", proxied, v6Unknown, false},
		{"yes keeps the group", proxied, v6Yes, false},
		{"known no drops to one transport", proxied, v6No, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := providerDisablesFamilyTransports(c.proxy, c.verdict); got != c.want {
				t.Fatalf("decision = %v, want %v", got, c.want)
			}
		})
	}
}

// TestProviderFamilyTransportTTLs: a no buys 15 minutes, a yes six hours, and
// expiry returns the proxy to unknown, which means the group again.
func TestProviderFamilyTransportTTLs(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	proxied := &connect.ProxySettings{Network: "tcp", Address: "198.51.100.1:1080"}
	for _, c := range []struct {
		name    string
		verdict v6Verdict
		age     time.Duration
		want    bool
	}{
		{"no at 14m59s still drops the group", v6No, 14*time.Minute + 59*time.Second, true},
		{"no at 15m returns the group", v6No, 15 * time.Minute, false},
		{"yes at 6h returns the group", v6Yes, 6 * time.Hour, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ledger := newV6Ledger(func() time.Time { return now })
			ledger.entries[v6LedgerKey(proxied)] = v6Entry{Verdict: c.verdict, CheckedAt: now.Add(-c.age)}
			if got := providerDisablesFamilyTransports(proxied, ledger.Verdict(v6LedgerKey(proxied))); got != c.want {
				t.Fatalf("decision = %v, want %v", got, c.want)
			}
		})
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
	assignment := "settings.DisablePlatformFamilyTransports = providerDisablesFamilyTransports(proxySettings, v6Verdicts().Verdict(v6LedgerKey(proxySettings)))"
	if !strings.Contains(string(source), assignment) {
		t.Fatalf("miner/run.go no longer sets DisablePlatformFamilyTransports (%q); a proxied identity would rebuild the family group", assignment)
	}
}
