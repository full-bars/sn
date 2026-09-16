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

// paceMonitor logs real-time warmup progress every 30s and flips
// proxyWarmupDone once the initial file-proxy ramp is judged complete.
// Ported from fork main.go's paceMonitor: connect.ProxyHealthSnapshot()/
// connect.ProxyHealthCount() became the package-local ProxyHealthSnapshot()/
// ProxyHealthCount() (proxy_health.go), which are fully ported and real.
//
// Without this, proxyWarmupDone.Store(true) is never called: URL-sourced
// proxies (proxy_url_source.go:1118) and hot-reloaded proxies
// (proxy_reload.go:648) both gate on proxyWarmupDone and would wait forever.
func paceMonitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		up, _, _, _, connecting := ProxyHealthSnapshot()
		total := ProxyHealthCount()
		if total < 5 {
			tlog("🔥 [pace] ✓ warmup: %d up, %d total (< 5) — done\n", up, total)
			proxyWarmupDone.Store(true)
			signalProxyReloadAfterWarmup()
			return
		}
		pct := float64(up) * 100 / float64(total)
		connectingN := len(connecting)
		elapsed := time.Since(provideStartTime)
		if elapsed > 60*time.Minute {
			tlog("🔥 [pace] warmup: %d/%d up (%.0f%%), %d connecting — forced done after 60m\n",
				up, total, pct, connectingN)
			proxyWarmupDone.Store(true)
			signalProxyReloadAfterWarmup()
			return
		}
		if pct < 50 && connectingN > 10 {
			tlog("🔥 [pace] ⚠ warmup: %d/%d up (%.0f%%), %d connecting, %d done\n",
				up, total, pct, connectingN, total-up-connectingN)
		} else if pct > 90 && connectingN < 5 {
			tlog("🔥 [pace] ✓ warmup: %d/%d up (%.0f%%), %d connecting — done\n",
				up, total, pct, connectingN)
			proxyWarmupDone.Store(true)
			signalProxyReloadAfterWarmup()
			return
		} else {
			tlog("🔥 [pace] warmup: %d/%d up (%.0f%%), %d connecting\n",
				up, total, pct, connectingN)
		}
	}
}

// signalProxyReloadAfterWarmup nudges the URL-sourced proxy loop to pick up
// now that file-proxy warmup has completed and it is no longer held back by
// proxyWarmupDone.
func signalProxyReloadAfterWarmup() {
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
		}
	}
}
