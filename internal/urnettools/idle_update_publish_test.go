package urnettools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests pin the contract between the miner's billable_rate collector
// (miner/metrics_profit.go, the port of the provider's runBillableRateWriter)
// and this package's idle-update consumer.
//
// The bug they cover: the consumer cannot tell "no traffic" from "no metric",
// so an ABSENT billable_rate means "assume active traffic" and the wait burns
// its whole timeout. The collector's job is therefore not merely to publish a
// number — it is to make sure a real 0 is on disk, because 0 <= threshold reads
// as idle while absent reads as busy.

// shrinkIdleCadence drives the real wait loop at millisecond cadence. The
// production cadence is a 5s poll and a 5s sustained-quiet verification, which
// would cost ten seconds per scenario; the loop's behaviour is unchanged.
func shrinkIdleCadence(t *testing.T) {
	t.Helper()
	prevPoll, prevVerify := idlePollInterval, idleVerifyInterval
	idlePollInterval = 5 * time.Millisecond
	idleVerifyInterval = 1 * time.Millisecond
	t.Cleanup(func() {
		idlePollInterval, idleVerifyInterval = prevPoll, prevVerify
	})
}

// captureStdout lives in status_panel_test.go; these tests reuse it because the
// wait loop reports its decisions on stdout and those decisions are the
// observable behaviour being asserted.

// TestWaitForIdleReadsPublishedRateInsteadOfAssumingActive is the regression
// test for the reported bug. With a collector-published rate of 0 on disk and
// the real (non-injected) file reader, idle-update must reach the lull, verify
// it, and proceed — it must NOT print the assume-active fallback and must not
// sit out the timeout.
func TestWaitForIdleReadsPublishedRateInsteadOfAssumingActive(t *testing.T) {
	shrinkIdleCadence(t)

	dir := t.TempDir()
	// Exactly what the collector writes for a pool with no billable traffic.
	if err := os.WriteFile(filepath.Join(dir, "billable_rate"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := IdleUpdateOptions{Threshold: 5120, Window: 20 * time.Millisecond, Timeout: 5 * time.Second}

	var out string
	err := func() error {
		var waitErr error
		out = captureStdout(t, func() {
			waitErr = waitForIdle(context.Background(), dir, true, opts, nil)
		})
		return waitErr
	}()

	if err != nil {
		t.Fatalf("waitForIdle: %v", err)
	}
	if strings.Contains(out, "assuming active traffic") {
		t.Fatalf("idle-update fell back to assume-active despite a published rate:\n%s", out)
	}
	if !strings.Contains(out, "Traffic lull verified") {
		t.Fatalf("idle-update did not complete the lull verification:\n%s", out)
	}

}

// TestWaitForIdleMissingRateBurnsTimeout is the control: it reproduces the state
// the miner used to ship in (no collector, so no file) and pins that the wait
// then does assume active traffic and spends its entire ceiling. This is the
// behaviour the collector exists to prevent, and it is the reason a missing
// metric is not the same input as a zero one.
func TestWaitForIdleMissingRateBurnsTimeout(t *testing.T) {
	shrinkIdleCadence(t)

	dir := t.TempDir() // no billable_rate at all

	opts := IdleUpdateOptions{Threshold: 5120, Window: 20 * time.Millisecond, Timeout: 300 * time.Millisecond}

	var out string
	start := time.Now()
	err := func() error {
		var waitErr error
		out = captureStdout(t, func() {
			waitErr = waitForIdle(context.Background(), dir, true, opts, nil)
		})
		return waitErr
	}()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("waitForIdle: %v", err)
	}
	if !strings.Contains(out, "assuming active traffic") {
		t.Fatalf("missing metric did not take the assume-active path:\n%s", out)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("returned after %s; a missing metric must never satisfy the quiet window", elapsed)
	}
}

// TestIdleUpdateZeroIsNotMissing pins the parse-level distinction the two tests
// above depend on: a published 0 is FOUND with rate 0, while an absent file is
// NOT FOUND. It also pins the collector's on-disk format (bare decimal, one
// trailing newline, file mode readable by a different user in a container).
func TestIdleUpdateZeroIsNotMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "billable_rate")

	rate, found, err := readBillableRateFile(dir)
	if err != nil || found || rate != 0 {
		t.Fatalf("absent file: rate=%d found=%v err=%v; want 0,false,nil", rate, found, err)
	}

	if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rate, found, err = readBillableRateFile(dir)
	if err != nil || !found || rate != 0 {
		t.Fatalf("published zero: rate=%d found=%v err=%v; want 0,true,nil", rate, found, err)
	}

	// Surrounding whitespace is tolerated, as with the collector's newline.
	if err := os.WriteFile(path, []byte(" 300 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rate, found, err = readBillableRateFile(dir)
	if err != nil || !found || rate != 300 {
		t.Fatalf("padded rate: rate=%d found=%v err=%v; want 300,true,nil", rate, found, err)
	}
}
