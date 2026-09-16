package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// network.go — additional network helpers ported from the fork.
// Functions already present in sn.go (networkConfig, networkConfigPath,
// readNetworkConfig, validateApiUrl, resolveApiUrl) are NOT duplicated here.

// validateConnectUrl requires a ws or wss URL.
func validateConnectUrl(rawUrl string) error {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return fmt.Errorf("invalid connect_url %q: %w", rawUrl, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("invalid connect_url %q: scheme must be ws or wss, got %q", rawUrl, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid connect_url %q: missing host", rawUrl)
	}
	return nil
}

// writeNetworkConfig validates apiUrl (http/https) and connectUrl
// (ws/wss), then writes them to ~/.urnetwork/network.json, creating the
// ~/.urnetwork directory if needed. Nothing is written if validation
// fails.
func writeNetworkConfig(apiUrl, connectUrl string) error {
	if err := validateApiUrl(apiUrl); err != nil {
		return err
	}
	if err := validateConnectUrl(connectUrl); err != nil {
		return err
	}
	p, err := networkConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(networkConfig{ApiUrl: apiUrl, ConnectUrl: connectUrl}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0600)
}

// resetNetworkConfig removes ~/.urnetwork/network.json. Removing a
// nonexistent file is not an error.
func resetNetworkConfig() error {
	p, err := networkConfigPath()
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// resolveConnectUrl implements the 3-tier precedence for the connect
// URL: --connect_url flag > saved network config > DefaultConnectUrl.
func resolveConnectUrl(opts map[string]interface{}) (string, error) {
	if connectUrl, ok := opts["--connect_url"].(string); ok && connectUrl != "" {
		return connectUrl, nil
	}
	cfg, ok, err := readNetworkConfig()
	if err != nil {
		return "", err
	}
	if ok {
		return cfg.ConnectUrl, nil
	}
	return DefaultConnectUrl, nil
}

// apiProbeHostPort extracts the API probe host:port from an API URL,
// falling back to defaultAPIHost/defaultAPIPort for URLs that don't
// parse into a host. Shared by provide()'s reachability-probe setup and
// `proxy add-source`'s one-shot fetch so both follow the chosen network.
func apiProbeHostPort(apiUrl string) (string, uint16) {
	if apiUrl == "" {
		return defaultAPIHost, uint16(defaultAPIPort)
	}
	u, err := url.Parse(apiUrl)
	if err != nil || u.Hostname() == "" {
		return defaultAPIHost, uint16(defaultAPIPort)
	}
	apiProbeHost := u.Hostname()
	apiProbePort := uint16(defaultAPIPort)
	if p := u.Port(); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port >= 1 && port <= 65535 {
			apiProbePort = uint16(port)
		}
	}
	return apiProbeHost, apiProbePort
}

// resolveAPIProbeHostPort resolves the API probe endpoint from the chosen
// network (saved network config > default), for call sites that run without
// docopt opts (e.g. the `proxy add-source` one-shot fetch).
func resolveAPIProbeHostPort() (string, uint16) {
	apiUrl := DefaultApiUrl
	if cfg, ok, err := readNetworkConfig(); err == nil && ok && cfg.ApiUrl != "" {
		apiUrl = cfg.ApiUrl
	}
	return apiProbeHostPort(apiUrl)
}
