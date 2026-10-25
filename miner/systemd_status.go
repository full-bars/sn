package miner

// Ported from the relay's provider/systemd_status.go (3.23-fix v33 provider).
//
// The relay composes one free-form sentence from live proxy counters so
// `systemctl status` answers "is this node actually serving", and pushes it
// through sd_notify on every state change. The miner carried every input but
// the composer: the resolution-status enum (proxy_manager_hooks.go), the park
// and pause counters (proxy_manager_audit_status.go) and the sd_notify
// primitive (hotswap_unix.go). notifySystemdStatus had no non-test caller at
// all, so the line never appeared and `systemctl status` showed the unit
// description instead of the pool's state. This file is the composer and the
// publisher, and the callers are the launch path, the auth path, the resolution
// path and the proxy audit tick.
//
// The wording, the severity bands and the park/pause notes are the relay's,
// character for character, so a box running either role reports the same
// sentence for the same pool state.
//
// Non-Linux builds compile unchanged: notifySystemdStatus's windows stub
// (hotswap_windows.go) makes every send a no-op, so the publisher is inert
// there rather than gated out with a build tag.

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// Proxy liveness counters, maintained solely to populate systemd's STATUS=
// line. They are deliberately not a source of truth for anything else: no
// control-flow decision reads them, so a transient miscount degrades an
// operator-facing string and nothing more.
//
// This is the relay's split: READY=1 (notifySystemdReady) is the startup
// barrier and says nothing about health, so the unit reaches "active" promptly
// and the line answers the separate question of whether traffic can flow.
var (
	proxiesConfigured    atomic.Int64
	proxiesAuthenticated atomic.Int64
)

// proxyStatusState is the pool this process launched plus the subset of it
// whose authentication has succeeded in this process.
//
// The relay keeps a bare incrementing counter for the live half. The miner
// cannot: the same identity can authenticate again after a reconnect or a hot
// restart, and park/backoff paths have no matching "went down" event in this
// port, so an event tally drifts. A set keyed by identity is idempotent under
// repeated successes and cannot go negative, and the launch set is kept beside
// it so an identity that is no longer configured can never inflate the count
// (the relay's live>configured case, which it clamped at render time).
var proxyStatusState = struct {
	sync.Mutex
	pool map[string]struct{}
	live map[string]struct{}
}{
	pool: map[string]struct{}{},
	live: map[string]struct{}{},
}

// setProxyStatusPool publishes the launch set: the identities this process is
// attempting to serve through. The proxy manager calls it once the launch list
// is fixed, which is also when the resolution status has just settled.
//
// Live marks for identities that are no longer launched are dropped here, so a
// reload that shrinks the pool cannot leave the line reporting more
// authenticated proxies than it runs.
func setProxyStatusPool(settings []*connect.ProxySettings) {
	pool := make(map[string]struct{}, len(settings))
	for _, s := range settings {
		if s == nil || s.Address == "" {
			continue
		}
		pool[s.Key()] = struct{}{}
	}

	proxyStatusState.Lock()
	proxyStatusState.pool = pool
	for key := range proxyStatusState.live {
		if _, ok := pool[key]; !ok {
			delete(proxyStatusState.live, key)
		}
	}
	proxyStatusState.Unlock()

	refreshProxySystemdStatus()
}

// markProxyStatusLive records that an identity authenticated. An identity the
// process is not launching is ignored: the auth path also runs for settings
// read by command subprocesses, and counting those would report proxies this
// process does not run.
func markProxyStatusLive(key string) {
	if key == "" {
		return
	}
	proxyStatusState.Lock()
	if _, ok := proxyStatusState.pool[key]; !ok {
		proxyStatusState.Unlock()
		return
	}
	if _, already := proxyStatusState.live[key]; already {
		proxyStatusState.Unlock()
		return
	}
	proxyStatusState.live[key] = struct{}{}
	proxyStatusState.Unlock()

	refreshProxySystemdStatus()
}

// markProxyStatusDown records that an identity is no longer authenticated, and
// refreshes only when that changes the count.
func markProxyStatusDown(key string) {
	if key == "" {
		return
	}
	proxyStatusState.Lock()
	_, wasLive := proxyStatusState.live[key]
	delete(proxyStatusState.live, key)
	proxyStatusState.Unlock()

	if wasLive {
		refreshProxySystemdStatus()
	}
}

// refreshProxySystemdStatus recomputes the two counters from the published
// pool and live sets and pushes the line. It is the periodic caller's entry
// point (the proxy audit tick) and the auth transitions' entry point, so every
// path renders from the same counters.
func refreshProxySystemdStatus() {
	proxyStatusState.Lock()
	configured := len(proxyStatusState.pool)
	live := len(proxyStatusState.live)
	proxyStatusState.Unlock()

	proxiesConfigured.Store(int64(configured))
	proxiesAuthenticated.Store(int64(live))

	reportProxyStatusToSystemd()
}

// Status severity bands for the live/configured proxy ratio. The exact
// percentage always renders alongside the word so the scale reads
// continuously; a node with 40% live and one with 9% live are both
// "critical", and the number says how bad it is.
const (
	statusActiveBand   = 90 // >= this percent of configured proxies live: healthy steady state
	statusPartialBand  = 70 // 70-89%: first real attention
	statusDegradedBand = 50 // 50-69%: significant loss; below 50% is critical
)

// maxStatusReasonLen caps the failure reason embedded in the STATUS= line so
// the total stays well under systemd's practical limit.
const maxStatusReasonLen = 60

