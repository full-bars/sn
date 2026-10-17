package miner

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// This file ports the 3.23-fix v33 provider's URL-source pipeline
// (proxy_url_source.go): the runtime overrides, dead-proxy removal and
// eviction, desired-set queries, the fetch-and-merge cycle, the dead-proxy
// reaper, the blacklist pruner, the fetcher loop and the scoped cleanup pass.
//
// P1(a) adaptations, all deliberate:
//   - resolveProxyURLMax/Refresh/Cleanup*/SelfHeal read a small in-process
//     control-state map (proxyManagerControlState) instead of v33's control
//     socket; P2 replaces that map's backing with the real socket state.
//   - the stage-1 table probe is P1(c): resolveProxyTableProbeConfig reports
//     the kill switch off, so the fetcher admits on the stage-0 SOCKS5+API
//     check exactly as v33 does with grading disabled.
//   - the miner's provide loop reads the desired set at startup; the reload
//     WATCHER that would apply a live trigger is P2, so the fetcher/reaper
//     persist state and write the trigger for the next start (documented in
//     the report).
//   - liveProxyHealthByKey and degradedReaperFrozen are calm no-op seams (see
//     proxy_manager_hooks.go) because the pinned connect does not export the
//     health registry v33 uses; P2 wires them.

// proxyManagerControlState is the P1(a) stand-in for v33's globalControlState
// (control_state.go). The control socket is P2; this map lets the override
// resolvers (and the ported tests) exercise the same precedence without it.
type proxyManagerControlState struct {
	mu     sync.Mutex
	values map[string]string
}

var globalProxyControlState = &proxyManagerControlState{values: map[string]string{}}

// get returns the stored value for key.
func (s *proxyManagerControlState) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	return v, ok
}

// set stores value for key.
func (s *proxyManagerControlState) set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
}

// reset clears all stored values. Tests only.
func (s *proxyManagerControlState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = map[string]string{}
}

// resetGlobalControlStateForTest clears the control-state map. Tests only.
func resetGlobalControlStateForTest() { globalProxyControlState.reset() }

// proxyURLMaxOverridePath returns <state>/proxy_url_max, a file an
// operator can write to cap the number of URL-sourced proxies at runtime.
func proxyURLMaxOverridePath() (string, error) {
	return providerStatePath("proxy_url_max")
}

// resolveProxyURLMax checks the control-socket state first, then the legacy
// override file, then startupMax (the value from --proxy_url_max at process
// start). A missing, empty, or unparseable value at any layer falls through
// to the next.
func resolveProxyURLMax(startupMax int) int {
	if v, ok := globalProxyControlState.get("proxy_url_max"); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}

	path, err := proxyURLMaxOverridePath()
	if err != nil {
		return startupMax
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupMax
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupMax
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return startupMax
	}
	return n
}

// resolveEffectiveProxyURLMax is the admit cap fetch cycles actually use:
// the configured ceiling, further limited by the AIMD-discovered target
// once the pool controller has established one. The target only constrains
// admission while self-heal is enabled; toggling self-heal off restores the
// configured ceiling immediately.
func resolveEffectiveProxyURLMax(startupMax int, selfHealEnabled bool) int {
	ceiling := resolveProxyURLMax(startupMax)
	if !resolveSelfHealEnabled(selfHealEnabled) {
		return ceiling
	}
	state, err := readProxyURLState()
	if err != nil || state.TargetPoolSize <= 0 {
		return ceiling
	}
	if ceiling == 0 || state.TargetPoolSize < ceiling {
		return state.TargetPoolSize
	}
	return ceiling
}

// proxyURLRefreshOverridePath returns <state>/proxy_url_refresh.
func proxyURLRefreshOverridePath() (string, error) {
	return providerStatePath("proxy_url_refresh")
}

// resolveProxyURLRefresh checks the control-socket state first, then the
// legacy override file, then startupInterval.
func resolveProxyURLRefresh(startupInterval time.Duration) time.Duration {
	if v, ok := globalProxyControlState.get("proxy_url_refresh"); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 10*time.Second {
			return d
		}
	}

	path, err := proxyURLRefreshOverridePath()
	if err != nil {
		return startupInterval
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupInterval
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 10*time.Second {
		return startupInterval
	}
	return d
}

// proxyCleanupScopeOverridePath returns <state>/proxy_dead_cleanup_scope.
func proxyCleanupScopeOverridePath() (string, error) {
	return providerStatePath("proxy_dead_cleanup_scope")
}

// resolveProxyCleanupScope checks the control-socket state first, then the
// legacy override file, then startupScope.
func resolveProxyCleanupScope(startupScope string) string {
	if v, ok := globalProxyControlState.get("proxy_dead_cleanup_scope"); ok && v != "" {
		return v
	}

	path, err := proxyCleanupScopeOverridePath()
	if err != nil {
		return startupScope
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupScope
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupScope
	}
	return v
}

// proxyCleanupIntervalOverridePath returns <state>/proxy_dead_cleanup_interval.
func proxyCleanupIntervalOverridePath() (string, error) {
	return providerStatePath("proxy_dead_cleanup_interval")
}

// resolveProxyCleanupInterval checks the control-socket state first, then
// the legacy override file, then startupInterval.
func resolveProxyCleanupInterval(startupInterval time.Duration) time.Duration {
	if v, ok := globalProxyControlState.get("proxy_dead_cleanup_interval"); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Minute {
			return d
		}
	}

	path, err := proxyCleanupIntervalOverridePath()
	if err != nil {
		return startupInterval
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupInterval
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute {
		return startupInterval
	}
	return d
}

// proxySelfHealOverridePath returns <state>/proxy_self_heal, a marker
// file for the `urnet-tools self-heal on|off` toggle. Absent means the
// startup default (off unless URNETWORK_SELF_HEAL=1); "on" enables;
// anything else disables.
func proxySelfHealOverridePath() (string, error) {
	return providerStatePath("proxy_self_heal")
}

// resolveSelfHealEnabled checks the control-socket state first, then the
// legacy override file, then startupEnabled (the value from
// URNETWORK_SELF_HEAL at process start). At either the socket or the file
// layer, "on" enables and any other non-empty value disables; a missing or
// empty value falls through to the next layer, and startupEnabled itself
// defaults to FALSE - self-heal is opt-in.
func resolveSelfHealEnabled(startupEnabled bool) bool {
	if v, ok := globalProxyControlState.get("proxy_self_heal"); ok {
		return strings.EqualFold(v, "on")
	}

	path, err := proxySelfHealOverridePath()
	if err != nil {
		return startupEnabled
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return startupEnabled
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return startupEnabled
	}
	return strings.EqualFold(v, "on")
}

// defaultAPIHost is the default target for the API reachability probe.
const defaultAPIHost = "api.bringyour.com"
const defaultAPIPort = 443

