//go:build linux

package miner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// hangExitPathSeams makes every step of the pre-exit path that could touch a
// hung state directory block until the returned restore func runs: the marker
// write, the status-file clear, and the stderr sink (a wedged journal pipe, or
// stderr redirected onto the same hung volume). The bounded waits are shrunk so
// the test does not sit through the production timeouts.
func hangExitPathSeams(t *testing.T) (markerCalls *atomic.Int32, restore func()) {
	t.Helper()
	release := make(chan struct{})
	prevMarker, prevStderr, prevClear := thrashExitMarkerWriteFn, swapThrashStderrWriteFn, swapThrashStatusClearFn
	prevPersist, prevNote := swapThrashPersistTimeout, swapThrashExitNoteTimeout

	markerCalls = &atomic.Int32{}
	thrashExitMarkerWriteFn = func() error {
		markerCalls.Add(1)
		<-release
		return nil
	}
	swapThrashStatusClearFn = func(func() (string, error)) { <-release }
	swapThrashStderrWriteFn = func(p []byte) (int, error) {
		<-release
		return len(p), nil
	}
	swapThrashPersistTimeout = 25 * time.Millisecond
	swapThrashExitNoteTimeout = 25 * time.Millisecond

	return markerCalls, func() {
		close(release)
		thrashExitMarkerWriteFn, swapThrashStderrWriteFn, swapThrashStatusClearFn = prevMarker, prevStderr, prevClear
		swapThrashPersistTimeout, swapThrashExitNoteTimeout = prevPersist, prevNote
	}
}

// muffleExitNote pipes events.log to a FIFO with no reader: any critLog or
// importantLogf on the pre-exit path blocks forever in open(2), which is exactly
// what a critLog against a state directory hung in swap I/O does.
func muffleExitNote(t *testing.T, stateDir string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(stateDir, "events.log"), 0o600); err != nil {
		t.Fatalf("mkfifo events.log: %v", err)
	}
}

// exitPathWatch builds a watch whose only exit is recorded, over a temp state
// directory. The production sinks are restored (not the rig's log collectors)
// so a pre-exit line that regressed to notef/critf would hit events.log and be
// caught — or, with muffleExitNote, block.
func exitPathWatch(t *testing.T, stateDir string) (*swapThrashWatch, chan int, *[]string) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)
	clock := swapThrashT0
	var logs []string
	watch, _ := swapThrashTestWatch(t, &scriptedSwapThrashSensor{}, &clock, &logs)
	watch.notef = importantLogf
	watch.critf = critLog
	watch.exitNotef = swapThrashExitNotef
	exited := make(chan int, 1)
	watch.exitFn = func(code int) { exited <- code }
	return watch, exited, &logs
}

// The pre-exit diagnostics are stderr-only: they must not append to (and fsync)
// events.log in the state directory whose hang fires the escalation, because a
// bounded marker write followed by an events.log write on its failure path would
// just move the block one line later and still keep the node from restarting.
// Checked on both branches: the reason line, and the marker-failure line.
func TestThrashExitDiagnosticsBypassTheStateDirectory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		markerErr error
		want      string
	}{
		{"healthy marker", nil, "restarting the provider now"},
		{"marker write fails", errors.New("timed out writing the exit marker"), "could not write the exit marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			watch, exited, _ := exitPathWatch(t, stateDir)

			prevStderr, prevMarker := swapThrashStderrWriteFn, thrashExitMarkerWriteFn
			t.Cleanup(func() { swapThrashStderrWriteFn, thrashExitMarkerWriteFn = prevStderr, prevMarker })
			var lines []string
			swapThrashStderrWriteFn = func(p []byte) (int, error) {
				lines = append(lines, string(p))
				return len(p), nil
			}
			if tc.markerErr != nil {
				thrashExitMarkerWriteFn = func() error { return tc.markerErr }
			}

			watch.exitOnRestart("[thrash]", "🚨 [memory] restarting the provider now")

			select {
			case code := <-exited:
				if code != swapThrashExitCode {
					t.Fatalf("exit code = %d, want swapThrashExitCode (%d)", code, swapThrashExitCode)
				}
			default:
				t.Fatal("the exit was not taken")
			}
			found := false
			for _, l := range lines {
				if strings.Contains(l, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("the %q line must reach stderr, got %q", tc.want, lines)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "events.log")); err == nil {
				t.Fatal("the pre-exit path wrote events.log in the state directory")
			}
		})
	}
}

// The hole this test exists for: a marker write that times out used to be
// followed by a critLog/notef line that fsyncs events.log in the same hung state
// directory, so the exit — and therefore the supervised restart — never
// happened. events.log is a FIFO here, so any critLog on this path blocks
// forever; the marker, the status clear and stderr are hung too. The exit must
// still be taken promptly.
func TestThrashExitSurvivesAHungLogPath(t *testing.T) {
	stateDir := t.TempDir()
	muffleExitNote(t, stateDir)
	watch, exited, _ := exitPathWatch(t, stateDir)

	markerCalls, restore := hangExitPathSeams(t)
	defer restore()

	start := time.Now()
	go watch.exitOnRestart("[thrash]", "🚨 [memory] restarting the provider now")

	select {
	case code := <-exited:
		if code != swapThrashExitCode {
			t.Fatalf("exit code = %d, want swapThrashExitCode (%d)", code, swapThrashExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a hung log path stopped the exit: the exit function was never called")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the exit path was not bounded: took %v", took)
	}
	if markerCalls.Load() == 0 {
		t.Fatal("the marker write was never attempted, so the timeout path was not exercised")
	}
}
