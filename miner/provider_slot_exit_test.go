package miner

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// A slot leaves with context.Canceled whenever the run or the reloader stops
// it. That is the routine exit the tally records as stopped, not a worker
// failure, so it must not reach the diagnostics.
func TestProviderSlotFailureSkipsCleanCancellation(t *testing.T) {
	// The exporter delivers in order, so a record the cancelled exit emitted
	// would arrive before this control. One delivered record afterwards is the
	// proof that nothing else was offered.
	control := func(t *testing.T, output *providerDiagnostics, sink *providerDiagnosticRecorder) {
		t.Helper()
		output.observe(providerStarted, 0, true, nil, 0, nil)
		raw := sink.next(t)
		if !bytes.Contains(raw, []byte(`"event":"provider_started"`)) {
			t.Fatalf("the cancelled exit reported a worker failure: %s", raw)
		}
		if delivered := output.snapshot().Runtime.Delivered; delivered != 1 {
			t.Fatalf("runtime delivered %d diagnostics, want only the control: a clean stop was reported as a failure", delivered)
		}
	}

	t.Run("the reloader cancels the slot", func(t *testing.T) {
		sink := &providerDiagnosticRecorder{records: make(chan []byte, 4)}
		output := newProviderDiagnosticTestOwner(t, sink)
		bringUp := newProviderBringUpTracker(1)

		reportProviderSlotFailure(t.Context(), output, bringUp, 4, context.Canceled)
		control(t, output, sink)

		bringUp.mu.Lock()
		failed := bringUp.failed
		bringUp.mu.Unlock()
		if failed != 0 {
			t.Fatalf("bring-up counted %d worker failures for a cancelled slot", failed)
		}
	})

	t.Run("the run stops every slot", func(t *testing.T) {
		sink := &providerDiagnosticRecorder{records: make(chan []byte, 4)}
		output := newProviderDiagnosticTestOwner(t, sink)
		bringUp := newProviderBringUpTracker(1)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		// An error left over from the teardown of a stopping run is still a
		// stop, not a worker failure.
		reportProviderSlotFailure(ctx, output, bringUp, 0, errors.New("synthetic teardown error"))
		control(t, output, sink)

		bringUp.mu.Lock()
		failed := bringUp.failed
		bringUp.mu.Unlock()
		if failed != 0 {
			t.Fatalf("bring-up counted %d worker failures while the run was stopping", failed)
		}
	})

	t.Run("a real failure is still one", func(t *testing.T) {
		sink := &providerDiagnosticRecorder{records: make(chan []byte, 4)}
		output := newProviderDiagnosticTestOwner(t, sink)
		bringUp := newProviderBringUpTracker(1)

		reportProviderSlotFailure(t.Context(), output, bringUp, 2, errors.New("synthetic slot failure"))

		raw := sink.next(t)
		if !bytes.Contains(raw, []byte(`"event":"provider_worker_failed"`)) || !bytes.Contains(raw, []byte(`"provider":2`)) {
			t.Fatalf("a failed slot did not report provider_worker_failed: %s", raw)
		}
		bringUp.mu.Lock()
		failed := bringUp.failed
		bringUp.mu.Unlock()
		if failed != 1 {
			t.Fatalf("bring-up counted %d worker failures, want 1", failed)
		}
	})
}
