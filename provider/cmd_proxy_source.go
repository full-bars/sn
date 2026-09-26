package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// refreshRemoval is one proxy state entry that the refresh diff would drop,
// carrying its identity key so the operator prompt never prints a raw key.
type refreshRemoval struct {
	key   string
	entry ProxyEntry
}

// planProxyRefresh diffs the desired proxy list against proxy.state by proxy
// identity (ProxySettings.Key(): address, or address+user), the same key reload
// tracks proxies by. Diffing by bare address listed every credentialed proxy as
// both removed (its identity key is not an address) and added. Results are
// sorted by key so the operator prompt is stable despite map iteration order.
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

func proxyRefresh(opts docopt.Opts) {
	force, _ := opts.Bool("--force")

	state, err := readProxyState()
	if err != nil {
		shmLogFatal(50, "could not read proxy.state (use 'provider proxy add/remove' to edit the proxy list for next startup)")
	}

	if state.StartedAt.IsZero() {
		shmLogFatal(51, "provider does not appear to be running (use 'provider proxy add/remove' to edit the proxy list for next startup)")
	}

	uptime := time.Since(state.StartedAt)

	const warmupThreshold = 8 * time.Hour
	if uptime < warmupThreshold && !force {
		shmLogFatal(52, "provider has only been running %s — proxies need 8-12h to warm up; use --force to override", formatDuration(uptime))
	}

	release, err := acquireProxyLock()
	if err != nil {
		shmLogFatal(53, "could not acquire proxy lock: %v", err)
	}
	defer release()

	var desired []*connect.ProxySettings
	if state.Source != "" {
		settings, err := readProxySettingsFromFile(state.Source)
		if err != nil {
			shmLogFatal(54, "could not read proxy file %s: %v", state.Source, err)
		}
		desired = settings
	} else {
		desired = readProxySettings()
	}

	// Diff by proxy identity (proxy.state keys are identity keys).
	currentSet := state.Proxies
	added, removed := planProxyRefresh(desired, currentSet)

	if len(added) == 0 && len(removed) == 0 {
		fmt.Println("proxy list is already up to date. Nothing to do.")
		return
	}

	// Warn if all proxies would be removed — the provider exits when the last proxy goroutine stops.
	if len(removed) == len(currentSet) && len(added) == 0 {
		fmt.Printf("WARNING: This will remove ALL proxies. The provider process will exit once the\n")
		fmt.Printf("last proxy goroutine stops. Restart with a proxy list to resume providing.\n\n")
	}

	// Print diff
	fmt.Printf("proxy refresh: %d proxies will be removed, %d will be added.\n\n", len(removed), len(added))
	if len(removed) > 0 {
		fmt.Println("  Removing:")
		for _, rp := range removed {
			fmt.Printf("    proxy[%d]  %s   — %s\n", rp.entry.ID, proxyKeyDisplay(rp.key), rp.entry.Health)
		}
	}
	if len(added) > 0 {
		fmt.Println("\n  Adding:")
		for _, key := range added {
			fmt.Printf("    %s\n", proxyKeyDisplay(key))
		}
	}

	// Check for high-risk removals
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
		if !confirmPrompt("Remove them anyway?") {
			fmt.Println("Aborted.")
			return
		}
		if !confirmPrompt("Are you sure? This may interrupt live traffic.") {
			fmt.Println("Aborted.")
			return
		}
	} else {
		if !confirmPrompt("Proceed?") {
			fmt.Println("Aborted.")
			return
		}
	}

	reloadPath, err := proxyReloadPath()
	if err != nil {
		shmLogFatal(55, "could not determine reload path: %v", err)
	}

	if err := writeReloadTrigger(reloadPath); err != nil {
		shmLogFatal(56, "could not write reload trigger: %v", err)
	}

	fmt.Println("Reload triggered. Provider will apply changes within 2 seconds.")
}

func proxyAddSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)
	if url == "" {
		shmLogFatal(70, "no URL provided")
	}

	release, err := acquireProxyLock()
	if err != nil {
		shmLogFatal(71, "could not acquire proxy lock: %v", err)
	}

	state, err := readProxyURLState()
	if err != nil {
		release()
		shmLogFatal(72, "could not read proxy_url.json: %v", err)
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
		shmLogFatal(73, "could not write proxy_url.json: %v", err)
	}
	release()

	fmt.Printf("added source: %s\nfetching now...\n", url)
	// maxTotal=0 here: the cap configured for the running provide() process
	// (--proxy_url_max) applies to its own background fetcher, not to this
	// one-shot CLI fetch. The next scheduled fetch will resume honoring it.
	// M6: probe the chosen network's API, not a hardcoded main-network host.
	probeHost, probePort := resolveAPIProbeHostPort()
	fetchAndMergeProxyURLs(context.Background(), []string{url}, 0, probeHost, probePort)
	fmt.Println("done.")
}

func proxyRemoveSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)

	release, err := acquireProxyLock()
	if err != nil {
		shmLogFatal(75, "could not acquire proxy lock: %v", err)
	}
	defer release()

	state, err := readProxyURLState()
	if err != nil {
		shmLogFatal(76, "could not read proxy_url.json: %v", err)
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
		shmLogFatal(74, "could not write proxy_url.json: %v", err)
	}
	fmt.Printf("removed source: %s\n", url)
	fmt.Println("note: previously fetched proxies from this source remain running; use 'proxy remove-dead' to prune any that go dead.")
}
