package miner

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// h3_proxy_set.go ports the 3.23-fix v33 provider's H3 identity set
// (provider/h3_proxy_set.go).
//
// Which proxied identities get the fallback transports (see h3Mode):
//
//	off, direct, on   none
//	<N>               the N best proxies: graded before ungraded, then by grade
//	                  score, then by decayed earnings, then by key for a stable
//	                  order. The direct identity never consumes one of the N.
//	all               every proxy
//
// A set of N is sticky. Once chosen it keeps its members while they remain
// candidates, and only vacancies are filled from the ranking, so grades drifting
// a little never reconnect a working identity. A change of setting, or of the
// candidate set, relaunches only the identities whose eligibility changed, and
// does so a few at a time (reconnect storms are the thing to avoid on a node
// that runs hundreds of identities).
//
// Port adaptation: the pinned full-bars/connect does not export the fork's
// per-proxy bandwidth registry (connect.ProxyBandwidthByKey), so "does this
// proxy have clients" is asked of the running provide run through the
// h3IdentityRelauncher seam instead of the library.

// h3RelaunchInterval is the pause between relaunches when a change of setting
// moves many proxies at once.
const h3RelaunchInterval = 300 * time.Millisecond

type h3Ranking struct {
	cap  int
	keys map[string]bool
}

var (
	h3RankingValue atomic.Pointer[h3Ranking]

	h3CandidatesMutex sync.Mutex
	h3Candidates      []*connect.ProxySettings

	// h3LaunchEligible is, per running proxy identity, whether it was launched
	// with the fallback transports.
	h3LaunchMutex    sync.Mutex
	h3LaunchEligible = map[string]bool{}
)

// h3ProxyEligible reports whether a proxied identity gets the fallback transports
// under a setting. Candidates not yet ranked are not eligible.
func h3ProxyEligible(setting h3Setting, key string) bool {
	switch setting.Mode {
	case h3ModeAll:
		return true
	case h3ModeTop:
		ranking := h3RankingValue.Load()
		return ranking != nil && ranking.cap == setting.Cap && ranking.keys[key]
	}
	return false
}

// rankH3Proxies picks n proxies, keeping previous members that are still
// candidates and filling vacancies from the ranking.
func rankH3Proxies(n int, previous map[string]bool, candidates []*connect.ProxySettings, state *ProxyState) map[string]bool {
	out := map[string]bool{}
	if n <= 0 {
		return out
	}
	type candidate struct {
		key      string
		graded   bool
		score    float64
		earnings float64
		kept     bool
	}
	now := time.Now()
	ranked := make([]candidate, 0, len(candidates))
	for _, settings := range candidates {
		key := settings.Key()
		c := candidate{key: key, earnings: proxyEarningsScoreFor(key, now), kept: previous[key]}
		if state != nil {
			if entry, ok := state.Proxies[key]; ok {
				c.score = entry.Score
				c.graded = entry.Graded
			}
		}
		ranked = append(ranked, c)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.graded != b.graded {
			return a.graded
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.earnings != b.earnings {
			return a.earnings > b.earnings
		}
		return a.key < b.key
	})
	// members that stay, best first, then vacancies
	for _, c := range ranked {
		if c.kept && len(out) < n {
			out[c.key] = true
		}
	}
	for _, c := range ranked {
		if len(out) >= n {
			break
		}
		out[c.key] = true
	}
	return out
}

func sameKeySet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

// noteH3Candidates records the proxies the provider runs and, under a count
// setting, re-ranks them. It reports whether the ranked set changed.
func noteH3Candidates(candidates []*connect.ProxySettings, setting h3Setting) (changed bool) {
	h3CandidatesMutex.Lock()
	h3Candidates = candidates
	h3CandidatesMutex.Unlock()
	return rerankH3(setting)
}

