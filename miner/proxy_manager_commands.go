package miner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// proxy_manager_commands.go ports the 3.23-fix v33 operator proxy
// subcommands onto the miner's manager/state/URL-cache functions:
//
//	proxy add-source <url>          (v33 proxyAddSource)
//	proxy remove-source <url>       (v33 proxyRemoveSource)
//	proxy refresh [--force]         (v33 proxyRefresh)
//	proxy trim <count> [--preview]  (v33 proxyTrim)
//	proxy summary                   (v33 proxySummary)
//	proxy exclude [<pattern>] [--remove]  (v33 proxyExclude)
//	proxy remove-dead [...]         (v33 proxyRemoveDead)
//	proxy activity                  (v33 proxyActivity)
//
// The state, cache, lock, trigger and removal functions all live in the
// P1(a) files; this file adds the operator surface only. Output text follows
// v33 with one deliberate change: the v33 strings use em dashes, which the
// workspace rules forbid, so the ASCII " - " is used instead.
//
// Three v33 facilities this port cannot host yet are seams with a default:
//
//   - the control socket (P2): `proxy trim --preview` asks the running
//     provider what it would shed (see previewViaControlSocket).
//   - the live health surface (P1(c)/P2): proxy summary and proxy activity
//     read the proxy_health.state / proxy_traffic.state files the provider
//     writes when present and report zero counts when it does not.
//   - contract metrics (P2): the v33 summary's "Contract Stats" block is not
//     printed here because the miner has no equivalent counter yet.

// proxyCommandFatal prints the v33 shmLogFatal line and exits with its code.
// v33's exit codes (50-72) are the operator-visible contract for scripts.
func proxyCommandFatal(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL [exit %d]: %s\n", code, fmt.Sprintf(format, args...))
	os.Exit(code)
}

// confirm asks a yes/no question on stdin. Anything but a line starting with
// "y" is a no (v33 confirm).
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.ToLower(strings.TrimSpace(scanner.Text())) == "y"
	}
	return false
}

// proxyHealthDir resolves the directory the provider writes its health
// snapshots to (v33 proxyHealthDir): URNETWORK_PROXY_HEALTH_DIR, else the
// provider state directory.
func proxyHealthDir() (string, bool) {
	if d := strings.TrimSpace(os.Getenv("URNETWORK_PROXY_HEALTH_DIR")); d != "" {
		return d, true
	}
	dir, err := providerStateDir()
	if err != nil {
		return "", false
	}
	return dir, true
}

// proxyHealthStateCounts parses the provider's proxy_health.state snapshot for
// the up/down/dead/degraded counters v33 reads in summary and activity.
// Absent or unparsable content reports zeroes, which is what v33 does when the
// health files do not exist yet.
func proxyHealthStateCounts() (up, down, dead, degraded int) {
	healthDir, ok := proxyHealthDir()
	if !ok {
		return 0, 0, 0, 0
	}
	data, err := os.ReadFile(filepath.Join(healthDir, "proxy_health.state"))
	if err != nil {
		return 0, 0, 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, " Up:") {
			fmt.Sscanf(line, " Up: %d | Down: %d | Dead: %d | Degraded: %d", &up, &down, &dead, &degraded)
		}
	}
	return up, down, dead, degraded
}

// previewViaControlSocket asks the running provider what a trim would shed
// (v33's dialControlSocket path). The miner's control socket is the P2 port, so
// P1(b) ships the seam with the same error the v33 CLI reports when the
// provider is not reachable. P2 replaces the body; nothing else changes.
var previewViaControlSocket = func(count int) (string, error) {
	return "", fmt.Errorf("the control socket is not available in this build")
}

// ---- proxy add-source / remove-source ----

