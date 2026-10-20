package miner

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/urnetwork/connect"
)

// proxy_manager_auth.go drives the ported v33 give-up/backoff accounting
// (proxy_manager_history.go) from the miner's authentication path, and
// enforces the resulting backoff window when the desired set is built.
//
// The v33 reference is the auth ladder in provider/main.go (RecordFailure per
// genuine failed attempt, RecordGiveUp once the ladder is exhausted, then
// SetBackoffUntil with proxyURLGiveUpRetryDelay, or eviction of a URL-sourced
// address after proxyURLGiveUpEvictAfterCycles) plus the launch-time Eligible
// gate in proxy_reload.go. The miner authenticates each proxy slot once per
// run and has no in-process auth ladder (the connect/SDK client strategy owns
// the dial), so the port keeps the accounting and the gate here, and
// noteProviderAuthFailure/noteProviderAuthSuccess are the two thin call sites
// in the provide loop. A proxy that keeps failing therefore accumulates
// lifetime failures across restarts in the same process (supervisor restarts)
// exactly as v33's process-lifetime history does.

const (
	// provenMaxAuthFailures / unprovenMaxAuthFailures are the v33 per-ladder
	// ceilings: a proxy that has completed at least one successful auth gets
	// the longer leash.
	provenMaxAuthFailures   = 10
	unprovenMaxAuthFailures = 3

	// proxyURLGiveUpRetryBase is the starting delay before a URL-sourced proxy
	// that exhausted its auth attempts is retried; it doubles per give-up
	// cycle up to proxyURLGiveUpRetryCap.
	proxyURLGiveUpRetryBase = 15 * time.Minute
	proxyURLGiveUpRetryCap  = 24 * time.Hour

	// proxyURLGiveUpEvictAfterCycles is the lifetime give-up count at which a
	// URL-sourced address is permanently evicted (see evictProxyURLAddress)
	// instead of requeued again.
	proxyURLGiveUpEvictAfterCycles = 4
)

// proxyAuthMaxFailures returns the attempt ceiling for a proxy identity: the
// unproven ceiling until the proxy has completed one successful auth.
func proxyAuthMaxFailures(key string) int {
	if globalProvenProxies.HasSucceeded(key) {
		return provenMaxAuthFailures
	}
	return unprovenMaxAuthFailures
}

// proxyURLGiveUpRetryDelay computes the requeue delay for a URL-sourced
// proxy's Nth give-up (giveUpCount is 1-indexed). Doubles
// proxyURLGiveUpRetryBase each cycle, capped at proxyURLGiveUpRetryCap, with up
// to 20% jitter so many addresses that gave up around the same time do not all
// retry in the same instant.
func proxyURLGiveUpRetryDelay(giveUpCount int) time.Duration {
	if giveUpCount < 1 {
		giveUpCount = 1
	}
	delay := proxyURLGiveUpRetryBase
	for i := 1; i < giveUpCount; i++ {
		delay *= 2
		if delay >= proxyURLGiveUpRetryCap {
			delay = proxyURLGiveUpRetryCap
			break
		}
	}
	jitter := time.Duration(rand.Int63n(int64(delay)/5 + 1)) // up to 20%
	return delay + jitter
}

// proxyGiveUpResult reports what one give-up cycle did, for the caller's log
// line.
type proxyGiveUpResult struct {
	key       string
	giveUp    int
	evicted   bool
	retryIn   time.Duration
	evictFail error
}

// proxyAuthGiveUp performs the accounting for one exhausted auth ladder
// (v33's give-up branch): a URL-sourced identity is evicted from the cache
// after proxyURLGiveUpEvictAfterCycles give-ups, otherwise it (and a curated
// identity) backs off for proxyURLGiveUpRetryDelay(giveUpCount). The backoff is
// recorded in the failure history so the desired-set gate skips it until the
// window elapses.
func proxyAuthGiveUp(settings *connect.ProxySettings, urlSourced bool) proxyGiveUpResult {
	key := settings.Key()
	result := proxyGiveUpResult{key: key}
	result.giveUp = globalProxyFailureHistory.RecordGiveUp(key)

	if urlSourced && result.giveUp >= proxyURLGiveUpEvictAfterCycles {
		address, _ := connect.SplitProxyKey(key)
		if err := evictProxyURLAddress(address); err != nil {
			// Eviction failed: fall back to the normal backoff so the address
			// is not relaunched immediately (v33 does the same).
			result.evictFail = err
			result.retryIn = proxyURLGiveUpRetryDelay(result.giveUp)
			globalProxyFailureHistory.SetBackoffUntil(key, time.Now().Add(result.retryIn))
			return result
		}
		result.evicted = true
		return result
	}

	result.retryIn = proxyURLGiveUpRetryDelay(result.giveUp)
	globalProxyFailureHistory.SetBackoffUntil(key, time.Now().Add(result.retryIn))
	return result
}

