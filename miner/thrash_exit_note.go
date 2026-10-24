package miner

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Pre-exit diagnostics.
//
// The two producers of exit 75 (the swap-thrash watchdog and the self-exit
// liveness loop) each log on their way to the exit. Those lines must NOT go
// through critLog/importantLogf: both append to and fsync events.log in the
// provider state directory — the very directory whose hang fires this
// escalation — and critLog serialises on critLogMu, so one stuck fsync blocks
// every later caller too. Bounding the marker write and then logging its
// failure through events.log would just move the block one line later, still
// before the exit, and the node would still never restart.
//
// The signal is plain stderr: no state-dir I/O, no fsync, no lock shared with
// the log writers, and the supervisor already captures it. The write itself is
// bounded as well (a wedged journal pipe, or stderr redirected onto the same
// hung volume, must not hold the exit either); a line that cannot be written in
// time is dropped, because the exit status is the contract.

// swapThrashExitNoteTimeout bounds each pre-exit diagnostic line. Short on
// purpose — it is a courtesy, not a step the restart depends on — but not so
// short that a briefly stalled journal loses the operator's only record of why
// the node restarted.
var swapThrashExitNoteTimeout = time.Second

// swapThrashStderrWriteFn is the pre-exit sink; a test seam so a test can prove
// the exit is still taken when even stderr would block.
var swapThrashStderrWriteFn = func(p []byte) (int, error) { return os.Stderr.Write(p) }

// swapThrashStatusClearFn removes the stale status file; a test seam for
// clearSwapThrashStatusFileBounded.
var swapThrashStatusClearFn = clearSwapThrashStatusFile

// swapThrashExitNotef writes one line to stderr for a log that runs on the way
// to a planned exit (status 75). See the file comment for why it is not critLog.
//
// The operator loses the events.log copy of these lines: a post-reboot grep of
// events.log will not show the restart reason or the marker failure. They land
// in the supervisor's output instead, next to its own record of the 75, and the
// anti-loop restart record is still written (bounded) before this point.
func swapThrashExitNotef(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	write := swapThrashStderrWriteFn // read here, not in the goroutine
	done := make(chan struct{}, 1)
	go func() {
		_, _ = write([]byte(line))
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(swapThrashExitNoteTimeout):
	}
}

// clearSwapThrashStatusFileBounded clears the stale status file with the same
// bounded wait as the cap write and the marker write.
//
// The removal is bookkeeping: a status file left behind only misreports a
// restart that is happening anyway. An unbounded os.Remove against a state
// directory hung in swap I/O would otherwise block the exit it is clearing the
// way for.
func clearSwapThrashStatusFileBounded(pathFn func() (string, error)) {
	clear := swapThrashStatusClearFn // read here, not in the goroutine
	done := make(chan struct{}, 1)
	go func() {
		clear(pathFn)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(swapThrashPersistTimeout):
	}
}

// exitOnRestart is the planned-restart exit of one watchdog: the reason line, the
// stale status clear, the exit marker, and the exit itself.
//
// Every step is bounded and the only diagnostics go to stderr (exitNotef), so
// the state directory whose hang this watchdog exists for can delay the exit by
// at most two short timeouts and can never stop it. The restart contract is the
// exit status; a marker that loses its race with the hang only costs the
// supervising `provide --all-operators` the ability to name the cause.
func (w *swapThrashWatch) exitOnRestart(tag, reasonFormat string, reasonArgs ...any) {
	if reasonFormat != "" {
		w.exitNotef(reasonFormat, reasonArgs...)
	}
	clearSwapThrashStatusFileBounded(w.statusPath)
	if err := requestThrashExitBounded(); err != nil {
		w.exitNotef("%s could not write the exit marker (%v); a supervising `provide --all-operators` will report this restart as a rejected sign-in", tag, err)
	}
	w.exitFn(swapThrashExitCode)
}