// removeDeadProxies removes the given proxies from whichever source they
// came from - an external file source, the internal config, or the URL
// cache - and triggers a hot-reload. addrsBySource groups proxy.state
// identity KEYS (address, or address+user for a credentialed proxy; see
// ProxySettings.Key()) by their Source tag ("file", "internal", or "url");
// unrecognized tags are ignored. File and internal removal match on the full
// identity so one account at a shared gateway is removed without the others;
// the URL cache is keyed by bare address, so it is cleared by the key's
// address part. Used by both the interactive `proxy remove-dead` command and
// the automatic scoped cleanup job, so removal logic only lives in one place.
func removeDeadProxies(state *ProxyState, addrsBySource map[string][]string) error {
	release, err := acquireProxyLock()
	if err != nil {
		return fmt.Errorf("could not acquire proxy lock: %w", err)
	}
	defer release()

	if fileAddrs := addrsBySource["file"]; len(fileAddrs) > 0 {
		if state.Source == "" {
			tlog("[proxy] warning: %d proxies tagged source=file but no file source is configured; skipping\n", len(fileAddrs))
		} else if err := removeKeysFromFile(state.Source, fileAddrs); err != nil {
			return fmt.Errorf("could not update proxy file: %w", err)
		}
	}

	if internalAddrs := addrsBySource["internal"]; len(internalAddrs) > 0 {
		proxyConfig := readProxyConfig()
		removeSet := map[string]bool{}
		for _, a := range internalAddrs {
			removeSet[a] = true
		}
		for proxyAddress, authKey := range proxyConfig.Servers {
			if removeSet[internalServerSettings(proxyConfig, proxyAddress, authKey).Key()] {
				delete(proxyConfig.Servers, proxyAddress)
			}
		}
		writeProxyConfig(proxyConfig)
	}

	if urlAddrs := addrsBySource["url"]; len(urlAddrs) > 0 {
		urlState, err := readProxyURLState()
		if err != nil {
			return fmt.Errorf("could not read proxy_url.json: %w", err)
		}
		// The URL cache is keyed by bare address; the names are identity keys.
		for _, k := range urlAddrs {
			addr, _ := connect.SplitProxyKey(k)
			delete(urlState.Cache, addr)
		}
		if err := writeProxyURLState(urlState); err != nil {
			return fmt.Errorf("could not write proxy_url.json: %w", err)
		}
	}

	// Release the lock before writing the reload trigger so the running
	// provider's reload() can acquire it. The deferred release is a no-op
	// (sync.Once idempotent guard in acquireProxyLockAt).
	release()
	reloadPath, err := proxyReloadPath()
	if err != nil {
		return fmt.Errorf("could not determine reload path: %w", err)
	}
	return writeReloadTrigger(reloadPath)
}

// evictProxyURLAddress permanently removes address from the URL cache and
// records it in the persisted blacklist in the same write, so a future
// fetch (which is add-only by design) can never silently bring it back,
// even across process restarts. Triggers a hot-reload so the live fleet
// reflects the removal immediately.
func evictProxyURLAddress(address string) error {
	release, err := acquireProxyLockWithRetry()
	if err != nil {
		return fmt.Errorf("could not acquire proxy lock: %w", err)
	}
	defer release()

	state, err := readProxyURLState()
	if err != nil {
		return fmt.Errorf("could not read proxy_url.json: %w", err)
	}

	delete(state.Cache, address)
	if state.Blacklist == nil {
		state.Blacklist = map[string]time.Time{}
	}
	state.Blacklist[address] = time.Now().UTC()

	if err := writeProxyURLState(state); err != nil {
		return fmt.Errorf("could not write proxy_url.json: %w", err)
	}

	// Release the lock before the trigger (deferred release is idempotent).
	release()
	reloadPath, err := proxyReloadPath()
	if err != nil {
		return fmt.Errorf("could not determine reload path: %w", err)
	}
	return writeReloadTrigger(reloadPath)
}

// currentDesiredProxyAddresses returns every address currently desired by
// this provider: the primary source (external file or internal config) merged
// with the URL cache. Used wherever "is this address still part of the fleet"
// needs to be independent of live health-registration state.
func currentDesiredProxyAddresses() (map[string]bool, error) {
	state, err := readProxyState()
	if err != nil {
		return nil, fmt.Errorf("could not read proxy.state: %w", err)
	}

	var desired []*connect.ProxySettings
	if state.Source != "" {
		desired, err = readProxySettingsFromFile(state.Source)
		if err != nil {
			return nil, fmt.Errorf("could not read proxy file %s: %w", state.Source, err)
		}
	} else {
		desired = readProxySettingsLegacy()
	}

	addrs := make(map[string]bool, len(desired))
	for _, s := range desired {
		addrs[s.Address] = true
	}

	urlState, err := readProxyURLState()
	if err != nil {
		return nil, fmt.Errorf("could not read proxy_url.json: %w", err)
	}
	for addr := range urlState.Cache {
		addrs[addr] = true
	}

	return addrs, nil
}

// currentDesiredProxyIdentities is currentDesiredProxyAddresses' identity-
// keyed counterpart (ProxySettings.Key(): address, or address+user for a
// shared-gateway proxy). Used where a caller must distinguish between two
// accounts sharing one address. URL cache entries are included using their
// own address+credentials the same way mergeProxyURLCache builds them.
func currentDesiredProxyIdentities() (map[string]bool, error) {
	state, err := readProxyState()
	if err != nil {
		return nil, fmt.Errorf("could not read proxy.state: %w", err)
	}

	var desired []*connect.ProxySettings
	if state.Source != "" {
		desired, err = readProxySettingsFromFile(state.Source)
		if err != nil {
			return nil, fmt.Errorf("could not read proxy file %s: %w", state.Source, err)
		}
	} else {
		desired = readProxySettingsLegacy()
	}

	keys := make(map[string]bool, len(desired))
	for _, s := range desired {
		keys[s.Key()] = true
	}

	urlState, err := readProxyURLState()
	if err != nil {
		return nil, fmt.Errorf("could not read proxy_url.json: %w", err)
	}
	for addr, entry := range urlState.Cache {
		settings := &connect.ProxySettings{Network: "tcp", Address: addr}
		if entry.User != "" || entry.Password != "" {
			settings.Auth = &proxy.Auth{User: entry.User, Password: entry.Password}
		}
		keys[settings.Key()] = true
	}

	return keys, nil
}

// desiredAddressesForHistoryPruning extends currentDesiredProxyIdentities
// with in-backoff identities: a self-heal shed deletes the address from the
// URL cache (unlike a give-up, which leaves it in place), so without this a
// shed proxy's history would get pruned before its backoff even elapses.
// The keep-set MUST be identity keys (ProxySettings.Key()).
func desiredAddressesForHistoryPruning() (map[string]bool, error) {
	keys, err := currentDesiredProxyIdentities()
	if err != nil {
		return nil, err
	}
	for k := range globalProxyFailureHistory.AddressesInBackoff(time.Now()) {
		keys[k] = true
	}
	return keys, nil
}

