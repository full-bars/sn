package miner

// Ledger persistence: the verdict decides the next build's transport shape, so
// losing it costs dual-stack for a restart, and a last-writer-wins file costs
// whichever side wrote second. These tests cover both.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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

// TestV6LedgerFileRoundTrip: what a run records, the next run reads.
func TestV6LedgerFileRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := testV6Path(t)

	ledger := newV6Ledger(func() time.Time { return now })
	ledger.Record("198.51.100.9:1080|session-a", v6Yes)
	ledger.Record("198.51.100.10:1080", v6No)
	persister := newV6LedgerPersister(path, ledger)
	if err := persister.SaveNow(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded := loadV6Ledger(path, func() time.Time { return now })
	if got := loaded.Verdict("198.51.100.9:1080|session-a"); got != v6Yes {
		t.Fatalf("reloaded verdict = %v, want yes", got)
	}
	if got := loaded.Verdict("198.51.100.10:1080"); got != v6No {
		t.Fatalf("reloaded verdict = %v, want no", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temporary file survived the write")
	}
}

// TestV6LedgerFileUnreadableIsEmpty: a corrupt or future-format file must not
// be trusted. Empty means unknown, which means every identity gets its group.
func TestV6LedgerFileUnreadableIsEmpty(t *testing.T) {
	path := testV6Path(t)
	if err := os.WriteFile(path, []byte(`{"version":99,"entries":{"x":{"verdict":"no"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ledger := loadV6Ledger(path, time.Now)
	if got := ledger.Verdict("x"); got != v6Unknown {
		t.Fatalf("verdict from a future-format file = %v, want unknown", got)
	}
}

// TestV6LedgerPersisterMergesNewerPerKey is the hot-swap case: parent and child
// each hold part of the picture, and neither may erase the other's fresher
// entry for a key.
func TestV6LedgerPersisterMergesNewerPerKey(t *testing.T) {
	t1 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	path := testV6Path(t)

	parent := newV6Ledger(func() time.Time { return t1 })
	parent.Record("shared", v6Yes)
	parent.Record("parent-only", v6No)
	if err := newV6LedgerPersister(path, parent).SaveNow(); err != nil {
		t.Fatal(err)
	}

	child := newV6Ledger(func() time.Time { return t2 })
	child.Record("shared", v6No)
	child.Record("child-only", v6Yes)
	if err := newV6LedgerPersister(path, child).SaveNow(); err != nil {
		t.Fatal(err)
	}

	merged := readV6File(t, path)
	if got := merged.Entries["shared"].Verdict; got != v6No {
		t.Fatalf("shared verdict = %v, want the child's newer no", got)
	}
	if _, ok := merged.Entries["parent-only"]; !ok {
		t.Fatal("the child's write erased the parent's entry")
	}
	if _, ok := merged.Entries["child-only"]; !ok {
		t.Fatal("the child's own entry is missing")
	}

	// And the reverse order: an older writer arriving late must not win.
	late := newV6Ledger(func() time.Time { return t1 })
	late.Record("shared", v6Yes)
	if err := newV6LedgerPersister(path, late).SaveNow(); err != nil {
		t.Fatal(err)
	}
	if got := readV6File(t, path).Entries["shared"].Verdict; got != v6No {
		t.Fatalf("a late older write overwrote the newer verdict: %v", got)
	}
}

// TestV6LedgerPersisterStops: after the drain calls Stop, closing transports
// cannot rewrite the file under a successor.
func TestV6LedgerPersisterStops(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := testV6Path(t)
	ledger := newV6Ledger(func() time.Time { return now })
	ledger.Record("a", v6Yes)
	persister := newV6LedgerPersister(path, ledger)
	persister.Stop()

	ledger.Record("a", v6No)
	if err := persister.SaveNow(); err != nil {
		t.Fatalf("save after stop: %v", err)
	}
	if got := readV6File(t, path).Entries["a"].Verdict; got != v6Yes {
		t.Fatalf("a stopped persister wrote %v; the successor's file was overwritten", got)
	}
}

// TestV6LedgerPersisterThrottles: a burst of flips writes once, and the drain's
// SaveNow still lands the result.
func TestV6LedgerPersisterThrottles(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := testV6Path(t)
	ledger := newV6Ledger(func() time.Time { return t0 })
	persister := newV6LedgerPersister(path, ledger)
	persister.now = func() time.Time { return t0 }
	ledger.Record("a", v6Yes)
	persister.Save()

	ledger.Record("a", v6No)
	persister.Save()
	if got := readV6File(t, path).Entries["a"].Verdict; got != v6Yes {
		t.Fatalf("the second write was not throttled: %v", got)
	}

	if err := persister.SaveNow(); err != nil {
		t.Fatal(err)
	}
	if got := readV6File(t, path).Entries["a"].Verdict; got != v6No {
		t.Fatalf("SaveNow did not write the latest verdict: %v", got)
	}
}

// TestV6LedgerPersisterConcurrentWritesDoNotCorrupt: writers share one temp
// path, so an unserialized pair could interleave a write and a rename and one
// could delete the other's file. Whatever the ordering, the file must parse and
// keep what was recorded before Stop.
func TestV6LedgerPersisterConcurrentWritesDoNotCorrupt(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	path := testV6Path(t)
	ledger := newV6Ledger(func() time.Time { return now })
	ledger.Record("keep", v6Yes)
	persister := newV6LedgerPersister(path, ledger)
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
		t.Fatalf("the entry recorded before Stop is %v; a concurrent write corrupted the file", got)
	}
}

// TestV6PersisterIsOneInstance: two slot setups racing must not each build a
// persister with its own mutex and throttle over one path.
func TestV6PersisterIsOneInstance(t *testing.T) {
	var wg sync.WaitGroup
	got := make([]*v6LedgerPersister, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = v6Persister()
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatal("concurrent callers got different persisters over the same file")
		}
	}
}
