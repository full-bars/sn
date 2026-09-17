package provider

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func launchOrder(schedules []ProxySchedule) []string {
	order := make([]string, len(schedules))
	for i, s := range schedules {
		order[i] = s.Settings.Address
	}
	return order
}

func TestEarningsBreaksTiesWithinTheSameGroup(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	restore := withGlobalEarningsStore(t)
	defer restore()

	// All three are cold file proxies, so provenance and warmth are equal
	// and only earnings can separate them.
	creditEarningsAt(globalProxyEarningsStore, "middle", 5_000_000, time.Now())
	creditEarningsAt(globalProxyEarningsStore, "richest", 900_000_000, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "poorest"},
		{Address: "middle"},
		{Address: "richest"},
	}
	sourceOf := map[string]string{"poorest": "file", "middle": "file", "richest": "file"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	want := []string{"richest", "middle", "poorest"}
	got := launchOrder(schedules)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("launch order = %v, want %v", got, want)
		}
	}
}

func TestProvenEarnerFromURLIsPromotedAheadOfColdFileProxies(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	restore := withGlobalEarningsStore(t)
	defer restore()

	// A URL-sourced proxy that has actually moved real billable traffic is
	// no longer an unproven address, so it stops waiting behind every
	// file-sourced proxy.
	creditEarningsAt(globalProxyEarningsStore, "url-earner", earningsPromotionBytes*2, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "file-plain"},
		{Address: "url-earner"},
	}
	sourceOf := map[string]string{"file-plain": "file", "url-earner": "url"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrder(schedules); got[0] != "url-earner" {
		t.Fatalf("launch order = %v, want proven URL earner first", got)
	}
}

func TestUnprovenURLProxyStaysBehindFileProxies(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	restore := withGlobalEarningsStore(t)
	defer restore()

	// Just under the bar. Promotion must require real earnings, not noise,
	// or every URL address eventually drifts past the file list.
	creditEarningsAt(globalProxyEarningsStore, "url-almost", earningsPromotionBytes-1, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "url-almost"},
		{Address: "file-plain"},
	}
	sourceOf := map[string]string{"file-plain": "file", "url-almost": "url"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrder(schedules); got[0] != "file-plain" {
		t.Fatalf("launch order = %v, want file proxy first", got)
	}
}

func TestWarmthStillOutranksEarnings(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	home := t.TempDir()
	restoreStore := withGlobalStore(t, filepath.Join(home, ".client_jwts.json"))
	defer restoreStore()
	restoreEarn := withGlobalEarningsStore(t)
	defer restoreEarn()

	validJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"network_id": "net-main",
	})
	_ = loadGlobalClientJWTStore().Put("warm-broke", clientJWTEntry{
		ByClientJWT: validJWT, ClientID: testClientId, NetworkID: "net-main",
	})

	// A cold identity has to mint against the rate-limited auth API. Being
	// a proven earner must not buy it a scarce mint slot ahead of a warm
	// identity that can dial straight through.
	creditEarningsAt(globalProxyEarningsStore, "cold-rich", 5_000_000_000, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "cold-rich"},
		{Address: "warm-broke"},
	}
	sourceOf := map[string]string{"cold-rich": "file", "warm-broke": "file"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrder(schedules); got[0] != "warm-broke" {
		t.Fatalf("launch order = %v, want warm proxy first", got)
	}
}