// proxyAddSource implements `provider proxy add-source <url>`: persist the
// source, then fetch it once so the operator sees the result immediately. The
// one-shot fetch runs with maxTotal=0 because the cap configured for the
// running provide process applies to its own background fetcher (v33
// behavior).
func proxyAddSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)
	if url == "" {
		proxyCommandFatal(70, "no URL provided")
	}

	release, err := acquireProxyLock()
	if err != nil {
		proxyCommandFatal(71, "could not acquire proxy lock: %v", err)
	}

	state, err := readProxyURLState()
	if err != nil {
		release()
		proxyCommandFatal(72, "could not read proxy_url.json: %v", err)
	}
	for _, existing := range state.Sources {
		if existing == url {
			release()
			fmt.Printf("source already added: %s\n", url)
			return
		}
	}
	state.Sources = append(state.Sources, url)
	if err := writeProxyURLState(state); err != nil {
		release()
		proxyCommandFatal(73, "could not write proxy_url.json: %v", err)
	}
	release()

	fmt.Printf("added source: %s\nfetching now...\n", url)
	// Probe the chosen network's API, not a hardcoded host (v33 M6).
	probeHost, probePort := defaultAPIHost, uint16(defaultAPIPort)
	if apiUrl, err := resolveApiUrl(opts); err == nil {
		probeHost, probePort = apiProbeHostPort(apiUrl)
	}
	fetchAndMergeProxyURLs(context.Background(), []string{url}, 0, probeHost, probePort)
	fmt.Println("done.")
}

