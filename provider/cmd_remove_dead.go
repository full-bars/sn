package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// removedProxy is one proxy the remove-dead command selected as a removal
// candidate, carrying its address and state entry.
type removedProxy struct {
	addr  string
	entry ProxyEntry
}

// removeDeadOptions captures the parsed remove-dead flags that drive which
// proxies are selected for removal.
type removeDeadOptions struct {
	sourceFilter string
	degradedDur  time.Duration
	authFailMin  int64
}

// collectRemoveDeadCandidates is the pure decision core of the `proxy
// remove-dead` command: given the provider state, the parsed options, and the
// provider uptime, it returns the proxies to be removed, split into categories
// (dead / inactive / degraded / authFailing). It is extracted from
// proxyRemoveDead so this removal-selection logic can be unit-tested.
func collectRemoveDeadCandidates(state *ProxyState, o removeDeadOptions, uptime time.Duration) (dead, inactive, degraded, authFailing []removedProxy) {
	for addr, e := range state.Proxies {
		// Resolve the effective source for untagged entries (pre-source-tagging).
		effectiveSource := e.Source
		if effectiveSource == "" {
			if state.Source != "" {
				effectiveSource = "file"
			} else {
				effectiveSource = "internal"
			}
		}
		if o.sourceFilter != "" && effectiveSource != o.sourceFilter {
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
			// This block uses `break` (exits the switch only), NOT `continue`
			// (which inside a switch targets the ENCLOSING for loop and would
			// skip the auth-fail check below). Deliberate: offline proxies
			// remain auth-fail eligible regardless of degraded status/age —
			// auth failures are an independent, actionable signal and
			// dropping it would hide removals.
			if o.degradedDur > 0 {
				ds, err := time.Parse(time.RFC3339, e.DownSince)
				if err != nil || time.Since(ds) < o.degradedDur {
					break
				}
				degraded = append(degraded, removedProxy{addr: addr, entry: e})
			}
		}
		// A proxy can land in BOTH a category (dead/inactive/degraded) AND
		// authFailing — a deliberate, pre-existing overlap.
		// A parked proxy is resting, not failing: proxy audit stopped it,
		// and its cumulative AuthFailures can be high from before. It is not
		// "up", so without this it would be collected here (and --degraded turns
		// this check on by default) and removed from the operator's proxy file.
		if o.authFailMin > 0 && e.Health != "up" && e.Health != proxyHealthParked {
			days := int64(max(1, int(uptime.Hours())/24))
			if e.AuthFailures >= o.authFailMin*days {
				authFailing = append(authFailing, removedProxy{addr: addr, entry: e})
			}
		}
	}
	return dead, inactive, degraded, authFailing
}

func proxyRemoveDead(opts docopt.Opts) {
	state, err := readProxyState()
	if err != nil || state.StartedAt.IsZero() {
		shmLogFatal(60, "provider does not appear to be running")
	}

	uptime := time.Since(state.StartedAt)
	// Uses the shared constant to stay in sync with connectingStaleAfter (M7 fix).
	const deadConfirmDelay = StagingWindowDuration
	if uptime < deadConfirmDelay {
		shmLogFatal(61, "provider has only been running %s — need %s uptime before dead status is confirmed", formatDuration(uptime), formatDuration(deadConfirmDelay))
	}

	// Parse options
	autoYes, _ := opts.Bool("--yes")
	preview, _ := opts.Bool("--preview")

	degradedDur := time.Duration(0)
	degradedFlag, _ := opts.Bool("--degraded")
	degVal, _ := opts.String("--degraded")
	if degradedFlag || degVal != "" {
		if degVal == "" || degVal == "true" {
			degradedDur = 24 * time.Hour // default: remove degraded > 24h
		} else {
			if d, err := time.ParseDuration(degVal); err == nil {
				degradedDur = d
			} else {
				fmt.Printf("invalid duration %q for --degraded (e.g. --degraded=24h)\n", degVal)
				return
			}
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

	var authFailMin int64
	if af, _ := opts.Int("--auth-failures"); af > 0 {
		authFailMin = int64(af)
	} else if degradedDur > 0 {
		authFailMin = 250
	}

	// Collect candidates by category.
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
			fmt.Printf("    proxy[%d]  %s%s%s\n", rp.entry.ID, rp.addr, ts, af)
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
		if autoYes || confirmPrompt(fmt.Sprintf("Remove %d dead proxies?", len(dead))) {
			toRemove = append(toRemove, dead...)
		}
	}

	if len(inactive) > 0 {
		printCategory("inactive", inactive)
		if autoYes || confirmPrompt(fmt.Sprintf("Remove %d inactive proxies?", len(inactive))) {
			toRemove = append(toRemove, inactive...)
		}
	}

	if len(degraded) > 0 {
		printCategory(fmt.Sprintf("degraded (offline > %s)", formatDuration(degradedDur)), degraded)
		if autoYes || confirmPrompt(fmt.Sprintf("Remove %d degraded proxies?", len(degraded))) {
			toRemove = append(toRemove, degraded...)
		}
	}

	if len(authFailing) > 0 {
		printCategory(fmt.Sprintf("auth-failing (>= %d/day)", authFailMin), authFailing)
		if autoYes || confirmPrompt(fmt.Sprintf("Remove %d auth-failing proxies?", len(authFailing))) {
			toRemove = append(toRemove, authFailing...)
		}
	}

	if len(toRemove) == 0 {
		fmt.Println("Nothing to remove.")
		return
	}

	addrsBySource := map[string][]string{}
	for _, rp := range toRemove {
		source := rp.entry.Source
		if source == "" {
			if state.Source != "" {
				source = "file"
			} else {
				source = "internal"
			}
		}
		addrsBySource[source] = append(addrsBySource[source], rp.addr)
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		shmLogFatal(62, "%v", err)
	}

	fmt.Printf("Removed %d proxies. Reload triggered.\n", len(toRemove))
}

// removeKeysFromFile deletes the lines of a --proxy_file source whose proxy
// identity (ProxySettings.Key(): address, or address+user) is in keys. Matching
// on identity, not address, is what keeps two accounts at one shared gateway
// apart: removing one must never remove the other.
func removeKeysFromFile(path string, keys []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	removeSet := map[string]bool{}
	for _, k := range keys {
		removeSet[k] = true
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		addr, user, _ := parseProxyAddress(trimmed)
		lineKey := (&connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: user}}).Key()
		if !removeSet[lineKey] {
			kept = append(kept, line)
		}
	}
	content := strings.Join(kept, "\n")
	if len(b) > 0 && b[len(b)-1] == '\n' && (len(content) == 0 || content[len(content)-1] != '\n') {
		content += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// confirmPrompt asks the user a yes/no question and returns true for 'y'.
func confirmPrompt(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		resp := scanner.Text()
		return strings.ToLower(strings.TrimSpace(resp)) == "y"
	}
	return false
}

