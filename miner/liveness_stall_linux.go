//go:build linux

package miner

import (
	"os"
	"time"
)

// livenessGate says whether a stall may be acted on: self-heal must be on, and
// at the start of an episode the daily restart budget (the same ring and
// backoff as a thrash restart) must allow one. Without the budget a stall that
// recurs every few minutes would restart the node for ever.
func livenessGate(episodeStart bool) (bool, string) {
	if !resolveSelfHealEnabled(os.Getenv("URNETWORK_SELF_HEAL") == "1") {
		return false, "self-heal is off"
	}
	if episodeStart {
		if allowed, _, reason, _ := thrashCapEscalationAllowed(readThrashCapState(), time.Now()); !allowed {
			return false, "the restart budget is spent (" + reason + ")"
		}
	}
	return true, ""
}

// recordLivenessStall leaves the next start leaner before the supervisor
// restarts a stalled miner. A stall of this kind is what an over-full box looks
// like from inside, and starting again with the same proxy count walks straight
// into it, so it is recorded like a thrash escape: a cap of a fraction of what
// was running, and an entry in the anti-loop ring that the daily ceiling counts.
// A cap already in force is kept as it is, not cut again from the already
// capped count: repeated stalls must not compound down to one proxy.
//
// The write is bounded (a blocked lock must not delay the decision), but a write
// that times out may still land later, so the returned undo always waits for the
// write to finish before it removes exactly what this call added. It runs when a
// stall resolves itself before the supervisor acts.
//
// note is the sink for the lines this writes while the caller is deciding to
// exit, and it is a parameter because the two callers must not share one. The
// self-exit loop runs this immediately before it writes the marker and exits,
// and critLog appends to and fsyncs events.log in the state directory whose
// hang fired the stall, with no timeout: one stuck fsync there would keep the
// restart from ever happening. That caller passes the bounded stderr sink
// (swapThrashExitNotef). The systemd caller withholds a ping instead of
// exiting, so its line belongs in events.log and it passes critLog. The undo
// below keeps critLog: progress resumed, so the process is not exiting and the
// durable line is the right one.
func recordLivenessStall(note func(format string, args ...any)) func() {
	now := time.Now()
	prev := readThrashCapState()
	cap := 0
	if _, capped := activeThrashCap(now); !capped {
		// The relay reads lastRunningProxyCount here; the miner's equivalent
		// number is the launch count the pool opened with (oomCapRunning), the
		// same one the OOM cap and the live `set oom-cap on` shed use.
		cap = thrashCapForNextStart(oomCapRunning())
	}
	done := make(chan error, 1)
	go func() { done <- recordThrashEscalation(cap, now) }()
	undo := func() {
		err := <-done
		if err != nil {
			return
		}
		undoThrashEscalation(now, prev)
		critLog("[liveness] the lean start cap was removed again: progress resumed before systemd acted\n")
	}
	select {
	case err := <-done:
		done <- err // keep it for undo
		if err != nil {
			note("[liveness] could not record the lean start cap: %v\n", err)
			return nil
		}
		if cap > 0 {
			note("[liveness] the next start is capped at %d proxies (60%% of what was running)\n", cap)
		}
	case <-time.After(livenessRecordTimeout):
		note("[liveness] timed out recording the lean start cap; it will be undone if the stall resolves\n")
	}
	return undo
}
