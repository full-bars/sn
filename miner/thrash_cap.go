package miner

// thrash_cap.go ports the relay's thrash-escape memory
// (provider/thrash_cap.go): when the thrash watchdog restarts the miner for
// swap thrash, the next start must come up SMALLER instead of rebuilding the
// same non-fitting proxy set. A plain restart alone does not fix an own-memory
// thrash — the pool rebuilds the same wall — so escalation writes a reduced
// start cap that the launch path and the reload trim honour through
// effectiveTrimCapSource (the tighter of operator trim, OOM cap, thrash cap).
//
// State lives at <state dir>/thrash_cap.json and expires 24h after the last
// escalation: a clean day relaxes the cap. A new escalation re-arms it, appends
// to the restarts ring, and pushes the expiry out.
//
// Difference from the relay: the state directory resolves through
// providerStateDir (URNETWORK_STATE_DIR honoured) instead of ~/.urnetwork, the
// same as oom_cap.json and every other miner provider file.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// trimCapThrash names the automatic thrash cap as the binding source in
	// trim logs (mirrors trimCapOOM in proxy_manager_oom_cap.go).
	trimCapThrash = "thrashcap"

	thrashCapFileName = "thrash_cap.json"

	// thrashExitMarkerPrefix names the one-shot marker the thrash exit writes
	// immediately before it exits 75: thrash_exit.<pid> beside thrash_cap.json.
	// The pid in the name is the whole point — it binds the marker to the
	// process that asked for the restart, not to a wall-clock instant.
	thrashExitMarkerPrefix = "thrash_exit."

	// thrashCapHold is how long an escalation's cap survives without a new
	// escalation.
	thrashCapHold = 24 * time.Hour

	// thrashCapFraction is how much of the pre-restart running count the next
	// start may rebuild; the rest is headroom for the same growth the escape
	// could not contain.
	thrashCapFraction = 0.6

	// thrashMaxRestarts24h is the hard ceiling on escapes per day. Past it the
	// watchdog logs and stops acting (the cap stays in force from the last one).
	thrashMaxRestarts24h = 3
)

// thrashRestartBackoff is the re-arm schedule between escapes, indexed by how
// many escapes already happened in the last 24h. At the current max of 3 per
// day the third rung (6h) is unreachable (a third restart is denied outright);
// it stays for spec parity and goes live if the daily max ever rises.
var thrashRestartBackoff = []time.Duration{
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
}

// thrashCapState is the persisted escape memory.
type thrashCapState struct {
	Cap         int     `json:"cap"`          // next-start running-proxy cap; 0 = none
	SetUnix     int64   `json:"set_unix"`     // when the cap was last (re-)armed
	ExpiresUnix int64   `json:"expires_unix"` // the cap stops applying after this
	Restarts    []int64 `json:"restarts"`     // unix seconds of recent escapes
}

// thrashCapPath returns the state file path, beside oom_cap.json.
func thrashCapPath() (string, error) {
	dir, err := oomCapDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, thrashCapFileName), nil
}

// readThrashCapState reads the persisted state; a missing file is the zero
// value (never an error the caller must handle).
func readThrashCapState() thrashCapState {
	var st thrashCapState
	if path, err := thrashCapPath(); err == nil {
		oomReadJSON(path, &st)
	}
	return st
}

// activeThrashCap returns the cap in force at now, if any. Expiry is checked
// here so every reader agrees on when a quiet day has relaxed the cap.
func activeThrashCap(now time.Time) (int, bool) {
	st := readThrashCapState()
	if st.Cap <= 0 || st.ExpiresUnix <= 0 {
		return 0, false
	}
	if now.Unix() >= st.ExpiresUnix {
		return 0, false
	}
	return st.Cap, true
}

