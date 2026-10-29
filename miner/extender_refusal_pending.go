package miner

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A refusal the listener has decided but the store has not kept is held here,
// for the process rather than for the listener that decided it. Every budget
// transaction charges what is held before it decides anything
// (mutateExtenderActivationRecord), so admission cannot pass a refusal that is
// only in memory: either the transaction keeps the refusal and sees its wait,
// or it fails and holds the role. A worker retries while anything is held, so
// a store that comes back with nothing else asking still keeps the refusal.
//
// Each held refusal is also written to its own file in the state directory,
// named extender-refusal-<attempt>.json, because the process can end before the store
// takes it: a restart, or a hotswap whose parent has released the store to
// its candidate. The next process, or the candidate, reads those files in the
// same transactions and removes each one only after the store has kept it. A
// file read twice is charged once, because the record remembers each attempt.

// extenderPendingRefusal is one refusal waiting to be kept.
type extenderPendingRefusal struct {
	account string
	attempt string
	// journaled is set once the refusal's file is durable.
	journaled bool
}

// extenderRefusalJournalFile is one held refusal on disk.
type extenderRefusalJournalFile struct {
	Account string `json:"account"`
	Attempt string `json:"attempt"`
	AtUnix  int64  `json:"at_unix"`
}

// The files live in the state directory itself, which already exists: a
// directory created for them would need its own name to be durable first,
// which Windows cannot assure.
const extenderRefusalJournalPrefix = "extender-refusal-"

// extenderRefusalRetryInterval paces the worker. A variable so a test can
// shorten it.
var extenderRefusalRetryInterval = providerStoreRetryInterval

// extenderRefusalWriteAttempts counts the transactions run only to keep held
// refusals.
var extenderRefusalWriteAttempts atomic.Int64

var extenderRefusalsHeld struct {
	mu     sync.Mutex
	items  map[string]extenderPendingRefusal
	worker bool
	// retryAt paces the callers: after a failed attempt none tries again
	// before it, however many status callbacks arrive.
	retryAt time.Time
	// generation ends a worker started before a reset.
	generation uint64
}

// holdExtenderRefusal keeps a decided refusal until a transaction charges it.
func holdExtenderRefusal(account, attempt string) {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	if extenderRefusalsHeld.items == nil {
		extenderRefusalsHeld.items = make(map[string]extenderPendingRefusal)
	}
	refusal := extenderPendingRefusal{account: account, attempt: attempt}
	refusal.journaled = journalExtenderRefusal(refusal) == nil
	extenderRefusalsHeld.items[attempt] = refusal
}

// journalExtenderRefusal publishes one refusal's file by rename, so a reader
// sees a whole file or none.
func journalExtenderRefusal(refusal extenderPendingRefusal) error {
	dir, err := providerStateDir()
	if err != nil {
		return err
	}
	data, err := json.Marshal(extenderRefusalJournalFile{Account: refusal.account, Attempt: refusal.attempt, AtUnix: time.Now().Unix()})
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".tmp-"+extenderRefusalJournalPrefix+"*")
	if err != nil {
		return err
	}
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		// Durable on every platform, unlike a rename and a directory sync,
		// which Windows cannot flush.
		err = providerStoreDurableRename(temp.Name(), filepath.Join(dir, extenderRefusalJournalName(refusal.attempt)))
	}
	if err != nil {
		_ = os.Remove(temp.Name())
		tlog("⚠️ [extender] could not write held refusal %s: %v\n", refusal.attempt, err)
	}
	return err
}

func extenderRefusalJournalName(attempt string) string {
	return extenderRefusalJournalPrefix + hex.EncodeToString([]byte(attempt)) + ".json"
}

// readExtenderRefusalJournal returns the refusals on disk. A directory that
// cannot be read is an error, so the transaction fails and holds the role
// rather than deciding without refusals it cannot see.
func readExtenderRefusalJournal(now time.Time) ([]extenderPendingRefusal, error) {
	dir, err := providerStateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refusals []extenderPendingRefusal
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), extenderRefusalJournalPrefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			// Kept and removed by another process since the listing.
			continue
		}
		if err != nil {
			return nil, err
		}
		var file extenderRefusalJournalFile
		if json.Unmarshal(data, &file) != nil || file.Attempt == "" {
			tlog("⚠️ [extender] dropping unreadable held refusal %s\n", path)
			_ = os.Remove(path)
			continue
		}
		// Past the decay a refusal no longer escalates anything, and the
		// record has forgotten the attempt, so charging it would count it
		// twice.
		if now.Sub(time.Unix(file.AtUnix, 0)) > extenderRefusalDecay {
			_ = os.Remove(path)
			continue
		}
		refusals = append(refusals, extenderPendingRefusal{account: file.Account, attempt: file.Attempt, journaled: true})
	}
	return refusals, nil
}

