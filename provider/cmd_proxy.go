package provider

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
)

// ProxyConfig holds the persisted proxy list configuration.
type ProxyConfig struct {
	Auths   map[string]*ProxyAuth `json:"auths"`
	Servers map[string]string     `json:"servers"` // address -> key
}

// ProxyAuth holds the credentials for a named auth key.
type ProxyAuth struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func proxyAuthAdd(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	key, _ := opts.String("key")
	user, _ := opts.String("proxy_user")
	password, _ := opts.String("proxy_password")

	if proxyConfig.Auths == nil {
		proxyConfig.Auths = map[string]*ProxyAuth{}
	}

	if _, ok := proxyConfig.Auths[key]; ok {
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("auth key \"%s\" exists. Overwrite? [yN]\n", key)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
				return
			}
		}
	}

	proxyConfig.Auths[key] = &ProxyAuth{
		User:     user,
		Password: password,
	}

	writeProxyConfig(proxyConfig)
}

func proxyAuthRemove(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	if all, _ := opts.Bool("--all"); all {
		clear(proxyConfig.Auths)
	} else {

		key, _ := opts.String("key")

		if proxyConfig.Auths == nil {
			proxyConfig.Auths = map[string]*ProxyAuth{}
		}

		delete(proxyConfig.Auths, key)
	}

	writeProxyConfig(proxyConfig)
}

// expandPath expands leading ~/ or ~\ to the user's home directory.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

func proxyAdd(opts docopt.Opts) {
	// File-backed providers (Workflow A: --proxy_file=<X>) load their proxies
	// from that external file on every reload; writes to the internal config
	// are discarded. So additions must be appended to the source file itself
	// (then reloaded), not the internal config. Delegate and return.
	if state, err := readProxyState(); err == nil && state.Source != "" {
		proxyAddFileBacked(state.Source, opts)
		return
	}

	proxyConfig := readProxyConfig()

	allKeyAddress := proxyAddCollectAddresses(opts)

	if proxyConfig.Servers == nil {
		proxyConfig.Servers = map[string]string{}
	}

keyAddressLoop:
	for _, keyAddress := range allKeyAddress {
		var key string
		var proxyAddress string
		i := strings.Index(keyAddress, "@")
		if 0 <= i {
			key = keyAddress[:i]
			proxyAddress = keyAddress[i+1:]
		} else {
			key = ""
			proxyAddress = keyAddress
		}

		address, user, password := parseProxyAddress(proxyAddress)
		if proxyConfig.Auths != nil {
			proxyAuth, ok := proxyConfig.Auths[key]
			if ok {
				user = proxyAuth.User
				password = proxyAuth.Password
			}
		}

		// Credential rotation: purge any existing entry for the same
		// host:port whose credentials differ, so adding the same address
		// with new credentials is a ROTATION, not a duplicate. The
		// reloader diffs by address only (desiredSet[s.Address]), so two
		// keys for one host:port with different user:pass made re-paste a
		// no-op — the same address was already "desired", the new creds
		// were silently dropped, and the running proxy kept the old auth
		// (LA7 incident 2026-09-18: 100 proxies pasted with new creds,
		// "added 100" printed, daemon kept dialing the old user).
		// Scan EVERY entry for this address before deciding anything. Stopping
		// at the first entry with identical credentials (the old behavior)
		// left any stale duplicate not yet visited in place, and Go's random
		// map order made whether it was purged nondeterministic.
		keepExisting := false
		var stale []string
		for existing, existingKey := range proxyConfig.Servers {
			existingAddress, existingUser, existingPassword := parseProxyAddress(existing)
			if existingAddress != address || existing == proxyAddress {
				continue
			}
			// Compare EFFECTIVE credentials: a stored key can carry its
			// credentials in the Auths table instead of in the server string,
			// and an alternate representation of the same credentials is not
			// a rotation.
			if proxyConfig.Auths != nil {
				if existingAuth, ok := proxyConfig.Auths[existingKey]; ok {
					existingUser = existingAuth.User
					existingPassword = existingAuth.Password
				}
			}
			if existingUser == user && existingPassword == password {
				keepExisting = true
				continue
			}
			stale = append(stale, existing)
		}
		for _, existing := range stale {
			delete(proxyConfig.Servers, existing)
			if keepExisting {
				fmt.Printf("removed stale duplicate entry for server %s\n", address)
			} else {
				fmt.Printf("rotated credentials for server %s\n", address)
			}
		}
		if keepExisting {
			continue keyAddressLoop
		}

		fmt.Printf(
			"added server %s (%s/%s)\n",
			address,
			obfuscateUser(user),
			obfuscatePassword(password),
		)

		proxyConfig.Servers[proxyAddress] = key
	}

	writeProxyConfig(proxyConfig)
}