// thrashRestartsWithin prunes the restarts ring to the 24h window ending at
// now. Order is preserved (oldest first).
func thrashRestartsWithin(st thrashCapState, now time.Time) []int64 {
	cutoff := now.Add(-thrashCapHold).Unix()
	out := make([]int64, 0, len(st.Restarts))
	for _, ts := range st.Restarts {
		if ts < cutoff {
			continue
		}
		if ts > now.Unix() {
			// A backwards clock step left a future-dated entry: keep the
			// restart (dropping it would silently bypass the cooldown) but
			// clamp it to now.
			ts = now.Unix()
		}
		out = append(out, ts)
	}
	return out
}

// thrashCapEscalationAllowed applies the anti-loop ladder: at most
// thrashMaxRestarts24h escapes per day, spaced by thrashRestartBackoff.
// Returns a stable code plus human reason when denied, and the count of
// escapes in the last 24h. The code is what callers dedupe on; the reason
// embeds changing durations and must never be compared.
func thrashCapEscalationAllowed(st thrashCapState, now time.Time) (bool, string, string, int) {
	recent := thrashRestartsWithin(st, now)
	n := len(recent)
	if n >= thrashMaxRestarts24h {
		return false, "cap-reached", fmt.Sprintf("restart cap reached (%d of max %d restarts in 24h); not restarting again today", n, thrashMaxRestarts24h), n
	}
	if n > 0 {
		idx := n - 1
		if idx >= len(thrashRestartBackoff) {
			idx = len(thrashRestartBackoff) - 1
		}
		wait := thrashRestartBackoff[idx]
		since := now.Sub(time.Unix(recent[n-1], 0))
		if since < wait {
			return false, "rearm", fmt.Sprintf("re-arming: last restart was %s ago, next allowed in %s", roundThrashDur(since), roundThrashDur(wait-since)), n
		}
	}
	return true, "", "", n
}

// thrashCapForNextStart sizes the refit cap: a fraction of what was running
// when the box started thrashing. 0 running means nothing to protect, so no
// cap is written.
func thrashCapForNextStart(running int) int {
	if running <= 0 {
		return 0
	}
	cap := int(float64(running) * thrashCapFraction)
	if cap < 1 {
		cap = 1
	}
	return cap
}

// recordThrashEscalation persists the escape: cap for the next start, the
// restarts ring with this escape appended, and a fresh expiry. Errors are
// returned so the caller can log; the escalation itself never depends on the
// write succeeding (the caller refuses the exit when it fails).
//
// This record is the anti-loop memory, NOT the exit-reason marker: the liveness
// stall path writes it mid-life, and a persist goroutine that outlives its
// timeout still lands after the watchdog refused to exit, so its presence says
// nothing about how this process died. The exit-reason marker is the one-shot
// thrash exit marker below (see requestThrashExit), which is written only at
// the exit and is keyed by pid.
func recordThrashEscalation(cap int, now time.Time) error {
	path, err := thrashCapPath()
	if err != nil {
		return err
	}
	// The write lock lives beside the file: the directory must exist before
	// oomWriteJSON tries to lock (same order oomCapDecide uses).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	prev := readThrashCapState()
	recent := thrashRestartsWithin(prev, now)
	// The restart is ALWAYS appended: the ring is the anti-loop's accounting,
	// and skipping it when the computed cap was 0 (nothing running) would allow
	// unthrottled restart loops. A cap of 0 keeps any cap the previous
	// escalation left standing instead of erasing it — the next start still
	// benefits from fitting better.
	writeCap := cap
	// Preserve/inherit the previous cap only while it is still UNEXPIRED, and
	// never loosen: the tighter of the two wins ("tighter automatic wins").
	if prev.Cap > 0 && prev.ExpiresUnix > now.Unix() && (writeCap <= 0 || prev.Cap < writeCap) {
		writeCap = prev.Cap
	}
	st := thrashCapState{
		Cap:         writeCap,
		SetUnix:     now.Unix(),
		ExpiresUnix: now.Add(thrashCapHold).Unix(),
		Restarts:    append(recent, now.Unix()),
	}
	return oomWriteJSON(path, st)
}

