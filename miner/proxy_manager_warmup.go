package miner

import (
	"context"
	"sync/atomic"
	"time"
)

// This file ports the relay's URL warmup hold onto the miner's one-shot
// launch model: provider/proxy_reload.go urlWarmupHold and its launch gate,
// provider/proxy_url_source.go's bounded fetcher wait, and the completion
// signal the relay's paceMonitor publishes. The miner has no reload pass, so
// the signal is tracked where the launch loop starts workers: the ramp is
// complete when every curated worker has passed its stagger, and every
// consumer is bounded by urlWarmupLaunchGrace so a fleet whose file proxies
// never warm can still refill from the URL pool.

// urlWarmupLaunchGrace bounds how long unproven URL-sourced proxies wait for
// the file-sourced pool to finish its initial warmup before launching anyway.
const urlWarmupLaunchGrace = 5 * time.Minute

// urlWarmupHold reports whether unproven URL-sourced proxies must still wait
// for the file-proxy warmup. Bounded by urlWarmupLaunchGrace from the moment
// the process started: a node with a permanent tail of file proxies that
// never auth never reaches the ramp bar, and an unbounded hold would then
// block qualified URL replacements and the URL fetcher behind proxies that
// are already dead.
func urlWarmupHold() bool {
	return !proxyWarmupDone.Load() && time.Since(processStart) < urlWarmupLaunchGrace
}

// waitURLWarmupHold waits until the warmup hold releases (warmup completes
// or the grace elapses) and reports false when ctx ends first.
func waitURLWarmupHold(ctx context.Context) bool {
	for urlWarmupHold() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return true
}

// The curated ramp signal. The launch loop records how many curated
// (non-URL) workers it must start and notes each one as it passes its
// stagger; when every one has started, the operator-curated ramp the URL
// pool must not compete with is under way and the hold releases. A run with
// no curated workers releases at once, and a loop that never runs leaves the
// hold to the grace.
var (
	warmupCuratedTotal    atomic.Int64
	warmupCuratedLaunched atomic.Int64
)

// warmupIsCurated reports whether a launch slot belongs to the operator-
// curated ramp: file and internal identities must finish starting before the
// URL pool may join them, while URL identities and the direct slot (which
// carries no proxy and no warmth) are not part of the ramp. The launch loop
// counts and notes through this one rule so the count can never disagree
// with the notes.
func warmupIsCurated(source string) bool {
	return source != "url" && source != ""
}

// noteCuratedWarmupTotal arms the signal with the number of curated workers
// the launch loop is about to start. Zero means there is no ramp to wait
// for, so warmup completes immediately.
func noteCuratedWarmupTotal(total int) {
	warmupCuratedTotal.Store(int64(total))
	warmupCuratedLaunched.Store(0)
	if total == 0 {
		proxyWarmupDone.Store(true)
	}
}

// noteCuratedWarmupLaunch records one curated worker passing its stagger.
// The last one completes the ramp.
func noteCuratedWarmupLaunch() {
	total := warmupCuratedTotal.Load()
	if total == 0 {
		return
	}
	if warmupCuratedLaunched.Add(1) >= total {
		proxyWarmupDone.Store(true)
	}
}

// shouldDeferURLWarmupLaunch reports whether the launch loop must hold this
// slot until the warmup hold releases. Only unproven URL-sourced identities
// defer: curated identities and promoted earners (decayed earnings at or
// above earningsPromotionBytes, known earners in the relay's gate) launch
// with the normal stagger, and a completed warmup or an elapsed grace
// defers nothing.
func shouldDeferURLWarmupLaunch(source string, key string, now time.Time) bool {
	if source != "url" || !urlWarmupHold() {
		return false
	}
	return proxyEarningsScore(key, now) < earningsPromotionBytes
}
