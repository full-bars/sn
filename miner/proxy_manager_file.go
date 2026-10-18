package miner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// This file ports the small external-proxy-file helpers of the 3.23-fix v33
// provider (main.go) that the URL pipeline and the dead-proxy removal path need
// on top of the miner's existing internal-config reader (readProxyConfig /
// readProxySettings in run.go).

// internalServerSettings resolves one internal-config server entry
// (Servers[proxyAddress] = authKey) to its ProxySettings: credentials embedded
// in the address form first, then overridden by the Auths entry named by
// authKey. Shared by the reader and the removal path so both derive the same
// identity (ProxySettings.Key()) for an entry.
func internalServerSettings(proxyConfig *ProxyConfig, proxyAddress, authKey string) *connect.ProxySettings {
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
		if proxyAuth, ok := proxyConfig.Auths[authKey]; ok {
			proxySettings.Auth = &proxy.Auth{
				User:     proxyAuth.User,
				Password: proxyAuth.Password,
			}
		}
	}
	return proxySettings
}

// expandPath expands a leading ~/ or ~\ to the user's home directory
// (v33 main.go expandPath). Any other path is returned unchanged.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// readProxySettingsFromFile reads proxy settings directly from an external file
// where each non-blank, non-comment line is ip:port:user:pass. Entries missing
// credentials are rejected. Returns an error only if the file itself cannot be
// read; malformed individual lines are skipped with a warning.
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

// removeKeysFromFile removes the lines whose identity key (address+user) is in
// keys from an external proxy file, atomically.
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

// formatDuration renders a duration at minute granularity for log lines
// (v33 formatDuration): "7m" under an hour, "3h 20m" above it.
func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