var fetchMu sync.Mutex

// readURLStateForMerge reads the persisted URL state for a merge cycle, or
// reports false when the file cannot be read. Callers must SKIP the cycle on
// false rather than substitute an empty state: an unreadable file is not an
// empty one, and merging against a fabricated empty state then writing it back
// wipes the cache, every grade, and the permanent blacklist.
func readURLStateForMerge() (*ProxyURLState, bool) {
	state, err := readProxyURLState()
	if err != nil {
		// Unparseable content is not a transient read error: skipping would
		// repeat forever (every other reader returns early too, so nothing ever
		// rewrites the file) and the URL pipeline would stay off. Keep the bad
		// file as evidence and start from an empty cache.
		if isProxyURLStateCorrupt(err) {
			dest, qerr := quarantineProxyURLState()
			if qerr == nil {
				importantLogf("[proxy][url] proxy_url.json is corrupt (%v); kept as %s and starting from an empty cache", err, dest)
				// The corrupt file cannot supply its configuration and nothing
				// else records it, so start from the last configuration seen:
				// an empty cache, but the sources, the permanent blacklist and
				// the exclude patterns survive into the next write.
				return rememberedURLStateConfig(), true
			}
			tlog("[proxy][url] warning: proxy_url.json is corrupt (%v) and could not be moved aside (%v), skipping this merge cycle\n", err, qerr)
			return nil, false
		}
		tlog("[proxy][url] warning: could not read proxy_url.json, skipping this merge cycle to protect the cache: %v\n", err)
		return nil, false
	}
	return state, true
}

