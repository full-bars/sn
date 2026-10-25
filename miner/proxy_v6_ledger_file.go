package miner

// Persistence for the per-proxy IPv6 verdicts (proxy_v6_probe.go).
//
// A verdict decides whether a proxied identity runs the v6 pin, so it is only
// worth anything if it survives a restart: a restart must not re-learn 500
// proxies from scratch. It lives in its own small file, v6-ledger.json, rather
// than in proxy.state, which several writers rewrite whole.
//
// Four properties make the file safe to share with a hot-swap successor:
//   - only the current OWNER writes: a hot-swap candidate starts as a reader
//     and becomes the writer at takeover, and the parent stops writing when it
//     starts draining;
//   - a write merges what is on disk with what is in memory, newer CheckedAt
//     winning per key, so neither side can erase the other's fresher evidence;
//   - a write goes to a temp file of its own and is renamed into place, so a
//     reader never sees a partial file and two processes never share a temp
//     path;
//   - writes are batched: a change marks the ledger dirty and the owner writes
//     at most once per v6LedgerSaveInterval, with a final write on Stop.
//
// A file that does not parse, or that was written by a future format, is
// treated as absent: an empty ledger means unknown, which means a probe.
// Dropping evidence is recoverable; trusting a misread file is not.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// v6LedgerSaveInterval bounds how often the ledger reaches disk.
const v6LedgerSaveInterval = 30 * time.Second

// loadV6Ledger reads the ledger at path. A missing, unreadable or unparseable
// file yields an empty ledger.
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

// readV6LedgerEntries reads the entries on disk, nil when there is no usable
// file.
func readV6LedgerEntries(path string) map[string]v6Entry {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var onDisk v6LedgerFile
	if err := json.Unmarshal(b, &onDisk); err != nil || onDisk.Version != v6LedgerVersion {
		return nil
	}
	return onDisk.Entries
}

// mergeNewer folds entries into the ledger, newer CheckedAt winning per key,
// and reports the keys whose entry changed.
func (l *v6Ledger) mergeNewer(entries map[string]v6Entry) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := []string{}
	for key, theirs := range entries {
		ours, ok := l.entries[key]
		if !ok || theirs.CheckedAt.After(ours.CheckedAt) {
			l.entries[key] = theirs
			changed = append(changed, key)
		}
	}
	return changed
}

// entry is the raw record for key, live or not.
func (l *v6Ledger) entry(key string) (v6Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	return e, ok
}

// v6LedgerPersister writes the ledger in batches, as its owner.
type v6LedgerPersister struct {
	path   string
	ledger *v6Ledger

	mu sync.Mutex
	// writeMu serializes the whole check-and-write sequence, so no write can
	// start after Stop or SetOwner(false) has returned.
	writeMu  sync.Mutex
	owner    bool
	stopped  bool
	dirty    bool
	lastSave time.Time
	// now is a seam for the batching, so tests do not sleep.
	now func() time.Time
}

// newV6LedgerPersister builds a persister that writes only once it is the
// owner: a hot-swap candidate passes false and takes ownership at takeover.
func newV6LedgerPersister(path string, ledger *v6Ledger, owner bool) *v6LedgerPersister {
	return &v6LedgerPersister{path: path, ledger: ledger, owner: owner, now: time.Now}
}

// MarkDirty records that the ledger changed. It never writes, so it is safe on
// any path, a dial included.
func (self *v6LedgerPersister) MarkDirty() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.dirty = true
}

// FlushIfDue writes when the ledger changed and the last write is at least
// v6LedgerSaveInterval old. The owner's loop calls it on its tick.
func (self *v6LedgerPersister) FlushIfDue() {
	self.mu.Lock()
	due := self.dirty && (self.lastSave.IsZero() || self.now().Sub(self.lastSave) >= v6LedgerSaveInterval)
	self.mu.Unlock()
	if due {
		_ = self.SaveNow()
	}
}

// SaveNow merges and writes, ignoring the batching, if this process is the
// owner and not stopped.
func (self *v6LedgerPersister) SaveNow() error {
	self.writeMu.Lock()
	defer self.writeMu.Unlock()
	self.mu.Lock()
	writable := self.owner && !self.stopped
	self.mu.Unlock()
	if !writable {
		return nil
	}
	return self.saveMerged()
}

// SetOwner hands the file to this process (true) or takes it away (false).
// Taking ownership writes once, so the new owner's view is on disk.
func (self *v6LedgerPersister) SetOwner(owner bool) {
	self.mu.Lock()
	self.owner = owner
	if owner {
		self.dirty = true
	}
	self.mu.Unlock()
	if owner {
		_ = self.SaveNow()
		return
	}
	// a write in flight finishes before this returns
	self.writeMu.Lock()
	self.writeMu.Unlock()
}

// Stop performs a final merge and write, if this process owns the file, and
// then refuses further writes.
func (self *v6LedgerPersister) Stop() {
	self.writeMu.Lock()
	defer self.writeMu.Unlock()
	self.mu.Lock()
	if self.stopped {
		self.mu.Unlock()
		return
	}
	self.stopped = true
	owner := self.owner
	self.mu.Unlock()
	if owner {
		_ = self.saveMerged()
	}
}

// saveMerged writes the union of what is on disk and what is in memory, newer
// CheckedAt winning per key. A failed write leaves the ledger dirty, so the
// next tick retries it. Must be called with writeMu.
func (self *v6LedgerPersister) saveMerged() error {
	err := self.writeMerged()
	if err != nil {
		self.mu.Lock()
		self.dirty = true
		self.mu.Unlock()
	}
	return err
}

func (self *v6LedgerPersister) writeMerged() error {
	if self.path == "" || self.ledger == nil {
		return nil
	}
	self.mu.Lock()
	self.dirty = false
	self.lastSave = self.now()
	self.mu.Unlock()

	self.ledger.mu.Lock()
	mine := make(map[string]v6Entry, len(self.ledger.entries))
	for k, v := range self.ledger.entries {
		mine[k] = v
	}
	self.ledger.mu.Unlock()
	for k, theirs := range readV6LedgerEntries(self.path) {
		ours, ok := mine[k]
		if !ok || theirs.CheckedAt.After(ours.CheckedAt) {
			mine[k] = theirs
		}
	}

	out, err := json.Marshal(v6LedgerFile{Version: v6LedgerVersion, Entries: mine})
	if err != nil {
		return fmt.Errorf("marshal v6 ledger: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(self.path), filepath.Base(self.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create v6 ledger temp: %w", err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(out)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write v6 ledger: %v %v", writeErr, closeErr)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod v6 ledger: %w", err)
	}
	if err := os.Rename(tmpPath, self.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename v6 ledger: %w", err)
	}
	return nil
}
