package miner

// Ledger persistence: a restart must not re-learn the pool, and a hot-swap
// pair must not erase each other's fresher verdicts.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testV6Path(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "v6-ledger.json")
}

func readV6File(t *testing.T, path string) v6LedgerFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var f v6LedgerFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse ledger: %v", err)
	}
	return f
}

func TestV6LedgerFileRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := testV6Path(t)
	ledger := newV6Ledger(func() time.Time { return now })
	ledger.Record("198.51.100.9:1080|session-a", v6Yes)
	ledger.Record("198.51.100.10:1080", v6No)
	if err := newV6LedgerPersister(path, ledger, true).SaveNow(); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded := loadV6Ledger(path, func() time.Time { return now })
	if got := loaded.Verdict("198.51.100.9:1080|session-a"); got != v6Yes {
		t.Fatalf("reloaded verdict = %v, want yes", got)
	}
	if got := loaded.Verdict("198.51.100.10:1080"); got != v6No {
		t.Fatalf("reloaded verdict = %v, want no", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("a temporary file survived the write: %s", entry.Name())
		}
	}
}

func TestV6LedgerFileUnreadableIsEmpty(t *testing.T) {
	path := testV6Path(t)
	if err := os.WriteFile(path, []byte(`{"version":99,"entries":{"x":{"verdict":"no"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadV6Ledger(path, time.Now).Verdict("x"); got != v6Unknown {
		t.Fatalf("verdict from a future-format file = %v, want unknown", got)
	}
}

// The hot-swap case: neither side may erase the other's fresher entry.
func TestV6LedgerPersisterMergesNewerPerKey(t *testing.T) {
	t1 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	path := testV6Path(t)

	parent := newV6Ledger(func() time.Time { return t1 })
	parent.Record("shared", v6Yes)
	parent.Record("parent-only", v6No)
	if err := newV6LedgerPersister(path, parent, true).SaveNow(); err != nil {
		t.Fatal(err)
	}
	child := newV6Ledger(func() time.Time { return t2 })
	child.Record("shared", v6No)
	child.Record("child-only", v6Yes)
	if err := newV6LedgerPersister(path, child, true).SaveNow(); err != nil {
		t.Fatal(err)
	}
	merged := readV6File(t, path)
	if got := merged.Entries["shared"].Verdict; got != v6No {
		t.Fatalf("shared verdict = %v, want the child's newer no", got)
	}
	if _, ok := merged.Entries["parent-only"]; !ok {
		t.Fatal("the child's write erased the parent's entry")
	}
	late := newV6Ledger(func() time.Time { return t1 })
	late.Record("shared", v6Yes)
	if err := newV6LedgerPersister(path, late, true).SaveNow(); err != nil {
		t.Fatal(err)
	}
	if got := readV6File(t, path).Entries["shared"].Verdict; got != v6No {
		t.Fatalf("a late older write overwrote the newer verdict: %v", got)
	}

	// and the reader side of a takeover
	readerClock := t1
	reader := newV6Ledger(func() time.Time { return readerClock })
	reader.Record("shared", v6Yes)
	readerClock = t2
	changed := reader.mergeNewer(readV6LedgerEntries(path))
	if got := reader.Verdict("shared"); got != v6No || len(changed) != 3 {
		t.Fatalf("merge from disk: shared %v, changed %v", got, changed)
	}
}

// A candidate is a reader until it owns the file; a stopped parent writes no
// more.
func TestV6LedgerPersisterOwnership(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := testV6Path(t)
	ledger := newV6Ledger(func() time.Time { return now })
	ledger.Record("a", v6Yes)
	candidate := newV6LedgerPersister(path, ledger, false)
	candidate.MarkDirty()
	candidate.FlushIfDue()
	if err := candidate.SaveNow(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a candidate that does not own the file wrote it")
	}
	candidate.SetOwner(true)
	if got := readV6File(t, path).Entries["a"].Verdict; got != v6Yes {
		t.Fatalf("taking ownership did not write: %v", got)
	}

	candidate.Stop()
	ledger.Record("a", v6No)
	candidate.MarkDirty()
	_ = candidate.SaveNow()
	if got := readV6File(t, path).Entries["a"].Verdict; got != v6Yes {
		t.Fatalf("a stopped persister wrote %v", got)
	}
}

// A burst writes once per interval; the change is not lost, it waits for the
// next tick.
func TestV6LedgerPersisterBatches(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	clock := t0
	path := testV6Path(t)
	ledger := newV6Ledger(func() time.Time { return t0 })
	persister := newV6LedgerPersister(path, ledger, true)
	persister.now = func() time.Time { return clock }

	ledger.Record("a", v6Yes)
	persister.MarkDirty()
	persister.FlushIfDue()
	ledger.Record("a", v6No)
	persister.MarkDirty()
	persister.FlushIfDue()
	if got := readV6File(t, path).Entries["a"].Verdict; got != v6Yes {
		t.Fatalf("the second write inside the interval was not batched: %v", got)
	}
	clock = clock.Add(v6LedgerSaveInterval)
	persister.FlushIfDue()
	if got := readV6File(t, path).Entries["a"].Verdict; got != v6No {
		t.Fatalf("the batched change never reached disk: %v", got)
	}
	// nothing new, nothing written
	before, _ := os.Stat(path)
	clock = clock.Add(v6LedgerSaveInterval)
	persister.FlushIfDue()
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a clean ledger was written again")
	}
}

// Concurrent writers of one process are serialized; the file always parses.
func TestV6LedgerPersisterConcurrentWritesDoNotCorrupt(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := testV6Path(t)
	ledger := newV6Ledger(func() time.Time { return now })
	ledger.Record("keep", v6Yes)
	persister := newV6LedgerPersister(path, ledger, true)
	if err := persister.SaveNow(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 20; j++ {
				_ = persister.SaveNow()
			}
		}()
	}
	persister.Stop()
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := readV6File(t, path).Entries["keep"].Verdict; got != v6Yes {
		t.Fatalf("the entry recorded before Stop is %v", got)
	}
}