// fetchAndMergeProxyURLs fetches every configured source, merges newly
// discovered addresses into the persisted cache (add-only - existing entries
// are never removed here), and triggers a hot-reload if anything new was
// found. A fetch failure for one URL logs a warning and is skipped; it never
// clears already-cached entries from that source.
//
// Only one fetch cycle may run at a time - if an earlier cycle's probing
// phase outlasts the refresh interval, the next tick's call returns
// immediately rather than racing on the same file.
func fetchAndMergeProxyURLs(ctx context.Context, urls []string, maxTotal int, apiHost string, apiPort uint16) {
	if len(urls) == 0 {
		return
	}

	if !fetchMu.TryLock() {
		return
	}
	defer fetchMu.Unlock()

	// Fetching from the network can be slow; do it before taking the lock so
	// we don't hold it across HTTP requests. Only the read-modify-write of
	// proxy_url.json below needs to be serialized against removeDeadProxies.
	fetched := make([][]string, len(urls))
	// grades accumulates the stage-1 result per address so the merge loop
	// can persist score/failed alongside ProbeOK. With the P1(a) kill switch
	// off this map carries only the stage-0 admission decision.
	grades := make(map[string]proxyURLGrade)
	// tierCounts breaks down this cycle's grades for the per-source log.
	tierCounts := map[string]int{}
	// countedGrade dedupes the breakdown by ADDRESS across sources: the same
	// address listed in two sources (bare + credentialed, or duplicated)
	// must count once.
	countedGrade := map[string]bool{}
	probeCfg := resolveProxyTableProbeConfig()
	// The confirmation floor is a PAID-only concept. The admission gate and the
	// reaper must keep their exact dial profile, so zero it explicitly here
	// rather than relying on nobody setting the global key.
	probeCfg.MinConfirmDials = 0
	tlog("[proxy][url] stage-1 table probe config: %s\n", describeProxyTableProbeConfig(probeCfg))
	// Advance the rotation once per FETCH CYCLE (not once per source URL).
	tableProbePassCounter.Add(1)

	// Probe ONLY addresses not already cached. A cached proxy already has a
	// grade; re-probing it every fetch cycle is redundant because most don't
	// change, and at aggressive refresh (5-15m) it turns the probe itself
	// into a suspicious traffic pattern through the same egress IPs. Quality
	// refresh of cached entries is the reaper's job. The cache snapshot is
	// read once up front; the merge re-reads it under the lock later, so any
	// race just means a proxy gets probed once more.
	cached := cachedProxyAddresses(mustReadProxyURLState())
	// probed is LIVE across sources: an address listed in source A AND
	// source B must be table-probed ONCE per cycle, not once per source.
	probed := map[string]bool{}
	skippedCached := 0
	// What each source did this cycle, for the per-source lines and the headline.
	labels := urlSourceLabels(urls)
	perSource := make([]urlSourceStats, len(urls))
	for i := range perSource {
		perSource[i].Label = labels[i]
	}
	for i, url := range urls {
		lines, err := fetchProxyURLLines(ctx, url)
		if err != nil {
			perSource[i].Failed = true
			label := labels[i]
			tlog("[proxy][url] fetch failed for %s: %v (skipping this cycle)\n", label, err)
			setProxyResolutionStatus(proxyResolutionFailed, fmt.Sprintf("%s: %v", label, err))
			warnProxySourceFailure(label, err.Error())
			continue
		}
		// Free public proxy lists are mostly dead entries. The staged probe
		// checks TCP reachability, SOCKS5 protocol compliance and API CONNECT
		// (stage 0), and - for survivors - a sampled table probe of real
		// destinations (stage 1), before anything ever enters the cache or
		// consumes an auth-rate-limiter slot. In P1(a) the stage-1 half is not
		// ported, so admission is the documented kill-switch behavior: only
		// stage-0-qualified proxies are admitted.
		var probeLines []string
		skippedThisSource := 0
		for _, line := range lines {
			addr, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			perSource[i].Lines++
			if cached[addr] || probed[addr] {
				skippedCached++
				skippedThisSource++
				continue
			}
			probed[addr] = true
			probeLines = append(probeLines, line)
		}
		perSource[i].Known = skippedThisSource
		lineGrades := probeAndGradeProxyURLLines(ctx, probeLines, apiHost, apiPort, probeCfg)
		// Count each NEW address once: dropped as dead, or graded but below the
		// bar.
		for _, line := range probeLines {
			addr, _, _, _ := parseProxyURLLine(line)
			switch g, ok := lineGrades[addr]; {
			case !ok:
				perSource[i].Dead++
			case !g.Qualified:
				perSource[i].Rejected++
			}
		}
		var qualified, belowBar, socks5Only []string
		for _, line := range lines {
			addr, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			g, ok := lineGrades[addr]
			if !ok {
				continue // probeDead: dropped entirely
			}
			grades[addr] = g
			countThisAddr := !countedGrade[addr]
			countedGrade[addr] = true
			switch {
			case g.Qualified:
				qualified = append(qualified, line)
				if g.Decidable {
					if countThisAddr {
						tierCounts[proxyGradeTier(g.Score)]++
					}
				} else {
					// Kill-switch-disabled or otherwise ungraded but admitted.
					if countThisAddr {
						tierCounts["ungraded"]++
					}
				}
			case g.Socks5Only:
				socks5Only = append(socks5Only, line)
				if countThisAddr {
					tierCounts["socks5-only"]++
				}
			default:
				belowBar = append(belowBar, line)
				if g.Decidable {
					if countThisAddr {
						tierCounts[proxyGradeTier(g.Score)]++
					}
				} else {
					// No verdict this cycle (cancelled/DNS-down): the entry
					// keeps its prior grade and is simply not admitted now.
					if countThisAddr {
						tierCounts["undecidable"]++
					}
				}
			}
		}
		// Qualified lines are cached with ProbeOK=true; below-bar and
		// socks5-only lines with ProbeOK=false so the background reaper can
		// retry them (they may have had a transient routing issue).
		fetched[i] = append(append(qualified, belowBar...), socks5Only...)
		if len(qualified) == 0 && len(belowBar) == 0 && len(socks5Only) == 0 && len(probeLines) > 0 {
			// The fetch itself succeeded but every NEW line was unparseable
			// or dead - distinct from the fetch-failed case above.
			tlog("[proxy][url] %s: fetched %d lines, %d new, all unparseable or dead\n", labels[i], len(lines), len(probeLines))
			warnProxySourceFailure(labels[i], fmt.Sprintf("fetched %d lines, %d new, all unparseable or dead", len(lines), len(probeLines)))
		}
		tlog("[proxy][url] probed %s: %d/%d new qualified (%d cached, skipped), %d below-bar, %d socks5-only\n",
			labels[i], len(qualified), len(probeLines), skippedThisSource, len(belowBar), len(socks5Only))
	}
	if skippedCached > 0 {
		tlog("[proxy][url] skipped %d already-cached addresses this cycle (grade refresh is the reaper's job)\n", skippedCached)
	}

	release, err := acquireProxyLock()
	if err != nil {
		tlog("[proxy][url] warning: could not acquire proxy lock: %v\n", err)
		return
	}
	defer release()

	state, ok := readURLStateForMerge()
	if !ok {
		// Skipping the cycle is deliberate. Continuing with an empty state and
		// writing it back would destroy every cached entry not re-listed this
		// cycle, every persisted Score/Graded/LastProbe, and the entire
		// Blacklist map, which a fetch never repopulates (it is add-only by
		// design).
		return
	}

	totalAdded := 0
	dirty := false

	// Best-overall admission: pool every candidate across ALL sources, rank
	// by tier (A first), and merge in that order. The cache fills with the
	// highest-tier proxies first; lower tiers fill only remaining slots.
	rankAddr := func(addr string) int {
		if g, ok := grades[addr]; ok {
			return rankFromGrade(g)
		}
		return -1
	}
	gradeFor := func(addr string) (proxyURLGrade, bool) {
		g, ok := grades[addr]
		return g, ok
	}
	cands := collectRankedCandidates(fetched, grades)
	admittedByTier := map[string]int{}
	var admittedLines []string
	for _, c := range cands {
		admittedLines = append(admittedLines, c.line)
	}
	// apiOKCount is 0 because gradeFor decides ProbeOK for every candidate
	// (each admitted line has a grade): the pool is ordered by tier RANK,
	// not by Qualified, so the positional fallback (ProbeOK = i < apiOKCount)
	// would be wrong if a candidate ever reached the merge without a grade.
	existingBefore := cachedProxyAddresses(state)
	added := mergeProxyURLEntries(state, admittedLines, 0, maxTotal, rankAddr, gradeFor)
	totalAdded += added
	// admittedByTier counts what actually entered the cache this cycle, per
	// letter grade. The same address can appear in multiple sources' fetched
	// lists, so count each address ONCE.
	countedTier := map[string]bool{}
	for _, c := range cands {
		if countedTier[c.address] {
			continue
		}
		if _, exists := state.Cache[c.address]; exists && c.grade.Qualified {
			countedTier[c.address] = true
			if c.grade.Decidable {
				admittedByTier[proxyGradeTier(c.grade.Score)]++
			} else {
				// Kill-switch-disabled or otherwise ungraded but admitted:
				// count into the "ungraded" bucket so the important marker
				// still fires when stage-1 is off.
				admittedByTier["ungraded"]++
			}
		}
	}
	if len(admittedByTier) > 0 {
		parts := make([]string, 0, len(admittedByTier))
		for _, tier := range []string{"A", "B", "C", "D", "F", "ungraded"} {
			if n := admittedByTier[tier]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", tier, n))
			}
		}
		importantLogf("[proxy][url] admitted by tier: %s (cap %d, total cached %d)\n",
			strings.Join(parts, " "), maxTotal, len(state.Cache))
	}

	markedAPI := 0
	markedSocks5 := 0
	// Dedupe by address so a cross-source duplicate is persisted and counted
	// once - the counters reflect distinct cached addresses, not pooled lines.
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c.address] {
			continue
		}
		seen[c.address] = true
		addr := c.address
		if entry, exists := state.Cache[addr]; exists {
			entry.LastProbe = time.Now()
			// Persist the stage-1 grade alongside ProbeOK so the
			// auth-time gate and fleet grading can consume it.
			//
			// Graded is set ONLY for a genuine stage-1 verdict
			// (g.Decidable). A socks5-only line never reached stage 1,
			// and a pass that could not ask anything produced no evidence.
			if g, ok := grades[addr]; ok {
				applyProxyGradeToEntry(&entry, g, time.Now())
			}
			if c.grade.Qualified {
				entry.ProbeOK = true
				entry.ProbeFails = 0
				markedAPI++
			} else {
				entry.ProbeOK = false
				markedSocks5++
			}
			state.Cache[addr] = entry
			dirty = true
		}
	}
	if markedSocks5 > 0 || markedAPI > 0 {
		tlog("[proxy][url] %d qualified entries saved, %d below-bar/socks5-only entries marked for reaper\n", markedAPI, markedSocks5)
	}

	// Attribute each newly cached address to the first source that listed it:
	// qualified ones were added to the pool, the rest are held for the reaper.
	attributed := map[string]bool{}
	for i, lines := range fetched {
		for _, line := range lines {
			addr, _, _, ok := parseProxyURLLine(line)
			if !ok || existingBefore[addr] || attributed[addr] {
				continue
			}
			entry, in := state.Cache[addr]
			if !in {
				continue
			}
			attributed[addr] = true
			if entry.ProbeOK {
				perSource[i].Added++
			} else {
				perSource[i].Held++
			}
		}
	}

	// One line per source, then the cycle headline.
	cycle := urlCycleStats{Sources: len(urls), PoolCached: len(state.Cache)}
	for _, ps := range perSource {
		importantLogf("%s\n", ps)
		if ps.Failed {
			cycle.Failed++
			continue
		}
		cycle.Admitted += ps.Added
		cycle.Held += ps.Held
		cycle.AlreadyKnown += ps.Known
		cycle.Rejected += ps.Rejected + ps.Dead
	}
	for _, entry := range state.Cache {
		if entry.ProbeOK {
			cycle.PoolQualified++
		}
	}
	if cycle.Failed >= len(urls) {
		tlog("%s\n", cycle)
	} else {
		importantLogf("%s\n", cycle)
	}

	// Grade breakdown is printed every cycle that produced any grade, even
	// when nothing was newly added.
	if len(tierCounts) > 0 {
		parts := make([]string, 0, len(tierCounts))
		for _, tier := range []string{"A", "B", "C", "D", "F", "ungraded", "undecidable", "socks5-only"} {
			if n := tierCounts[tier]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", tier, n))
			}
		}
		importantLogf("[proxy][url] probe grade breakdown: %s\n", strings.Join(parts, " "))
	}

	if totalAdded == 0 && !dirty {
		return
	}
	if err := writeProxyURLState(state); err != nil {
		tlog("[proxy][url] warning: could not write proxy_url.json: %v\n", err)
		return
	}
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy][url] warn: reload trigger write failed: %v\n", err)
		}
	}
}