// proxyAddCollectAddresses gathers the addresses to add from opts, resolving
// --url/positional-URL sources through proxyAddSource and expanding file
// arguments inline (a path positional or --proxy_file/--file whose contents
// are read line-by-line). Shared by the internal-config add path and the
// file-backed add path so both accept the same inputs.
func proxyAddCollectAddresses(opts docopt.Opts) []string {
	allKeyAddress := []string{}
	if allKeyAddressAny, ok := opts["<key_address>"]; ok {
		allKeyAddress = append(allKeyAddress, allKeyAddressAny.([]string)...)
	}

	url, _ := opts.String("--url")
	if url == "" {
		url, _ = opts.String("--URL")
	}
	if url != "" {
		proxyAddSource(docopt.Opts{"<url>": url})
	}

	proxyPath, _ := opts.String("--proxy_file")
	if proxyPath == "" {
		proxyPath, _ = opts.String("--file")
	}

	var remainingKeyAddress []string
	for _, item := range allKeyAddress {
		if strings.HasPrefix(item, "http://") || strings.HasPrefix(item, "https://") {
			proxyAddSource(docopt.Opts{"<url>": item})
			continue
		}
		candidate := expandPath(item)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			b, err := os.ReadFile(candidate)
			if err != nil {
				panic(err)
			}
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && line[0] != '#' {
					remainingKeyAddress = append(remainingKeyAddress, line)
				}
			}
			continue
		}
		remainingKeyAddress = append(remainingKeyAddress, item)
	}
	allKeyAddress = remainingKeyAddress

	if proxyPath != "" {
		proxyPath = expandPath(proxyPath)
		b, err := os.ReadFile(proxyPath)
		if err != nil {
			panic(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && line[0] != '#' {
				allKeyAddress = append(allKeyAddress, line)
			}
		}
	}

	return allKeyAddress
}

// proxyAddFileBacked appends new proxy addresses to a Workflow A source file
// (the --proxy_file=... the running provider reads on every reload) instead
// of the internal config, which a file-backed reload discards. Lines already
// present in the file are skipped (dedup by full line, matching add-time
// semantics); the file is written atomically and a reload trigger is fired so
// the additions take effect without a restart. URL sources are still routed
// to proxyAddSource — URL sources are additive and work alongside a file
// source.
func proxyAddFileBacked(sourcePath string, opts docopt.Opts) {
	release, err := acquireProxyLockWithRetry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy add: could not acquire proxy lock: %v\n", err)
		return
	}
	defer release()

	allKeyAddress := proxyAddCollectAddresses(opts)

	// Read existing file content, preserving comments/blank lines, and track
	// which normalized proxy lines are already present.
	existing := map[string]bool{}
	var out []string
	if b, err := os.ReadFile(sourcePath); err == nil {
		s := strings.TrimSuffix(string(b), "\n")
		s = strings.TrimSuffix(s, "\r")
		if len(s) > 0 {
			for _, line := range strings.Split(s, "\n") {
				line = strings.TrimSuffix(line, "\r")
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					out = append(out, line)
					continue
				}
				existing[trimmed] = true
				out = append(out, line)
			}
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "proxy add: could not read %s: %v\n", sourcePath, err)
		return
	}

	added := 0
	for _, keyAddress := range allKeyAddress {
		// Strip a key@ prefix; the source file stores bare host:port:user:pass.
		proxyAddress := keyAddress
		if i := strings.Index(keyAddress, "@"); 0 <= i {
			proxyAddress = keyAddress[i+1:]
		}
		trimmed := strings.TrimSpace(proxyAddress)
		if existing[trimmed] {
			fmt.Printf("server %s already present in %s\n", trimmed, sourcePath)
			continue
		}
		existing[trimmed] = true
		out = append(out, trimmed)
		address, user, password := parseProxyAddress(trimmed)
		fmt.Printf("added server %s (%s/%s)\n", address, obfuscateUser(user), obfuscatePassword(password))
		added++
	}

	if added == 0 {
		fmt.Println("no new proxies to add")
		return
	}

	content := strings.Join(out, "\n") + "\n"
	if err := atomicWriteFile(sourcePath, []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "proxy add: could not write %s: %v\n", sourcePath, err)
		return
	}
	fmt.Printf("appended %d proxy(ies) to %s\n", added, sourcePath)

	// Apply immediately without restarting: file-backed reloads re-read the
	// source file, so a reload trigger is all that's needed (paste does the
	// same).
	reloadPath, err := proxyReloadPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
	} else if err := writeReloadTrigger(reloadPath); err != nil {
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
	}
}

