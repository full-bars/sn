package miner

import (
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
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
// semantics); the file is written atomically, comments and blank lines are
// preserved, and a reload trigger is fired so the additions take effect
// without a restart.
func proxyAddFileBacked(sourcePath string, opts docopt.Opts) {
	release, err := acquireProxyLockWithRetry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy add: could not acquire proxy lock: %v\n", err)
		return
	}
	defer release()

	allKeyAddress := proxyAddCollectAddresses(opts)

	// Read the existing file content, preserving comments/blank lines, and
	// track which proxy lines are already present.
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
		if trimmed == "" {
			continue
		}
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

	// Apply immediately without a restart: a file-backed reload re-reads the
	// source file, so a reload trigger is all that is needed (paste writes the
	// same trigger after its add).
	reloadPath, err := proxyReloadPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
	} else if err := writeReloadTrigger(reloadPath); err != nil {
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
	}
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