// capFailedList bounds the persisted Failed list so a wide sample_width
// cannot inflate the hot proxy_url.json file unboundedly.
const maxFailedStored = 8

func capFailedList(hosts []string) []string {
	if len(hosts) <= maxFailedStored {
		return hosts
	}
	out := make([]string, maxFailedStored)
	copy(out, hosts[:maxFailedStored])
	return out
}

// reaperProbeTarget holds a single candidate address and a snapshot of its
// probe state, collected under the lock then probed outside it.
type reaperProbeTarget struct {
	addr       string
	entry      ProxyURLEntry
	wasProbeOK bool
}

// runURLProxyReaper iterates the URL cache and re-probes entries whose
// ProbeOK is false (socks5-only from a previous fetch, or entries added
// before the probe fields existed). Entries that fail proxyAPIMaxFails
// consecutive probes are moved to the persistent Blacklist. Runs every
// proxyReaperInterval. Exits when ctx is cancelled.
func runURLProxyReaper(ctx context.Context, apiHost string, apiPort uint16) {
	ticker := time.NewTicker(proxyReaperInterval)
	defer ticker.Stop()

	for {
		// Publish the countdown BEFORE the pass so the first cycle is not
		// "unknown" and the estimate is not systematically late by the
		// pass duration.
		setNextGradeRefreshAt(time.Now().Add(proxyReaperInterval))
		select {
		case <-ctx.Done():
			// Clear the published refresh estimate on shutdown so the
			// summary countdown does not go stale.
			setNextGradeRefreshAt(time.Time{})
			return
		case <-ticker.C:
		}
		runURLProxyReaperOnce(ctx, apiHost, apiPort)
	}
}

