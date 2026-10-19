package miner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// This file ports the 3.23-fix v33 provider's URL-source cache
// (proxy_url.go), the SSRF guard for operator-supplied source URLs
// (ssrf_guard.go) and the host-pattern matcher (proxy_match.go). The fork-only
// port keeps the v33 file formats and behavior; the grade tier helpers from
// v33 proxy_grade_tier.go are included because the merge path and the ported
// tests depend on them, while the stage-1 table probe that PRODUCES those
// grades is P1(c) (see proxy_manager_probe.go).

// ProxyURLState is the on-disk record of configured live proxy URL sources
// and the addresses fetched from them so far. Unlike proxy.state, this file
// is additive-only by design: fetched addresses are only ever removed by
// removeDeadProxies/evictProxyURLAddress (manual or automatic cleanup),
// never by a fetch cycle.
type ProxyURLState struct {
	Sources []string                 `json:"sources"`
	Cache   map[string]ProxyURLEntry `json:"cache"`

	// Blacklist records addresses that were permanently evicted (see
	// evictProxyURLAddress) and the time they were blacklisted. Permanent,
	// no expiry: mergeProxyURLEntries skips any address found here, so a
	// fetch (which is otherwise add-only) can never silently bring a
	// blacklisted address back, even across process restarts.
	Blacklist map[string]time.Time `json:"blacklist,omitempty"`

	// ExcludePatterns holds case-insensitive host substrings set by
	// `proxy remove --match`. Any fetched proxy whose host matches one of
	// these is skipped at merge time, so a URL source refresh can never
	// re-add proxies the operator removed by pattern. Managed by
	// `proxy remove --match` / `proxy unexclude`.
	ExcludePatterns []string `json:"exclude_patterns,omitempty"`

	// DegradedCleanupThreshold sets how long a URL-sourced proxy can be
	// degraded before the automatic cleanup cycle evicts it. A zero value
	// (default) disables degraded auto-cleanup - only dead/inactive proxies
	// are removed, matching the pre-v24.18 behavior.
	DegradedCleanupThreshold string `json:"degraded_cleanup_threshold,omitempty"`

	// TargetPoolSize is the AIMD-discovered operating point for URL-sourced
	// proxies - grown while pressure is low, cut multiplicatively under
	// sustained pressure. 0 = controller hasn't run. proxy_url_max remains a
	// hard ceiling on top of this.
	TargetPoolSize int `json:"target_pool_size,omitempty"`
}

// ProxyURLEntry records the auth (if any) for one address fetched from a URL
// source. Most public proxy lists provide unauthenticated entries.
type ProxyURLEntry struct {
	User       string    `json:"user,omitempty"`
	Password   string    `json:"password,omitempty"`
	ProbeOK    bool      `json:"probe_ok"`              // passed API reachability probe
	ProbeFails int       `json:"probe_fails,omitempty"` // consecutive API probe failures
	LastProbe  time.Time `json:"last_probe,omitempty"`  // last API probe time

	// Score is the stage-1 table probe result (ok/total) from the last
	// graded pass, 0 when the entry has never been table-probed.
	Score float64 `json:"score,omitempty"`
	// Graded is true once a stage-1 table probe has run and recorded its
	// result.
	Graded bool `json:"graded,omitempty"`
	// Failed lists the target hostnames that did not answer the last
	// stage-1 pass, for diagnostics and fleet reporting.
	Failed []string `json:"failed,omitempty"`
	// LastGraded is when the last GENUINE stage-1 grade landed for this
	// entry (distinct from LastProbe, which is also bumped by liveness-only
	// re-checks).
	LastGraded time.Time `json:"last_graded,omitempty"`
}

func proxyURLStatePath() (string, error) {
	return providerStatePath("proxy_url.json")
}

func readProxyURLState() (*ProxyURLState, error) {
	path, err := proxyURLStatePath()
	if err != nil {
		return nil, err
	}
	return readProxyURLStateFrom(path)
}