func proxyRemove(opts docopt.Opts) {
	if pattern, _ := opts.String("--match"); pattern != "" {
		proxyRemoveMatch(pattern, opts)
		return
	}

	proxyConfig := readProxyConfig()

	if all, _ := opts.Bool("--all"); all {
		clear(proxyConfig.Servers)
		// Take the cross-process lock so a concurrent daemon health-snapshot
		// write can't clobber this reset and silently revive the removed
		// proxies (finding #11). Proxy config write above is process-local;
		// this serializes the proxy.state reset against the daemon.
		stateLockRelease, stateLockErr := acquireProxyLockWithRetry()
		if stateLockErr != nil {
			tlog("[proxy] warning: could not acquire proxy lock for state reset: %v\n", stateLockErr)
			writeProxyConfig(proxyConfig)
			return
		}
		// Reset proxy.state so the next run starts ID assignment from 0.
		// Without this, the monotonic counter resumes above whatever IDs were
		// saved from previous runs, producing confusingly high and mixed IDs
		// even when the same proxies are re-added.
		if state, err := readProxyState(); err == nil {
			state.Proxies = map[string]ProxyEntry{}
			state.NextID = 0
			if err := writeProxyState(state); err != nil {
				tlog("[proxy] warning: could not reset proxy.state: %v\n", err)
			}
		}
		stateLockRelease()
		// Also clear the URL cache and source URLs so previously-fetched free
		// proxies don't reappear after a restart. The user wants only the
		// proxies they explicitly added; URL sources must be re-added if
		// desired.
		if urlState, err := readProxyURLState(); err == nil {
			urlState.Cache = map[string]ProxyURLEntry{}
			urlState.Sources = nil
			if err := writeProxyURLState(urlState); err != nil {
				tlog("[proxy] warning: could not clear proxy_url.json cache: %v\n", err)
			}
		}
	} else {

		allKeyAddress := []string{}
		if allKeyAddressAny, ok := opts["<key_address>"]; ok {
			allKeyAddress = append(allKeyAddress, allKeyAddressAny.([]string)...)
		}

		if proxyConfig.Servers == nil {
			proxyConfig.Servers = map[string]string{}
		}

		for _, keyAddress := range allKeyAddress {
			var key string
			var address string
			i := strings.Index(keyAddress, "@")
			if 0 <= i {
				key = keyAddress[:i]
				address = keyAddress[i+1:]
			} else {
				key = ""
				address = keyAddress
			}

			if key == "" || proxyConfig.Servers[address] == key {
				delete(proxyConfig.Servers, address)
			}
		}
	}

	writeProxyConfig(proxyConfig)
}

func proxyRemoveMatch(pattern string, opts docopt.Opts) {
	autoYes, _ := opts.Bool("--yes")
	preview, _ := opts.Bool("--preview")

	proxyConfig := readProxyConfig()

	// state and urlState are optional: the provider may never have run,
	// or there may be no URL sources. Missing stores just mean fewer
	// places to search.
	var stateProxies map[string]ProxyEntry
	var stateSource string
	state, stateErr := readProxyState()
	if stateErr == nil {
		stateProxies = state.Proxies
		stateSource = state.Source
	} else {
		state = &ProxyState{}
	}
	urlState, urlErr := readProxyURLState()
	if urlErr != nil {
		urlState = &ProxyURLState{}
	}

	addrsBySource, display := collectMatchingProxies(
		pattern, proxyConfig.Servers, stateProxies, stateSource, urlState.Cache)

	if len(display) == 0 {
		fmt.Printf("no proxies matched %q — nothing to do\n", pattern)
		return
	}

	const sampleMax = 10
	fmt.Printf("%d proxies match %q:\n", len(display), pattern)
	for i, d := range display {
		if i == sampleMax {
			fmt.Printf("    ... and %d more\n", len(display)-sampleMax)
			break
		}
		fmt.Printf("    %s\n", d)
	}

	if preview {
		fmt.Println("=== PREVIEW (no changes will be made) ===")
		return
	}

	if !autoYes && !confirmPrompt(fmt.Sprintf("Remove %d proxies and exclude %q from future URL fetches?", len(display), pattern)) {
		fmt.Println("Aborted.")
		return
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		fmt.Printf("removal failed: %v\n", err)
		return
	}

	// Persist the exclude pattern so URL source refreshes cannot re-add
	// matching proxies. Re-read to avoid clobbering the cache changes
	// removeDeadProxies just wrote.
	if urlState, err := readProxyURLState(); err == nil {
		if addExcludePattern(urlState, pattern) {
			if err := writeProxyURLState(urlState); err != nil {
				fmt.Printf("warning: could not persist exclude pattern: %v\n", err)
			}
		}
	}

	fmt.Printf("Removed %d proxies matching %q. Pattern excluded from future URL fetches.\n", len(display), pattern)
	fmt.Println("The running provider will apply the change via hot reload (no restart).")
}

// proxyExclude manages the URL-fetch exclude patterns:
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
		fmt.Printf("removed exclude pattern %q — matching proxies may return on the next URL fetch\n", pattern)
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
	fmt.Printf("added exclude pattern %q — future URL fetches will skip matching hosts\n", pattern)
	fmt.Println("note: already-cached/running proxies are not removed; use 'proxy remove --match' for that")
}

// obfuscateUser returns a masked representation of a proxy user string.
func obfuscateUser(user string) string {
	if user == "" {
		return "<no user>"
	} else if len(user) < 6 {
		return "***"
	} else {
		return fmt.Sprintf("%s***%s", user[:2], user[len(user)-2:])
	}
}

// obfuscatePassword returns a masked representation of a proxy password string.
func obfuscatePassword(password string) string {
	if password == "" {
		return "<no password>"
	} else if len(password) < 6 {
		return "***"
	} else {
		return fmt.Sprintf("%s***%s", password[:2], password[len(password)-2:])
	}
}
