package miner

// Ported from the 3.23-fix v33 provider's proxy_audit_status_test.go (read-only
// source candidate/v33-ops).
//
// Tests for the published audit status snapshot. The two v33 tests that drive
// the control socket and the /metrics exporter (TestControlStatus_CarriesAuditStatus,
// TestProviderMetrics_ExportAuditGauges) are P2: the miner's control socket and
// metrics surface do not exist yet, and the snapshot they read is the one the
// tests here pin.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func resetProxyAuditStatus(t *testing.T) {
	t.Helper()
	currentProxyAuditStatus.Store(nil)
	t.Cleanup(func() { currentProxyAuditStatus.Store(nil) })
}

func TestProxyAuditStatus_ObserveModePublishesWouldParkCount(t *testing.T) {
	resetProxyAuditStatus(t)
	h := newGovHarness("a:1")
	h.act = false
	h.twoBadTicks("a:1")

	st := proxyAuditStatusSnapshot()
	if st == nil {
		t.Fatalf("a completed tick must publish a status")
	}
	if st.Acting || st.WouldPark != 1 || len(st.Parked) != 0 {
		t.Fatalf("observe mode: expected acting=false would_park=1 parked=0, got %+v", st)
	}
}

func TestProxyAuditStatus_ActModePublishesParkedProxies(t *testing.T) {
	resetProxyAuditStatus(t)
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")

	st := proxyAuditStatusSnapshot()
	if st == nil || !st.Acting || st.WouldPark != 0 || st.Parks24h != 1 {
		t.Fatalf("act mode: expected acting=true would_park=0 parks_24h=1, got %+v", st)
	}
	if len(st.Parked) != 1 || st.Parked[0].Addr != "a:1" || !st.Parked[0].Until.After(h.now) {
		t.Fatalf("expected a:1 parked with a future end time, got %+v", st.Parked)
	}
}

// A tick that bails out (paid set untrusted) must not claim a false "nothing
// parked": what proxy audit is holding stays listed. It must also say it is
// paused, or a stuck audit looks exactly like a quiet one.
func TestProxyAuditStatus_UntrustedTickKeepsParksAndSaysPaused(t *testing.T) {
	resetProxyAuditStatus(t)
	h := newGovHarness("a:1")
	h.act = true
	h.twoBadTicks("a:1")
	before := proxyAuditStatusSnapshot()
	if before == nil || len(before.Parked) != 1 {
		t.Fatalf("setup: expected one parked proxy, got %+v", before)
	}

	h.fileOK = false
	h.now = h.now.Add(5 * time.Minute)
	h.g.runOnce()

	after := proxyAuditStatusSnapshot()
	if after == nil || !after.Paused {
		t.Fatalf("an untrusted tick must publish a paused status, got %+v", after)
	}
	if len(after.Parked) != 1 || after.Parked[0].Addr != "a:1" {
		t.Fatalf("a paused audit is still holding its parks, got %+v", after.Parked)
	}
}

// Parks are keyed by proxy identity. The published status is operator-facing
// JSON: it must carry the ADDRESS (what `proxy audit release` takes) and an
// obfuscated user to tell accounts at one gateway apart, never the raw key with
// its \x1f separator.
func TestProxyAuditStatus_ParkedIdentityKeyPublishedAsAddressAndUser(t *testing.T) {
	resetProxyAuditStatus(t)
	key := identityKey("gw.example:1080", "alice-01")
	h := newGovHarness(key)
	h.act = true
	h.twoBadTicks(key)

	st := proxyAuditStatusSnapshot()
	if st == nil || len(st.Parked) != 1 {
		t.Fatalf("expected one parked proxy, got %+v", st)
	}
	p := st.Parked[0]
	if p.Addr != "gw.example:1080" || p.User != "al***01" {
		t.Fatalf("parked = %+v, want addr gw.example:1080 and obfuscated user al***01", p)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `\u001f`) || strings.Contains(string(b), "alice-01") {
		t.Fatalf("status JSON leaks the raw identity key or the full user: %s", b)
	}
}
