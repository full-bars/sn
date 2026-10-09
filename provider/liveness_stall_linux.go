//go:build linux

package provider

import "time"

// recordLivenessStall leaves the next start leaner before systemd restarts a
// stalled provider. A stall of this kind is what an over-full box looks like
// from inside, and starting again with the same proxy count walks straight into
// it, so it is recorded exactly like a thrash escape: a cap of a fraction of
// what was running, and an entry in the anti-loop ring that the daily ceiling
// counts. The write is bounded; a blocked lock must not delay the decision.
func recordLivenessStall() {
	now := time.Now()
	cap := thrashCapForNextStart(int(lastRunningProxyCount.Load()))
	done := make(chan error, 1)
	go func() { done <- recordThrashEscalation(cap, now) }()
	select {
	case err := <-done:
		if err != nil {
			critLog("[liveness] could not record the lean start cap: %v\n", err)
			return
		}
		if cap > 0 {
			critLog("[liveness] the next start is capped at %d proxies (60%% of what was running)\n", cap)
		}
	case <-time.After(thrashPersistTimeout):
		critLog("[liveness] timed out recording the lean start cap\n")
	}
}
