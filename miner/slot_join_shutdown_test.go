package miner

import (
	"context"
	"fmt"
	"os"
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
