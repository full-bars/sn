package provider

// proxy_leg.go: evidence that a proxy cannot carry traffic to the platform.
//
// A proxy can pass its SOCKS login and still be unable to reach the platform:
// the platform dial through it times out inside the proxy's own connect step.
// Those failures used to surface only as the flattened "[t]auth error ... http
// request attempts exhausted", which RecordProxyAuthFailure files under
// timeouts. Nothing sheds on timeouts, and the auth-failure counters drive the
// wrong remedies (renewing a good identity, deleting a stored one), so such a
// proxy parked its client in a one-minute retry loop indefinitely while status
// still counted it as authenticated.
//
// The connect library reports the typed result of every control dial to a
// logger that implements connect.ControlDialObserver, and each proxy has its
// own logger, so the failure can be attributed to the proxy. It is counted
// here as its own category.
//
// What acts on it is a SHADOW decision for now: after a long streak of
// proxy-leg failures with no platform dial succeeding through the proxy, while
// the platform was reached through another path recently, the runner logs that
// it would shed the proxy and counts it. Nothing is cancelled. The second
// condition is the outage guard: when the platform itself is down every proxy
// shows the same failure, and none of them is to blame.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// proxyLegShedStreak is how many proxy-leg failures in a row, with no
	// platform dial succeeding through the proxy, make it a shed candidate.
	proxyLegShedStreak = 5
	// proxyLegShedMinAge is how long the streak must have lasted. The client
	// loop retries about once a minute, so this is several attempts apart,
	// not one burst.
	proxyLegShedMinAge = 10 * time.Minute
	// platformReachedWindow is how recent a successful platform dial through
	// any proxy or the direct path must be for the platform to count as up.
	platformReachedWindow = 5 * time.Minute
	// proxyLegShadowInterval is how often the shadow runner evaluates.
	proxyLegShadowInterval = time.Minute
)

// lastPlatformDialOK is the unix nano time of the latest platform dial that
// succeeded through any identity, direct or proxied.
var lastPlatformDialOK atomic.Int64

// proxyLegShadowShedTotal counts the proxies the shadow runner flagged since
// the process started. Nothing was shed.
var proxyLegShadowShedTotal atomic.Int64

// isProxyLegError reports whether err is a failure inside the proxy's own
// connect step: the socks dialer returns a *net.OpError whose Op names the
// socks command and whose Source is the proxy. A local cancellation is not
// evidence about the proxy.
func isProxyLegError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && strings.HasPrefix(opErr.Op, "socks")
}

// RecordProxyLegFailure counts one proxy-leg failure for the proxy at index.
// It does not touch the auth or timeout counters: it is its own category.
func RecordProxyLegFailure(index int, err error) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok {
		return
	}
	now := time.Now()
	h.failures.ProxyLegFailures.Add(1)
	if h.legStreak == 0 {
		h.legStreakSince = now
	}
	h.legStreak++
	if err != nil {
		h.lastError = err.Error()
		h.lastErrorAt = now
	}
}

// recordPlatformDialOK notes a platform dial that succeeded through the proxy
// at index (index < 0 for none): the proxy carries traffic, so its streak
// ends, and the platform counts as reached.
func recordPlatformDialOK(index int) {
	lastPlatformDialOK.Store(time.Now().UnixNano())
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		h.legStreak = 0
		h.legStreakSince = time.Time{}
		h.legWouldShed = false
	}
}

func platformReachedRecently(now time.Time) bool {
	last := lastPlatformDialOK.Load()
	return last != 0 && now.Sub(time.Unix(0, last)) < platformReachedWindow
}

// proxyLegCandidate is a proxy the shadow decision would shed.
type proxyLegCandidate struct {
	Index   int
	Address string
	Streak  int
	Age     time.Duration
}

// proxyLegShedCandidates lists the proxies that meet the shed condition at
// now and have not been flagged already. It does not flag them.
func proxyLegShedCandidates(now time.Time) []proxyLegCandidate {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if !platformReachedRecently(now) {
		// Outage guard: with no platform dial succeeding anywhere, a proxy's
		// failures say nothing about the proxy.
		return nil
	}
	var out []proxyLegCandidate
	for index, h := range proxyHealthByIndex {
		if h.legWouldShed || h.legStreak < proxyLegShedStreak || h.legStreakSince.IsZero() {
			continue
		}
		age := now.Sub(h.legStreakSince)
		if age < proxyLegShedMinAge {
			continue
		}
		out = append(out, proxyLegCandidate{Index: index, Address: h.address, Streak: h.legStreak, Age: age})
	}
	return out
}

// flagProxyLegWouldShed marks the proxy so it is reported once per streak. It
// reports whether this call set the mark.
func flagProxyLegWouldShed(index int) bool {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok || h.legWouldShed {
		return false
	}
	h.legWouldShed = true
	return true
}

// proxyLegTotals is the failure sum over the registered proxies and how many
// are flagged right now, for the scrape.
func proxyLegTotals() (failures int64, flagged int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	for _, h := range proxyHealthByIndex {
		failures += h.failures.ProxyLegFailures.Load()
		if h.legWouldShed {
			flagged++
		}
	}
	return failures, flagged
}

// evaluateProxyLegShadow reports each new candidate once and counts it.
func evaluateProxyLegShadow(now time.Time) int {
	n := 0
	for _, c := range proxyLegShedCandidates(now) {
		if !flagProxyLegWouldShed(c.Index) {
			continue
		}
		n++
		proxyLegShadowShedTotal.Add(1)
		tlog("🔥 [proxy][shed] shadow: would shed proxy[%d] (%s): %d consecutive failures inside the proxy's own connect step over %s, while the platform was reached through another path. Nothing was changed.\n",
			c.Index, c.Address, c.Streak, c.Age.Round(time.Second))
	}
	return n
}

// runProxyLegShadow evaluates the shadow decision until ctx ends.
func runProxyLegShadow(ctx context.Context) {
	ticker := time.NewTicker(proxyLegShadowInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			evaluateProxyLegShadow(now)
		}
	}
}
