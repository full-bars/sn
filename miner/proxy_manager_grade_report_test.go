package miner

import (
	"testing"
	"time"
)

// Ported from the 3.23-fix v33 provider's proxy_grade_report_test.go,
// proxy_url_lastgraded_test.go and proxy_grade_identity_lookup_test.go
// (read-only source candidate/v33-ops). proxyGradeFor is the pure seam for the
// A-F grade surfacing (and the trim resolver): given both grade stores, it
// returns the grade payload for a proxy key. Paid/file (proxy.state) wins over
// URL (proxy_url.json) when both are graded; ok=false when neither store has a
// graded entry.

func TestProxyGradeFor_PaidWinsWhenBothGraded(t *testing.T) {
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.95, Graded: true, Failed: []string{"a.com"}},
	}}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {Score: 0.82, Graded: true},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", paid, url)
	if !ok {
		t.Fatal("expected ok=true when both stores graded")
	}
	if info.Score != 0.95 {
		t.Errorf("Score = %f, want paid store's 0.95", info.Score)
	}
	if info.Tier != "A" {
		t.Errorf("Tier = %q, want A for 0.95", info.Tier)
	}
	if len(info.Failed) != 1 || info.Failed[0] != "a.com" {
		t.Errorf("Failed = %v, want paid store's [a.com]", info.Failed)
	}
}

func TestProxyGradeFor_URLUsedWhenPaidNotGraded(t *testing.T) {
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.0, Graded: false}, // exists but never graded
	}}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {Score: 0.71, Graded: true},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", paid, url)
	if !ok {
		t.Fatal("expected ok=true when URL store graded")
	}
	if info.Score != 0.71 {
		t.Errorf("Score = %f, want URL store's 0.71", info.Score)
	}
	if info.Tier != "C" {
		t.Errorf("Tier = %q, want C for 0.71", info.Tier)
	}
}

func TestProxyGradeFor_NeitherGraded(t *testing.T) {
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.0, Graded: false},
	}}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"9.9.9.9:1080": {Score: 0.9, Graded: true}, // different address
	}}

	if _, ok := proxyGradeFor("1.2.3.4:1080", paid, url); ok {
		t.Fatal("expected ok=false when neither store has a graded entry")
	}
}

func TestProxyGradeFor_PaidLastGradedUnix(t *testing.T) {
	gradedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.99, Graded: true, LastGraded: gradedAt},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", paid, &ProxyURLState{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != gradedAt.Unix() {
		t.Errorf("LastGraded = %d, want %d", info.LastGraded, gradedAt.Unix())
	}
}

func TestProxyGradeFor_URLNoLastGradedReportsZero(t *testing.T) {
	// A graded URL entry with a LastProbe but no LastGraded (e.g. a
	// pre-migration proxy_url.json, or liveness-only bumps) must report
	// last_graded = 0 - the honest "grade never re-recorded" answer, not
	// a liveness timestamp masquerading as a grade time.
	lastProbe := time.Date(2026, 8, 10, 13, 30, 0, 0, time.UTC)
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {Score: 0.61, Graded: true, LastProbe: lastProbe},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", &ProxyState{}, url)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != 0 {
		t.Errorf("LastGraded = %d, want 0 (no grade timestamp recorded)", info.LastGraded)
	}
	if info.Tier != "D" {
		t.Errorf("Tier = %q, want D for 0.61", info.Tier)
	}
}

func TestProxyGradeFor_TierMapping(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{1.00, "A"},
		{0.90, "A"},
		{0.89, "B"},
		{0.80, "B"},
		{0.79, "C"},
		{0.70, "C"},
		{0.69, "D"},
		{0.60, "D"},
		{0.59, "F"},
		{0.00, "F"},
	}
	for _, c := range cases {
		paid := &ProxyState{Proxies: map[string]ProxyEntry{
			"1.2.3.4:1080": {Score: c.score, Graded: true},
		}}
		info, ok := proxyGradeFor("1.2.3.4:1080", paid, &ProxyURLState{})
		if !ok {
			t.Fatalf("score %.2f: expected ok=true", c.score)
		}
		if info.Tier != c.want {
			t.Errorf("score %.2f: Tier = %q, want %q", c.score, info.Tier, c.want)
		}
	}
}

func TestProxyGradeFor_ZeroLastGraded(t *testing.T) {
	// A graded entry with a zero LastGraded/LastProbe must report 0, not a
	// negative epoch or a panic.
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.9, Graded: true}, // LastGraded zero value
	}}
	info, ok := proxyGradeFor("1.2.3.4:1080", paid, &ProxyURLState{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != 0 {
		t.Errorf("LastGraded = %d, want 0 for zero-value timestamp", info.LastGraded)
	}
}

func TestProxyGradeForURLLastGradedFromFieldNotLastProbe(t *testing.T) {
	gradedAt := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	// LastProbe is FRESH (recent liveness check), LastGraded is OLD (weeks
	// ago) - exactly the misleading case the MEDIUM describes.
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {
			Score: 0.61, Graded: true,
			LastProbe:  time.Now(),
			LastGraded: gradedAt,
		},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", &ProxyState{}, url)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != gradedAt.Unix() {
		t.Errorf("LastGraded = %d, want %d (must use the grade timestamp, not LastProbe)",
			info.LastGraded, gradedAt.Unix())
	}
}

func TestProxyGradeForURLZeroLastGraded(t *testing.T) {
	// A graded URL entry with no LastGraded recorded (e.g. a pre-migration
	// proxy_url.json) must report 0, not fall back to a liveness timestamp.
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {
			Score: 0.71, Graded: true,
			LastProbe: time.Now(), // fresh liveness, no grade record
		},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", &ProxyState{}, url)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != 0 {
		t.Errorf("LastGraded = %d, want 0 when no grade timestamp is recorded", info.LastGraded)
	}
}

// TestProxyGradeFor_IdentityKeyResolvesBothStores: the paid store is keyed by
// identity, so a credentialed proxy's grade is found by its identity key, and
// the address-keyed URL cache by the key's address.
func TestProxyGradeFor_IdentityKeyResolvesBothStores(t *testing.T) {
	key := identityKey("10.0.0.1:1080", "u")
	paid := &ProxyState{Proxies: map[string]ProxyEntry{key: {Graded: true, Score: 0.9}}}
	if g, ok := proxyGradeFor(key, paid, nil); !ok || g.Score != 0.9 {
		t.Fatalf("paid grade by identity key: ok=%v %+v", ok, g)
	}

	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{"10.0.0.2:1080": {Graded: true, Score: 0.7}}}
	urlKey := identityKey("10.0.0.2:1080", "u")
	if g, ok := proxyGradeFor(urlKey, &ProxyState{}, url); !ok || g.Score != 0.7 {
		t.Fatalf("URL cache is address-keyed and must resolve via the key's address: ok=%v %+v", ok, g)
	}
}

// TestBuildTrimGradeResolver_CredentialedURLProxy: the trim resolver is handed
// identity keys and must grade credentialed URL proxies too, otherwise they
// read as ungraded and shed before graded ones.
func TestBuildTrimGradeResolver_CredentialedURLProxy(t *testing.T) {
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{"10.0.0.2:1080": {Graded: true, Score: 0.95}}}
	resolve := buildTrimGradeResolver(&ProxyState{}, url)
	if score, ok := resolve(identityKey("10.0.0.2:1080", "u")); !ok || score != 0.95 {
		t.Fatalf("credentialed URL proxy must resolve its grade: ok=%v score=%v", ok, score)
	}
}