// proxyRemoveSource implements `provider proxy remove-source <url>`. Fetched
// addresses stay in the cache: the source only stops contributing new ones.
func proxyRemoveSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)

	release, err := acquireProxyLock()
	if err != nil {
		proxyCommandFatal(75, "could not acquire proxy lock: %v", err)
	}
	defer release()

	state, err := readProxyURLState()
	if err != nil {
		proxyCommandFatal(76, "could not read proxy_url.json: %v", err)
	}

	kept := make([]string, 0, len(state.Sources))
	found := false
	for _, existing := range state.Sources {
		if existing == url {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	if !found {
		fmt.Printf("source not found: %s\n", url)
		return
	}

	state.Sources = kept
	if err := writeProxyURLState(state); err != nil {
		proxyCommandFatal(74, "could not write proxy_url.json: %v", err)
	}
	fmt.Printf("removed source: %s\n", url)
	fmt.Println("note: previously fetched proxies from this source remain running; use 'proxy remove-dead' to prune any that go dead.")
}

// ---- proxy exclude ----

// proxyExclude manages the URL-fetch exclude patterns (v33 proxyExclude):
//
//	proxy exclude                    list active patterns
//	proxy exclude <pattern>          add a pattern
//	proxy exclude <pattern> --remove delete a pattern
func proxyExclude(opts docopt.Opts) {
	pattern, _ := opts.String("<pattern>")
	removeFlag, _ := opts.Bool("--remove")

	urlState, err := readProxyURLState()
	if err != nil {
		fmt.Printf("could not read proxy_url.json: %v\n", err)
		return
	}

	if pattern == "" {
		if removeFlag {
			fmt.Println("usage: proxy exclude <pattern> --remove")
			return
		}
		if len(urlState.ExcludePatterns) == 0 {
			fmt.Println("no exclude patterns set")
			return
		}
		fmt.Printf("%d exclude patterns (URL fetches skip matching hosts):\n", len(urlState.ExcludePatterns))
		for _, p := range urlState.ExcludePatterns {
			fmt.Printf("    %s\n", p)
		}
		return
	}

	if removeFlag {
		if !removeExcludePattern(urlState, pattern) {
			fmt.Printf("pattern %q is not in the exclude list\n", pattern)
			if len(urlState.ExcludePatterns) > 0 {
				fmt.Printf("current patterns: %s\n", strings.Join(urlState.ExcludePatterns, ", "))
			}
			return
		}
		if err := writeProxyURLState(urlState); err != nil {
			fmt.Printf("could not write proxy_url.json: %v\n", err)
			return
		}
		fmt.Printf("removed exclude pattern %q - matching proxies may return on the next URL fetch\n", pattern)
		return
	}

	if !addExcludePattern(urlState, pattern) {
		fmt.Printf("pattern %q is already excluded\n", pattern)
		return
	}
	if err := writeProxyURLState(urlState); err != nil {
		fmt.Printf("could not write proxy_url.json: %v\n", err)
		return
	}
	fmt.Printf("added exclude pattern %q - future URL fetches will skip matching hosts\n", pattern)
	fmt.Println("note: already-cached/running proxies are not removed; use 'proxy remove --match' for that")
}

// ---- proxy refresh ----

// refreshRemoval is a tracked proxy that `proxy refresh` would remove: its
// proxy.state identity key and last-known entry.
type refreshRemoval struct {
	key   string
	entry ProxyEntry
}

// planProxyRefresh diffs the desired proxy list against proxy.state by proxy
// identity (ProxySettings.Key(): address, or address+user), the same key the
// manager tracks proxies by. Diffing by bare address listed every credentialed
// proxy as both removed (its identity key is not an address) and added.
// Results are sorted by key so the operator prompt is stable despite map
// iteration order.
func planProxyRefresh(desired []*connect.ProxySettings, current map[string]ProxyEntry) (added []string, removed []refreshRemoval) {
	desiredSet := map[string]bool{}
	for _, s := range desired {
		desiredSet[s.Key()] = true
	}
	addedSet := map[string]bool{}
	for _, s := range desired {
		if _, ok := current[s.Key()]; !ok && !addedSet[s.Key()] {
			addedSet[s.Key()] = true
			added = append(added, s.Key())
		}
	}
	for key, e := range current {
		if !desiredSet[key] {
			e.Health = classifyHealth(e)
			removed = append(removed, refreshRemoval{key: key, entry: e})
		}
	}
	sort.Strings(added)
	sort.Slice(removed, func(i, j int) bool { return removed[i].key < removed[j].key })
	return added, removed
}

// classifyHealth labels an entry with no recorded health yet.
func classifyHealth(e ProxyEntry) string {
	if e.Health != "" {
		return e.Health
	}
	return "starting"
}

// proxyRefreshDesired reads the curated half of the desired set the way v33's
// proxyRefresh does: the proxy.state Source file when one is configured
// (workflow A), else the miner's internal <state>/proxy config. URL-sourced
// entries are not part of this read, exactly as in v33, so a refresh against a
// curated-only list still reports URL-cached proxies as removals.
func proxyRefreshDesired(state *ProxyState) ([]*connect.ProxySettings, error) {
	if state.Source != "" {
		return readProxySettingsFromFile(state.Source)
	}
	return readProxySettingsLegacy(), nil
}

// proxyRefresh implements `provider proxy refresh [--force]`: diff the desired
// list against proxy.state and, after confirmation, write the reload trigger
// so the running provider applies the change. The 8-hour warmup gate protects
// proxies mid-ramp; --force bypasses it.
func proxyRefresh(opts docopt.Opts) {
	force, _ := opts.Bool("--force")

	state, err := readProxyState()
	if err != nil {
		proxyCommandFatal(50, "could not read proxy.state (use 'provider proxy add/remove' to edit the proxy list for next startup)")
	}

	if state.StartedAt.IsZero() {
		proxyCommandFatal(51, "provider does not appear to be running (use 'provider proxy add/remove' to edit the proxy list for next startup)")
	}

	uptime := time.Since(state.StartedAt)

	const warmupThreshold = 8 * time.Hour
	if uptime < warmupThreshold && !force {
		proxyCommandFatal(52, "provider has only been running %s - proxies need 8-12h to warm up; use --force to override", formatDuration(uptime))
	}

	release, err := acquireProxyLock()
	if err != nil {
		proxyCommandFatal(53, "could not acquire proxy lock: %v", err)
	}
	defer release()

	desired, err := proxyRefreshDesired(state)
	if err != nil {
		proxyCommandFatal(54, "could not read proxy file %s: %v", state.Source, err)
	}

	// Diff by proxy identity (proxy.state keys are identity keys).
	currentSet := state.Proxies
	added, removed := planProxyRefresh(desired, currentSet)

	if len(added) == 0 && len(removed) == 0 {
		fmt.Println("proxy list is already up to date. Nothing to do.")
		return
	}

	// Warn if all proxies would be removed - the provider exits when the last
	// proxy goroutine stops.
	if len(removed) == len(currentSet) && len(added) == 0 {
		fmt.Printf("WARNING: This will remove ALL proxies. The provider process will exit once the\n")
		fmt.Printf("last proxy goroutine stops. Restart with a proxy list to resume providing.\n\n")
	}

	fmt.Printf("proxy refresh: %d proxies will be removed, %d will be added.\n\n", len(removed), len(added))
	if len(removed) > 0 {
		fmt.Println("  Removing:")
		for _, rp := range removed {
			fmt.Printf("    proxy[%d]  %s   - %s\n", rp.entry.ID, proxyKeyDisplay(rp.key), rp.entry.Health)
		}
	}
	if len(added) > 0 {
		fmt.Println("\n  Adding:")
		for _, key := range added {
			fmt.Printf("    %s\n", proxyKeyDisplay(key))
		}
	}

	// up, recently_offline, offline and long_offline all have significant warm
	// state; dead and inactive are low-risk (single confirmation).
	highRisk := false
	for _, rp := range removed {
		switch rp.entry.Health {
		case "up", "recently_offline", "offline", "long_offline":
			highRisk = true
		}
		if highRisk {
			break
		}
	}

	if highRisk {
		fmt.Printf("\nWARNING: One or more proxies being removed are online or have recent warm state.\n")
		if !confirm("Remove them anyway?") {
			fmt.Println("Aborted.")
			return
		}
		if !confirm("Are you sure? This may interrupt live traffic.") {
			fmt.Println("Aborted.")
			return
		}
	} else {
		if !confirm("Proceed?") {
			fmt.Println("Aborted.")
			return
		}
	}

	reloadPath, err := proxyReloadPath()
	if err != nil {
		proxyCommandFatal(55, "could not determine reload path: %v", err)
	}

	if err := writeReloadTrigger(reloadPath); err != nil {
		proxyCommandFatal(56, "could not write reload trigger: %v", err)
	}

	fmt.Println("Reload triggered. Provider will apply changes within 2 seconds.")
}

// ---- proxy trim ----

// proxyTrim implements `provider proxy trim <count> [--preview]`: it sets (or
// clears) the operator hard cap on running proxies and triggers a reload so the
// running provider sheds the worst-graded above the cap. --preview lists what
// would be shed without writing anything.
func proxyTrim(opts docopt.Opts) {
	state, err := readProxyState()
	if err != nil {
		proxyCommandFatal(70, "could not read provider state: %v", err)
	}

	count := 0
	if s, _ := opts.String("<count>"); s != "" {
		if strings.EqualFold(s, "off") {
			count = 0
		} else {
			if n, err := atoiNonNegative(s); err == nil {
				count = n
			} else {
				fmt.Printf("invalid proxy count %q (use a number or 'off')\n", s)
				return
			}
		}
	}
	preview, _ := opts.Bool("--preview")

	if count <= 0 {
		if preview {
			fmt.Println("preview: would clear the proxy trim cap")
			return
		}
		// Clearing works even while the provider is stopped (a bad cap must be
		// removable), so the running check must NOT gate this path.
		if err := writeTrimTarget(0); err != nil {
			proxyCommandFatal(71, "could not clear proxy trim cap: %v", err)
		}
		fmt.Println("proxy trim: cap cleared")
		if state != nil && !state.StartedAt.IsZero() {
			triggerProxyReload()
		}
		return
	}

	if state.StartedAt.IsZero() {
		proxyCommandFatal(70, "provider does not appear to be running")
	}

	if preview {
		text, err := previewViaControlSocket(count)
		if err != nil {
			// Never fall back to a local computation: this process has no
			// running proxies, so it would report "nothing to shed" and
			// mislead.
			fmt.Printf("preview unavailable: %v (the running provider computes the preview; it must be running and on a build with trim_preview)\n", err)
			return
		}
		fmt.Print(text)
		return
	}

	if err := writeTrimTarget(count); err != nil {
		proxyCommandFatal(72, "could not write proxy trim cap: %v", err)
	}
	fmt.Printf("proxy trim: cap set to %d running proxies; reloading to shed the worst-graded above it\n", count)
	triggerProxyReload()
}

// atoiNonNegative parses a non-negative decimal count.
func atoiNonNegative(s string) (int, error) {
	n := 0
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty count")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0, fmt.Errorf("count out of range: %q", s)
		}
	}
	return n, nil
}