// snapshotExtenderRefusals is what the next transaction charges: what this
// process holds and what any process left on disk.
func snapshotExtenderRefusals(now time.Time) ([]extenderPendingRefusal, error) {
	onDisk, err := readExtenderRefusalJournal(now)
	if err != nil {
		return nil, err
	}
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	pending := make([]extenderPendingRefusal, 0, len(extenderRefusalsHeld.items)+len(onDisk))
	for _, refusal := range extenderRefusalsHeld.items {
		pending = append(pending, refusal)
	}
	for _, refusal := range onDisk {
		if _, held := extenderRefusalsHeld.items[refusal.attempt]; !held {
			pending = append(pending, refusal)
		}
	}
	return pending, nil
}

// settleExtenderRefusals drops refusals a committed transaction has kept, the
// file after the store, so no crash between them loses the refusal.
func settleExtenderRefusals(kept []extenderPendingRefusal) {
	dir, dirErr := providerStateDir()
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	for _, refusal := range kept {
		delete(extenderRefusalsHeld.items, refusal.attempt)
		if dirErr == nil {
			_ = os.Remove(filepath.Join(dir, extenderRefusalJournalName(refusal.attempt)))
		}
	}
}

// journalHeldExtenderRefusals writes the files that failed when the refusal was
// held, so a process that ends before the store returns still leaves them.
func journalHeldExtenderRefusals() {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	for attempt, refusal := range extenderRefusalsHeld.items {
		if !refusal.journaled && journalExtenderRefusal(refusal) == nil {
			refusal.journaled = true
			extenderRefusalsHeld.items[attempt] = refusal
		}
	}
}

func extenderRefusalsHeldCount() int {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	return len(extenderRefusalsHeld.items)
}

// flushExtenderRefusals tries to keep what is held, and starts the worker when
// the store will not take it. Status callbacks are not paced, so after a
// failure this does nothing until the retry interval has passed.
func flushExtenderRefusals() {
	extenderRefusalsHeld.mu.Lock()
	paced := time.Now().Before(extenderRefusalsHeld.retryAt)
	extenderRefusalsHeld.mu.Unlock()
	if paced || keepExtenderRefusals() == nil {
		return
	}
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	extenderRefusalsHeld.retryAt = time.Now().Add(extenderRefusalRetryInterval)
	if !extenderRefusalsHeld.worker {
		extenderRefusalsHeld.worker = true
		go retryExtenderRefusals(extenderRefusalsHeld.generation)
	}
}

// keepExtenderRefusals runs an empty transaction, which charges what is held.
func keepExtenderRefusals() error {
	pending, err := snapshotExtenderRefusals(time.Now())
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	extenderRefusalWriteAttempts.Add(1)
	return mutateExtenderActivationRecord(extenderActivationAccountKey(pending[0].account), func(*extenderActivationRecord) error { return nil })
}

func retryExtenderRefusals(generation uint64) {
	for {
		time.Sleep(extenderRefusalRetryInterval)
		extenderRefusalsHeld.mu.Lock()
		if generation != extenderRefusalsHeld.generation {
			extenderRefusalsHeld.mu.Unlock()
			return
		}
		extenderRefusalsHeld.mu.Unlock()
		journalHeldExtenderRefusals()
		_ = keepExtenderRefusals()
		extenderRefusalsHeld.mu.Lock()
		if generation != extenderRefusalsHeld.generation || len(extenderRefusalsHeld.items) == 0 {
			if generation == extenderRefusalsHeld.generation {
				extenderRefusalsHeld.worker = false
			}
			extenderRefusalsHeld.mu.Unlock()
			return
		}
		extenderRefusalsHeld.mu.Unlock()
	}
}

// resetExtenderRefusals forgets what is held and ends the worker. Tests use it
// between state directories.
func resetExtenderRefusals() {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	extenderRefusalsHeld.items = nil
	extenderRefusalsHeld.worker = false
	extenderRefusalsHeld.retryAt = time.Time{}
	extenderRefusalsHeld.generation++
}
