package miner

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/clientauth"
)

// proxy_manager_warmth.go ports the 3.23-fix v33 provider's proxy_warmth.go:
// the authentication warmth of a proxy identity, the per-tier launch stagger,
// and the launch ordering that spends the auth API's scarce mint slots on the
// identities that can dial without one.
//
// Warmth answers "can this identity sign in without minting a fresh client
// JWT against the rate-limited auth API": a stored, unexpired client JWT is
// valid and dials straight through, an expired one whose client_id still
// parses can be renewed in one step, and everything else has to mint.
//
// Where v33 reads its client JWT store (client_jwt_store.go, keyed by
// identity), the miner's stored credential is the per-proxy file the auth
// path already writes and reuses (providerClientJwtPath). The evaluation below
// reads exactly that file, so it asks the same question of the same bytes the
// sign-in will use: a file whose token is still valid is a warm identity.
// One difference follows from the storage shape: v33's store keeps a
// client_id alongside the token, so an expired token whose payload lacks the
// client_id claim could still be renewed and counted renewable. The miner has
// no sidecar, so renewability is decided from the token payload alone — the
// client_id claim must parse — and a payload without it is cold. That is
// strictly more conservative (a cold identity only costs a staggered delay,
// never a wrong reuse), and it is why the tier is an ordering heuristic and
// not an authorization decision: the sign-in path still decides for itself.

// ProxyWarmthTier represents the authentication warmth state of a proxy.
type ProxyWarmthTier int

const (
	// WarmthCold indicates no stored client JWT or invalid/mismatched identity.
	// Requires fresh minting against the rate-limited auth API.
	WarmthCold ProxyWarmthTier = iota

	// WarmthRenewable indicates a stored client JWT that has expired but retains
	// a valid client_id eligible for 1-step renewal without minting fresh.
	WarmthRenewable

	// WarmthValid indicates a fully valid, unexpired client JWT ready for immediate use.
	WarmthValid
)

func (t ProxyWarmthTier) String() string {
	switch t {
	case WarmthValid:
		return "valid"
	case WarmthRenewable:
		return "renewable"
	default:
		return "cold"
	}
}

const (
	// WarmValidStagger is the accelerated launch gap for proxies with valid, unexpired client JWTs.
	// Because these proxies bypass the auth API (/mint) and dial WebRTC directly, they ramp up
	// at 25ms intervals rather than 150ms (e.g. 100 hot proxies launch within ~2.5s).
	WarmValidStagger = 25 * time.Millisecond

	// WarmRenewableStagger is the launch gap for proxies whose stored client JWT expired but retain
	// their valid client_id for 1-step renewal.
	WarmRenewableStagger = 50 * time.Millisecond

	// ColdFileStagger is the standard launch gap for fresh file-sourced proxies.
	ColdFileStagger = 150 * time.Millisecond

	// ColdURLStagger is the conservative launch gap for fresh URL-sourced proxies.
	ColdURLStagger = 500 * time.Millisecond
)

// anyStoredProviderNetworkID returns the first network_id found in a stored
// per-proxy client JWT, or "" when none carries one. This is the last-resort
// half of currentProviderNetworkID (client_jwt_hotrestart.go): the in-memory
// client JWT store is consulted first, and when it holds no unambiguous id
// the state directory's credential files are read in sorted order so the
// answer is deterministic.
func anyStoredProviderNetworkID() string {
	dir, err := providerStateDir()
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".provider-") && strings.HasSuffix(name, ".jwt") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if nid, ok := jwtNetworkId(strings.TrimSpace(string(b))); ok && nid != "" {
			return nid
		}
	}
	return ""
}

// proxyLaunchStagger returns the per-proxy stagger duration based on warmth and source.
func proxyLaunchStagger(tier ProxyWarmthTier, isURLSourced bool) time.Duration {
	switch tier {
	case WarmthValid:
		return WarmValidStagger
	case WarmthRenewable:
		return WarmRenewableStagger
	default:
		if isURLSourced {
			return ColdURLStagger
		}
		return ColdFileStagger
	}
}

