package provider

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
)

// parseProxyAddress splits a proxy address into address, user, password.
func parseProxyAddress(proxyAddress string) (address string, user string, password string) {
	r := regexp.MustCompile("^(.*:\\d*):([^:]*):([^:]*)$")
	groups := r.FindStringSubmatch(proxyAddress)
	if groups != nil {
		address = groups[1]
		user = groups[2]
		password = groups[3]
		return
	}
	// assume host:port
	address = proxyAddress
	return
}

// resolveDuration returns the --flag value if set and parseable, else the
// env var if set and parseable, else def.
func resolveDuration(opts docopt.Opts, flag, envVar string, def time.Duration) time.Duration {
	if v, _ := opts.String(flag); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		tlog("[proxy][url] warning: invalid duration %q for %s; using default %s\n", v, flag, def)
		return def
	}
	if v := os.Getenv(envVar); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		tlog("[proxy][url] warning: invalid duration %q for %s; using default %s\n", v, envVar, def)
	}
	return def
}

// resolveInt is resolveDuration's integer counterpart.
func resolveInt(opts docopt.Opts, flag, envVar string, def int) int {
	if v, _ := opts.String(flag); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		tlog("[proxy][url] warning: invalid integer %q for %s; using default %d\n", v, flag, def)
		return def
	}
	if v := os.Getenv(envVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		tlog("[proxy][url] warning: invalid integer %q for %s; using default %d\n", v, envVar, def)
	}
	return def
}

// resolveString is resolveDuration's plain-string counterpart.
func resolveString(opts docopt.Opts, flag, envVar, def string) string {
	if v, _ := opts.String(flag); v != "" {
		return v
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return def
}

// resolveProxyURLs collects --proxy_url flag values, PROXY_URL env var
// values (comma-separated), and persisted sources from proxy_url.json,
// deduplicated, in that priority order.
func resolveProxyURLs(opts docopt.Opts) []string {
	var urls []string

	for _, flag := range []string{"--proxy_url", "--url", "--URL"} {
		if v, ok := opts[flag]; ok && v != nil {
			switch vv := v.(type) {
			case []string:
				urls = append(urls, vv...)
			case string:
				if vv != "" {
					urls = append(urls, vv)
				}
			}
		}
	}

	if envURLs := os.Getenv("PROXY_URL"); envURLs != "" {
		for _, u := range strings.Split(envURLs, ",") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
	}

	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		urls = append(urls, urlState.Sources...)
	}

	seen := map[string]bool{}
	deduped := make([]string, 0, len(urls))
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			deduped = append(deduped, u)
		}
	}
	return deduped
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		resp := scanner.Text()
		return strings.ToLower(strings.TrimSpace(resp)) == "y"
	}
	return false
}