// ---- proxy summary ----

// proxySummary prints the operator-facing state summary (v33 proxySummary).
// The live up/degraded counters come from the provider's health snapshot file
// when it exists; the source, cache and blacklist counts come from
// proxy.state and proxy_url.json. v33's Contract Stats block is not printed:
// the miner has no equivalent counter until the P2 metrics port.
func proxySummary() {
	state, _ := readProxyState()

	up, _, dead, degraded := proxyHealthStateCounts()
	connecting := 0

	fileCount := 0
	urlCount := 0
	internalCount := 0
	total := 0
	if state != nil {
		total = len(state.Proxies)
		for _, e := range state.Proxies {
			switch e.Source {
			case "url":
				urlCount++
			case "file":
				fileCount++
			case "internal":
				internalCount++
			default:
				if state.Source != "" {
					fileCount++
				} else {
					internalCount++
				}
			}
		}
		connecting = total - up - dead - degraded
		if connecting < 0 {
			connecting = 0
		}
	}

	urlState, _ := readProxyURLState()
	urlSources := 0
	urlCached := 0
	urlBlacklisted := 0
	if urlState != nil {
		urlSources = len(urlState.Sources)
		urlCached = len(urlState.Cache)
		urlBlacklisted = len(urlState.Blacklist)
	}

	healthDir, _ := proxyHealthDir()

	fmt.Println("=========================================================================")
	fmt.Println(" PROXY SUMMARY")
	fmt.Printf(" Updated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Println("=========================================================================")
	fmt.Println()
	fmt.Printf("  Total proxies:      %d\n", total)
	fmt.Printf("  Up:                 %d\n", up)
	fmt.Printf("  Connecting:         %d\n", connecting)
	fmt.Printf("  Degraded:           %d\n", degraded)
	fmt.Printf("  Dead:               %d\n", dead)
	fmt.Println()
	fmt.Println(" --- Sources ---")
	fileSource := "(internal)"
	if state != nil && state.Source != "" {
		fileSource = state.Source
	}
	fmt.Printf("  File proxies:       %d  (%s)\n", fileCount, fileSource)
	fmt.Printf("  URL proxies:        %d\n", urlCount)
	fmt.Printf("  Internal proxies:   %d\n", internalCount)
	fmt.Println()
	fmt.Println(" --- URL Sources ---")
	fmt.Printf("  Source URLs:        %d\n", urlSources)
	fmt.Printf("  Cached addresses:   %d\n", urlCached)
	fmt.Printf("  Blacklisted:        %d\n", urlBlacklisted)
	if len(urlState.ExcludePatterns) > 0 {
		fmt.Printf("  Exclude patterns:   %s\n", strings.Join(urlState.ExcludePatterns, ", "))
	}
	if urlSources > 0 {
		fmt.Println()
		for _, s := range urlState.Sources {
			fmt.Printf("    %s\n", s)
		}
	}
	fmt.Println()
	if state != nil && !state.StartedAt.IsZero() {
		fmt.Printf("  Provider started:   %s\n", state.StartedAt.Format(time.RFC3339))
	}
	if p, err := proxyStatePath(); err == nil {
		fmt.Printf("  Proxy state file:   %s\n", p)
	}
	fmt.Printf("  Health state:       %s/proxy_health.state\n", healthDir)
	if p, err := proxyURLStatePath(); err == nil {
		fmt.Printf("  URL state:          %s\n", p)
	}
	fmt.Println("=========================================================================")
}

// ---- proxy activity ----

// proxyActivity prints the activity snapshot (v33 proxyActivity's
// non-interactive path). The raw-mode live view needs a terminal dependency
// the miner does not carry; the snapshot is the same output v33 prints when
// stdin is not a terminal.
func proxyActivity() {
	fmt.Println("Proxy Activity Monitor")
	fmt.Println("Press Ctrl+C to exit")
	fmt.Println()

	up, _, _, degraded := proxyHealthStateCounts()
	fmt.Printf("Up: %d | Degraded: %d\n", up, degraded)

	healthDir, ok := proxyHealthDir()
	if !ok {
		return
	}
	if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_traffic.state")); err == nil {
		fmt.Print(string(data))
	}
}