// rerankH3 recomputes the ranked set for a count setting from the last noted
// candidates.
func rerankH3(setting h3Setting) (changed bool) {
	if setting.Mode != h3ModeTop {
		// the ranking only exists under a count; leaving it drops the members
		return h3RankingValue.Swap(nil) != nil
	}
	h3CandidatesMutex.Lock()
	candidates := h3Candidates
	h3CandidatesMutex.Unlock()
	state, _ := readProxyState()
	previous := h3RankingValue.Load()
	var previousKeys map[string]bool
	if previous != nil && previous.cap == setting.Cap {
		previousKeys = previous.keys
	}
	keys := rankH3Proxies(setting.Cap, previousKeys, candidates, state)
	if previous != nil && previous.cap == setting.Cap && sameKeySet(previous.keys, keys) {
		return false
	}
	h3RankingValue.Store(&h3Ranking{cap: setting.Cap, keys: keys})
	return true
}

func recordH3Launch(key string, eligible bool) {
	h3LaunchMutex.Lock()
	h3LaunchEligible[key] = eligible
	h3LaunchMutex.Unlock()
}

func forgetH3Launch(key string) {
	h3LaunchMutex.Lock()
	delete(h3LaunchEligible, key)
	h3LaunchMutex.Unlock()
}

// h3ProxiesToRelaunch is the running proxies whose launch eligibility differs
// from what the setting says now, sorted for a stable order.
func h3ProxiesToRelaunch(setting h3Setting) []string {
	h3LaunchMutex.Lock()
	defer h3LaunchMutex.Unlock()
	var keys []string
	for key, launched := range h3LaunchEligible {
		if launched != h3ProxyEligible(setting, key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// h3RelaunchRetryInterval is how long a pass waits before retrying the proxies
// it left alone because they had clients.
const h3RelaunchRetryInterval = time.Minute

// h3RelaunchMaxPasses bounds the retries (an hour), so a proxy that always has
// a client is not chased forever; it picks the new setting up whenever it next
// restarts for any other reason.
const h3RelaunchMaxPasses = 60

// relaunchH3Proxies restarts, a few at a time, the proxies whose eligibility
// changed so they pick up the current setting. A proxy with clients is left
// alone and retried later: widening the transport set must never cut a live
// session. A newer setting ends this pass, because its own pass takes over.
func relaunchH3Proxies(ctx context.Context, relauncher h3IdentityRelauncher, setting h3Setting) {
	for pass := 0; pass < h3RelaunchMaxPasses; pass++ {
		if resolveH3Mode(globalControlState, currentProxyResolution()) != setting {
			return
		}
		keys := h3ProxiesToRelaunch(setting)
		if len(keys) == 0 {
			return
		}
		if pass == 0 {
			tlog("[h3] %s: relaunching %d proxies whose transport set changed\n", setting, len(keys))
		}
		relaunched, busy := 0, 0
		for _, key := range keys {
			if relauncher.hasClients(key) {
				busy++
				continue
			}
			if relauncher.detachForRelaunch(key) {
				relaunched++
			}
			forgetH3Launch(key)
			select {
			case <-ctx.Done():
				return
			case <-time.After(h3RelaunchInterval):
			}
		}
		if busy == 0 {
			return
		}
		tlog("[h3] %s: %d proxies have clients, retrying in %s\n", setting, busy, h3RelaunchRetryInterval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(h3RelaunchRetryInterval):
		}
	}
}

// relaunchH3ProxiesNow runs a relaunch pass over the running provide run, in the
// background. Nil relauncher (offline tools, unit tests) does nothing.
func relaunchH3ProxiesNow(setting h3Setting) {
	relauncher := currentH3IdentityRelauncher()
	if relauncher == nil {
		return
	}
	go func() {
		defer func() {
			if cause := recover(); cause != nil {
				importantLogf("[h3] relaunch pass panicked: %v\n", cause)
			}
		}()
		relaunchH3Proxies(relauncher.runContext(), relauncher, setting)
	}()
}