// thrashExitMarkerPath is this file's one-shot exit marker for one pid:
// thrash_exit.<pid>, in the same state directory as thrash_cap.json.
func thrashExitMarkerPath(dir string, pid int) string {
	return filepath.Join(dir, thrashExitMarkerPrefix+strconv.Itoa(pid))
}

// writeThrashExitMarker leaves thrash_exit.<this pid> in the state directory.
func writeThrashExitMarker() error {
	dir, err := oomCapDir()
	if err != nil {
		return err
	}
	// the directory must exist before the write, same as the record's
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pid := os.Getpid()
	return os.WriteFile(thrashExitMarkerPath(dir, pid), []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// requestThrashExit marks this process's imminent exit with the thrash code and
// must be called immediately before the exit function, on every path that
// exits 75 for a planned restart (the swap-thrash escalation and the liveness
// self-exit).
//
// Exit 75 has two producers — this planned restart and the network sign-in
// quarantine (run.go providerQuarantinedExitCode) — and a supervising
// `provide --all-operators` has to name the right one for the operator. The
// marker is keyed by this pid and written only here, at the exit, so it cannot
// be produced by anything that keeps running: thrash_cap.json is written
// mid-life by the liveness stall path and by a persist goroutine that can land
// after its timeout without the exit being taken, and comparing its mtime with
// the child's start time therefore reads a later genuine quarantine as a
// thrash restart. A marker failure is logged to stderr and never blocks the
// exit: the exit code must still be taken, and the cost is the wrong log line,
// not a wrong action. The write itself is unbounded, so every production
// caller uses requestThrashExitBounded below, which bounds it with
// swapThrashPersistTimeout: a state directory hung in swap I/O must not keep
// the exit from being taken.
func requestThrashExit() {
	if err := writeThrashExitMarker(); err != nil {
		swapThrashExitNotef("[thrash] could not write the exit marker in the state directory (%v); a supervising `provide --all-operators` will read this planned restart as a network sign-in quarantine", err)
	}
}

// thrashExitMarkerWriteFn writes the one-shot marker; a test seam for
// requestThrashExitBounded (the real write is writeThrashExitMarker).
var thrashExitMarkerWriteFn = writeThrashExitMarker

// requestThrashExitBounded writes the exit marker with the same bounded wait as
// the anti-loop record, and returns the error for the caller to log.
//
// writeThrashExitMarker runs MkdirAll and WriteFile with no limit, and both
// callers run it immediately before the exit it explains. On a state directory
// hung in swap I/O — the exact condition that fires the watchdog — the
// unbounded write would keep the exit from ever being taken, so the node could
// not restart. The restart contract is the exit status, not the marker: a
// timeout here is returned for the caller to log and the exit proceeds.
func requestThrashExitBounded() error {
	done := make(chan error, 1)
	write := thrashExitMarkerWriteFn // read here, not in the goroutine
	go func() { done <- write() }()
	select {
	case err := <-done:
		return err
	case <-time.After(swapThrashPersistTimeout):
		return errors.New("timed out writing the exit marker")
	}
}

// clearThrashExitMarkers removes every exit marker in dir. A marker is
// one-shot: the supervisor consumes it when it sees the exit, so one still here
// belongs to a child whose exit nobody saw (the supervisor was killed first).
// Clearing them before the next child runs keeps a reused pid from inheriting
// another process's planned restart.
func clearThrashExitMarkers(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), thrashExitMarkerPrefix) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// undoThrashEscalation removes the ring entry for now and, if the cap on disk is
// the one that call wrote, puts the previous cap back. It edits the current
// state rather than restoring a copy taken earlier, so an entry another writer
// added in between survives. The liveness self-exit uses it when a stall
// resolves itself before the supervisor acts.
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

// roundThrashDur renders a duration in whole human units ("29m", "1h58m") for
// the re-arm reason lines. The watchdog's own roundSwapThrashDur stays a
// separate, second-granularity helper for the critical-state line.
func roundThrashDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	d = d.Round(time.Minute)
	if h := d / time.Hour; h > 0 {
		return fmt.Sprintf("%dh%02dm", h, (d%time.Hour)/time.Minute)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}