// ---- proxy remove-dead ----

// removedProxy is one proxy the remove-dead command selected as a removal
// candidate, carrying its proxy.state identity key (field name kept: addr) and
// state entry.
type removedProxy struct {
	addr  string
	entry ProxyEntry
}

// formatRemovedProxyLine renders one remove-dead candidate for the operator.
// rp.addr is a proxy.state identity KEY, so it goes through proxyKeyDisplay:
// the raw key embeds a \x1f separator that must never reach the terminal.
func formatRemovedProxyLine(rp removedProxy) string {
	ts := ""
	if rp.entry.DownSince != "" {
		if t, err := time.Parse(time.RFC3339, rp.entry.DownSince); err == nil {
			ts = fmt.Sprintf(" down_since=%s", formatDuration(time.Since(t).Truncate(time.Second)))
		}
	}
	af := ""
	if rp.entry.AuthFailures > 0 {
		af = fmt.Sprintf(" auth_errors=%d", rp.entry.AuthFailures)
	}
	return fmt.Sprintf("proxy[%d]  %s%s%s", rp.entry.ID, proxyKeyDisplay(rp.addr), ts, af)
}

// removeDeadOptions captures the parsed remove-dead flags that drive which
// proxies are selected for removal: the source filter ("" = all), the degraded
// offline threshold (0 = disabled), and the minimum auth-failures/day (0 =
// disabled) that qualifies a proxy as auth-failing.
type removeDeadOptions struct {
	sourceFilter string
	degradedDur  time.Duration
	authFailMin  int64
}

