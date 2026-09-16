package provider

// DESIGN ADAPTATION: Stubs for symbols referenced by provide() but not
// yet ported from main.go. Will be removed when main.go is ported.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// provideStartTime records when provide() began; used for uptime display
// and warmup pacing.
var provideStartTime time.Time

// proxyLaunchCount tracks how many proxy goroutines have passed the stagger
// delay and entered provideWithProxy. Used by paceMonitor for progress logging.
var proxyLaunchCount atomic.Int64

// ApplyAutoTuning was removed from connect in v2026. The fork applied
// automatic buffer sizing based on system resources; stub returns true
// (auto-tuning "applied" — no-op for now).
func ApplyAutoTuning(_ *connect.ClientSettings, _ *connect.LocalUserNatSettings) bool {
	return true
}

// TriggerPulse wakes all stalled transports and proxies so they retry
// connections. Stub: the new connect handles this internally.
func TriggerPulse() {}

// EnableProfiling enables pprof and custom metric endpoints on addr.
// Stub: not yet ported.
func EnableProfiling(_ string) error {
	return nil
}

// sessionFilesAllowlist mirrors internal/urnettools/session_cmds.go's allowlist
// so only canonical session files are promoted from staging.
var sessionFilesAllowlist = map[string]bool{
	"jwt":              true,
	"client_id":        true,
	"client_secret":    true,
	"provider.key":     true,
	"provider.cert":    true,
	".provider.key":    true,
	".provider.cert":   true,
	"node_name":        true,
	"relay_jwt":        true,
	"provider.json":    true,
	"relay_client_id":  true,
	"relay_secret":     true,
	"relay_client_key": true,
}

// isSessionFile reports whether name is in the session allowlist.
func isSessionFile(name string) bool {
	return sessionFilesAllowlist[name]
}

// applyStagedSession atomically swaps in identity and proxy-list files
// from ~/.urnetwork/.session-staging/ if a .session-pending marker exists.
// Real implementation ported from fork main.go:723.
func applyStagedSession() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	stagingDir := filepath.Join(urNetworkDir, ".session-staging")
	pending := filepath.Join(urNetworkDir, ".session-pending")

	if _, err := os.Stat(pending); os.IsNotExist(err) {
		return
	}

	tlog("[session] applying staged session from %s\n", stagingDir)

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		tlog("[session] could not read staging dir: %v\n", err)
		return
	}
	for _, e := range entries {
		if !isSessionFile(e.Name()) {
			continue
		}
		if info, err := e.Info(); err != nil || !info.Mode().IsRegular() {
			tlog("[session] skip staged %s: not a regular file\n", e.Name())
			continue
		}
		src := filepath.Join(stagingDir, e.Name())
		dst := filepath.Join(urNetworkDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			tlog("[session] rename %s -> %s failed: %v\n", src, dst, err)
		}
	}
	os.RemoveAll(stagingDir)
	os.Remove(pending)
	tlog("[session] staged session applied\n")
}

// paceMonitor tracks proxy warmup progress. Stub: will be fully ported.
func paceMonitor(_ context.Context) {}