// runURLProxyReaperOnce performs a single reaper pass: collect candidates,
// probe them, apply results. Split out from runURLProxyReaper so it can be
// exercised directly in tests without waiting on proxyReaperInterval.
//
// Probing happens outside the proxy lock so a large batch of dead entries
// doesn't block concurrent reloads, fetches, or removeDeadProxies calls.
// The lock is held only while collecting candidates and while applying the
// results atomically.
func runURLProxyReaperOnce(ctx context.Context, apiHost string, apiPort uint16) {
	// Collect candidates under the lock, then probe outside it.
	var candidates []reaperProbeTarget
	func() {
		release, err := acquireProxyLock()
		if err != nil {
			return
		}
		defer release()

		state, err := readProxyURLState()
		if err != nil {
			return
		}
		for addr, entry := range state.Cache {
			if !entry.ProbeOK {
				// Unproven or previously-failed proxy: re-probe if due.
				if !entry.LastProbe.IsZero() && time.Since(entry.LastProbe) < proxyReaperInterval {
					continue
				}
				candidates = append(candidates, reaperProbeTarget{addr: addr, entry: entry})
				continue
			}
			// Once-good proxy: re-probe only when stale, so dead-but-cached
			// entries don't accumulate invisibly until the give-up pipeline
			// evicts them. Without this, the reaper has no exit for proxies
			// that passed the initial probe then died later.
			staleAfter := reaperStaleThreshold(currentPressure())
			if !entry.LastProbe.IsZero() && time.Since(entry.LastProbe) < staleAfter {
				continue
			}
			candidates = append(candidates, reaperProbeTarget{
				addr: addr, entry: entry, wasProbeOK: true,
			})
		}
	}()

	if len(candidates) == 0 {
		return
	}

	// Order the probe loop so the 32/cycle grade-refresh budget lands on the
	// genuinely oldest grades, not a random map-order slice. Liveness-only
	// candidates (ProbeOK=false) do not spend refresh budget, so they go first
	// regardless of age. Among the stale once-good candidates, oldest
	// LastProbe first.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].wasProbeOK != candidates[j].wasProbeOK {
			return !candidates[i].wasProbeOK // liveness-only first, they don't spend budget
		}
		return candidates[i].entry.LastProbe.Before(candidates[j].entry.LastProbe)
	})

	// Probe every candidate outside the lock. Serial probing caps total
	// cycle time but no longer blocks concurrent proxy operations.
	probeCfg := resolveProxyTableProbeConfig()
	// The confirmation floor is a PAID-only concept. The admission gate and the
	// reaper must keep their exact dial profile, so zero it explicitly here.
	probeCfg.MinConfirmDials = 0
	type probeResultEntry struct {
		addr       string
		result     probeResult
		wasProbeOK bool
		lastProbe  time.Time
		table      tableProbeResult
		// tableProbed records whether the stage-1 table probe actually ran
		// for this entry (as opposed to being skipped because the 32/cycle
		// refresh budget was exhausted). The apply step uses it to decide
		// whether to re-stamp LastProbe.
		tableProbed bool
	}
	// Grade refresh is bounded per cycle: each stale once-good candidate
	// that passes stage 0 would otherwise run a serial table probe of
	// sample_width destinations, and a large cache could stretch the reaper
	// cycle past its own tick.
	const proxyReaperMaxGradeRefresh = 32
	refreshBudget := proxyReaperMaxGradeRefresh
	results := make([]probeResultEntry, 0, len(candidates))
	for _, c := range candidates {
		res := probeProxy(ctx, c.addr, c.entry.User, c.entry.Password, apiHost, apiPort)
		entry := probeResultEntry{
			addr:       c.addr,
			result:     res,
			wasProbeOK: c.wasProbeOK,
			lastProbe:  c.entry.LastProbe,
		}
		// Quality refresh on the stale re-probe of once-good entries. The
		// refresh is gated on the kill switch: with enabled:false the
		// operator has turned stage-1 off and the reaper must not run it.
		if c.wasProbeOK && res == probeAPIReachable && probeCfg.Enabled && refreshBudget > 0 {
			entry.table = probeTableThroughProxy(ctx, c.addr, c.entry.User, c.entry.Password, "", 0, probeCfg)
			entry.tableProbed = true
			refreshBudget--
		}
		results = append(results, entry)
	}

	// Re-acquire the lock and atomically apply all results.
	func() {
		release, err := acquireProxyLockWithRetry()
		if err != nil {
			return
		}
		defer release()

		state, err := readProxyURLState()
		if err != nil {
			return
		}

		changed := false
		// Grade-refresh counters for the per-cycle summary.
		refreshedGrades := 0
		tierChanges := 0
		frozen := degradedReaperFrozen()
		liveHealth := liveProxyHealthByKey()
		heldBack := 0
		defer func() {
			if 0 < heldBack {
				outageEvictionHoldLog(fmt.Sprintf("url reaper (%d probe results ignored)", heldBack))
			}
		}()
		for _, r := range results {
			entry, ok := state.Cache[r.addr]
			if !ok {
				continue // removed by a concurrent writer
			}
			if entry.LastProbe.After(r.lastProbe) {
				continue // updated by a concurrent fetch or another reaper cycle
			}
			// A failed API probe means nothing while the backend is failing (the
			// API stage fails through every proxy), and nothing for a proxy that is
			// still connecting or parked waiting its turn under the attempt budget.
			// Demoting or blacklisting then, with a 24h cooldown, would empty the
			// pool just when it is needed. A hostile TLS result is still acted on.
			if (r.result == probeSocks5Only || r.result == probeDead) &&
				(frozen || proxyHeldOrConnecting(entry, r.addr, liveHealth)) {
				heldBack++
				continue
			}
			if r.wasProbeOK {
				// Stale re-probe of a once-good entry: demote on failure,
				// or refresh timestamp on success.
				switch r.result {
				case probeAPIReachable:
					entry.ProbeOK = true
					entry.ProbeFails = 0
					// Quality refresh: the table probe re-graded the proxy
					// on the stale cadence. Persist the fresh grade so a
					// degraded proxy is re-ranked (and, if it fell below
					// the bar, is no longer spawned by mergeProxyURLCache),
					// and a recovered proxy gets a fair new chance.
					if r.table.Decidable {
						oldTier := ""
						oldScore := entry.Score
						wasGraded := entry.Graded // capture BEFORE the write below
						if entry.Graded {
							oldTier = proxyGradeTier(entry.Score)
						}
						newTier := proxyGradeTier(r.table.Score)
						entry.Score = r.table.Score
						entry.Graded = true
						entry.Failed = capFailedList(r.table.Failed)
						entry.LastGraded = time.Now()
						refreshedGrades++
						if oldTier != newTier {
							tierChanges++
							importantLogf("[proxy][url] reaper: refreshed grade for %s -> %s (%.2f, %d/%d)\n",
								r.addr, newTier, r.table.Score, r.table.OK, r.table.SampleWidth)
							emitProxyGradeDelta(r.addr, oldTier, newTier, oldScore, r.table.Score, wasGraded)
						}
					}
					// Re-stamp the staleness clock ONLY when the quality
					// refresh actually ran. A budget loser keeps its stale
					// LastProbe, so it sorts first next tick.
					if r.tableProbed {
						entry.LastProbe = time.Now()
					}
					state.Cache[r.addr] = entry
					changed = true
				case probeSocks5Only, probeDead:
					entry.LastProbe = time.Now()
					entry.ProbeOK = false
					entry.ProbeFails = 1
					state.Cache[r.addr] = entry
					tlog("[proxy][url] reaper: demoted %s from ProbeOK after stale re-probe\n", r.addr)
					changed = true
				case probeTLSFailed:
					// A once-good proxy that now fails TLS verification has
					// turned hostile (MITM/interceptor). Demote it on the
					// stale re-probe exactly like a dead/socks5-only result.
					entry.LastProbe = time.Now()
					entry.ProbeOK = false
					entry.ProbeFails = 1
					state.Cache[r.addr] = entry
					tlog("[proxy][url] reaper: demoted %s from ProbeOK after stale TLS-verify failure\n", r.addr)
					changed = true
				}
				continue
			}

			entry.LastProbe = time.Now()

			// liveHealth is keyed by proxy IDENTITY (address+user for a
			// credentialed entry), while r.addr is the URL cache's bare
			// address. Look the entry up under its real identity key or a
			// live credentialed proxy reads as dead and accrues ProbeFails
			// until it is wrongly blacklisted.
			liveKey := r.addr
			if entry.User != "" {
				liveKey = (&connect.ProxySettings{Network: "tcp", Address: r.addr, Auth: &proxy.Auth{User: entry.User}}).Key()
			}
			isLive := false
			if h, ok := liveHealth[liveKey]; ok && h.Health == "up" {
				isLive = true
			}

			switch r.result {
			case probeAPIReachable:
				entry.ProbeOK = true
				entry.ProbeFails = 0
				state.Cache[r.addr] = entry
				changed = true

			case probeSocks5Only, probeDead:
				if isLive {
					entry.ProbeOK = true
					entry.ProbeFails = 0
					state.Cache[r.addr] = entry
					changed = true
					break
				}

				entry.ProbeOK = false
				entry.ProbeFails++
				state.Cache[r.addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[r.addr] = time.Now().UTC()
					delete(state.Cache, r.addr)
					reason := "socks5-only"
					if r.result == probeDead {
						reason = "dead"
					}
					tlog("[proxy][url] reaper: blacklisted %s (%s, %d fails)\n", r.addr, reason, entry.ProbeFails)
				}
				changed = true

			case probeTLSFailed:
				// TLS verification through the proxy failed - the proxy
				// intercepts/terminates TLS (MITM) instead of relaying it.
				// This is a hard rejection: a hostile node must not stay in
				// the pool even if it currently relays client traffic, so
				// unlike probeSocks5Only there is NO isLive escape here.
				entry.ProbeOK = false
				entry.ProbeFails++
				state.Cache[r.addr] = entry
				if entry.ProbeFails >= proxyAPIMaxFails {
					if state.Blacklist == nil {
						state.Blacklist = map[string]time.Time{}
					}
					state.Blacklist[r.addr] = time.Now().UTC()
					delete(state.Cache, r.addr)
					tlog("[proxy][url] reaper: blacklisted %s (tls-verify, %d fails)\n", r.addr, entry.ProbeFails)
				}
				changed = true
			}
		}

		if refreshedGrades > 0 {
			importantLogf("[proxy][url] reaper: refreshed grades for %d stale entries (%d tier changes)\n",
				refreshedGrades, tierChanges)
		}

		if changed {
			if err := writeProxyURLState(state); err != nil {
				tlog("[proxy][url] reaper: could not write proxy_url.json: %v\n", err)
			}
			if reloadPath, err := proxyReloadPath(); err == nil {
				if err := writeReloadTrigger(reloadPath); err != nil {
					tlog("[proxy][url] warn: reload trigger write failed: %v\n", err)
				}
			}
		}
	}()
}

