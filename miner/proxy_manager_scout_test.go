package miner

// Ported from the 3.23-fix v33 provider's proxy_earnings_priority_test.go's
// scout cases and provider/proxy_scout.go (read-only source candidate/v33-ops).
//
// The v33 test installs the scorer through connect's registry
// (connect.RegisterProxy + ProxyKeyByIndex); the pinned full-bars/connect does
// not export either, so the port pins the same ranking rules through the two
// seams (scoutKeyByIndex, proxyEarningsScoreFor) the connect wiring will use.

import (
	"testing"
	"time"
)

func withScoutSeams(t *testing.T) {
	t.Helper()
	resetScoutSourcesForTest()
	prevKey := scoutKeyByIndex
	prevScore := proxyEarningsScoreFor
	t.Cleanup(func() {
		scoutKeyByIndex = prevKey
		proxyEarningsScoreFor = prevScore
		resetScoutSourcesForTest()
	})
}

// TestScoutScore_FileProxiesLeadUnlessAURLProxyHasProvenItself pins the v33
// rule: while the reconnect budget is throttled, recovery scouts are ranked by
// decayed earnings plus a head start for file/internal provenance equal to the
// promotion threshold. A URL proxy that has actually earned real traffic
// outranks a cold file proxy, exactly as at launch.
func TestScoutScore_FileProxiesLeadUnlessAURLProxyHasProvenItself(t *testing.T) {
	withScoutSeams(t)

	keys := map[int]string{1: "file-cold", 2: "url-cold", 3: "url-earner", 4: "file-earner", 5: "internal-cold"}
	scoutKeyByIndex = func(index int) string { return keys[index] }
	earnings := map[string]float64{
		"url-earner":  float64(earningsPromotionBytes * 2),
		"file-earner": float64(earningsPromotionBytes * 2),
	}
	proxyEarningsScoreFor = func(addr string, _ time.Time) float64 { return earnings[addr] }

	register := func(index int, key, source string) {
		_ = index
		registerScoutSource(key, source)
	}
	register(1, "file-cold", "file")
	register(2, "url-cold", "url")
	register(3, "url-earner", "url")
	register(4, "file-earner", "file")
	register(5, "internal-cold", "internal")

	fileCold, urlCold := scoutScoreForIndex(1), scoutScoreForIndex(2)
	urlEarner, fileEarner := scoutScoreForIndex(3), scoutScoreForIndex(4)
	internalCold := scoutScoreForIndex(5)

	if !(fileCold > urlCold) {
		t.Fatalf("a cold file proxy must outrank a cold URL proxy: file=%d url=%d", fileCold, urlCold)
	}
	if !(urlEarner > fileCold) {
		t.Fatalf("a URL proxy that has earned real traffic outranks a cold file proxy: url=%d file=%d", urlEarner, fileCold)
	}
	if !(fileEarner > urlEarner) {
		t.Fatalf("at equal earnings the file proxy leads: file=%d url=%d", fileEarner, urlEarner)
	}
	if internalCold != fileCold {
		t.Fatalf("an operator-configured proxy ranks with file proxies: internal=%d file=%d", internalCold, fileCold)
	}
	if scoutScoreForIndex(404) != 0 {
		t.Fatal("an unknown proxy scores zero")
	}
}

// A negative or unknown earnings score never lowers a scout below the file
// bonus, and a URL proxy with no earnings is exactly the bonus size behind a
// cold file proxy.
func TestScoutScore_NoEarningsScoresByProvenanceOnly(t *testing.T) {
	withScoutSeams(t)
	scoutKeyByIndex = func(index int) string {
		switch index {
		case 1:
			return "url-quiet"
		case 2:
			return "file-quiet"
		}
		return ""
	}
	registerScoutSource("url-quiet", "url")
	registerScoutSource("file-quiet", "file")

	if got := scoutScoreForIndex(1); got != 0 {
		t.Fatalf("a URL proxy with no earnings scores zero, got %d", got)
	}
	if got := scoutScoreForIndex(2); got != uint64(earningsPromotionBytes) {
		t.Fatalf("a cold file proxy scores exactly the file bonus, got %d", got)
	}
}

func TestScoutScoreForKey_UnknownAndEmptyKeysScoreZero(t *testing.T) {
	withScoutSeams(t)
	if got := scoutScoreForKey(""); got != 0 {
		t.Fatalf("empty key must score zero, got %d", got)
	}
	if got := scoutScoreForKey("nobody:1"); got != 0 {
		t.Fatalf("an unregistered url-less key must score zero, got %d", got)
	}
}

// A registered source is what gives a key the file head start; an unregistered
// key with the same earnings must not get it.
func TestScoutScoreForKey_FileBonusAppliesOnlyToRegisteredProvenance(t *testing.T) {
	withScoutSeams(t)
	proxyEarningsScoreFor = func(string, time.Time) float64 { return 7 }
	registerScoutSource("file-key:1", "file")
	registerScoutSource("url-key:1", "url")

	if got, want := scoutScoreForKey("file-key:1"), uint64(7)+uint64(earningsPromotionBytes); got != want {
		t.Fatalf("file key = %d, want %d", got, want)
	}
	if got := scoutScoreForKey("url-key:1"); got != 7 {
		t.Fatalf("url key = %d, want 7 (no bonus)", got)
	}
}

func TestTagProxySourceRegistersTheSourceForScouting(t *testing.T) {
	defer resetScoutSourcesForTest()
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}

	tagProxySourceIfUnset(state, "a:1", "url")
	if got := scoutSourceOf("a:1"); got != "url" {
		t.Fatalf("source = %q, want url", got)
	}
	// the first tag wins, and the registry must agree with the persisted state
	tagProxySourceIfUnset(state, "a:1", "file")
	if got := scoutSourceOf("a:1"); got != "url" {
		t.Fatalf("source changed to %q after a second tag, the first tag wins", got)
	}
}

// installScoutScorer must hand the provider's key ranking to the connect
// install seam, and the installed callback must score through the same
// provenance/earnings seams v33's does.
func TestInstallScoutScorer_HandsTheRankingToConnect(t *testing.T) {
	prevInstall := installConnectScoutScorer
	t.Cleanup(func() { installConnectScoutScorer = prevInstall })
	prevKey := scoutKeyByIndex
	t.Cleanup(func() { scoutKeyByIndex = prevKey })
	prevScore := proxyEarningsScoreFor
	t.Cleanup(func() { proxyEarningsScoreFor = prevScore })
	defer resetScoutSourcesForTest()

	var installed func(int) uint64
	installConnectScoutScorer = func(fn func(int) uint64) { installed = fn }

	installScoutScorer()
	if installed == nil {
		t.Fatal("installScoutScorer must hand a scorer to the connect install seam")
	}

	scoutKeyByIndex = func(index int) string {
		if index == 1 {
			return "scout-check:1"
		}
		return ""
	}
	proxyEarningsScoreFor = func(addr string, _ time.Time) float64 {
		if addr == "scout-check:1" {
			return 5
		}
		return 0
	}
	registerScoutSource("scout-check:1", "file")
	if got := installed(1); got != 5+uint64(earningsPromotionBytes) {
		t.Fatalf("installed scorer = %d, want the earnings plus the file bonus", got)
	}
	if got := installed(2); got != 0 {
		t.Fatalf("unknown index must score zero, got %d", got)
	}
}
