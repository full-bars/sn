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
//     failure warnings). The grade deltas and the fetcher/reaper countdown
//     state below are no longer seams: the P1(c) grade summary reads them.
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

// Proxy resolution statuses for the miner status surface and the direct-only
// H3 default (P2 consumes these). The values are the 3.23-fix v33 provider's
// (provider/systemd_status.go): the zero value is "no resolution attempted
// yet", which is what keeps the direct-only H3 default off during startup.
const (
	proxyResolutionPending   int32 = 0 // no resolution attempted yet
	proxyResolutionFailed    int32 = 1 // attempted, source unreachable
	proxyResolutionEmpty     int32 = 2 // a proxy source was configured but yielded zero usable proxies
	proxyResolutionOK        int32 = 3 // resolved with at least one proxy
	proxyResolutionZeroValid int32 = 4 // no proxy source configured; direct-only is a valid zero config
	proxyResolutionNoSource  int32 = 5 // no proxy source configured and the direct transport is off: nothing is served
)

var (
	proxyResolutionStatus atomic.Int32
	proxyResolutionReason atomic.Value // string
)

// currentProxyResolution exposes the settled proxy-resolution state to the
// transport default logic (direct-only H3 opt-in) and the status surface.
func currentProxyResolution() int32 {
	return proxyResolutionStatus.Load()
}

// setProxyResolutionStatus records the last resolution outcome.
//
// Direct-only H3 defaults depend on this status becoming ZeroValid, so the
// re-apply belongs here — the one choke point every path goes through — and
// fires only on transitions into or out of it. Without the guard it would
// re-run on every settled reload (the periodic reload loop), spamming the log
// with "applied h3=on" once a minute forever.
func setProxyResolutionStatus(status int32, reason string) {
	prev := proxyResolutionStatus.Load()
	proxyResolutionStatus.Store(status)
	proxyResolutionReason.Store(reason)
	if (status == proxyResolutionZeroValid) != (prev == proxyResolutionZeroValid) {
		applyDirectOnlyH3Defaults()
	}
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
	importantLogf("[proxy][url] WARNING: proxy source %s: %s — retrying\n", label, reason)
}

// URL fetcher / reaper countdown state for the grade summary. The fetcher
// publishes these so the summary can show the next window; the summary renders
// them (proxy_manager_grade_summary.go).
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

// getNextFetchProbeAt returns the published next fetch window start (zero when
// none is scheduled).
func getNextFetchProbeAt() time.Time {
	if ns := nextFetchProbe.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// setNextGradeRefreshAt publishes the next reaper window start (zero clears).
func setNextGradeRefreshAt(t time.Time) {
	if t.IsZero() {
		nextGradeRefresh.Store(0)
		return
	}
	nextGradeRefresh.Store(t.UnixNano())
}

// getNextGradeRefreshAt returns the published next reaper window start (zero
// when none is scheduled).
func getNextGradeRefreshAt() time.Time {
	if ns := nextGradeRefresh.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}