func TestEarningsRankSummaryReportsPromotionsAndTopEarner(t *testing.T) {
	restore := withGlobalEarningsStore(t)
	defer restore()

	now := time.Now()
	creditEarningsAt(globalProxyEarningsStore, "url-promoted", earningsPromotionBytes*3, now)
	creditEarningsAt(globalProxyEarningsStore, "url-quiet", 10, now)
	creditEarningsAt(globalProxyEarningsStore, "file-top", earningsPromotionBytes*10, now)

	proxies := []*connect.ProxySettings{
		{Address: "url-promoted"},
		{Address: "url-quiet"},
		{Address: "file-top"},
		{Address: "file-cold"},
	}
	sourceOf := map[string]string{
		"url-promoted": "url", "url-quiet": "url",
		"file-top": "file", "file-cold": "file",
	}

	ranked, promoted, topAddr, topScore := earningsHistorySummary(proxies, sourceOf, now)

	if promoted != 1 {
		t.Errorf("promoted = %d, want 1", promoted)
	}
	// Three of the four have a non-zero score.
	if ranked != 3 {
		t.Errorf("ranked = %d, want 3", ranked)
	}
	if topAddr != "file-top" {
		t.Errorf("topAddr = %q, want file-top", topAddr)
	}
	if topScore != earningsPromotionBytes*10 {
		t.Errorf("topScore = %v, want %v", topScore, float64(earningsPromotionBytes*10))
	}
}

func TestEarningsRankSummaryOnAFreshNode(t *testing.T) {
	restore := withGlobalEarningsStore(t)
	defer restore()

	proxies := []*connect.ProxySettings{{Address: "a"}, {Address: "b"}}
	sourceOf := map[string]string{"a": "url", "b": "file"}

	ranked, promoted, topAddr, _ := earningsHistorySummary(proxies, sourceOf, time.Now())
	if promoted != 0 || ranked != 0 || topAddr != "" {
		t.Fatalf("fresh node summary = (%d, %d, %q), want (0, 0, \"\")", promoted, ranked, topAddr)
	}
}

// TestWarmURLBeatsColdPromotedURL verifies that warmth is the primary
// sort rule: a warm URL proxy must launch before a cold promoted URL
// proxy, even though the cold proxy has higher earnings. A cold
// promoted proxy would consume a scarce auth mint slot while the warm
// identity could have dialled straight through.
func TestWarmURLBeatsColdPromotedURL(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	home := t.TempDir()
	restoreStore := withGlobalStore(t, filepath.Join(home, ".client_jwts.json"))
	defer restoreStore()
	restoreEarn := withGlobalEarningsStore(t)
	defer restoreEarn()

	validJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"network_id": "net-main",
	})
	_ = loadGlobalClientJWTStore().Put("url-warm", clientJWTEntry{
		ByClientJWT: validJWT, ClientID: testClientId, NetworkID: "net-main",
	})

	// The cold promoted URL proxy has huge earnings but no warm JWT.
	creditEarningsAt(globalProxyEarningsStore, "url-cold-promoted", earningsPromotionBytes*10, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "url-cold-promoted"},
		{Address: "url-warm"},
	}
	sourceOf := map[string]string{
		"url-cold-promoted": "url",
		"url-warm":          "url",
	}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrder(schedules); got[0] != "url-warm" {
		t.Fatalf("launch order = %v, want warm URL proxy first (warmth must outrank earnings)", got)
	}
}

// TestWarmURLBeatsColdFileProxy verifies that a warm URL proxy launches
// ahead of a cold file proxy even though file proxies have provenance
// advantage. Warmth is the primary sort rule.
func TestWarmURLBeatsColdFileProxy(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	home := t.TempDir()
	restoreStore := withGlobalStore(t, filepath.Join(home, ".client_jwts.json"))
	defer restoreStore()

	validJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"network_id": "net-main",
	})
	_ = loadGlobalClientJWTStore().Put("url-warm", clientJWTEntry{
		ByClientJWT: validJWT, ClientID: testClientId, NetworkID: "net-main",
	})

	proxies := []*connect.ProxySettings{
		{Address: "file-cold"},
		{Address: "url-warm"},
	}
	sourceOf := map[string]string{
		"file-cold": "file",
		"url-warm":  "url",
	}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrder(schedules); got[0] != "url-warm" {
		t.Fatalf("launch order = %v, want warm URL proxy first (warmth outranks provenance)", got)
	}
}

