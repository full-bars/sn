package miner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shutdownLog collects what the bounded wait logs.
type shutdownLog struct{ text strings.Builder }

func (l *shutdownLog) logf(format string, args ...any) {
	fmt.Fprintf(&l.text, format, args...)
}

// A worker that ignores cancellation must not pin the shutdown: the wait
// returns within the grace, writes the goroutine dump, and the error maps to a
// non-zero exit code.
func TestShutdownTimesOutOnWorkerIgnoringCancel(t *testing.T) {
	join := newSlotJoin()
	if !join.add() {
		t.Fatal("add refused")
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release); join.done() })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out shutdownLog

	started := time.Now()
	err := join.waitBounded(ctx, nil, 50*time.Millisecond, out.logf)
	if elapsed := time.Since(started); 5*time.Second < elapsed {
		t.Fatalf("shutdown took %s, want about the 50ms grace", elapsed)
	}
	if err != errProviderShutdownTimeout {
		t.Fatalf("waitBounded = %v, want errProviderShutdownTimeout", err)
	}
	text := out.text.String()
	if !strings.Contains(text, providerShutdownDumpMarker) {
		t.Fatalf("no dump marker in the log:\n%s", text)
	}
	if !strings.Contains(text, "goroutine profile: total") || strings.Count(text, "@ 0x") < 1 {
		t.Fatalf("the log carries no goroutine dump:\n%.300s", text)
	}
	code, ok := providerRunExitCode(err)
	if !ok || code == 0 {
		t.Fatalf("exit code = %d (ok=%v), want a non-zero code", code, ok)
	}
}

// The mirror: a normal drain returns nil, writes no dump and asks for no exit
// code, so the process exits as it always did.
func TestShutdownCleanDrainExitsZeroWithoutDump(t *testing.T) {
	join := newSlotJoin()
	if !join.add() {
		t.Fatal("add refused")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	go join.done()
	var out shutdownLog

	err := join.waitBounded(ctx, nil, 5*time.Second, out.logf)
	if err != nil {
		t.Fatalf("waitBounded = %v, want nil", err)
	}
	if out.text.Len() != 0 {
		t.Fatalf("a clean drain wrote to the log:\n%s", out.text.String())
	}
	if code, ok := providerRunExitCode(err); ok {
		t.Fatalf("a clean drain asked for exit code %d", code)
	}
}

// The grace starts at cancellation: a live run is not cut off, however long
// its workers run.
func TestShutdownDoesNotTimeOutWhileRunIsLive(t *testing.T) {
	join := newSlotJoin()
	if !join.add() {
		t.Fatal("add refused")
	}
	var out shutdownLog
	done := make(chan error, 1)
	go func() { done <- join.waitBounded(t.Context(), nil, 10*time.Millisecond, out.logf) }()
	select {
	case err := <-done:
		t.Fatalf("a live run's wait returned %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	join.done()
	if err := <-done; err != nil {
		t.Fatalf("drain after done = %v", err)
	}
}

// The helper is only half the fix if the run still calls the unbounded wait.
func TestShutdownRunUsesBoundedJoin(t *testing.T) {
	src, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if strings.Contains(text, "\tjoin.wait(") {
		t.Fatal("run.go still calls the unbounded join.wait")
	}
	if !strings.Contains(text, "join.waitBounded(ctx,") {
		t.Fatal("run.go does not call join.waitBounded")
	}
}

// A timed-out shutdown writes its incident bundle before run's defers, and the
// bundle's goroutine profile names the worker that ignored the cancel.
func TestShutdownTimeoutWritesIncidentBundle(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	var armed []int
	origWatchdog := providerShutdownWatchdog
	t.Cleanup(func() { providerShutdownWatchdog = origWatchdog })
	providerShutdownWatchdog = func(after time.Duration, code int) { armed = append(armed, code) }

	join := newSlotJoin()
	if !join.add() {
		t.Fatal("add refused")
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release); join.done() })
	go func() { <-release }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out shutdownLog
	if err := join.waitBounded(ctx, nil, 50*time.Millisecond, out.logf); err != errProviderShutdownTimeout {
		t.Fatalf("waitBounded = %v, want errProviderShutdownTimeout", err)
	}
	providerShutdownExpire("workers did not drain")

	if len(armed) != 1 || armed[0] != providerShutdownTimeoutExitCode {
		t.Fatalf("watchdog armed with %v, want one arming with code %d", armed, providerShutdownTimeoutExitCode)
	}
	root, err := incidentRootDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no incident bundle under %s (err=%v)", root, err)
	}
	dir := filepath.Join(root, entries[len(entries)-1].Name())
	meta, err := os.ReadFile(filepath.Join(dir, "meta.txt"))
	if err != nil || !strings.Contains(string(meta), "trigger=shutdown") {
		t.Fatalf("bundle %s meta.txt does not record a shutdown trigger: %v\n%s", dir, err, meta)
	}
	goroutines, err := os.ReadFile(filepath.Join(dir, "goroutines.txt"))
	if err != nil || !strings.Contains(string(goroutines), "TestShutdownTimeoutWritesIncidentBundle") {
		t.Fatalf("bundle goroutines.txt does not show the stuck worker: %v", err)
	}
}