// evaluateProxyWarmth checks whether a proxy identity has a warm client JWT
// stored on disk (see providerClientJwtPath). identities is the run's slot
// resolver: it makes the computed credential path match the one the provide
// path uses for this identity, so an address shared by two accounts reads its
// own file, never the sibling's.
func evaluateProxyWarmth(settings *connect.ProxySettings, currentNetworkID string, identities providerSlotIdentities) ProxyWarmthTier {
	if !hotRestartEnabled() {
		return WarmthCold
	}
	if settings == nil {
		return WarmthCold
	}
	path, err := providerClientJwtPath(identities, settings)
	if err != nil {
		return WarmthCold
	}
	token, err := clientauth.ReadToken(path)
	if err != nil || token == "" {
		return WarmthCold
	}
	if currentNetworkID == "" {
		currentNetworkID = currentProviderNetworkID()
	}
	entryNetworkID, _ := jwtNetworkId(token)
	// If currentNetworkID is missing, try to fill it in from the stored token.
	if currentNetworkID == "" && entryNetworkID != "" {
		currentNetworkID = entryNetworkID
	}
	// If neither side has a network_id, provideAuth cannot verify identity reuse and must mint fresh.
	if currentNetworkID == "" && entryNetworkID == "" {
		return WarmthCold
	}
	// Reject known network mismatches.
	if entryNetworkID != "" && currentNetworkID != "" && entryNetworkID != currentNetworkID {
		return WarmthCold
	}
	if err := validateJWTExpiry(token); err == nil && jwtContainsClientId(token) {
		return WarmthValid
	}
	// Salvage priority: if client_id is a valid identity, it can be renewed to salvage
	// reputation even if the expired token payload lacked the client_id claim.
	if _, err := connect.ParseId(jwtClientId(token)); err == nil {
		return WarmthRenewable
	}
	return WarmthCold
}

// ProxySchedule holds the launch configuration and cumulative delay for a proxy.
type ProxySchedule struct {
	Settings *connect.ProxySettings
	Tier     ProxyWarmthTier
	Delay    time.Duration
	Stagger  time.Duration
}