func readProxyURLStateFrom(path string) (*ProxyURLState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ProxyURLState{Cache: map[string]ProxyURLEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy_url.json: %w", err)
	}
	var s ProxyURLState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, &proxyURLParseError{err: err}
	}
	if s.Cache == nil {
		s.Cache = map[string]ProxyURLEntry{}
	}
	// Remember the configuration half of the file. A later quarantine starts
	// from an empty cache, and none of this is cache: nothing else records the
	// sources, the permanent blacklist, the exclude patterns or the cleanup
	// threshold, so losing them here loses them for good.
	rememberURLStateConfig(&s)
	return &s, nil
}

// lastURLStateConfig is the configuration half of the newest proxy_url.json
// that parsed. See rememberURLStateConfig.
var (
	lastURLStateConfigMu sync.Mutex
	lastURLStateConfig   ProxyURLState
)

// rememberURLStateConfig keeps the operator-set half of the state - sources,
// blacklist, exclude patterns, degraded-cleanup threshold - for the quarantine
// path. A corrupt file is moved aside and the cache starts empty, but the
// configuration is not the cache: nothing else records it, and the first write
// after a quarantine would otherwise persist it as empty and lose it for good.
func rememberURLStateConfig(s *ProxyURLState) {
	if s == nil {
		return
	}
	lastURLStateConfigMu.Lock()
	defer lastURLStateConfigMu.Unlock()
	lastURLStateConfig = ProxyURLState{
		Sources:                  append([]string(nil), s.Sources...),
		Blacklist:                copyURLBlacklist(s.Blacklist),
		ExcludePatterns:          append([]string(nil), s.ExcludePatterns...),
		DegradedCleanupThreshold: s.DegradedCleanupThreshold,
	}
}

// rememberedURLStateConfig returns the last configuration seen with an empty
// cache, ready to be filled by the merge that quarantined the file.
func rememberedURLStateConfig() *ProxyURLState {
	lastURLStateConfigMu.Lock()
	defer lastURLStateConfigMu.Unlock()
	return &ProxyURLState{
		Sources:                  append([]string(nil), lastURLStateConfig.Sources...),
		Blacklist:                copyURLBlacklist(lastURLStateConfig.Blacklist),
		ExcludePatterns:          append([]string(nil), lastURLStateConfig.ExcludePatterns...),
		DegradedCleanupThreshold: lastURLStateConfig.DegradedCleanupThreshold,
		Cache:                    map[string]ProxyURLEntry{},
	}
}

func copyURLBlacklist(in map[string]time.Time) map[string]time.Time {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(in))
	for addr, at := range in {
		out[addr] = at
	}
	return out
}

// proxyURLParseError marks an error from decoding the file's content, as
// opposed to reading it. json.Unmarshal can fail with more than a SyntaxError or
// an UnmarshalTypeError (an invalid last_probe or blacklist timestamp comes back
// as a time parse error), so classifying by the decoder's error types missed
// content that nothing will ever make parse.
type proxyURLParseError struct{ err error }

func (e *proxyURLParseError) Error() string { return "parse proxy_url.json: " + e.err.Error() }
func (e *proxyURLParseError) Unwrap() error { return e.err }

// isProxyURLStateCorrupt reports whether err from readProxyURLState means the
// file's CONTENT cannot be parsed (empty, truncated or hand-edited), as opposed
// to an I/O error reading it. A read error can be transient and the file is
// worth keeping; unparseable content is not transient and nothing will ever
// make it parse, so it must not be treated like one.
func isProxyURLStateCorrupt(err error) bool {
	var parseErr *proxyURLParseError
	return errors.As(err, &parseErr)
}

