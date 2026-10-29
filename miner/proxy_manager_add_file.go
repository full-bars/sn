package miner

import (
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// proxy_manager_add_file.go ports the Workflow A half of the v33 proxyAdd
// command: when proxy.state selects an external proxy file (C-10), additions
// must land in THAT file - a file-backed reload re-reads it and discards the
// internal config - so `proxy add` delegates here. Paste hands its normalized
// batch to `proxy add --proxy_file=<tmp> -f`, so this routing is also what
// makes a paste take effect for a file-backed provider.

// proxyAddFileBacked appends new proxy addresses to a Workflow A source file
// (the --proxy_file=... the running provider reads on every reload) instead of
// the internal config, which a file-backed reload discards. Lines already
// present in the file are skipped (dedup by trimmed line, matching add-time
// semantics) and a line whose address and user match a stored one replaces it
// rather than sitting beside it; the file is written atomically, comments and blank lines are
// preserved, and a reload trigger is fired so the additions take effect
// without a restart.
func proxyAddFileBacked(sourcePath string, opts docopt.Opts) error {
	release, err := acquireProxyLockWithRetry()
	if err != nil {
		return fmt.Errorf("could not acquire proxy lock: %w", err)
	}
	defer release()

	allKeyAddress := proxyAddCollectAddresses(opts)
	auths := readProxyConfig().Auths
	entries := make([]string, 0, len(allKeyAddress))
	for _, raw := range allKeyAddress {
		entry, err := checkedProxyAddEntry(raw, auths)
		if err != nil {
			return err
		}
		if entry.key != "" {
			auth, ok := auths[entry.key]
			if !ok || auth == nil {
				return fmt.Errorf("unregistered key prefix cannot be used in a file-backed source")
			}
			if auth.User == "" || auth.Password == "" {
				return fmt.Errorf("registered key has incomplete credentials")
			}
			encoded := canonicalProxyKey(entry.address, auth.User, auth.Password)
			address, user, password := parseProxyAddress(encoded)
			if address != entry.address || user != auth.User || password != auth.Password {
				return fmt.Errorf("registered credentials cannot be represented unambiguously in a file-backed source")
			}
			entries = append(entries, encoded)
		} else {
			entries = append(entries, strings.TrimSpace(raw))
		}
	}

	// Keep an existing source's permissions when replacing it. New sources
	// contain credentials and must not be made world-readable.
	perm := os.FileMode(0600)
	var owner *fileOwner
	if info, err := os.Stat(sourcePath); err == nil {
		perm = info.Mode().Perm()
		owner, _ = sourceOwnerOf(info)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("could not stat source: %w", err)
	}

	// Read the existing file content, preserving comments and blank lines.
	// Proxy lines are tracked by identity (address plus user): the password is
	// not part of it, so a line with a new password is the same proxy with a
	// changed credential and replaces the stored line.
	var lines []proxyFileLine
	if b, err := os.ReadFile(sourcePath); err == nil {
		s := strings.TrimSuffix(string(b), "\n")
		s = strings.TrimSuffix(s, "\r")
		if len(s) > 0 {
			for _, line := range strings.Split(s, "\n") {
				line = strings.TrimSuffix(line, "\r")
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					lines = append(lines, proxyFileLine{text: line})
					continue
				}
				lines = append(lines, proxyFileLine{text: line, proxy: trimmed, id: proxyFileLineIdentity(trimmed)})
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("could not read source: %w", err)
	}

	added := 0
	for _, trimmed := range entries {
		var changed bool
		lines, changed = upsertProxyFileLine(lines, trimmed)
		address, user, password := parseProxyAddress(trimmed)
		if !changed {
			fmt.Printf("server %s already present in %s\n", redactProxySource(trimmed), sourcePath)
			continue
		}
		fmt.Printf("added server %s\n", redactProxySource(canonicalProxyKey(address, user, password)))
		added++
	}

	if added == 0 {
		fmt.Println("no new proxies to add")
		return nil
	}

	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	content := strings.Join(out, "\n") + "\n"
	if err := atomicWriteFileOwned(sourcePath, []byte(content), perm, owner); err != nil {
		return fmt.Errorf("could not write source: %w", err)
	}
	fmt.Printf("appended %d proxy(ies) to %s\n", added, sourcePath)

	// Apply immediately without a restart: a file-backed reload re-reads the
	// source file, so a reload trigger is all that is needed (paste writes the
	// same trigger after its add).
	reloadPath, err := proxyReloadPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
	} else if err := writeReloadTrigger(reloadPath); err != nil {
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
	}
	return nil
}

// proxyAddCollectAddresses gathers the addresses `proxy add` was asked for:
// positional <key_address> entries plus the lines of --proxy_file. A leading ~
// is expanded (the paste hand-off passes a temp path; operators pass files
// like ~/proxies.txt).
func proxyAddCollectAddresses(opts docopt.Opts) []string {
	allKeyAddress := []string{}
	if allKeyAddressAny, ok := opts["<key_address>"]; ok {
		allKeyAddress = append(allKeyAddress, allKeyAddressAny.([]string)...)
	}
	if proxyPath, _ := opts.String("--proxy_file"); proxyPath != "" {
		b, err := os.ReadFile(expandPath(proxyPath))
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

// proxyFileLine is one line of the source file. proxy is the trimmed proxy
// entry, empty for comments and blank lines; id is its identity, empty when the
// entry carries no user.
type proxyFileLine struct {
	text  string
	proxy string
	id    string
}

// proxyFileLineIdentity is the identity key (address plus user) of a proxy
// line, or "" when the line has no user to tell accounts apart.
func proxyFileLineIdentity(entry string) string {
	address, user, _ := parseProxyAddress(entry)
	if user == "" {
		return ""
	}
	return (&connect.ProxySettings{Address: address, Auth: &proxy.Auth{User: user}}).Key()
}

// upsertProxyFileLine stores entry in lines and reports whether anything
// changed. An entry whose identity is already stored replaces that line in
// place and drops any other lines of the same identity; an identical single
// line is left alone. Entries without a user are matched by exact text.
func upsertProxyFileLine(lines []proxyFileLine, entry string) ([]proxyFileLine, bool) {
	id := proxyFileLineIdentity(entry)
	var matches []int
	for i, l := range lines {
		if l.proxy == "" {
			continue
		}
		if (id != "" && l.id == id) || (id == "" && l.proxy == entry) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		return append(lines, proxyFileLine{text: entry, proxy: entry, id: id}), true
	}
	if len(matches) == 1 && lines[matches[0]].proxy == entry {
		return lines, false
	}
	lines[matches[0]] = proxyFileLine{text: entry, proxy: entry, id: id}
	drop := map[int]bool{}
	for _, i := range matches[1:] {
		drop[i] = true
	}
	kept := lines[:0]
	for i, l := range lines {
		if !drop[i] {
			kept = append(kept, l)
		}
	}
	return kept, true
}