// proxyAuthSlowBackoff records a backoff WITHOUT give-up accounting, for the
// two v33 cases where the failure is not evidence about the proxy (the backend
// is degraded, or the attempt was cut short by a deadline).
func proxyAuthSlowBackoff(key string) time.Duration {
	delay := proxyURLGiveUpRetryDelay(proxyURLGiveUpEvictAfterCycles - 1)
	globalProxyFailureHistory.SetBackoffUntil(key, time.Now().Add(delay))
	return delay
}

// noteProviderAuthSuccess is the miner auth path's success hook: the
// identity is proven, its failure/give-up history clears, and any slow-retry
// drop record goes away (v33's MarkSucceeded + Reset + ClearDropped).
func noteProviderAuthSuccess(settings *connect.ProxySettings) {
	if settings == nil || settings.Address == "" {
		return
	}
	key := settings.Key()
	globalProvenProxies.MarkSucceeded(key)
	globalProxyFailureHistory.Reset(key)
	globalProxySlowRetryState.ClearDropped(key)
	// Live health: a proxy that just authenticated is up, so the URL reaper
	// shields it and the health report shows it (v33 connect health parity).
	noteLiveProxyHealthUp(key)
}

// noteProviderAuthFailure is the miner auth path's failure hook. A cancelled
// attempt is not a failure (v33: context cancellation is never a give-up), and
// a URL-sourced identity whose lifetime failures have reached the ceiling gives
// up here so the backoff record exists before the desired set is next built.
//
// A deadline error DOES count here, unlike v33, which measures the attempt and
// treats a deadline-cut attempt as slow rather than broken. The miner's auth
// path is one connect/SDK call without that measurement, so the hook has no
// slow-versus-broken signal to apply; counting it keeps a proxy that never
// completes an authentication inside the give-up/eviction accounting instead
// of leaving it unaccounted forever.
func noteProviderAuthFailure(settings *connect.ProxySettings, err error) providerAuthGiveUpInfo {
	if settings == nil || settings.Address == "" {
		return providerAuthGiveUpInfo{}
	}
	if err != nil && errors.Is(err, context.Canceled) {
		return providerAuthGiveUpInfo{}
	}
	key := settings.Key()
	// Live health: a failed authentication means the proxy is not up right now,
	// so the reaper stops shielding it and the traffic/health report shows it
	// as degraded.
	noteLiveProxyHealthFailure(key)
	failures := globalProxyFailureHistory.RecordFailure(key)
	if failures < proxyAuthMaxFailures(key) {
		return providerAuthGiveUpInfo{}
	}
	result := proxyAuthGiveUp(settings, isURLSourcedProxyKey(key))
	info := providerAuthGiveUpInfo{giveUp: result.giveUp, retryIn: result.retryIn, evicted: result.evicted}
	if result.evicted {
		tlog("[proxy][auth] %s give-up %d of %d: authentication failed %d times; evicted from the URL cache and will not be retried\n",
			proxyKeyDisplay(key), result.giveUp, proxyURLGiveUpEvictAfterCycles, failures)
		return info
	}
	if result.evictFail != nil {
		tlog("[proxy][auth] %s give-up %d: could not evict (%v); backing off %s\n",
			proxyKeyDisplay(key), result.giveUp, result.evictFail, formatDuration(result.retryIn))
		return info
	}
	tlog("[proxy][auth] %s give-up %d of %d after %d failed authentications; will retry in %s\n",
		proxyKeyDisplay(key), result.giveUp, proxyURLGiveUpEvictAfterCycles, failures, formatDuration(result.retryIn))
	return info
}

// isURLSourcedProxyKey reports whether the given identity key is a
// URL-sourced proxy, the split that decides eviction versus slow retry.
func isURLSourcedProxyKey(key string) bool {
	state, err := readProxyState()
	if err != nil || state == nil {
		return false
	}
	entry, ok := state.Proxies[key]
	if !ok {
		return false
	}
	return effectiveRemoveDeadSource(state, entry) == "url"
}

// dropBackoffHeldURLProxies removes from the desired set every URL-sourced
// identity whose give-up backoff window has not elapsed, and reports how many
// it held back. This is the port of v33's launch-time Eligible gate in
// proxy_reload.go: without it, any rebuild of the desired set (another proxy's
// give-up, a URL refresh) would relaunch a dead address immediately and defeat
// the escalating backoff.
func dropBackoffHeldURLProxies(desiredSet map[string]*connect.ProxySettings, sourceOf map[string]string, now time.Time) int {
	held := 0
	for key, source := range sourceOf {
		if source != "url" {
			continue
		}
		if globalProxyFailureHistory.Eligible(key, now) {
			continue
		}
		delete(desiredSet, key)
		delete(sourceOf, key)
		held++
	}
	return held
}