// systemdStatusLine renders the current state. Split out from the send so it
// can be asserted directly in tests without a NOTIFY_SOCKET.
func systemdStatusLine() string {
	live := proxiesAuthenticated.Load()
	total := proxiesConfigured.Load()

	switch {
	case total == 0:
		switch proxyResolutionStatus.Load() {
		case proxyResolutionFailed:
			reason := getProxyResolutionReason()
			if reason == "" {
				reason = "unknown error"
			}
			if len(reason) > maxStatusReasonLen {
				reason = reason[:maxStatusReasonLen] + "..."
			}
			return fmt.Sprintf("degraded: proxy source unreachable (%s), retrying%s", reason, auditPauseNote())
		case proxyResolutionEmpty:
			return "degraded: proxy source returned no usable proxies, retrying" + auditPauseNote()
		case proxyResolutionNoSource:
			// Configured deliberately to serve nothing: the direct transport
			// is off and no proxy source is configured. It is degraded, not
			// settled, because nothing is actually being provided.
			return "degraded: no proxy source configured and the direct transport is off" + auditPauseNote()
		case proxyResolutionZeroValid:
			// Direct-only: no proxy source configured, and this is a valid,
			// completed zero-proxy config. The provider is serving via the
			// native direct transport, so it is not degraded.
			return "active: providing on direct/local IP (no proxy source configured)"
		default: // proxyResolutionPending or stale OK
			return "starting: resolving proxies" + auditPauseNote()
		}
	default:
		// Parks and pauses are deliberate, not outages: the word is chosen
		// against the proxies that SHOULD be live (configured minus parked),
		// while the rendered percentage still reflects the configured set.
		// The band compares on the true ratio, not the rounded percentage, so
		// an 89.5% node reads partial, not an accidental active.
		parked := proxiesParked.Load()
		if parked > total {
			parked = total
		}
		eff := total - parked
		ratio := 0.0
		if total > 0 {
			ratio = float64(live) / float64(total)
		}
		effRatio := ratio
		if eff > 0 {
			effRatio = float64(live) / float64(eff)
		}
		pct := int(math.Round(ratio * 100))
		if pct > 100 {
			// live > configured happens transiently when a reload shrinks
			// the desired set; a >100% figure would be nonsense in
			// systemctl status.
			pct = 100
		}
		word := "critical"
		switch {
		case effRatio*100 >= statusActiveBand:
			word = "active"
		case effRatio*100 >= statusPartialBand:
			word = "partial"
		case effRatio*100 >= statusDegradedBand:
			word = "degraded"
		}
		line := fmt.Sprintf("%s: %d/%d proxies authenticated (%d%%)", word, live, total, pct)
		// The percentage and the park/pause notes always render, even at
		// zero live: an operator with every proxy parked still sees why.
		line += parkAndPauseNote(total)
		if word == "degraded" || word == "critical" {
			return line + ", retrying"
		}
		return line
	}
}

// parkAndPauseNote appends the proxy-audit notes: proxies the engine is
// holding out are expected to be down, and a paused audit parks nothing.
func parkAndPauseNote(total int64) string {
	parked := proxiesParked.Load()
	if parked > total {
		parked = total
	}
	var b strings.Builder
	if parked > 0 {
		fmt.Fprintf(&b, ", %d parked by proxy audit", parked)
	}
	b.WriteString(auditPauseNote())
	return b.String()
}

// auditPauseNote returns the pause suffix for a status line, or "" when the
// proxy audit engine is able to work. The outage branches call this so a
// paused audit is visible even while the line reads degraded or starting.
func auditPauseNote() string {
	if proxyAuditPaused.Load() {
		return "; proxy audit paused (paid proxy list unreadable)"
	}
	return ""
}

// proxyStatusSendMu serializes compose and send so two transitions on
// different goroutines (the auth path and a reload both report) cannot
// interleave: the line is composed from the live counters, and a store landing
// between another caller's compose and send would put a stale reading on the
// wire until the next event or audit tick.
var proxyStatusSendMu sync.Mutex

// reportProxyStatusToSystemd pushes the current line to systemd. Errors are
// discarded on purpose: this is cosmetic reporting, and a provider must never
// fail or stall because a status datagram did not land.
func reportProxyStatusToSystemd() {
	proxyStatusSendMu.Lock()
	defer proxyStatusSendMu.Unlock()
	_ = notifySystemdStatus(systemdStatusLine())
}

// announceStartupComplete tells systemd the unit has finished starting
// (READY=1). A Type=notify unit is held in "activating" until it arrives and
// is killed when TimeoutStartSec (90 s by default) runs out, so the provider
// must send it; 3.23-fix sends it at the same point (provider/main.go).
//
// READY is the startup barrier, not a health signal. It is sent once the
// process is self-managing: control state loaded, queued overrides drained
// and the control socket bound. Everything after that is a retry loop that
// supervises itself, so no proxy authentication, no control-plane contact and
// no serving may gate it: proxy auth backs off for hours against a
// rate-limited API, and a start gated on it would be killed while retrying
// correctly. Health travels on STATUS= (notifySystemdStatus).
//
// A failed send is logged, never fatal: without a notify socket the call is
// already a no-op, and a unit that is not Type=notify ignores the message.
var announceStartupComplete = func() {
	if err := notifySystemdReady(); err != nil {
		tlog("[systemd] READY=1 notify failed: %s\n", err)
	}
}
