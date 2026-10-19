package miner

// Ported from the 3.23-fix v33 provider's proxy_scout.go (read-only source
// candidate/v33-ops).
//
// Recovery scouts. While the reconnect attempt budget is throttled only a few
// proxies may try to connect each second, so which ones matters. They are ranked
// by what the provider knows and the connect package does not: how much each
// proxy has earned, and where it came from.
//
// Port adaptation: the v33 connect package exports the scout hooks this file
// installs into (SetProxyScoutScorer, SetProxySource, and the registry's
// ProxyKeyByIndex). The pinned full-bars/connect in this tree exports none of
// them, so the miner keeps the provider side - the provenance registry and the
// ranking rule - and leaves the two connect-facing directions as seams:
// scoutKeyByIndex resolves the connect registry index to a proxy key, and
// installConnectScoutScorer installs the callback. P1(c2)/P2 wires them (one
// line each) when the dependency exposes the API; until then the ranking is
// exercised through the seams by tests. Nothing here fabricates scores.

import (
	"sync"
	"time"
)

// scoutSources maps a proxy identity key to its provenance ("file", "internal"
// or "url"). It is fed by tagProxySourceIfUnset, which both the startup launch
// and hot reload already call for every proxy.
var scoutSources sync.Map

func registerScoutSource(key, source string) {
	if source == "" {
		return
	}
	scoutSources.Store(key, source)
	// v33 also labels the connect package's transport churn counters with the
	// same source (connect.SetProxySource). The pinned connect does not export
	// that hook, so the label stays local until P2 wires it.
}

func scoutSourceOf(key string) string {
	v, _ := scoutSources.Load(key)
	s, _ := v.(string)
	return s
}

func resetScoutSourcesForTest() {
	scoutSources.Range(func(k, _ any) bool {
		scoutSources.Delete(k)
		return true
	})
}

// scoutFileBonus is the head start a paid, file-listed or operator-configured
// proxy has over one scraped from a URL, in earned-bytes terms. URL lists are
// far likelier to be full of dead proxies. It equals the promotion threshold on
// purpose: a URL proxy that has actually moved that much billable traffic
// ranks level with a cold file proxy, and past it ranks ahead, the same rule
// that orders proxies at launch.
const scoutFileBonus = uint64(earningsPromotionBytes)

// scoutKeyByIndex resolves a connect proxy registry index to its identity key
// (v33: connect.ProxyKeyByIndex). The pinned full-bars/connect does not export
// the registry, so the seam returns "" (unknown) until P2 wires the real
// mapping; a test may install its own.
var scoutKeyByIndex = func(index int) string { return "" }

// installConnectScoutScorer hands the ranking to the connect package (v33:
// connect.SetProxyScoutScorer). The pinned connect does not export the hook;
// P2 replaces this seam with the real install when it does.
var installConnectScoutScorer = func(fn func(index int) uint64) {}

// scoutScoreForKey ranks a proxy identity key as a recovery scout: its decayed
// earnings, plus the head start for file-sourced proxies. Unknown proxies score
// zero.
func scoutScoreForKey(key string) uint64 {
	if key == "" {
		return 0
	}
	score := uint64(max(0, proxyEarningsScoreFor(key, time.Now())))
	switch scoutSourceOf(key) {
	case "file", "internal":
		score += scoutFileBonus
	}
	return score
}

// scoutScoreForIndex ranks the proxy at a registry index as a recovery scout.
// It is the function shape the connect budget calls; the index resolves
// through scoutKeyByIndex.
func scoutScoreForIndex(index int) uint64 {
	return scoutScoreForKey(scoutKeyByIndex(index))
}

// installScoutScorer hands the ranking to the connect package.
func installScoutScorer() {
	installConnectScoutScorer(scoutScoreForIndex)
}