// The gap the watchdog closes: run's return path has a defer that blocks, here
// reloader.stop behind a reload that holds r.mu. The grace expires, the bundle
// is written, and the process must still be ended with the timeout code while
// that defer is still blocked.
func TestShutdownWatchdogEndsRunWhenDeferBlocks(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	origGrace, origBound := providerShutdownGrace, providerShutdownExitBound
	providerShutdownGrace, providerShutdownExitBound = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { providerShutdownGrace, providerShutdownExitBound = origGrace, origBound })

	exited := make(chan int, 1)
	origExit := exitFunc
	exitFunc = func(code int) {
		select {
		case exited <- code:
		default:
		}
	}
	t.Cleanup(func() { exitFunc = origExit })

	join := newSlotJoin()
	if !join.add() {
		t.Fatal("add refused")
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release); join.done() })
	go func() { <-release }()

	reloader := newMinerProxyReloader(nil, nil)
	reloader.mu.Lock()
	t.Cleanup(reloader.mu.Unlock)

	// Stand-in for run's return path: the reloader stop is registered before
	// the join, so it runs on the way out exactly as in run.
	returned := make(chan error, 1)
	go func() {
		returned <- func() (err error) {
			defer reloader.stop()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var out shutdownLog
			if err := join.waitBounded(ctx, nil, providerShutdownGrace, out.logf); err != nil {
				providerShutdownExpire("workers did not drain")
				return err
			}
			return nil
		}()
	}()

	select {
	case code := <-exited:
		if code == 0 {
			t.Fatalf("process ended with code 0, want non-zero")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the process was not ended while a deferred stop was blocked")
	}
	select {
	case <-returned:
		t.Fatal("the deferred stop returned; the test did not block the return path")
	default:
	}
}

// A quarantine keeps its exit code when a shutdown timeout is joined to it.
func TestShutdownTimeoutKeepsQuarantineExitCode(t *testing.T) {
	err := errors.Join(errProviderNetworkSignInQuarantined, errProviderShutdownTimeout)
	code, ok := providerRunExitCode(err)
	if !ok || code != providerQuarantinedExitCode {
		t.Fatalf("exit code = %d (ok=%v), want quarantine code %d", code, ok, providerQuarantinedExitCode)
	}
	if code, ok := providerRunExitCode(errProviderShutdownTimeout); !ok || code != providerShutdownTimeoutExitCode {
		t.Fatalf("timeout exit code = %d (ok=%v), want %d", code, ok, providerShutdownTimeoutExitCode)
	}
}

// A clean drain never arms the exit watchdog: the arming is the timeout path's
// alone, so a normal stop keeps its exit code 0 and its logs.
func TestShutdownCleanDrainArmsNoWatchdog(t *testing.T) {
	var armed []int
	origWatchdog := providerShutdownWatchdog
	t.Cleanup(func() { providerShutdownWatchdog = origWatchdog })
	providerShutdownWatchdog = func(after time.Duration, code int) { armed = append(armed, code) }

	join := newSlotJoin()
	if !join.add() {
		t.Fatal("add refused")
	}
	go join.done()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := join.waitBounded(ctx, nil, 5*time.Second, (&shutdownLog{}).logf); err != nil {
		t.Fatalf("waitBounded = %v, want nil", err)
	}
	if len(armed) != 0 {
		t.Fatalf("a clean drain armed the watchdog with %v", armed)
	}
}

// The grace plus the exit bound must end the process inside systemd's default
// 90s stop timeout, with room for a clean drain to finish first.
func TestShutdownBudgetFitsBeforeSystemdKill(t *testing.T) {
	if total := providerShutdownGrace + providerShutdownExitBound; total > 40*time.Second {
		t.Fatalf("grace %s plus exit bound %s = %s, want at most 40s", providerShutdownGrace, providerShutdownExitBound, total)
	}
	if providerShutdownGrace < 30*time.Second {
		t.Fatalf("grace %s is too short for a clean drain", providerShutdownGrace)
	}
}