// effectiveRemoveDeadSource resolves the source a state entry belongs to.
// Untagged (pre-source-tagging) entries resolve through state.Source: a
// configured file source means "file", otherwise the internal config.
func effectiveRemoveDeadSource(state *ProxyState, e ProxyEntry) string {
	if e.Source != "" {
		return e.Source
	}
	if state != nil && state.Source != "" {
		return "file"
	}
	return "internal"
}

// collectRemoveDeadCandidates is the pure decision core of the `proxy
// remove-dead` command: given the provider state, the parsed options, and the
// provider uptime, it returns the proxies to be removed, split into categories
// (dead / inactive / degraded / authFailing). It is extracted from
// proxyRemoveDead so this removal-selection logic, historically the site of the
// most regressions, can be unit-tested without docopt, interactive prompts, or
// a live provider that has been running past the 65-minute uptime gate.
func collectRemoveDeadCandidates(state *ProxyState, o removeDeadOptions, uptime time.Duration) (dead, inactive, degraded, authFailing []removedProxy) {
	for addr, e := range state.Proxies {
		if o.sourceFilter != "" && effectiveRemoveDeadSource(state, e) != o.sourceFilter {
			continue
		}

		switch e.Health {
		case "dead":
			dead = append(dead, removedProxy{addr: addr, entry: e})
		case "inactive":
			inactive = append(inactive, removedProxy{addr: addr, entry: e})
		case "recently_offline", "offline", "long_offline":
			// Degraded removal requires an explicit --degraded threshold:
			// without it (degradedDur==0) offline proxies are NOT collected
			// here (matches runProxyURLCleanupOnce), so a plain `proxy
			// remove-dead` does not silently prune every offline proxy.
			// This block uses `break` (exits the switch only), NOT `continue`:
			// offline proxies remain auth-fail eligible regardless of degraded
			// status/age, because auth failures are an independent signal.
			if o.degradedDur > 0 {
				ds, err := time.Parse(time.RFC3339, e.DownSince)
				if err != nil || time.Since(ds) < o.degradedDur {
					break
				}
				degraded = append(degraded, removedProxy{addr: addr, entry: e})
			}
		}
		// A proxy can land in BOTH a category (dead/inactive/degraded) AND
		// authFailing - a deliberate, pre-existing overlap. At removal
		// removeDeadProxies de-dupes by source bucket and each per-source
		// removal is idempotent, so the overlap has no removal-correctness
		// impact (it only double-prints in the confirmation prompt).
		// A parked proxy is resting, not failing: proxy audit stopped it and
		// its cumulative AuthFailures can be high from before. It is not "up",
		// so without this check it would be collected here.
		if o.authFailMin > 0 && e.Health != "up" && e.Health != proxyHealthParked {
			days := int64(max(1, int(uptime.Hours())/24))
			if e.AuthFailures >= o.authFailMin*days {
				authFailing = append(authFailing, removedProxy{addr: addr, entry: e})
			}
		}
	}
	return dead, inactive, degraded, authFailing
}

// removalBucketsBySource groups the selected candidates by the source they must
// be removed from, the same bucketing proxyRemoveDead feeds removeDeadProxies.
func removalBucketsBySource(state *ProxyState, toRemove []removedProxy) map[string][]string {
	addrsBySource := map[string][]string{}
	for _, rp := range toRemove {
		source := effectiveRemoveDeadSource(state, rp.entry)
		addrsBySource[source] = append(addrsBySource[source], rp.addr)
	}
	return addrsBySource
}

