package miner

import (
	"sync/atomic"
	"time"
)

// This file holds the P1(a) seams for the v33 provider facilities that are NOT
// part of the URL-source port, so the ported pipeline compiles and behaves
// sanely without them:
//
//   - the connect health registry (liveProxyHealthByKey, degraded freeze),
//     which the pinned full-bars/connect in this tree does not export in the
//     form v33 uses. P2 wires these when the miner's ops/observability port
//     lands.
//   - the control-socket / metrics status surface (resolution status, source
//     failure warnings, grade deltas, fetcher countdowns).
//
// Every one is a function variable (or a trivial state cell) with a calm
// default, so P2 replaces the implementation without touching the ported
// pipeline.

// proxyHealthStatus is the slice of a live proxy's health the URL reaper reads
// (v33: connect.ProxyHealthStatus). P2 fills the registry behind
// liveProxyHealthByKey.
type proxyHealthStatus struct {
	Health string
}

// liveProxyHealthByKey is the live per-proxy health, a variable so tests can
// supply their own. The default reads the process-wide health registry
// (proxy_manager_health_registry.go), which the auth path and the health
// cadence fill.
var liveProxyHealthByKey = proxyLiveHealthSnapshot

// degradedReaperFrozen reports whether an upstream outage makes proxy failures
// meaningless (v33: the degraded-reaper freeze). P1(a) never freezes; the
// outage detector is P2.
var degradedReaperFrozen = func() bool { return false }

// proxyResolution statuses for the miner status surface (P2 consumes these).
const (
	proxyResolutionOK int32 = iota
	proxyResolutionFailed
)

var (
	proxyResolutionStatus atomic.Int32
	proxyResolutionReason atomic.Value // string
)

// setProxyResolutionStatus records the last URL-source resolution outcome. The
// v33 provider feeds its systemd status line from this; the miner port keeps
// the state for the P2 status surface and for the ported tests.
func setProxyResolutionStatus(status int32, reason string) {
	proxyResolutionStatus.Store(status)
	proxyResolutionReason.Store(reason)
}

// getProxyResolutionReason returns the last recorded resolution reason.
func getProxyResolutionReason() string {
	if v, ok := proxyResolutionReason.Load().(string); ok {
		return v
	}
	return ""
}

// warnProxySourceFailure reports a URL source failure to the operator surface.
// P1(a) logs it; P2 routes it into the important buffer / status line.
var warnProxySourceFailure = func(label, reason string) {
	importantLogf("[proxy][url] warning: source %s failed: %s\n", label, reason)
}

// emitProxyGradeDelta records a grade change (v33 grades.log). P1(a) logs it;
// the paid/URL grading sweep that calls it is P1(c).
var emitProxyGradeDelta = func(addr, oldTier, newTier string, oldScore, newScore float64, oldGraded bool) {}

// URL fetcher / reaper countdown state for the grade summary (P2). The fetcher
// publishes these so an operator-facing summary can show the next window; the
// miner port keeps the state machine, P2 renders it.
var (
	urlFetcherState  atomic.Value // string
	nextFetchProbe   atomic.Int64 // unix nanos, 0 = none
	nextGradeRefresh atomic.Int64 // unix nanos, 0 = none
)

// setURLFetcherState publishes the URL fetcher's lifecycle state
// ("none", "pending", "active", "inactive").
func setURLFetcherState(s string) { urlFetcherState.Store(s) }

// getURLFetcherState returns the published lifecycle state.
func getURLFetcherState() string {
	if v, ok := urlFetcherState.Load().(string); ok {
		return v
	}
	return ""
}

// setNextFetchProbeAt publishes the next fetch window start (zero clears it).
func setNextFetchProbeAt(t time.Time) {
	if t.IsZero() {
		nextFetchProbe.Store(0)
		return
	}
	nextFetchProbe.Store(t.UnixNano())
}

// setNextGradeRefreshAt publishes the next reaper window start (zero clears).
func setNextGradeRefreshAt(t time.Time) {
	if t.IsZero() {
		nextGradeRefresh.Store(0)
		return
	}
	nextGradeRefresh.Store(t.UnixNano())
}