func classifyHealth(e ProxyEntry) string {
	if e.Health != "" {
		return e.Health
	}
	return "starting"
}

// readProxyConfig reads the persisted proxy configuration.
func readProxyConfig() *ProxyConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		tlog("[proxy] Error: could not find user home directory: %v\n", err)
		return &ProxyConfig{}
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	if _, err := os.Stat(proxyPath); errors.Is(err, os.ErrNotExist) {
		return &ProxyConfig{}
	}

	b, err := os.ReadFile(proxyPath)
	if err != nil {
		tlog("[proxy] Error: could not read proxy config at %s: %v\n", proxyPath, err)
		return &ProxyConfig{}
	}

	var proxyConfig ProxyConfig
	err = json.Unmarshal(b, &proxyConfig)
	if err != nil {
		tlog("[proxy] Error: could not parse proxy config at %s: %v\n", proxyPath, err)
		return &ProxyConfig{}
	}
	return &proxyConfig
}

// writeProxyConfig persists the proxy configuration.
func writeProxyConfig(proxyConfig *ProxyConfig) {
	home, err := os.UserHomeDir()
	if err != nil {
		tlog("[proxy] Error: could not find user home directory: %v\n", err)
		return
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	if _, err := os.Stat(urNetworkDir); os.IsNotExist(err) {
		err = os.MkdirAll(urNetworkDir, 0700)
		if err != nil {
			tlog("[proxy] Error: could not create %s: %v\n", urNetworkDir, err)
			return
		}
	}

	b, err := json.Marshal(proxyConfig)
	if err != nil {
		tlog("[proxy] Error: could not marshal proxy config: %v\n", err)
		return
	}

	err = atomicWriteFile(proxyPath, b, 0700)
	if err != nil {
		tlog("[proxy] Error: could not write proxy config at %s: %v\n", proxyPath, err)
		return
	}

	// Automatically trigger a hot-reload so running providers pick up the changes
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
		}
	}
}

// readProxySettings reads the current proxy settings from the persisted proxy
// configuration file (~/.urnetwork/proxy).
func readProxySettings() []*connect.ProxySettings {
	proxyConfig := readProxyConfig()

	if proxyConfig.Servers == nil {
		return nil
	}

	var allProxySettings []*connect.ProxySettings
	for proxyAddress, key := range proxyConfig.Servers {
		address, user, password := parseProxyAddress(proxyAddress)
		proxySettings := &connect.ProxySettings{
			Network: "tcp",
			Address: address,
		}
		if user != "" || password != "" {
			proxySettings.Auth = &proxy.Auth{
				User:     user,
				Password: password,
			}
		}
		if proxyConfig.Auths != nil {
			proxyAuth, ok := proxyConfig.Auths[key]
			if ok {
				proxySettings.Auth = &proxy.Auth{
					User:     proxyAuth.User,
					Password: proxyAuth.Password,
				}
			}
		}
		allProxySettings = append(allProxySettings, proxySettings)
	}

	return allProxySettings
}

// readProxySettingsFromFile reads proxy settings directly from an external file
// where each non-blank, non-comment line is ip:port:user:pass. Entries missing
// credentials are rejected.
func readProxySettingsFromFile(path string) ([]*connect.ProxySettings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read proxy file %s: %w", path, err)
	}
	var all []*connect.ProxySettings
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		address, user, password := parseProxyAddress(line)
		if user == "" || password == "" {
			tlog("[proxy] error: proxy %q missing credentials — required format ip:port:user:pass; skipping\n", line)
			continue
		}
		all = append(all, &connect.ProxySettings{
			Network: "tcp",
			Address: address,
			Auth:    &proxy.Auth{User: user, Password: password},
		})
	}
	return all, nil
}