// quarantineProxyURLState moves an unparseable proxy_url.json aside to
// proxy_url.json.corrupt (replacing any earlier quarantined copy, so the disk
// cost stays bounded) and returns where it went. The evidence is kept; the
// pipeline can then start from an empty cache instead of skipping every merge
// cycle for as long as the bad file sits there.
func quarantineProxyURLState() (string, error) {
	path, err := proxyURLStatePath()
	if err != nil {
		return "", err
	}
	dest := path + ".corrupt"
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func writeProxyURLState(s *ProxyURLState) error {
	path, err := proxyURLStatePath()
	if err != nil {
		return err
	}
	return writeProxyURLStateTo(path, s)
}

func writeProxyURLStateTo(path string, s *ProxyURLState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// parseProxyURLLine parses one line from a remote proxy list. Unlike
// parseProxyAddress (used by the internal config, which requires credentials),
// entries without credentials are valid here - open/anonymous proxies are
// the common case for public proxy lists. Accepted forms:
//
//	host:port
//	host:port:user:pass
//	socks5://host:port
//	socks5://user:pass@host:port
//
// Returns ok=false if the line is blank, a comment, or uses an unsupported
// protocol scheme (this fork is SOCKS5-only).
func parseProxyURLLine(line string) (address, user, password string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", "", "", false
	}

	if idx := strings.Index(line, "://"); idx != -1 {
		scheme := line[:idx]
		if !strings.EqualFold(scheme, "socks5") {
			tlog("[proxy][url] unsupported scheme %q (only socks5 is supported); skipping %q\n", scheme, line)
			return "", "", "", false
		}
		rest := line[idx+3:]
		if at := strings.LastIndex(rest, "@"); at != -1 {
			cred := rest[:at]
			address = rest[at+1:]
			if parts := strings.SplitN(cred, ":", 2); len(parts) == 2 {
				user, password = parts[0], parts[1]
			}
			return address, user, password, address != ""
		}
		address, user, password = parseProxyAddress(rest)
		return address, user, password, address != ""
	}

	address, user, password = parseProxyAddress(line)
	return address, user, password, address != ""
}

// maxProxyURLFetchBytes caps how much of a proxy list response we read,
// defending against a misbehaving or malicious endpoint returning an
// unbounded body.
const maxProxyURLFetchBytes = 10 * 1024 * 1024 // 10 MiB

// proxyURLHTTPClient is a custom HTTP client with redirect limits and
// timeouts - instead of the global http.DefaultClient which has no guards -
// plus SSRF hardening: it refuses to dial or redirect to non-global
// (loopback/link-local/private RFC1918/ULA/multicast) addresses, since
// proxy-source URLs are operator-supplied and must only reach public lists.
var proxyURLHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("stopped after 3 redirects")
		}
		return ssrfVerifyURLHost(req.URL.String())
	},
	Transport: sourceURLTransport(),
}