// pruneURLProxyBlacklist removes blacklist entries older than
// proxyBlacklistCooldown, giving previously-dead addresses a chance to
// re-enter via a fresh fetch cycle. Runs every proxyBlacklistPruneInterval.
func pruneURLProxyBlacklist(ctx context.Context) {
	ticker := time.NewTicker(proxyBlacklistPruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		release, err := acquireProxyLock()
		if err != nil {
			continue
		}

		state, err := readProxyURLState()
		if err != nil {
			release()
			continue
		}

		cutoff := time.Now().UTC().Add(-proxyBlacklistCooldown)
		pruned := 0
		for addr, when := range state.Blacklist {
			if when.Before(cutoff) {
				delete(state.Blacklist, addr)
				pruned++
			}
		}

		if pruned > 0 {
			tlog("[proxy][url] pruned %d blacklist entries older than %s\n", pruned, proxyBlacklistCooldown)
			if err := writeProxyURLState(state); err != nil {
				tlog("[proxy][url] pruner: could not write proxy_url.json: %v\n", err)
			}
		}

		release()
	}
}

// runProxyURLFetcher periodically fetches configured proxy list URLs and
// merges new entries into the running proxy set. The first fetch runs
// immediately after warmup/cooldown; subsequent fetches run every
// refreshInterval. Exits when ctx is cancelled. A no-op if urls is empty.
//
// The fetch interval stretches proportionally to system pressure (up to 8x
// at high pressure), so a drowning box adds probe/auth/reload work more
// slowly instead of stopping dead. A near-empty cache is never stretched,
// so a fresh box under load still bootstraps.
func runProxyURLFetcher(ctx context.Context, urls []string, refreshInterval time.Duration, maxTotal int, apiHost string, apiPort uint16, selfHealEnabled bool) {
	if len(urls) == 0 {
		// Publish why the countdown line will stay zero forever: nothing
		// to fetch.
		setURLFetcherState("none")
		return
	}
	setURLFetcherState("pending")

	// Wait for file-proxy warmup to finish before the first fetch, so URL-
	// sourced proxies never compete for auth rate-limiter slots with the
	// operator-curated file proxies during the initial ramp.
	for !proxyWarmupDone.Load() {
		select {
		case <-ctx.Done():
			setURLFetcherState("inactive")
			return
		case <-time.After(5 * time.Second):
		}
	}

	// Startup cooldown: a process that crash-loops (bad binary, SELinux
	// exec trap, OOM) must NOT re-fetch + re-probe the URL lists on every
	// 5s restart - that is the probe-amplification loop. The first fetch is
	// deferred until the process has been alive probeStartupCooldown, so a
	// crash-looping box can never reach it.
	select {
	case <-ctx.Done():
		setURLFetcherState("inactive")
		return
	case <-time.After(probeStartupCooldown):
	}

	// The initial fetch is always allowed (cold-start / starvation escape).
	fetchAndMergeProxyURLs(ctx, urls, resolveEffectiveProxyURLMax(maxTotal, selfHealEnabled), apiHost, apiPort)
	lastFetch := time.Now()

	activeInterval := resolveProxyURLRefresh(refreshInterval)
	// Publish the initial next-fetch estimate WITH the pressure stretch
	// applied, mirroring the loop's own math below.
	initialCache := readURLCacheSize()
	initialNext := activeInterval
	if initialCache >= 50 {
		initialNext = time.Duration(float64(activeInterval) * fetchStretch(currentPressure()))
	}
	setNextFetchProbeAt(lastFetch.Add(initialNext))
	ticker := time.NewTicker(activeInterval)
	// A plain `defer ticker.Stop()` binds the ticker *value* at defer time;
	// once the loop below reassigns `ticker` on an interval change, that
	// defer only ever stops the original, now-discarded ticker. Deferring a
	// closure reads the current value of `ticker` when the function
	// actually returns.
	defer func() { ticker.Stop() }()
	for {
		select {
		case <-ctx.Done():
			setNextFetchProbeAt(time.Time{})
			setURLFetcherState("inactive")
			return
		case <-ticker.C:
			// Re-check runtime overrides on every tick
			if ni := resolveProxyURLRefresh(refreshInterval); ni != activeInterval {
				ticker.Stop()
				ticker = time.NewTicker(ni)
				activeInterval = ni
				tlog("[proxy][url] refresh interval changed to %s\n", ni)
			}

			// Pressure-proportional pacing: under load the effective interval
			// stretches up to 8x so a drowning box adds probe/auth/reload work
			// more slowly instead of stopping dead (old binary gate).
			// currentPressure() is 0 when self-heal is off, so no stretching.
			pressure := currentPressure()
			cacheSize := readURLCacheSize()
			if !shouldFetchNow(time.Since(lastFetch), activeInterval, pressure, cacheSize) {
				tlog("[proxy][url] fetch deferred: pressure=%.2f stretch=%.1fx elapsed=%s cache=%d\n",
					pressure, fetchStretch(pressure), formatDuration(time.Since(lastFetch)), cacheSize)
				continue
			}
			lastFetch = time.Now()
			fetchAndMergeProxyURLs(ctx, urls, resolveEffectiveProxyURLMax(maxTotal, selfHealEnabled), apiHost, apiPort)
			// Publish the next-fetch estimate for the grade summary
			// countdown, mirroring shouldFetchNow's stretch math exactly
			// (reuse cacheSize read above) and anchored to lastFetch.
			effectiveNext := activeInterval
			if cacheSize >= 50 {
				effectiveNext = time.Duration(float64(activeInterval) * fetchStretch(currentPressure()))
			}
			setNextFetchProbeAt(lastFetch.Add(effectiveNext))
		}
	}
}

// shouldFetchNow decides whether enough time has passed since the last URL
// fetch, given the pressure-stretched effective interval. The starvation
// floor is kept from the old gate: a near-empty cache (<50) is never
// stretched, so a fresh box under load still bootstraps.
func shouldFetchNow(sinceLast, base time.Duration, pressure float64, cacheSize int) bool {
	// 1s slack absorbs ticker/handler scheduling jitter so a calm box is
	// never deferred a whole interval by landing microseconds short.
	const slack = time.Second
	if cacheSize < 50 {
		return sinceLast >= base-slack
	}
	effective := time.Duration(float64(base) * fetchStretch(pressure))
	return sinceLast >= effective-slack
}