// prioritizeAndScheduleProxies sorts proxies by warmth, then provenance, then earnings,
// then computes cumulative launch delays.
// Note: the proxies slice is reordered in place.
func prioritizeAndScheduleProxies(
	proxies []*connect.ProxySettings,
	proxySourceOf map[string]string,
	currentNetworkID string,
) ([]ProxySchedule, int, int, int) {
	if currentNetworkID == "" {
		currentNetworkID = currentProviderNetworkID()
	}
	// The slot resolver over the same proxy list the run will start: warmth
	// must read the credential file this identity actually owns, identical to
	// the path the provide path computes (see providerClientJwtPath).
	identities := newProviderSlotIdentities(proxies)
	warmthMap := make(map[string]ProxyWarmthTier, len(proxies))
	// Earnings are read once per proxy rather than inside the comparator:
	// the store is mutex-guarded and a sort does O(n log n) comparisons, so
	// reading per comparison would take the lock tens of thousands of times
	// on a node carrying thousands of proxies.
	earningsMap := make(map[string]float64, len(proxies))
	now := time.Now()
	var warmCount, renewableCount, coldCount int

	for _, s := range proxies {
		// warmthMap/earningsMap are keyed by identity (ProxySettings.Key()),
		// not bare address, so two accounts sharing a gateway address get
		// independent tier/earnings records instead of colliding in these
		// maps. The stored client JWT is per identity too (see
		// providerClientJwtPath), so each account has its own hot-restart
		// credential. proxyEarningsScore is looked up by the real key: the
		// earnings store is identity-keyed too (see
		// proxy_manager_earnings_store.go).
		key := s.Key()
		tier := evaluateProxyWarmth(s, currentNetworkID, identities)
		warmthMap[key] = tier
		earningsMap[key] = proxyEarningsScore(key, now)
		switch tier {
		case WarmthValid:
			warmCount++
		case WarmthRenewable:
			renewableCount++
		default:
			coldCount++
		}
	}

	// trusted reports whether an address launches with the file list rather
	// than behind it. File and internal proxies are trusted by provenance.
	// A URL-sourced address is trusted once it has actually moved real
	// billable traffic: at that point it is no longer an unproven address
	// off a public list, it is a known earner, and making it wait behind
	// every file proxy costs real throughput during the warmup window.
	trusted := func(key string) bool {
		return proxySourceOf[key] != "url" || earningsMap[key] >= earningsPromotionBytes
	}

	sort.SliceStable(proxies, func(i, j int) bool {
		addrI := proxies[i].Key()
		addrJ := proxies[j].Key()

		// 1. Primary rule: higher warmth tier dials first. Warmth is the
		//    primary rule because a cold identity must mint against the
		//    rate-limited auth API. Promoting a rich cold proxy ahead of a
		//    warm proxy would spend a scarce mint slot and stall an identity
		//    that could have dialled straight through.
		tierI := warmthMap[addrI]
		tierJ := warmthMap[addrJ]
		if tierI != tierJ {
			return tierI > tierJ
		}

		// 2. Secondary rule: trusted provenance dials first. File-sourced
		//    and internal proxies always qualify; a URL-sourced proven
		//    earner is promoted into the same group.
		trustI := trusted(addrI)
		trustJ := trusted(addrJ)
		if trustI != trustJ {
			return trustI
		}

		// 3. Tertiary rule: within one tier and group, the bigger earner
		//    dials first. This is where the ranking does most of its work,
		//    since the warm tier is where launches are cheap.
		earnI := earningsMap[addrI]
		earnJ := earningsMap[addrJ]
		if earnI != earnJ {
			return earnI > earnJ
		}

		// 4. Stable fallback preserves original order
		return false
	})

	// Exploration quota: interleave 1 unproven proxy for every 5 top-earners
	// among cold proxies to prevent permanent starvation. Without this, a
	// node that restarts frequently starves unproven proxies behind the
	// cumulative cold-proxy ramp delay and they never accumulate traffic.
	//
	// "unproven" means untrusted provenance (URL-sourced, below promotion
	// threshold). The interleave picks from the tail of the sorted list
	// (cold unproven proxies) and inserts one after every 5 trusted cold
	// proxies, preserving the relative order within each group.
	trustedCold := make([]*connect.ProxySettings, 0, len(proxies))
	unprovenCold := make([]*connect.ProxySettings, 0, len(proxies))
	for _, s := range proxies {
		if warmthMap[s.Key()] == WarmthCold {
			if trusted(s.Key()) {
				trustedCold = append(trustedCold, s)
			} else {
				unprovenCold = append(unprovenCold, s)
			}
		}
	}
	if len(unprovenCold) > 0 && len(trustedCold) > 0 {
		reordered := make([]*connect.ProxySettings, 0, len(proxies))
		// Append warm + renewable proxies first (they always go first).
		for _, s := range proxies {
			if warmthMap[s.Key()] != WarmthCold {
				reordered = append(reordered, s)
			}
		}
		// Interleave trusted cold with unproven cold.
		unprovenIdx := 0
		for i, s := range trustedCold {
			reordered = append(reordered, s)
			if (i+1)%5 == 0 && unprovenIdx < len(unprovenCold) {
				reordered = append(reordered, unprovenCold[unprovenIdx])
				unprovenIdx++
			}
		}
		for ; unprovenIdx < len(unprovenCold); unprovenIdx++ {
			reordered = append(reordered, unprovenCold[unprovenIdx])
		}
		copy(proxies, reordered)
	}

	schedules := make([]ProxySchedule, len(proxies))
	var cumulativeDelay time.Duration

	for i, s := range proxies {
		key := s.Key()
		tier := warmthMap[key]
		isURL := proxySourceOf[key] == "url"
		promoted := isURL && earningsMap[key] >= earningsPromotionBytes
		// Promoted URL proxies belong with the cold file group; they have
		// earned trusted provenance and should not be penalised with the
		// conservative ColdURLStagger.
		stagger := proxyLaunchStagger(tier, isURL && !promoted)

		schedules[i] = ProxySchedule{
			Settings: s,
			Tier:     tier,
			Delay:    cumulativeDelay,
			Stagger:  stagger,
		}
		cumulativeDelay += stagger
	}

	return schedules, warmCount, renewableCount, coldCount
}

// proxyLaunchSlot is one entry of the provide run's launch list: the settings
// to provide with and the warmth-derived pacing to start them at. It is what
// the miner's launch loop consumes (see providerRunSettings.run), and it keeps
// the direct slot (nil settings) in the list without asking the scheduler
// about it: the direct identity is not a proxy, has no stored client JWT, and
// v33 starts its direct connection separately from this ordering.
type proxyLaunchSlot struct {
	// Index is the slot's position in the original provider list, so the
	// diagnostics still label a provider by the slot it was configured as
	// even when the launch order reorders the start. v33 uses its stable
	// proxy ID for this; the miner's launch list is positional.
	Index    int
	Settings *connect.ProxySettings
	Delay    time.Duration
	Stagger  time.Duration
}

