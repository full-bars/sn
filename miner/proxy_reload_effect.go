package miner

import "sync/atomic"

// proxyReloadWatcherActive records whether THIS process runs the reload watcher
// that consumes proxy.reload. The watcher sets it when it starts and clears it
// when it stops.
//
// Operator-facing messages are decided by this, not by a build-time constant: a
// binary without the watcher never sets it, a binary with it does, and no flag
// has to be flipped or can be forgotten. That is the lesson of the port — the
// message and the mechanism cannot disagree if the message asks the mechanism.
var proxyReloadWatcherActive atomic.Bool

// proxyReloadWatcherProbe asks the running provider whether it has a reload
// watcher. It is a seam so tests can answer without a socket.
//
// It reports two things: whether a provider is running at all, and whether that
// provider applies reloads live. "Not running" and "running without a watcher"
// are different answers to the operator, so they are not collapsed.
var proxyReloadWatcherProbe = func() (live bool, running bool) {
	resp, err := dialControlSocket(controlRequest{Cmd: "status"})
	if err != nil {
		return false, false
	}
	return resp.ReloadWatcher, true
}

// proxyChangeEffectNotice is the operator-facing answer to "did my proxy change
// take effect?". It reports what the running provider can actually do.
func proxyChangeEffectNotice() string {
	live, running := proxyReloadWatcherProbe()
	switch {
	case !running:
		return "No provider is running; this applies when it starts."
	case live:
		return "The running provider applies proxy changes on its own (no restart)."
	default:
		return "The running provider applies this on its next restart (run: urnet-tools restart)."
	}
}

// proxyTrimEffectNotice is the trim-specific wording. Setting the cap is read
// on every trim path, so that half is immediate either way; what waits is the
// shed of the proxies above the new cap.
func proxyTrimEffectNotice() string {
	live, running := proxyReloadWatcherProbe()
	switch {
	case !running:
		return "the shed of excess proxies happens when the provider starts"
	case live:
		return "shedding the worst-graded above it now"
	default:
		return "the shed of excess proxies applies on the next provider restart (run: urnet-tools restart)"
	}
}

// proxyRefreshEffectNotice is the refresh-specific wording: a refresh rewrites
// the desired set, so whether the RUNNING pool changed depends entirely on the
// provider's watcher.
func proxyRefreshEffectNotice() string {
	live, running := proxyReloadWatcherProbe()
	switch {
	case !running:
		return "Proxy refresh requested. No provider is running; this applies when it starts."
	case live:
		return "Proxy state updated. The running provider applies proxy changes on its own (no restart)."
	default:
		return "Proxy refresh requested. The running provider applies this on its next restart (run: urnet-tools restart)."
	}
}