// stripURLFromError returns the underlying cause of a *net/url.Error, whose own
// message repeats the request URL (credentials and query tokens included). Any
// other error is returned unchanged.
func stripURLFromError(err error) error {
	var ue *neturl.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// fetchProxyURLLines fetches a proxy list from a URL and splits it into
// lines. It does not parse the lines - callers parse each line with
// parseProxyURLLine. Returns an error on network failure, non-200 status, or
// an empty body; never blocks longer than 30s.
func fetchProxyURLLines(ctx context.Context, url string) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		// url.Parse failures are also *net/url.Error values whose message embeds
		// the whole URL, query token included.
		return nil, fmt.Errorf("build request: %w", stripURLFromError(err))
	}
	resp, err := proxyURLHTTPClient.Do(req)
	if err != nil {
		// http.Client.Do returns a *net/url.Error whose message embeds the full
		// request URL, credentials included. That string lands in the log,
		// the resolution reason and the operator warning, so surface the
		// underlying cause (the URL-free URL field) instead.
		return nil, fmt.Errorf("fetch: %w", stripURLFromError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyURLFetchBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty response body")
	}

	lines := strings.Split(string(b), "\n")
	// Filter out empty lines
	var result []string
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

// mergeProxyURLEntries adds genuinely new addresses from lines into
// state.Cache (mutating it in place). Already-cached addresses are left
// untouched, blacklisted and excluded addresses are skipped, and when the
// cache is at maxTotal the lowest-ranked cached entry is evicted to make room
// for a higher-ranked candidate. maxTotal caps the total cache size; 0 means
// unlimited.
//
// rankAddr (may be nil) reports the priority of a candidate address so the
// cache keeps the BEST entries across all sources rather than the first
// ones to arrive. A nil rankAddr preserves the old behavior (at cap, remaining
// lines are skipped without evicting anything).
//
// gradeFor (may be nil) supplies the stage-1 grade for a newly added
// address so it is persisted with Score/Graded at insert time. In P1(a) the
// grade map is empty (stage-1 is P1(c)), so this path degenerates to
// ProbeOK = i < apiOKCount, which is exactly the v33 kill-switch behavior.
func mergeProxyURLEntries(state *ProxyURLState, lines []string, apiOKCount int, maxTotal int, rankAddr func(addr string) int, gradeFor func(addr string) (proxyURLGrade, bool)) (added int) {
	if state.Cache == nil {
		state.Cache = map[string]ProxyURLEntry{}
	}
	evictions := 0
	for i, line := range lines {
		address, user, password, ok := parseProxyURLLine(line)
		if !ok {
			continue
		}
		if _, exists := state.Cache[address]; exists {
			continue
		}
		if _, blacklisted := state.Blacklist[address]; blacklisted {
			continue
		}
		if hostMatchesAny(state.ExcludePatterns, address) {
			continue
		}
		if maxTotal > 0 && len(state.Cache) >= maxTotal {
			// Cache full. With a rank function, evict the lowest-ranked
			// cached entry if this candidate is better - best-overall
			// selection across all sources, not first-come-first-served.
			if rankAddr == nil {
				break
			}
			candidateRank := rankAddr(address)
			evictAddr, evictRank := lowestRankedCacheEntry(state)
			if evictAddr == "" || candidateRank <= evictRank {
				// No evictable entry, or this candidate is not better than
				// the worst one already cached. The production caller feeds
				// candidates RANK-SORTED (collectRankedCandidates, best
				// first), so once a candidate fails to outrank the lowest
				// cached entry, every later candidate is at most its rank
				// and cannot evict either - stop scanning instead of
				// re-scanning the whole cache per line.
				break
			}
			delete(state.Cache, evictAddr)
			evictions++
		}
		entry := ProxyURLEntry{User: user, Password: password, ProbeOK: i < apiOKCount}
		if gradeFor != nil {
			if g, ok := gradeFor(address); ok {
				// Address-keyed ProbeOK: the grade's Qualified flag decides
				// it, independent of Decidable - a kill-switch-disabled
				// admission carries Qualified=true with Decidable=false
				// (stage 1 never ran), and must still be ProbeOK=true.
				entry.ProbeOK = g.Qualified
				applyProxyGradeToEntry(&entry, g, time.Now())
			}
		}
		state.Cache[address] = entry
		added++
	}
	// One aggregate line per merge call (per fetch cycle), not one per
	// evicted address - the important buffer must not become a per-proxy
	// stream on a large cache.
	if evictions > 0 {
		importantLogf("[proxy][url] cap eviction: %d entries evicted for higher-ranked candidates this cycle\n", evictions)
	}
	return added
}

// applyProxyGradeToEntry persists a stage-1 grade onto a cache entry: the
// Score/Graded/Failed fields, gated on a genuine decidable verdict that is
// not socks5-only (an empty or cancelled pass leaves the prior grade
// intact). Shared by the merge insert path and the fetch cache-update loop so
// the persist rule cannot drift.
func applyProxyGradeToEntry(entry *ProxyURLEntry, g proxyURLGrade, gradedAt time.Time) {
	if g.Decidable && !g.Socks5Only {
		entry.Score = g.Score
		entry.Graded = true
		entry.Failed = capFailedList(g.Failed)
		// Stamp the grade time ONLY on a genuine stage-1 verdict. A
		// socks5-only or undecidable probe never reaches here, so
		// LastGraded stays at its prior value (or zero = never graded).
		entry.LastGraded = gradedAt
	}
}

// rankCacheEntry computes the tier priority of an already-cached entry from
// its PERSISTED grade. Ungraded entries (never graded, or graded in a cycle
// whose write was skipped) rank below every graded one, so cap eviction
// prefers dropping them first. This is distinct from the candidate-rank
// closure the caller supplies: a cached entry from a previous cycle is not
// in this cycle's grades map, so ranking it through the closure would
// collapse every old entry to -1 and evict good proxies ahead of worse ones.
func rankCacheEntry(e ProxyURLEntry) int {
	if e.Graded {
		return proxyTierRank(proxyGradeTier(e.Score))
	}
	return -1
}

// lowestRankedCacheEntry returns the address of the lowest-ranked cached
// entry and its rank, using rankCacheEntry on each persisted entry. Ties
// are broken by the entry's raw Score (lower score evicts first), so
// eviction is deterministic across map orders and strictly prefers the
// worse proxy within a tier.
func lowestRankedCacheEntry(state *ProxyURLState) (string, int) {
	lowestAddr := ""
	lowestRank := math.MaxInt
	lowestScore := math.MaxFloat64
	for addr, e := range state.Cache {
		r := rankCacheEntry(e)
		if r < lowestRank || (r == lowestRank && e.Score < lowestScore) {
			lowestRank = r
			lowestScore = e.Score
			lowestAddr = addr
		}
	}
	return lowestAddr, lowestRank
}

// mergeProxyURLCache adds entries from urlState.Cache into desiredSet for any
// address not already present, and records "url" provenance for those newly
// added addresses in sourceOf. An address already in desiredSet (from the
// primary --proxy_file / internal-config source) always wins - its entry and
// its sourceOf tag are left untouched. urlState may be nil (e.g. read error
// upstream), in which case this is a no-op.
func mergeProxyURLCache(desiredSet map[string]*connect.ProxySettings, sourceOf map[string]string, urlState *ProxyURLState) {
	if urlState == nil {
		return
	}
	// The stage-1 admission bar, resolved once: graded-below-bar entries
	// must not enter the desired set at all. They stay in the cache; the
	// reaper's stale sweep re-grades them so a proxy that recovered clears
	// the bar again without a manual flush.
	//
	// The filter is itself gated on cfg.Enabled: flipping the kill switch
	// off must bring previously-graded below-bar proxies BACK into the
	// desired set.
	//
	// An address already claimed by the primary source always wins over a
	// URL-sourced entry at that SAME address, regardless of account:
	// primary sources are operator-curated and intentional, URL sources are
	// auto-discovered. This is address-level precedence, distinct from
	// desiredSet's own key (identity) space.
	primaryAddrs := make(map[string]bool, len(desiredSet))
	for _, s := range desiredSet {
		primaryAddrs[s.Address] = true
	}

	cfg := resolveProxyTableProbeConfig()
	for addr, entry := range urlState.Cache {
		if primaryAddrs[addr] {
			continue
		}
		if cfg.Enabled && entry.Graded && entry.Score < cfg.PassBar {
			continue
		}
		settings := &connect.ProxySettings{Network: "tcp", Address: addr}
		if entry.User != "" || entry.Password != "" {
			settings.Auth = &proxy.Auth{User: entry.User, Password: entry.Password}
		}
		// desiredSet is keyed by identity (ProxySettings.Key()), not bare
		// address.
		key := settings.Key()
		desiredSet[key] = settings
		sourceOf[key] = "url"
	}
}

// ---- grade tier helpers (v33 proxy_grade_tier.go) ----

// proxyGradeTier maps a score to its letter grade. The mapping is deliberately
// fixed (not config-driven): A >= 0.9, B >= 0.8, C >= 0.7, D >= 0.6,
// F < 0.6.
func proxyGradeTier(score float64) string {
	switch {
	case score >= 0.9:
		return "A"
	case score >= 0.8:
		return "B"
	case score >= 0.7:
		return "C"
	case score >= 0.6:
		return "D"
	default:
		return "F"
	}
}

// proxyTierRank returns a numeric priority for a tier: higher is better.
// The empty tier (undecidable / never graded) ranks below F so cap
// eviction and prioritization always prefer graded entries.
func proxyTierRank(tier string) int {
	switch tier {
	case "A":
		return 4
	case "B":
		return 3
	case "C":
		return 2
	case "D":
		return 1
	case "F":
		return 0
	default:
		return -1
	}
}

// rankedProxyCandidate is one URL-source line with its tier rank, pooled
// across all sources for best-overall admission.
type rankedProxyCandidate struct {
	address string
	line    string
	rank    int
	grade   proxyURLGrade
	// hasGrade is true when the grade map contained a genuine verdict for
	// this address (decidable or not); used to attach the persisted grade.
	hasGrade bool
}

// rankFromGrade computes the tier rank for a stage-1 grade: decidable,
// non-socks5-only grades rank A=4..F=0 by letter; everything else (socks5-only,
// undecidable, ungraded) ranks -1. Used by BOTH the funnel's candidate
// ranking (collectRankedCandidates) and the merge's candidate-rank closure so
// the two can never drift.
func rankFromGrade(g proxyURLGrade) int {
	if g.Decidable && !g.Socks5Only {
		return proxyTierRank(proxyGradeTier(g.Score))
	}
	return -1
}

// collectRankedCandidates pools every parseable line from all sources with
// its tier rank and sorts best-first (A, B, C, D, F, then ungraded/
// socks5-only). The sort is STABLE within a tier, so same-tier candidates
// keep source order. This ordering is what makes cache admission a true
// A to B to C to D funnel.
func collectRankedCandidates(fetched [][]string, grades map[string]proxyURLGrade) []rankedProxyCandidate {
	var cands []rankedProxyCandidate
	for _, lines := range fetched {
		for _, line := range lines {
			address, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			c := rankedProxyCandidate{address: address, line: line, rank: -1}
			if g, ok := grades[address]; ok {
				c.hasGrade = true
				c.grade = g
				c.rank = rankFromGrade(g)
			}
			cands = append(cands, c)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].rank > cands[j].rank
	})
	return cands
}