// readURLCacheSize reads the current URL proxy cache size from proxy_url.json.
// Returns 0 on error (fetch will not be gated).
func readURLCacheSize() int {
	release, err := acquireProxyLock()
	if err != nil {
		return 0
	}
	defer release()
	state, err := readProxyURLState()
	if err != nil {
		return 0
	}
	return len(state.Cache)
}

// runProxyURLCleanupOnce removes dead/inactive/degraded proxies whose source
// matches scope ("url" removes only url-sourced proxies; "all" removes any
// source; any other value, including "none", removes nothing and returns 0).
// When ProxyURLState.DegradedCleanupThreshold is set, URL-sourced proxies
// offline longer than that threshold are also removed. The threshold is
// read from proxy_url.json every cycle, so changes take effect without a
// restart. Returns the number of proxies removed.
func runProxyURLCleanupOnce(scope string) (removed int) {
	if scope != "url" && scope != "all" {
		return 0
	}

	// While the backend is failing every proxy reads dead or offline, so nothing
	// in the persisted health says anything about the proxy itself: removing
	// then would empty the pool of good proxies. Hold off until it heals.
	if degradedReaperFrozen() {
		outageEvictionHoldLog("url cleanup")
		return 0
	}

	state, err := readProxyState()
	if err != nil {
		tlog("[proxy][cleanup] warning: could not read proxy.state: %v\n", err)
		return 0
	}

	// For degraded cleanup, require the provider to have been running
	// long enough to avoid killing proxies that just haven't authed yet
	// during this startup cycle.
	uptime := time.Since(state.StartedAt)
	const minUptime = 65 * time.Minute

	// Read degraded threshold from proxy_url.json (may be empty = disabled).
	var degradedThreshold time.Duration
	if urlState, err := readProxyURLState(); err == nil && urlState.DegradedCleanupThreshold != "" {
		if d, err := time.ParseDuration(urlState.DegradedCleanupThreshold); err == nil && d > 0 {
			degradedThreshold = d
		}
	}

	addrsBySource := map[string][]string{}
	for addr, e := range state.Proxies {
		// Skip untagged entries (pre-source-tagging)
		if e.Source == "" {
			continue
		}
		if scope == "url" && e.Source != "url" {
			continue
		}

		// Dead: apply the same uptime guard as degraded to avoid evicting
		// proxies that simply haven't authed yet during warmup. The health
		// system reports "dead" before the first auth completes, so without
		// this guard the startup cleanup pass would mass-evict warming URL
		// proxies. Inactive (7+ days unseen) is always safe to remove.
		if e.Health == "dead" {
			if uptime > minUptime {
				addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
				removed++
			}
			continue
		}
		if e.Health == "inactive" {
			addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
			removed++
			continue
		}

		// Degraded: only remove if:
		//   1. A threshold is configured
		//   2. The provider has been running long enough (>65 min)
		//   3. DownSince is set AND the proxy has been down past the threshold
		// This prevents killing proxies that are still warming up or whose
		// DownSince is stale from a prior provider session.
		if degradedThreshold > 0 && uptime > minUptime &&
			(e.Health == "recently_offline" || e.Health == "offline" || e.Health == "long_offline") {
			if e.DownSince != "" {
				if ds, err := time.Parse(time.RFC3339, e.DownSince); err == nil && time.Since(ds) >= degradedThreshold {
					addrsBySource[e.Source] = append(addrsBySource[e.Source], addr)
					removed++
					continue
				}
			}
		}
	}

	if removed == 0 {
		return 0
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		tlog("[proxy][cleanup] warning: %v\n", err)
		return 0
	}
	tlog("[proxy][cleanup] automatically removed %d dead/inactive/degraded proxies (scope=%s, degraded_threshold=%s)\n", removed, scope, degradedThreshold)
	return removed
}

// cleanupTickInterval is the fixed check cadence for runProxyURLCleanup.
// The actual cleanup cadence is the pressure-scaled effective interval
// computed each tick (see cleanupIntervalScale); this just bounds how often
// that effective interval is re-evaluated. Var (not const) so tests can
// shorten it instead of waiting on the real 15-minute cadence.
var cleanupTickInterval = 15 * time.Minute

// runProxyURLCleanup runs runProxyURLCleanupOnce on a pressure-scaled
// interval until ctx is cancelled: cleanup sheds load, so overload is when
// it should run MORE often, not less. When scope is "none" or another
// disabling value the loop still runs so that runtime toggles (off to on)
// work live - it just skips the cleanup call until scope becomes active
// again.
func runProxyURLCleanup(ctx context.Context, scope string, interval time.Duration, selfHealEnabled bool) {
	lastRun := time.Time{} // zero value means immediate first pass, same gating as before
	ticker := time.NewTicker(cleanupTickInterval)
	defer ticker.Stop()
	for {
		effective := time.Duration(float64(resolveProxyCleanupInterval(interval)) * cleanupIntervalScale(currentPressure()))
		if time.Since(lastRun) >= effective {
			if activeScope := resolveProxyCleanupScope(scope); activeScope == "url" || activeScope == "all" {
				runProxyURLCleanupOnce(activeScope)
			}
			lastRun = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// proxyHeldOrConnecting reports whether the proxy is still connecting or parked
// waiting its turn under the reconnect attempt budget, so a failed probe says
// nothing about it. A credentialed entry is looked up under its real identity key.
func proxyHeldOrConnecting(entry ProxyURLEntry, addr string, liveHealth map[string]proxyHealthStatus) bool {
	key := addr
	if entry.User != "" {
		key = (&connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: entry.User}}).Key()
	}
	h, ok := liveHealth[key]
	return ok && h.Health == "connecting"
}

type holdLogGate struct {
	mu    sync.Mutex
	every time.Duration
	last  time.Time
}

// Allow reports whether enough time has passed since the last allowed call.
func (g *holdLogGate) Allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && now.Sub(g.last) < g.every {
		return false
	}
	g.last = now
	return true
}

var outageEvictionHoldLogThrottle = &holdLogGate{every: 10 * time.Minute}

// outageEvictionHoldLog says, at most every ten minutes, that an automatic
// eviction pass held off because the backend is failing.
func outageEvictionHoldLog(what string) {
	if outageEvictionHoldLogThrottle.Allow(time.Now()) {
		importantLogf("[proxy][outage] %s held off: the backend is degraded, so proxy failures are not evidence; no proxy will be removed or blacklisted until it heals\n", what)
	}
}

// proxyGiveUpMayCount reports whether a failed authentication counts toward
// evicting the proxy. During an outage every proxy fails to authenticate, so it
// must not.
func proxyGiveUpMayCount() bool { return !degradedReaperFrozen() }
