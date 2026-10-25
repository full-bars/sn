package miner

// Persistence for the per-proxy IPv6 verdicts.
//
// The verdict decides the transport shape at the NEXT build, so it is only
// worth anything if it survives a restart. It lives in its own small file
// rather than in proxy.state: proxy.state is rewritten whole on every change,
// and with hundreds of identities a v6 flip there would mean a whole-file
// rewrite per flip and a last-writer-wins race with a hot-swap successor.
//
// Three properties make the file safe to share with a hot-swap child:
//   - writes merge on the way out, newer CheckedAt winning per key, so neither
//     side can erase the other's fresher evidence;
//   - a write is a temp file plus rename, so a reader never sees a partial one;
//   - the persister can be stopped, and the drain path calls that before the
//     parent's closing transports start producing failures.
//
// A file that does not parse, or that was written by a future format, is
// treated as absent: an empty ledger means unknown, which means the group for
// one run. Dropping evidence is recoverable; trusting a misread file is not.

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// v6LedgerSaveInterval bounds how often a verdict change reaches disk. A burst
// of flips in one restart writes once, and the drain path calls SaveNow so
// nothing is lost at shutdown.
const v6LedgerSaveInterval = 30 * time.Second

// loadV6Ledger reads the ledger at path. A missing, unreadable or unparseable
// file yields an empty ledger: unknown verdicts, so every identity gets the
// group once and re-learns.
func loadV6Ledger(path string, now func() time.Time) *v6Ledger {
	ledger := newV6Ledger(now)
	if path == "" {
		return ledger
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ledger
	}
	if err := json.Unmarshal(b, ledger); err != nil {
		return newV6Ledger(now)
	}
	return ledger
}

// v6LedgerPersister writes the ledger without letting a write storm or a
// hot-swap handover corrupt it.
type v6LedgerPersister struct {
	path   string
	ledger *v6Ledger

	mu       sync.Mutex
	stopped  bool
	lastSave time.Time
	// now is a seam for the save throttle, so tests do not sleep.
	now func() time.Time
}

func newV6LedgerPersister(path string, ledger *v6Ledger) *v6LedgerPersister {
	return &v6LedgerPersister{path: path, ledger: ledger, now: time.Now}
}

// Save writes at most once per v6LedgerSaveInterval. It is the hook the
// observer calls on a verdict change, so it must never block a dial for long:
// the merge reads one small file under the ledger's own lock only for the
// snapshot.
func (self *v6LedgerPersister) Save() {
	self.mu.Lock()
	if self.stopped {
		self.mu.Unlock()
		return
	}
	if self.lastSave != (time.Time{}) && self.now().Sub(self.lastSave) < v6LedgerSaveInterval {
		self.mu.Unlock()
		return
	}
	self.lastSave = self.now()
	self.mu.Unlock()
	_ = self.SaveNow()
}

// SaveNow merges and writes, ignoring the throttle.
func (self *v6LedgerPersister) SaveNow() error {
	self.mu.Lock()
	if self.stopped {
		self.mu.Unlock()
		return nil
	}
	self.mu.Unlock()
	return self.saveMerged()
}

// Stop performs a final merge and write and then refuses further writes. The
// drain path calls it before its closing transports can produce failures that
// would otherwise overwrite a successor's fresher entries.
func (self *v6LedgerPersister) Stop() {
	self.mu.Lock()
	if self.stopped {
		self.mu.Unlock()
		return
	}
	self.stopped = true
	self.mu.Unlock()
	_ = self.saveMerged()
}

// saveMerged writes the union of what is on disk and what is in memory, newer
// CheckedAt winning per key.
func (self *v6LedgerPersister) saveMerged() error {
	if self.path == "" || self.ledger == nil {
		return nil
	}

	self.ledger.mu.Lock()
	mine := make(map[string]v6Entry, len(self.ledger.entries))
	for k, v := range self.ledger.entries {
		mine[k] = v
	}
	self.ledger.mu.Unlock()

	if b, err := os.ReadFile(self.path); err == nil {
		var onDisk v6LedgerFile
		if err := json.Unmarshal(b, &onDisk); err == nil && onDisk.Version == v6LedgerVersion {
			for k, theirs := range onDisk.Entries {
				ours, ok := mine[k]
				if !ok || theirs.CheckedAt.After(ours.CheckedAt) {
					mine[k] = theirs
				}
			}
		}
	}

	out, err := json.Marshal(v6LedgerFile{Version: v6LedgerVersion, Entries: mine})
	if err != nil {
		return fmt.Errorf("marshal v6 ledger: %w", err)
	}
	tmp := self.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return fmt.Errorf("write v6 ledger: %w", err)
	}
	if err := os.Rename(tmp, self.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename v6 ledger: %w", err)
	}
	return nil
}

// The process keeps one ledger: the verdict is per proxy, not per slot call,
// and every provider slot in a run must read the same start state.
var (
	processV6LedgerOnce sync.Once
	processV6Ledger     *v6Ledger
	processV6Persister  *v6LedgerPersister
)

// v6Verdicts is the run-wide ledger, loaded on first use. A run with no state
// dir still gets a usable in-memory ledger; it just cannot persist.
func v6Verdicts() *v6Ledger {
	processV6LedgerOnce.Do(func() {
		processV6Ledger = loadV6Ledger(mustStatePathQuiet("v6-ledger.json"), nil)
	})
	return processV6Ledger
}

// v6Persister is the writer for the run-wide ledger.
func v6Persister() *v6LedgerPersister {
	processV6LedgerOnce.Do(func() {
		processV6Ledger = loadV6Ledger(mustStatePathQuiet("v6-ledger.json"), nil)
	})
	if processV6Persister == nil {
		processV6Persister = newV6LedgerPersister(mustStatePathQuiet("v6-ledger.json"), processV6Ledger)
	}
	return processV6Persister
}
