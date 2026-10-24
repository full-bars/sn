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
// The relaunch is only ever started from an APPLY path (applyH3CandidateSet: the
// provide run's manager start and its applied reloads; a control-key change;
// a UDP-check verdict). The desired-set read (proxyManager.desired) must stay
// side-effect free: the reload watcher calls it on every reload, and a relaunch
// issued from a read detaches running slots outside the reloader's bookkeeping,
// which then lists them as unchanged and never re-spawns them.
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
// under a setting. Candidates not yet ranked are not eligible. A proxy whose UDP
// path cannot carry QUIC is not eligible either (see proxy_udp_check.go): its
// identity stays on H1 rather than failing every QUIC attempt on a relay that
// does not work or does not exit where the proxy's TCP side does. A proxy that
// has not been checked yet is eligible, the same as the relay treats an
// unverifiable one. While the platform is unreachable no proxy is eligible (see
// h3_outage_shed.go).
func h3ProxyEligible(setting h3Setting, key string) bool {
	if h3OutageShedding.Load() {
		return false
	}
	if !h3ProxyModeEligible(setting, key) {
		return false
	}
	return proxyUDPAdmitsH3(key)
}

// h3ProxyModeEligible is the mode half of h3ProxyEligible: what the setting
// admits, before the proxy's own UDP path is considered.
func h3ProxyModeEligible(setting h3Setting, key string) bool {
	switch setting.Mode {
	case h3ModeAll:
		return true
	case h3ModeTop:
		ranking := h3RankingValue.Load()
		return ranking != nil && ranking.cap == setting.Cap && ranking.keys[key]
	}
	return false
}

// h3ProxyLaunchedEligible reports whether a running identity was launched with
// the fallback transports.
func h3ProxyLaunchedEligible(key string) bool {
	h3LaunchMutex.Lock()
	defer h3LaunchMutex.Unlock()
	return h3LaunchEligible[key]
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
//
// It is the read half of applyH3CandidateSet: nothing here starts or stops an
// identity. The proxies the setting admits have their UDP path checked in the
// background (see proxy_udp_check.go), so the H3 gate has a verdict by the time
// an identity asks for eligibility. The check is bounded and remembered, so
// noting an unchanged pool does not probe again.
func noteH3Candidates(candidates []*connect.ProxySettings, setting h3Setting) (changed bool) {
	h3CandidatesMutex.Lock()
	h3Candidates = candidates
	h3CandidatesMutex.Unlock()
	refreshProxyUDPChecks(context.Background(), candidates, setting)
	return rerankH3(setting)
}

// applyH3CandidateSet is the APPLY path for a new candidate set: it notes the
// proxies the run serves and, when the resulting ranking moved, relaunches the
// identities that joined or left it. The provide run calls it after the manager
// start and after every reload it applied (run.go), so the candidate set and the
// ranking track what the box actually runs.
//
// It must not be called from a desired-set read (proxyManager.desired): the read
// runs on every reload, and a relaunch started there detaches slots the reloader
// still believes are running, so its next diff reports them unchanged and they
// are never spawned again.
func applyH3CandidateSet(candidates []*connect.ProxySettings, setting h3Setting) (relaunched bool) {
	if !noteH3Candidates(candidates, setting) {
		return false
	}
	relaunchH3ProxiesNow(setting)
	return true
}

// noteH3Launch records, for one identity that is starting, whether it is being
// built with the fallback transports under `setting`, and returns that
// eligibility so the caller builds the carrier policy from the same decision.
// The provide run calls it as each proxy slot builds its transport; the direct
// identity carries no row here (it is always eligible, mode off excepted).
//
// The setting is passed in rather than read here so the launch row and the
// provider carrier policy the device is built with cannot disagree: a change
// landing between two reads would leave a relaunch rebuilding an identity that
// was already built with the new mode, or one that never will be.
func noteH3Launch(settings *connect.ProxySettings, setting h3Setting) bool {
	if settings == nil {
		return false
	}
	key := settings.Key()
	eligible := h3ProxyEligible(setting, key)
	recordH3Launch(key, eligible)
	return eligible
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
	relaunchH3ProxiesPaced(ctx, relauncher, setting, h3RelaunchInterval)
}

// relaunchH3ProxiesPaced is relaunchH3Proxies with the pause between relaunches
// chosen by the caller, for a relaunch that must not arrive as a burst.
func relaunchH3ProxiesPaced(ctx context.Context, relauncher h3IdentityRelauncher, setting h3Setting, interval time.Duration) {
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
			// Forget the old launch BEFORE detaching: the relauncher starts the
			// replacement as part of the detach, and that launch records its
			// own row. Forgetting afterwards would erase the fresh row and the
			// identity would never be relaunched again.
			forgetH3Launch(key)
			if relauncher.detachForRelaunch(key) {
				relaunched++
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
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
	relaunchH3ProxiesNowPaced(setting, h3RelaunchInterval)
}

// relaunchH3ProxiesNowPaced is relaunchH3ProxiesNow at a chosen pace.
func relaunchH3ProxiesNowPaced(setting h3Setting, interval time.Duration) {
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
		relaunchH3ProxiesPaced(relauncher.runContext(), relauncher, setting, interval)
	}()
}