// cachedProxyAddresses returns the set of addresses already in the cache.
// The fetch cycle probes ONLY addresses not in this set - a cached proxy
// already has a grade; quality refresh of cached entries is the reaper's job.
func cachedProxyAddresses(state *ProxyURLState) map[string]bool {
	addrs := map[string]bool{}
	if state == nil {
		return addrs
	}
	for addr := range state.Cache {
		addrs[addr] = true
	}
	return addrs
}

// mustReadProxyURLState reads proxy_url.json without failing the caller: on
// any error it returns an empty state (the fetch cycle then treats every
// address as new and probes it - safe, just slightly more probing than
// necessary for one cycle).
func mustReadProxyURLState() *ProxyURLState {
	state, err := readProxyURLState()
	if err != nil {
		// A corrupt or unreadable proxy_url.json must not look identical to
		// a fresh install: log it, or every cached address is treated as new
		// and re-probed on every cycle with no signal to the operator.
		tlog("[proxy][url] warning: could not read proxy_url.json for cached-address snapshot: %v (probing all addresses this cycle)\n", err)
		return &ProxyURLState{Cache: map[string]ProxyURLEntry{}}
	}
	return state
}

// ---- host pattern matching (v33 proxy_match.go) ----

// hostOfAddress returns the host portion of a "host:port" address.
// If the address has no port, the whole string is returned.
func hostOfAddress(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

// matchProxyHost reports whether the host portion of proxyAddress contains
// pattern, case-insensitively. proxyAddress may be "host:port" or the
// credentialed "host:port:user:pass" form used in proxy.json keys; the
// port and credentials are never matched. An empty pattern matches nothing.
func matchProxyHost(pattern, proxyAddress string) bool {
	if pattern == "" {
		return false
	}
	// Strip credentials if present ("host:port:user:pass" form).
	address, _, _ := parseProxyAddress(proxyAddress)
	host := hostOfAddress(address)
	return strings.Contains(strings.ToLower(host), strings.ToLower(pattern))
}

// hostMatchesAny reports whether address's host matches any of patterns.
func hostMatchesAny(patterns []string, address string) bool {
	for _, p := range patterns {
		if matchProxyHost(p, address) {
			return true
		}
	}
	return false
}

// addExcludePattern appends pattern to state.ExcludePatterns unless an
// equal pattern (case-insensitive) is already present. Returns true if
// the pattern was added.
func addExcludePattern(state *ProxyURLState, pattern string) bool {
	for _, p := range state.ExcludePatterns {
		if strings.EqualFold(p, pattern) {
			return false
		}
	}
	state.ExcludePatterns = append(state.ExcludePatterns, pattern)
	return true
}

// removeExcludePattern removes pattern (case-insensitive exact match)
// from state.ExcludePatterns. Returns true if a pattern was removed.
func removeExcludePattern(state *ProxyURLState, pattern string) bool {
	for i, p := range state.ExcludePatterns {
		if strings.EqualFold(p, pattern) {
			state.ExcludePatterns = append(state.ExcludePatterns[:i], state.ExcludePatterns[i+1:]...)
			return true
		}
	}
	return false
}

// ---- SSRF guard (v33 ssrf_guard.go) ----

// ssrfAllowLoopback allows loopback dials/redirects when true. This is used
// exclusively in unit tests to allow httptest.NewServer mock endpoints.
var ssrfAllowLoopback atomic.Bool

// isBlockedSourceIP reports whether ip must never be contacted by a fetcher.
func isBlockedSourceIP(ip net.IP) bool {
	if ip == nil {
		return true // unresolvable - treat as unsafe
	}
	if ssrfAllowLoopback.Load() && ip.IsLoopback() {
		return false
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// ssrfLookupHost resolves hostname and reports whether it maps to any
// blocked (non-global) address.
func ssrfLookupHost(host string) error {
	addrs, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if isBlockedSourceIP(a) {
			return fmt.Errorf("source URL host %q resolves to non-global address %s", host, a)
		}
	}
	return nil
}

// ssrfVerifyURLHost rejects a URL whose recorded host resolves only to blocked
// (non-global) addresses. host may be an IP literal or a hostname.
func ssrfVerifyURLHost(rawURL string) error {
	// Neither error may carry rawURL: a redirect target can hold a query token,
	// and these errors reach the fetch-failure log. A url.Parse failure is a
	// *url.Error that embeds the URL, so only its cause is returned.
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return stripURLFromError(err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("source URL has no host")
	}
	return ssrfLookupHost(host)
}

// ssrfDialContext wraps a plain TCP dialer so any dial to a blocked
// (non-global) destination is refused immediately, before bytes are sent.
func ssrfDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedSourceIP(ip) {
			return nil, fmt.Errorf("refusing dial to non-global address %s (SSRF guard)", addr)
		}
		d := net.Dialer{}
		return d.DialContext(ctx, network, addr)
	}

	resolver := net.Resolver{}
	ipAddrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{}
	var lastErr error
	for _, ipAddr := range ipAddrs {
		if isBlockedSourceIP(ipAddr.IP) {
			lastErr = fmt.Errorf("refusing dial to non-global address %s (SSRF guard, host %s)", ipAddr.IP, host)
			continue
		}
		conn, dialErr := d.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses found for host %s", host)
	}
	return nil, lastErr
}

// sourceURLTransport builds the hardened transport used by the URL fetch path.
func sourceURLTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:       1,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	t.DialContext = ssrfDialContext
	return t
}