// scheduleProxyLaunch orders the launch list for the provide run: v33's
// prioritizeAndScheduleProxies over the proxy entries, then the direct slot(s)
// unchanged. It also emits the two startup lines v33 logs beside the
// prioritization — the tier counts and the earnings-history report the
// earnings store exists to fill in.
func scheduleProxyLaunch(providers []*connect.ProxySettings, proxySourceOf map[string]string, currentNetworkID string) []proxyLaunchSlot {
	// indexed carries each proxy's original slot position through the
	// reorder, so the launch loop keeps labelling a provider by the slot it
	// was configured as (see proxyLaunchSlot.Index).
	type indexedProxy struct {
		index int
		proxy *connect.ProxySettings
	}
	var proxies []indexedProxy
	var direct []int
	for index, p := range providers {
		if p == nil {
			direct = append(direct, index)
			continue
		}
		proxies = append(proxies, indexedProxy{index: index, proxy: p})
	}

	settings := make([]*connect.ProxySettings, len(proxies))
	for i, p := range proxies {
		settings[i] = p.proxy
	}
	schedules, warmCount, renewableCount, coldCount := prioritizeAndScheduleProxies(settings, proxySourceOf, currentNetworkID)
	if 0 < len(proxies) {
		tlog("[proxy][startup] launch prioritization: %d total (warm: %d, renewable: %d, cold: %d)\n",
			len(proxies), warmCount, renewableCount, coldCount)
		if ranked, promoted, topAddr, topScore := earningsHistorySummary(settings, proxySourceOf, time.Now()); ranked == 0 {
			tlog("[proxy][earn] earnings ranking: no history yet, launching by warmth and source only\n")
		} else {
			tlog("[proxy][earn] earnings ranking: %d of %d proxies have earnings history, %d URL-sourced promoted to launch with the file list, top earner %s at %s\n",
				ranked, len(proxies), promoted, topAddr, formatBytes(uint64(topScore)))
		}
	}

	// The schedule's settings are the same pointers as the proxy entries;
	// recover each one's original index by pointer identity.
	indexOf := make(map[*connect.ProxySettings]int, len(proxies))
	for _, p := range proxies {
		indexOf[p.proxy] = p.index
	}

	out := make([]proxyLaunchSlot, 0, len(providers))
	for _, sched := range schedules {
		out = append(out, proxyLaunchSlot{
			Index:    indexOf[sched.Settings],
			Settings: sched.Settings,
			Delay:    sched.Delay,
			Stagger:  sched.Stagger,
		})
	}
	for _, index := range direct {
		out = append(out, proxyLaunchSlot{Index: index})
	}
	return out
}

// backoffPacerWithDelay waits baseDelay (±50% of slotDuration as jitter)
// before the caller starts, or returns false as soon as proxyCtx is done. The
// jitter spreads dials inside one stagger slot so a fleet does not hit the
// same second in lockstep.
func backoffPacerWithDelay(baseDelay time.Duration, slotDuration time.Duration, proxyCtx context.Context) bool {
	if baseDelay <= 0 && slotDuration <= 0 {
		return true
	}

	var jitter time.Duration
	if slotDuration > 0 {
		halfSlotMs := int(slotDuration.Milliseconds()) / 2
		if halfSlotMs > 0 {
			jMs := rand.Intn(halfSlotMs + 1)
			if rand.Intn(2) == 0 {
				jMs = -jMs
			}
			jitter = time.Duration(jMs) * time.Millisecond
		}
	}

	wait := baseDelay + jitter
	if wait < 0 {
		wait = 0
	}

	select {
	case <-proxyCtx.Done():
		return false
	case <-time.After(wait):
	}
	return true
}

// backoffPacer calculates the effective start delay for the n-th proxy goroutine.
// staggerMs is the base gap between consecutive proxy starts; ±50% random
// jitter spreads dials within each slot. File proxies typically pass 0 for
// immediate launch (the backend's own rate limiter handles bursts), while
// URL-sourced proxies use a non-zero stagger to avoid thundering-herd.
func backoffPacer(n int, staggerMs int, now time.Time, proxyCtx context.Context) bool {
	if staggerMs <= 0 {
		return true
	}
	baseDelay := time.Duration(n) * time.Duration(staggerMs) * time.Millisecond
	return backoffPacerWithDelay(baseDelay, time.Duration(staggerMs)*time.Millisecond, proxyCtx)
}

// validateJWTExpiry, jwtContainsClientId, jwtClientId and jwtNetworkId are the
// shared local-JWT helpers; the merged tree keeps their single canonical copy
// in client_jwt_hotrestart.go (this file was rebased onto it so the two
// branches could not drift into two divergent implementations).