// TestEqualNonZeroEarningsPreservesOrder verifies that sort.SliceStable
// preserves input order when two proxies have identical non-zero
// earnings within the same warmth tier and provenance group.
func TestEqualNonZeroEarningsPreservesOrder(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	restore := withGlobalEarningsStore(t)
	defer restore()

	now := time.Now()
	creditEarningsAt(globalProxyEarningsStore, "alpha", 5_000_000, now)
	creditEarningsAt(globalProxyEarningsStore, "bravo", 5_000_000, now)
	creditEarningsAt(globalProxyEarningsStore, "charlie", 5_000_000, now)

	proxies := []*connect.ProxySettings{
		{Address: "alpha"},
		{Address: "bravo"},
		{Address: "charlie"},
	}
	sourceOf := map[string]string{
		"alpha": "file", "bravo": "file", "charlie": "file",
	}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	got := launchOrder(schedules)
	want := []string{"alpha", "bravo", "charlie"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("launch order = %v, want %v (stable sort should preserve input order on tie)", got, want)
		}
	}
}

// TestPromotedColdURLProxyUsesColdFileStagger verifies that a promoted
// cold URL proxy receives ColdFileStagger (150 ms) instead of
// ColdURLStagger (500 ms) in the stagger calculation, since it is
// sorted among cold file proxies after promotion.
func TestPromotedColdURLProxyUsesColdFileStagger(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	restore := withGlobalEarningsStore(t)
	defer restore()

	creditEarningsAt(globalProxyEarningsStore, "url-promoted", earningsPromotionBytes*2, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "url-promoted"},
		{Address: "file-plain"},
	}
	sourceOf := map[string]string{"url-promoted": "url", "file-plain": "file"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	for _, sched := range schedules {
		if sched.Settings.Address == "url-promoted" {
			if sched.Stagger != ColdFileStagger {
				t.Fatalf("promoted cold URL proxy stagger = %v, want ColdFileStagger (%v)", sched.Stagger, ColdFileStagger)
			}
		}
	}
}

// TestExplorationQuotaInterleavesUnprovenProxy verifies the exploration
// quota: for every 5 trusted cold proxies, one unproven proxy is
// interleaved to prevent permanent starvation.
func TestExplorationQuotaInterleavesUnprovenProxy(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	restore := withGlobalEarningsStore(t)
	defer restore()

	now := time.Now()
	// 7 cold file proxies (trusted) and 3 cold URL proxies (unproven).
	trustedAddrs := make([]string, 7)
	for i := range trustedAddrs {
		trustedAddrs[i] = fmt.Sprintf("file-%d", i)
	}
	unprovenAddrs := make([]string, 3)
	for i := range unprovenAddrs {
		unprovenAddrs[i] = fmt.Sprintf("url-%d", i)
	}

	proxies := make([]*connect.ProxySettings, 0, 10)
	for _, a := range trustedAddrs {
		proxies = append(proxies, &connect.ProxySettings{Address: a})
		creditEarningsAt(globalProxyEarningsStore, a, 100_000_000, now)
	}
	for _, a := range unprovenAddrs {
		proxies = append(proxies, &connect.ProxySettings{Address: a})
	}

	sourceOf := map[string]string{}
	for _, a := range trustedAddrs {
		sourceOf[a] = "file"
	}
	for _, a := range unprovenAddrs {
		sourceOf[a] = "url"
	}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	order := launchOrder(schedules)
	// The first unproven proxy should appear at position 5 (after 5 trusted).
	// Find the index of the first unproven proxy.
	firstUnprovenIdx := -1
	for i, addr := range order {
		if addr == unprovenAddrs[0] {
			firstUnprovenIdx = i
			break
		}
	}
	if firstUnprovenIdx < 0 {
		t.Fatalf("unproven proxy not found in launch order: %v", order)
	}
	// Warm + renewable are empty, so cold starts at index 0.
	// With 7 trusted and 3 unproven, the interleave puts one unproven after 5 trusted.
	if firstUnprovenIdx != 5 {
		t.Fatalf("first unproven proxy at index %d, want 5 (after 5 trusted cold proxies)", firstUnprovenIdx)
	}
}
