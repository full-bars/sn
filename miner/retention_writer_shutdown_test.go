package miner

// retention_writer_shutdown_test.go covers the retention writer's shutdown
// contract: a writer that panics must still end with the run (its supervisor is
// the only thing that closes the done channel that flushRetentionEvents waits
// on), the flush must be bounded even when the writer cannot finish, and the
// orderly channel-close path must still write the buffered events.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingRetentionWriter stands in for the file writer so a test can assert
// what the loop wrote, and in what batches.
type recordingRetentionWriter struct {
	mu      sync.Mutex
	batches [][]string
	closed  bool
}

func (w *recordingRetentionWriter) write(events []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.batches = append(w.batches, append([]string(nil), events...))
}

func (w *recordingRetentionWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

func (w *recordingRetentionWriter) snapshot() (batches [][]string, closed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.batches, w.closed
}

// resetRetentionEventWriterForTest revives the process-wide writer so a test can
// exercise its shutdown contract. It first waits for any writer left by an
// earlier test (bounded now, so it cannot hang the suite), then clears the
// singleton: the once, the channels, the closed flag and the registered run
// context.
func resetRetentionEventWriterForTest(t *testing.T) {
	t.Helper()
	flushRetentionEvents()
	retentionEventMu.Lock()
	retentionEventClosed = false
	retentionEventCh = nil
	retentionEventDone = nil
	retentionEventMu.Unlock()
	retentionEventOnce = sync.Once{}
	retentionEventRunMu.Lock()
	retentionEventRunCtx = nil
	retentionEventWriterCancel = nil
	retentionEventRunMu.Unlock()
}

// TestRetentionWriterPanicStillEndsWithCancelledRun is the shutdown hang the
// review found: the writer's context used to be a background one that only the
// writer itself cancelled, so a writer panicking on every attempt left the
// supervisor doubling its backoff forever, retentionEventDone never closed, and
// flushRetentionEvents — called by the hotswap drain monitor before the parent
// exits — blocked for good. Deriving the writer's context from the run's makes a
// cancelled run end the supervisor, so the done channel closes on the panic path
// too.
func TestRetentionWriterPanicStillEndsWithCancelledRun(t *testing.T) {
	resetRetentionEventWriterForTest(t)
	t.Cleanup(func() { resetRetentionEventWriterForTest(t) })

	origNewWriter := newRetentionWriter
	t.Cleanup(func() { newRetentionWriter = origNewWriter })

	attempted := make(chan struct{}, 1)
	var attempts atomic.Int64
	newRetentionWriter = func() retentionWriter {
		if attempts.Add(1) == 1 {
			attempted <- struct{}{}
		}
		panic("retention writer constructor")
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	setRetentionEventRunContext(runCtx)

	startRetentionEventWriter()

	select {
	case <-attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervised retention writer never ran")
	}

	// The run ends. That must end the writer's supervisor — which closes
	// retentionEventDone — even though every writer attempt panics and the
	// supervisor is backing off.
	cancelRun()

	flushed := make(chan struct{})
	go func() {
		flushRetentionEvents()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("flushRetentionEvents blocked after the run was cancelled: the panicking writer's supervisor was never stopped")
	}
}

// TestRetentionWriterFlushIsBounded covers the second half: even if the writer
// is wedged (a stuck write, or a panic the run has not cancelled), the flush
// must return rather than hold shutdown and the parent's drain exit forever.
func TestRetentionWriterFlushIsBounded(t *testing.T) {
	wedged := make(chan struct{}) // never closed: a writer that cannot finish
	runCtx, cancelRun := context.WithCancel(context.Background())

	start := time.Now()
	if waitRetentionWriterDone(wedged, runCtx, 150*time.Millisecond, time.Second) {
		t.Fatal("reported the writer done when its done channel was never closed")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the wait took %s, want about the 150ms timeout", elapsed)
	}

	// Once the run has ended the writer is already being stopped, so it gets
	// only the grace, not the full timeout.
	cancelRun()
	start = time.Now()
	if waitRetentionWriterDone(wedged, runCtx, 10*time.Second, 150*time.Millisecond) {
		t.Fatal("reported the writer done after the run ended, with its done channel still open")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the wait took %s after the run ended, want about the 150ms grace", elapsed)
	}

	done := make(chan struct{})
	close(done)
	if !waitRetentionWriterDone(done, context.Background(), time.Second, time.Second) {
		t.Fatal("a finished writer was reported as timed out")
	}
}

// TestRetentionWriterOrderlyCloseStillDrains pins the behaviour the fix must not
// change: closing the channel makes the loop write what is buffered exactly
// once, in one batch, and stop.
func TestRetentionWriterOrderlyCloseStillDrains(t *testing.T) {
	events := make(chan string, 4)
	events <- "a"
	events <- "b"
	events <- "c"
	close(events)

	w := &recordingRetentionWriter{}
	retentionEventWriteLoop(context.Background(), events, w, func() {})

	batches, closed := w.snapshot()
	if len(batches) != 1 {
		t.Fatalf("wrote %d batches, want 1: %v", len(batches), batches)
	}
	if len(batches[0]) != 3 || batches[0][0] != "a" || batches[0][1] != "b" || batches[0][2] != "c" {
		t.Fatalf("batch = %v, want [a b c]", batches[0])
	}
	if closed {
		t.Fatal("the loop closed a writer it did not open; the supervisor's defer owns that")
	}
}

// TestRetentionWriterDrainsBufferedEventsOnCancelledRun covers the cancel path:
// a run ending without a flush (no channel close) must not drop the events that
// were already buffered, and must not wait for more.
func TestRetentionWriterDrainsBufferedEventsOnCancelledRun(t *testing.T) {
	events := make(chan string, 4)
	events <- "x"
	events <- "y"

	runCtx, cancelRun := context.WithCancel(context.Background())
	cancelRun()

	w := &recordingRetentionWriter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		retentionEventWriteLoop(runCtx, events, w, func() {})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the write loop did not end with the cancelled run")
	}

	batches, _ := w.snapshot()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("batches = %v, want one batch of both buffered events", batches)
	}
}

// TestRetentionWriterSkipsEmptyBatches guards the guard: the cancel path can end
// with nothing buffered, and an empty write would still open and stat the file.
func TestRetentionWriterSkipsEmptyBatches(t *testing.T) {
	events := make(chan string)
	runCtx, cancelRun := context.WithCancel(context.Background())
	cancelRun()

	w := &recordingRetentionWriter{}
	retentionEventWriteLoop(runCtx, events, w, func() {})

	batches, _ := w.snapshot()
	if len(batches) != 0 {
		t.Fatalf("wrote %v with nothing buffered, want no writes", batches)
	}
}
