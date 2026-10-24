//go:build !windows

package miner

// The refusal path writes the action ledger like the restart path does, and the
// append ends in a blocking flock. These tests take that lock by hand, which is
// what a second provider process or a slow state volume does, so they live in a
// unix-only file: windows locks the same file through LockFileEx.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// holdLedgerLock takes the inter-process append lock the action ledger uses and
// holds it until release runs, so a test can prove a watchdog sample does not
// wait on a contended ledger. flock blocks a second open of the same file even
// inside one process: it is held per open file description.
func holdLedgerLock(t *testing.T) func() {
	t.Helper()
	path, err := ledgerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path+fileLockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			unix.Flock(int(f.Fd()), unix.LOCK_UN)
			f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// thrashRefusalRig builds a watch whose ladder refuses to restart (self-heal
// off), drives it to thrashing, and counts the incident bundles it captured, so
// the ledger assertions below can also pin the ledger/evidence pairing.
func thrashRefusalRig(t *testing.T) (watch *swapThrashWatch, clock *time.Time, evidence *int, dir string) {
	t.Helper()
	now := swapThrashT0
	// Storm readings keep growing so the ladder stays in thrashing for every
	// attempt: a repeated reading would read as zero pressure and relax it.
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{
		swapThrashCalmReading(),
		swapThrashStormReading(30 * time.Second),
		swapThrashStormReading(60 * time.Second),
		swapThrashStormReading(120 * time.Second),
		swapThrashStormReading(180 * time.Second),
		swapThrashStormReading(240 * time.Second),
	}}
	var logs []string
	var exits []int
	watch, _ = swapThrashEscalationWatch(t, sensor, &now, &logs, &exits)
	watch.selfHealFn = func() bool { return false }
	bundles := 0
	watch.evidenceFn = func(swapThrashEscalation, swapThrashRates, swapThrashRead, string, int) { bundles++ }
	// The record budget is seconds in production; shorten it so a hung ledger
	// is observable in a test-sized wait.
	previousTimeout := swapThrashRecordTimeout
	swapThrashRecordTimeout = 100 * time.Millisecond
	t.Cleanup(func() { swapThrashRecordTimeout = previousTimeout })

	path, err := ledgerPath()
	if err != nil {
		t.Fatal(err)
	}
	// Baseline, then two under-pressure samples: t+120s is thrashing.
	watch.sample(now)
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second} {
		now = swapThrashT0.Add(at)
		watch.sample(now)
	}
	return watch, &now, &bundles, filepath.Dir(path)
}

// waitForLedgerEntries waits for the bounded goroutine behind a decision to land
// its line after the test releases the lock.
func waitForLedgerEntries(t *testing.T, dir string, want int) []ledgerEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries := ledgerEntries(t, dir)
		if len(entries) >= want || time.Now().After(deadline) {
			return entries
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A refused restart is recorded in the action ledger, and that append must not
// be able to stall the sample loop: ledgerRecord ends in a blocking flock, so
// the watchdog's own claim ("nothing here blocks") only holds if the refusal
// uses the restart record's goroutine + bounded wait. A ledger held elsewhere
// must cost at most swapThrashRecordTimeout.
func TestThrashRefusalDoesNotBlockOnTheLedger(t *testing.T) {
	watch, clock, evidence, dir := thrashRefusalRig(t)
	release := holdLedgerLock(t)

	*clock = swapThrashT0.Add(120 * time.Second)
	done := make(chan struct{})
	go func() {
		watch.sample(*clock)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("sample() waited on a held ledger lock: the refusal record must be bounded like the restart record")
	}
	if watch.lastActionCode != "self-heal-off" {
		t.Fatalf("want the refusal recorded as self-heal-off, got %q", watch.lastActionCode)
	}
	// The ledger line was still owed (the goroutine is parked on the lock), and
	// the bundle that pairs with it must already be written.
	if got := ledgerEntries(t, dir); len(got) != 0 {
		t.Fatalf("nothing can be in the ledger while its lock is held, got %+v", got)
	}
	if *evidence != 1 {
		t.Fatalf("a refusal must capture its bundle with the ledger line, got %d bundles", *evidence)
	}

	release()
	entries := waitForLedgerEntries(t, dir, 1)
	if len(entries) != 1 {
		t.Fatalf("the bounded refusal record must land once the lock frees, got %+v", entries)
	}
	if e := entries[0]; e.Actor != "thrash" || e.Action != "alert" || e.Reason == "" {
		t.Fatalf("refusal entry: %+v", e)
	}
}

// One ledger line per distinct refusal code: the retry interval re-attempts the
// gate, and the text of a denial embeds changing durations, so the dedupe is by
// code - the same code must not append again, a new code must.
func TestThrashRefusalLedgersOneLinePerDistinctCode(t *testing.T) {
	watch, clock, evidence, dir := thrashRefusalRig(t)

	*clock = swapThrashT0.Add(120 * time.Second)
	watch.sample(*clock)
	if got := ledgerEntries(t, dir); len(got) != 1 {
		t.Fatalf("first refusal: want one entry, got %+v", got)
	}

	// The retry interval would throttle the next attempt; the dedupe is what
	// this test pins, so let the attempt through and keep the code the same.
	watch.machine.lastAttempt = time.Time{}
	*clock = swapThrashT0.Add(180 * time.Second)
	watch.sample(*clock)
	if got := ledgerEntries(t, dir); len(got) != 1 {
		t.Fatalf("a repeat of the same refusal code must not re-ledger, got %+v", got)
	}
	if *evidence != 1 {
		t.Fatalf("a deduped refusal must not capture a second bundle, got %d", *evidence)
	}

	// A different gate now refuses: a new code is a new decision.
	watch.selfHealFn = func() bool { return true }
	watch.hotSwapBusyFn = func() bool { return true }
	watch.machine.lastAttempt = time.Time{}
	*clock = swapThrashT0.Add(240 * time.Second)
	watch.sample(*clock)
	entries := ledgerEntries(t, dir)
	if len(entries) != 2 {
		t.Fatalf("a new refusal code must ledger its own line, got %+v", entries)
	}
	if e := entries[1]; e.Actor != "thrash" || e.Action != "alert" || !strings.Contains(e.Reason, "hot-swap") {
		t.Fatalf("second refusal entry: %+v", e)
	}
	if *evidence != 2 {
		t.Fatalf("each distinct refusal must capture one bundle, got %d", *evidence)
	}
}