// proxyRemoveDead implements `provider proxy remove-dead [...]`: it removes
// confirmed dead proxies from whichever source they came from, optionally also
// collecting inactive ones and the long-degraded/auth-failing ones. Selection
// is in collectRemoveDeadCandidates; this function parses the flags, gates on
// the 65-minute dead-confirmation delay, and drives the prompts.
func proxyRemoveDead(opts docopt.Opts) {
	state, err := readProxyState()
	if err != nil || state.StartedAt.IsZero() {
		proxyCommandFatal(60, "provider does not appear to be running")
	}

	uptime := time.Since(state.StartedAt)
	const deadConfirmDelay = 65 * time.Minute
	if uptime < deadConfirmDelay {
		proxyCommandFatal(61, "provider has only been running %s - need %s uptime before dead status is confirmed", formatDuration(uptime), formatDuration(deadConfirmDelay))
	}

	autoYes, _ := opts.Bool("--yes")
	preview, _ := opts.Bool("--preview")

	degradedDur := time.Duration(0)
	degradedFlag, _ := opts.Bool("--degraded")
	degVal, _ := opts.String("--degraded")
	if degradedFlag || degVal != "" {
		if degVal == "" || degVal == "true" {
			degradedDur = 24 * time.Hour // default: remove degraded > 24h
		} else if d, err := time.ParseDuration(degVal); err == nil {
			degradedDur = d
		} else {
			fmt.Printf("invalid duration %q for --degraded (e.g. --degraded=24h)\n", degVal)
			return
		}
	}

	var sourceFilter string
	if s, _ := opts.String("--source"); s != "" {
		if s != "url" && s != "file" && s != "internal" {
			fmt.Printf("invalid source %q (use 'url', 'file', or 'internal')\n", s)
			return
		}
		sourceFilter = s
	}

	authFailMin := int64(0)
	if af, _ := opts.Int("--auth-failures"); af > 0 {
		authFailMin = int64(af)
	} else if degradedDur > 0 {
		authFailMin = 250
	}

	dead, inactive, degraded, authFailing := collectRemoveDeadCandidates(state, removeDeadOptions{
		sourceFilter: sourceFilter,
		degradedDur:  degradedDur,
		authFailMin:  authFailMin,
	}, uptime)

	if len(dead) == 0 && len(inactive) == 0 && len(degraded) == 0 && len(authFailing) == 0 {
		fmt.Println("Nothing to remove.")
		return
	}

	printCategory := func(label string, items []removedProxy) {
		if len(items) == 0 {
			return
		}
		sourceStr := ""
		if sourceFilter != "" {
			sourceStr = fmt.Sprintf(" [source=%s]", sourceFilter)
		}
		fmt.Printf("  %d %s%s:\n", len(items), label, sourceStr)
		for _, rp := range items {
			fmt.Printf("    %s\n", formatRemovedProxyLine(rp))
		}
		fmt.Println()
	}

	if preview {
		fmt.Println("=== PREVIEW (no changes will be made) ===")
		printCategory("dead", dead)
		printCategory("inactive", inactive)
		printCategory(fmt.Sprintf("degraded (offline > %s)", formatDuration(degradedDur)), degraded)
		if authFailMin > 0 {
			printCategory(fmt.Sprintf("auth-failing (>= %d/day)", authFailMin), authFailing)
		}
		total := len(dead) + len(inactive) + len(degraded) + len(authFailing)
		fmt.Printf("Would remove %d proxies total.\n", total)
		return
	}

	var toRemove []removedProxy

	if len(dead) > 0 {
		printCategory("dead", dead)
		if autoYes || confirm(fmt.Sprintf("Remove %d dead proxies?", len(dead))) {
			toRemove = append(toRemove, dead...)
		}
	}

	if len(inactive) > 0 {
		printCategory("inactive", inactive)
		if autoYes || confirm(fmt.Sprintf("Remove %d inactive proxies?", len(inactive))) {
			toRemove = append(toRemove, inactive...)
		}
	}

	if len(degraded) > 0 {
		printCategory(fmt.Sprintf("degraded (offline > %s)", formatDuration(degradedDur)), degraded)
		if autoYes || confirm(fmt.Sprintf("Remove %d degraded proxies?", len(degraded))) {
			toRemove = append(toRemove, degraded...)
		}
	}

	if len(authFailing) > 0 {
		printCategory(fmt.Sprintf("auth-failing (>= %d/day)", authFailMin), authFailing)
		if autoYes || confirm(fmt.Sprintf("Remove %d auth-failing proxies?", len(authFailing))) {
			toRemove = append(toRemove, authFailing...)
		}
	}

	if len(toRemove) == 0 {
		fmt.Println("Nothing to remove.")
		return
	}

	if err := removeDeadProxies(state, removalBucketsBySource(state, toRemove)); err != nil {
		proxyCommandFatal(62, "%v", err)
	}

	fmt.Printf("Removed %d proxies. Reload triggered.\n", len(toRemove))
}
