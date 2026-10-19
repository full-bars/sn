//go:build linux

package provider

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

// recordLivenessStall leaves the next start leaner before systemd restarts a
// stalled provider. A stall of this kind is what an over-full box looks like
// from inside, and starting again with the same proxy count walks straight into
// it, so it is recorded like a thrash escape: a cap of a fraction of what was
// running, and an entry in the anti-loop ring that the daily ceiling counts.
// A cap already in force is kept as it is, not cut again from the already
// capped count: repeated stalls must not compound down to one proxy.
//
// The write is bounded (a blocked lock must not delay the decision), but a write
// that times out may still land later, so the returned undo always waits for the
// write to finish before it removes exactly what this call added. It runs when a
// stall resolves itself before systemd acts.
func recordLivenessStall() func() {
	now := time.Now()
	prev := readThrashCapState()
	cap := 0
	if _, capped := activeThrashCap(now); !capped {
		cap = thrashCapForNextStart(int(lastRunningProxyCount.Load()))
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
			critLog("[liveness] could not record the lean start cap: %v\n", err)
			return nil
		}
		if cap > 0 {
			critLog("[liveness] the next start is capped at %d proxies (60%% of what was running)\n", cap)
		}
	case <-time.After(livenessRecordTimeout):
		critLog("[liveness] timed out recording the lean start cap; it will be undone if the stall resolves\n")
	}
	return undo
}

// undoThrashEscalation removes the ring entry for now and, if the cap on disk is
// the one that call wrote, puts the previous cap back. It edits the current state
// rather than restoring a copy taken earlier, so an entry another writer added
// in between survives.
func undoThrashEscalation(now time.Time, prev thrashCapState) {
	path, err := thrashCapPath()
	if err != nil {
		return
	}
	st := readThrashCapState()
	kept := st.Restarts[:0:0]
	removed := false
	for _, ts := range st.Restarts {
		if !removed && ts == now.Unix() {
			removed = true
			continue
		}
		kept = append(kept, ts)
	}
	st.Restarts = kept
	if st.SetUnix == now.Unix() {
		st.Cap, st.SetUnix, st.ExpiresUnix = prev.Cap, prev.SetUnix, prev.ExpiresUnix
	}
	if err := oomWriteJSON(path, st); err != nil {
		critLog("[liveness] could not restore the start cap after progress resumed: %v\n", err)
	}
}
