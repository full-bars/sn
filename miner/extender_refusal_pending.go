package miner

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
// A status callback queues what it decided (a refusal, the spare that funds a
// retry, a success) and does not write the store itself. The queue is applied,
// in the order it was decided, by whichever transaction comes next, so a burst
// of callbacks costs one transaction and one journal file per pace interval
// and loses nothing: admission applies the queue before it decides, so it is
// closed to a refusal that is only in the queue.
//
// Held refusals are also written to files in the state directory, named
// extender-refusal-<id>.json, because the process can end before the store
// takes them: a restart, or a hotswap whose parent has released the store to
// its candidate. One file carries every refusal held when it was written. The
// next process, or the candidate, reads those files in the same transactions
// and removes each one only after the store has kept all of it. A file read
// twice is charged once, because the record remembers each attempt.

// The barrier orders publication against admission. An admission reads what is
// held, then decides and commits; a refusal published between its read and its
// commit would be missed, and the admission would record a post into a wait the
// refusal already set. So both run under one barrier: a process-local mutex and
// a lock file in the state directory, which a hotswap parent and its candidate
// both take. A publication either finishes before an admission reads, and is
// charged by it, or starts after the admission commits, and is charged by the
// next transaction.
//
// Lock order is the barrier first, then the held mutex. Nothing takes the
// barrier while it holds the held mutex, and nothing holds the held mutex
// across the disk or the store: it guards memory only, so what only reads or
// changes memory (unseal, the at-risk count) is never stuck behind a write
// that stalls. The barrier is what a stalled write keeps, and with it every
// publication and admission waits, which is the failure closed.
var extenderRefusalBarrierMu sync.Mutex

// extenderRefusalBarrierName is the lock file's base name; acquireFileLock
// adds its own suffix, which keeps it clear of the journal's .json files.
const extenderRefusalBarrierName = "extender-refusal-barrier"

// lockExtenderRefusalBarrier takes the barrier. The release is always valid.
// The error says the lock file could not be taken, so the interprocess half is
// not held: an admission must then fail, and a publication holds the refusal
// in memory without a file.
func lockExtenderRefusalBarrier() (func(), error) {
	extenderRefusalBarrierMu.Lock()
	releaseFile, err := lockExtenderRefusalBarrierFile()
	return func() {
		if releaseFile != nil {
			releaseFile()
		}
		extenderRefusalBarrierMu.Unlock()
	}, err
}

// lockExtenderRefusalBarrierFile takes the interprocess half of the barrier.
// The caller holds extenderRefusalBarrierMu.
func lockExtenderRefusalBarrierFile() (func(), error) {
	dir, err := providerStateDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return acquireFileLock(filepath.Join(dir, extenderRefusalBarrierName))
}

// errExtenderRefusalBarrierBusy says a bounded wait ended with the barrier, or
// a store or disk call made under it, still busy.
var errExtenderRefusalBarrierBusy = errors.New("still blocked on the refusal barrier, the state store or the disk")

// lockExtenderRefusalBarrierBy takes the barrier unless the deadline passes
// first. Taking it blocks on a mutex and a lock file, neither of which a clock
// can interrupt, so the wait runs aside; one that wins after the deadline
// releases at once.
func lockExtenderRefusalBarrierBy(deadline time.Time) (func(), error) {
	type taken struct {
		release func()
		err     error
	}
	result := make(chan taken, 1)
	var mu sync.Mutex
	gaveUp := false
	go func() {
		release, err := lockExtenderRefusalBarrier()
		mu.Lock()
		defer mu.Unlock()
		if gaveUp {
			release()
			return
		}
		result <- taken{release, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case got := <-result:
		return got.release, got.err
	case <-timer.C:
	}
	mu.Lock()
	defer mu.Unlock()
	select {
	case got := <-result:
		return got.release, got.err
	default:
		gaveUp = true
		return func() {}, errExtenderRefusalBarrierBusy
	}
}

// extenderAdmissionSnapshotHook runs between an admission's read of the held
// refusals and its transaction. A test uses it to publish a refusal at the one
// moment that used to be missed.
var extenderAdmissionSnapshotHook func()

// extenderPendingKind is what a queued operation does to the record.
type extenderPendingKind int

const (
	extenderOpRefusal extenderPendingKind = iota
	extenderOpSpare
	extenderOpSuccess
)

// extenderPendingOp is one decision waiting to be kept.
type extenderPendingOp struct {
	kind    extenderPendingKind
	account string
	// attempt names a refusal, which the record charges once.
	attempt string
	family  extenderActivationFamily
	at      time.Time
	seq     uint64
	// journaled is set once a refusal's file is durable, and journalFile
	// names it.
	journaled   bool
	journalFile string
}

// extenderRefusalJournalEntry is one held refusal on disk.
type extenderRefusalJournalEntry struct {
	Account string `json:"account"`
	Attempt string `json:"attempt"`
	AtUnix  int64  `json:"at_unix"`
}

// extenderRefusalJournalFile is one journal file. A build that wrote one
// refusal per file filled the entry's fields at the top level, so those are
// still read.
type extenderRefusalJournalFile struct {
	extenderRefusalJournalEntry
	Refusals []extenderRefusalJournalEntry `json:"refusals,omitempty"`
}

// The files live in the state directory itself, which already exists: a
// directory created for them would need its own name to be durable first,
// which Windows cannot assure.
const extenderRefusalJournalPrefix = "extender-refusal-"

// extenderRefusalRetryInterval paces the worker after the store failed.
const extenderRefusalRetryInterval = providerStoreRetryInterval

// extenderPersistPace bounds what status callbacks drive: at most one store
// transaction and one journal file per interval, however many callbacks
// arrive. The rest of the queue waits for the worker, or for any admission,
// which applies it all. Nanoseconds in an atomic so a test can set it while a
// worker runs.
var extenderPersistPace atomic.Int64

func init() { extenderPersistPace.Store(int64(time.Second)) }

func persistPace() time.Duration { return time.Duration(extenderPersistPace.Load()) }

// extenderRefusalWriteAttempts counts the transactions run only to keep what
// is held, and extenderRefusalJournalWrites the journal files written.
var (
	extenderRefusalWriteAttempts atomic.Int64
	extenderRefusalJournalWrites atomic.Int64
)

var extenderRefusalsHeld struct {
	mu  sync.Mutex
	ops []extenderPendingOp
	seq uint64
	// worker is set while a goroutine is applying the queue.
	worker bool
	// retryAt paces the callbacks' store transactions: none starts before
	// it, however many status callbacks arrive. journalAt does the same for
	// journal files.
	retryAt   time.Time
	journalAt time.Time
	// generation ends a worker started before a reset.
	generation uint64
	// sealed is set once the process has committed to giving up its memory (a
	// handoff past its gate): a refusal decided from then on is journaled at
	// once, ignoring the pace, because the successor reads only files.
	sealed bool
}

// queueExtenderOpLocked appends one operation. The caller holds the barrier
// and the held mutex.
func queueExtenderOpLocked(op extenderPendingOp) {
	extenderRefusalsHeld.seq++
	op.seq = extenderRefusalsHeld.seq
	extenderRefusalsHeld.ops = append(extenderRefusalsHeld.ops, op)
}

// holdExtenderRefusal queues a decided refusal until a transaction charges it,
// and journals it unless a file was written within the pace. Once sealed, a
// refusal the journal cannot take keeps the barrier (keepSealedRefusal), so the
// successor cannot admit while this process alone holds it.
func holdExtenderRefusal(account, attempt string) {
	extenderRefusalBarrierMu.Lock()
	releaseFile, barrierErr := lockExtenderRefusalBarrierFile()
	release := func() {
		if releaseFile != nil {
			releaseFile()
		}
		extenderRefusalBarrierMu.Unlock()
	}
	extenderRefusalsHeld.mu.Lock()
	for _, op := range extenderRefusalsHeld.ops {
		if op.kind == extenderOpRefusal && op.attempt == attempt {
			extenderRefusalsHeld.mu.Unlock()
			release()
			return
		}
	}
	queueExtenderOpLocked(extenderPendingOp{kind: extenderOpRefusal, account: account, attempt: attempt, at: time.Now()})
	sealed := extenderRefusalsHeld.sealed
	journalDue := !time.Now().Before(extenderRefusalsHeld.journalAt)
	extenderRefusalsHeld.mu.Unlock()
	// Without the interprocess half of the barrier the file could appear
	// after an admission read, so the refusal stays in memory, where this
	// process's own admissions still charge it.
	keep := false
	if sealed {
		keep = barrierErr != nil || journalQueuedRefusals() != nil
	} else if barrierErr == nil && journalDue {
		_ = journalQueuedRefusals()
	}
	extenderRefusalsHeld.mu.Lock()
	if hasUnjournaledRefusalLocked() {
		startExtenderWorkerLocked()
	}
	extenderRefusalsHeld.mu.Unlock()
	if keep {
		go keepSealedRefusal(releaseFile)
		return
	}
	release()
}

// keepSealedRefusal holds the barrier for a sealed process whose refusal the
// journal did not take, and retries the journal until it does, or until the
// handoff is abandoned. The candidate's admission needs the same barrier, so it
// waits rather than deciding without a refusal only this process knows. The
// process's own bounded wait still ends it: this goroutine holds the barrier
// to the last, and the lock file is released with the process. The caller
// hands over extenderRefusalBarrierMu and, when it was taken, the lock file.
func keepSealedRefusal(releaseFile func()) {
	defer func() {
		if releaseFile != nil {
			releaseFile()
		}
		extenderRefusalBarrierMu.Unlock()
	}()
	for {
		time.Sleep(extenderRefusalGateRetry)
		if releaseFile == nil {
			releaseFile, _ = lockExtenderRefusalBarrierFile()
		}
		extenderRefusalsHeld.mu.Lock()
		sealed := extenderRefusalsHeld.sealed
		extenderRefusalsHeld.mu.Unlock()
		if !sealed || (releaseFile != nil && journalQueuedRefusals() == nil) {
			return
		}
	}
}

// holdExtenderSpare queues the spare that funds a retry of family.
func holdExtenderSpare(account string, family extenderActivationFamily) {
	release, _ := lockExtenderRefusalBarrier()
	defer release()
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	queueExtenderOpLocked(extenderPendingOp{kind: extenderOpSpare, account: account, family: family, at: time.Now()})
}

// holdExtenderSuccess queues the success that clears an account's refusal.
func holdExtenderSuccess(account string) {
	release, _ := lockExtenderRefusalBarrier()
	defer release()
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	queueExtenderOpLocked(extenderPendingOp{kind: extenderOpSuccess, account: account, at: time.Now()})
}

func hasUnjournaledRefusalLocked() bool {
	for _, op := range extenderRefusalsHeld.ops {
		if op.kind == extenderOpRefusal && !op.journaled {
			return true
		}
	}
	return false
}

// journalQueuedRefusals writes one file holding every refusal not yet
// journaled, and marks them. The caller holds the barrier, which keeps every
// other journal write and every settle out, and not the held mutex: the file is
// written with that mutex free and the refusals marked by sequence after.
func journalQueuedRefusals() error {
	extenderRefusalsHeld.mu.Lock()
	var entries []extenderRefusalJournalEntry
	written := make(map[uint64]bool)
	for _, op := range extenderRefusalsHeld.ops {
		if op.kind == extenderOpRefusal && !op.journaled {
			entries = append(entries, extenderRefusalJournalEntry{Account: op.account, Attempt: op.attempt, AtUnix: op.at.Unix()})
			written[op.seq] = true
		}
	}
	if len(entries) > 0 {
		extenderRefusalsHeld.journalAt = time.Now().Add(persistPace())
	}
	extenderRefusalsHeld.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	name, err := journalExtenderRefusals(entries)
	if err != nil {
		return err
	}
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	for i := range extenderRefusalsHeld.ops {
		if op := &extenderRefusalsHeld.ops[i]; written[op.seq] {
			op.journaled, op.journalFile = true, name
		}
	}
	return nil
}

// journalExtenderRefusals publishes one file by rename, so a reader sees a
// whole file or none, and returns its name.
func journalExtenderRefusals(entries []extenderRefusalJournalEntry) (string, error) {
	extenderRefusalJournalWrites.Add(1)
	dir, err := providerStateDir()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(extenderRefusalJournalFile{Refusals: entries})
	if err != nil {
		return "", err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	name := extenderRefusalJournalPrefix + hex.EncodeToString(id) + ".json"
	temp, err := os.CreateTemp(dir, ".tmp-"+extenderRefusalJournalPrefix+"*")
	if err != nil {
		return "", err
	}
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		// Fails where the platform cannot make the name crash-durable, so a
		// refusal is never reported journaled on the strength of a rename
		// that a power loss could undo.
		err = providerStoreDurableRename(temp.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(temp.Name())
		tlog("⚠️ [extender] could not write held refusals: %v\n", err)
		return "", err
	}
	return name, nil
}

// extenderRefusalSnapshot is what one transaction applies, and what it removes
// once it has committed.
type extenderRefusalSnapshot struct {
	ops []extenderPendingOp
	// files are the journal files the snapshot read, all of whose refusals
	// are in ops.
	files []string
}

// readExtenderRefusalJournal returns the refusals on disk and the files they
// came from. A directory that cannot be read is an error, so the transaction
// fails and holds the role rather than deciding without refusals it cannot
// see.
func readExtenderRefusalJournal(now time.Time) ([]extenderPendingOp, []string, error) {
	dir, err := providerStateDir()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var ops []extenderPendingOp
	var files []string
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
			return nil, nil, err
		}
		var file extenderRefusalJournalFile
		if json.Unmarshal(data, &file) != nil {
			tlog("⚠️ [extender] dropping unreadable held refusal %s\n", path)
			_ = os.Remove(path)
			continue
		}
		refusals := file.Refusals
		if file.Attempt != "" {
			refusals = append(refusals, file.extenderRefusalJournalEntry)
		}
		files = append(files, path)
		for _, refusal := range refusals {
			// Past the decay a refusal no longer escalates anything, and the
			// record has forgotten the attempt, so charging it would count
			// it twice.
			at := time.Unix(refusal.AtUnix, 0)
			if refusal.Attempt == "" || now.Sub(at) > extenderRefusalDecay {
				continue
			}
			ops = append(ops, extenderPendingOp{kind: extenderOpRefusal, account: refusal.Account, attempt: refusal.Attempt, at: at, journaled: true, journalFile: entry.Name()})
		}
	}
	return ops, files, nil
}

// snapshotExtenderRefusals is what the next transaction applies: what any
// process left on disk, then what this process holds in the order it was
// decided. The caller holds the barrier, which keeps anything from being
// queued until it has committed.
func snapshotExtenderRefusals(now time.Time) (extenderRefusalSnapshot, error) {
	onDisk, files, err := readExtenderRefusalJournal(now)
	if err != nil {
		return extenderRefusalSnapshot{}, err
	}
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	held := make(map[string]bool)
	for _, op := range extenderRefusalsHeld.ops {
		if op.kind == extenderOpRefusal {
			held[op.attempt] = true
		}
	}
	snapshot := extenderRefusalSnapshot{files: files}
	for _, op := range onDisk {
		if !held[op.attempt] {
			snapshot.ops = append(snapshot.ops, op)
		}
	}
	snapshot.ops = append(snapshot.ops, extenderRefusalsHeld.ops...)
	return snapshot, nil
}

// settleExtenderRefusals drops what a committed transaction has kept, the
// files after the store, so no crash between them loses a refusal.
func settleExtenderRefusals(kept extenderRefusalSnapshot) {
	dir, dirErr := providerStateDir()
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	applied := make(map[uint64]bool, len(kept.ops))
	for _, op := range kept.ops {
		if op.seq != 0 {
			applied[op.seq] = true
		}
	}
	files := append([]string(nil), kept.files...)
	remaining := extenderRefusalsHeld.ops[:0]
	for _, op := range extenderRefusalsHeld.ops {
		if applied[op.seq] {
			if op.journalFile != "" && dirErr == nil {
				files = append(files, filepath.Join(dir, op.journalFile))
			}
			continue
		}
		remaining = append(remaining, op)
	}
	extenderRefusalsHeld.ops = remaining
	extenderRefusalsHeld.mu.Unlock()
	for _, path := range files {
		_ = os.Remove(path)
	}
	extenderRefusalsHeld.mu.Lock()
}

// journalHeldExtenderRefusals writes the file for what was not journaled when
// it was held, so a process that ends before the store returns still leaves it.
func journalHeldExtenderRefusals() error {
	release, barrierErr := lockExtenderRefusalBarrier()
	defer release()
	if barrierErr != nil {
		return barrierErr
	}
	return journalQueuedRefusals()
}

// extenderRefusalsHeldCount is the number of refusals not yet kept.
func extenderRefusalsHeldCount() int {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	count := 0
	for _, op := range extenderRefusalsHeld.ops {
		if op.kind == extenderOpRefusal {
			count++
		}
	}
	return count
}

// flushExtenderRefusals applies the queue when a callback is the first in a
// pace interval, and leaves it to the worker otherwise. Status callbacks are
// not paced, so a burst of them runs one transaction.
func flushExtenderRefusals() {
	extenderRefusalsHeld.mu.Lock()
	if len(extenderRefusalsHeld.ops) == 0 {
		extenderRefusalsHeld.mu.Unlock()
		return
	}
	if time.Now().Before(extenderRefusalsHeld.retryAt) {
		startExtenderWorkerLocked()
		extenderRefusalsHeld.mu.Unlock()
		return
	}
	extenderRefusalsHeld.mu.Unlock()
	err := keepExtenderRefusals()
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	if err != nil {
		extenderRefusalsHeld.retryAt = time.Now().Add(extenderRefusalRetryInterval)
	} else {
		extenderRefusalsHeld.retryAt = time.Now().Add(persistPace())
	}
	if len(extenderRefusalsHeld.ops) > 0 {
		startExtenderWorkerLocked()
	}
}

// startExtenderWorkerLocked starts the worker if none runs.
func startExtenderWorkerLocked() {
	if !extenderRefusalsHeld.worker {
		extenderRefusalsHeld.worker = true
		go retryExtenderRefusals(extenderRefusalsHeld.generation)
	}
}

// keepExtenderRefusals runs an empty transaction, which applies what is
// queued.
func keepExtenderRefusals() error {
	now := time.Now()
	release, err := lockExtenderRefusalBarrier()
	var snapshot extenderRefusalSnapshot
	if err == nil {
		snapshot, err = snapshotExtenderRefusals(now)
	}
	release()
	if err != nil {
		return err
	}
	if len(snapshot.ops) == 0 {
		return nil
	}
	extenderRefusalWriteAttempts.Add(1)
	return mutateExtenderActivationRecord(extenderActivationAccountKey(snapshot.ops[0].account), func(*extenderActivationRecord) error { return nil })
}

// retryExtenderRefusals applies the queue until it is empty: at the pace while
// the store takes it, and at the retry interval while it does not.
func retryExtenderRefusals(generation uint64) {
	for {
		extenderRefusalsHeld.mu.Lock()
		wait := time.Until(extenderRefusalsHeld.retryAt)
		extenderRefusalsHeld.mu.Unlock()
		if wait < persistPace() {
			wait = persistPace()
		}
		time.Sleep(wait)
		extenderRefusalsHeld.mu.Lock()
		if generation != extenderRefusalsHeld.generation {
			extenderRefusalsHeld.mu.Unlock()
			return
		}
		extenderRefusalsHeld.mu.Unlock()
		_ = journalHeldExtenderRefusals()
		err := keepExtenderRefusals()
		extenderRefusalsHeld.mu.Lock()
		if generation != extenderRefusalsHeld.generation {
			extenderRefusalsHeld.mu.Unlock()
			return
		}
		if err != nil {
			extenderRefusalsHeld.retryAt = time.Now().Add(extenderRefusalRetryInterval)
		} else {
			extenderRefusalsHeld.retryAt = time.Now().Add(persistPace())
		}
		if len(extenderRefusalsHeld.ops) == 0 {
			extenderRefusalsHeld.worker = false
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
	extenderRefusalsHeld.ops = nil
	extenderRefusalsHeld.worker = false
	extenderRefusalsHeld.retryAt = time.Time{}
	extenderRefusalsHeld.journalAt = time.Time{}
	extenderRefusalsHeld.sealed = false
	extenderRefusalsHeld.generation++
}

// extenderRefusalGateRetry paces the wait of a process that cannot end yet,
// and extenderRefusalGateDeadline bounds it. Variables so a test can shorten
// them.
var (
	extenderRefusalGateRetry    = time.Second
	extenderRefusalGateDeadline = 30 * time.Second
)

// extenderRefusalsAtRisk counts the refusals that are neither kept by the
// store nor in a durable file, which is what a process end would lose.
func extenderRefusalsAtRisk() int {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	count := 0
	for _, op := range extenderRefusalsHeld.ops {
		if op.kind == extenderOpRefusal && !op.journaled {
			count++
		}
	}
	return count
}

// ensureExtenderRefusalsDurable tries to commit what is held and, failing
// that, to journal it, ignoring the pace. It fails while any refusal is still
// neither, so a caller that is about to give up the store or the process does
// not do it on top of a decision that only memory holds. Where the platform
// cannot make a journal file crash-durable (durablesync.ErrNotCrashDurable)
// only a committed refusal counts, so this fails until the store takes it.
func ensureExtenderRefusalsDurable() error {
	_ = keepExtenderRefusals()
	_ = journalHeldExtenderRefusals()
	if at := extenderRefusalsAtRisk(); at > 0 {
		return fmt.Errorf("%d decided activation refusals are neither in the state store nor in a durable file", at)
	}
	return nil
}

// sealExtenderRefusals is the gate a handoff passes before it gives up this
// process's memory. It makes every decided refusal durable, and fails while one
// is not. Under the barrier it then seals the queue, so a refusal decided after
// the gate is journaled where the successor reads it, in the same barrier hold
// its admission needs; none can sit in memory between the gate and the
// successor's first decision. A refusal that cannot be journaled even then
// stays counted at risk, which the wait below reports.
func sealExtenderRefusals() error {
	if err := ensureExtenderRefusalsDurable(); err != nil {
		return err
	}
	release, err := lockExtenderRefusalBarrier()
	defer release()
	if err != nil {
		// Without the lock file a journal written from here could appear
		// after the candidate's read, so the seal promises nothing.
		return fmt.Errorf("refusal barrier: %w", err)
	}
	extenderRefusalsHeld.mu.Lock()
	extenderRefusalsHeld.sealed = true
	extenderRefusalsHeld.mu.Unlock()
	// A refusal queued between the gate and the seal.
	if err := journalQueuedRefusals(); err != nil {
		unsealExtenderRefusals()
		return fmt.Errorf("%d decided activation refusals are neither in the state store nor in a durable file", extenderRefusalsAtRisk())
	}
	return nil
}

// unsealExtenderRefusals returns to paced journaling after a handoff is
// abandoned and the process keeps serving. It takes only the held mutex, which
// no disk write holds, so it returns while a journal write is stalled; the
// write keeps the barrier until it returns, and keepSealedRefusal sees the
// seal gone at its next turn.
func unsealExtenderRefusals() {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	extenderRefusalsHeld.sealed = false
}

// extenderDurableAttempts counts the durability attempts still running, so a
// test can let one that outlived its deadline finish.
var extenderDurableAttempts sync.WaitGroup

// awaitExtenderRefusalsDurable retries ensureExtenderRefusalsDurable until it
// succeeds or the deadline passes. Each attempt runs aside, because it takes the
// barrier and does store and disk I/O, none of which a clock can interrupt; at
// the deadline an attempt still running is left behind. The error says which
// way it ended: an attempt still blocked, or attempts that kept failing.
func awaitExtenderRefusalsDurable(deadline time.Time) error {
	started := time.Now()
	var lastErr error
	for {
		done := make(chan error, 1)
		extenderDurableAttempts.Add(1)
		go func() {
			defer extenderDurableAttempts.Done()
			done <- ensureExtenderRefusalsDurable()
		}()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case err := <-done:
			timer.Stop()
			if err == nil {
				return nil
			}
			lastErr = err
		case <-timer.C:
			if lastErr == nil {
				return fmt.Errorf("gave up after %s, %w", time.Since(started).Round(time.Millisecond), errExtenderRefusalBarrierBusy)
			}
			return fmt.Errorf("gave up after %s, %w (the attempt before failed: %v)", time.Since(started).Round(time.Millisecond), errExtenderRefusalBarrierBusy, lastErr)
		}
		if !time.Now().Add(extenderRefusalGateRetry).Before(deadline) {
			return fmt.Errorf("the state store and the journal stayed unwritable for %s: %w", time.Since(started).Round(time.Millisecond), lastErr)
		}
		time.Sleep(extenderRefusalGateRetry)
	}
}

// waitExtenderRefusalsDurable waits for a decided refusal that only memory
// holds to reach the store or a file, because a process that ends first lets
// its successor, or its own next start, post into a wait the operator already
// set. The wait is bounded: a store and a journal that stay unwritable (a full
// or read-only disk, or a platform that cannot make the journal durable once
// the store is released), or a call that hangs under the barrier, must not keep
// the process from ending. Time is reserved for the recovery record, but a
// blocked record is abandoned at the same deadline and may never reach disk.
var extenderLossAttempts sync.WaitGroup

func waitExtenderRefusalsDurable(what string) bool {
	deadline := time.Now().Add(extenderRefusalGateDeadline)
	err := awaitExtenderRefusalsDurable(time.Now().Add(extenderRefusalGateDeadline * 2 / 3))
	if err == nil {
		return true
	}
	done := make(chan struct{})
	extenderLossAttempts.Add(1)
	go func() {
		defer extenderLossAttempts.Done()
		recordExtenderRefusalsLost(what, err)
		close(done)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	return false
}

// recordExtenderRefusalsLost is what a process that ends past a refusal it could
// not keep leaves behind. An elapsed deadline proves only that the attempts
// failed until then, not that the disk stays unwritable, so first it writes the
// recovery marker, which holds every account's extender role for the longest
// refusal wait at the next decision of any process (providerStoreRecoveryUntil):
// the lost refusal's wait is kept, over-long, rather than forgotten. Then it
// records the loss on disk wherever a line can be made durable, the state
// directory's events.log first. Where nothing can be written, stdout is all
// that is left, and the next start may post into the operator's wait.
func recordExtenderRefusalsLost(what string, cause error) {
	at := extenderRefusalsAtRisk()
	holdErr := holdExtenderRoleAfterLostRefusal(time.Now())
	hold := "the extender role is held for " + formatDuration(extenderRefusalBackoffMax) + " instead"
	if holdErr != nil {
		hold = fmt.Sprintf("no hold could be written either (%v), so the next start may post into the operator's wait", holdErr)
	}
	line := fmt.Sprintf("EXTENDER REFUSALS LOST at %s: %d decided refusals neither in the state store nor in a durable file: %v; %s", what, at, cause, hold)
	tlog("❌ [extender] %s\n", line)
	if where, err := writeExtenderLossRecord(line); err != nil {
		tlog("❌ [extender] no crash-durable loss record confirmed: %v\n", err)
	} else {
		tlog("[extender] the loss above is recorded in %s\n", where)
	}
}

// holdExtenderRoleAfterLostRefusal extends the recovery marker to cover the
// longest refusal wait from now.
func holdExtenderRoleAfterLostRefusal(now time.Time) error {
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		return err
	}
	return extendProviderStoreRecovery(path, now.Add(extenderRefusalBackoffMax))
}

// extenderLossRecordFallbacks are where the loss is recorded when the state
// directory takes nothing. A variable so a test can point it elsewhere.
var extenderLossRecordFallbacks = func() []string {
	paths := []string{filepath.Join(os.TempDir(), "urnetwork-miner-events.log")}
	if cache, err := os.UserCacheDir(); err == nil {
		paths = append(paths, filepath.Join(cache, "urnetwork-miner", "events.log"))
	}
	return paths
}

// writeExtenderLossRecord appends line to the first log that takes it durably,
// and says which.
func writeExtenderLossRecord(line string) (string, error) {
	var paths []string
	if path, err := critLogPath(); err == nil {
		paths = append(paths, path)
	}
	paths = append(paths, extenderLossRecordFallbacks()...)
	var errs []error
	for _, path := range paths {
		err := critLogDurable(path, line)
		if err == nil {
			return path, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}
	return "", errors.Join(errs...)
}

// quiesceExtenderRefusalsForExec is the last check before execve replaces the
// image. It takes the barrier, by the deadline, and checks under it that no
// refusal is held only in memory. The caller keeps the barrier until execve, so
// no callback can queue a refusal between the check and the exec: one that
// arrives waits at the barrier and ends with the image, as a refusal the SDK
// has not yet delivered ends with any process. The release is always valid.
func quiesceExtenderRefusalsForExec(deadline time.Time) (func(), error) {
	release, err := lockExtenderRefusalBarrierBy(deadline)
	if err != nil {
		return release, err
	}
	if at := extenderRefusalsAtRisk(); at > 0 {
		return release, fmt.Errorf("%d decided activation refusals are neither in the state store nor in a durable file", at)
	}
	return release, nil
}

// extenderRefusalBlocksUncountedLaunch says why a launch made without the
// budget would post into a refusal's wait, or nil when nothing says so: a
// refusal this process holds, one in the journal, or a hold that stands in for
// one. A journal or marker that cannot be read counts as one.
func extenderRefusalBlocksUncountedLaunch(now time.Time) error {
	if held := extenderRefusalsHeldCount(); held > 0 {
		return fmt.Errorf("%d decided activation refusals are not yet in the state store", held)
	}
	onDisk, _, err := readExtenderRefusalJournal(now)
	if err != nil {
		return fmt.Errorf("the held refusals cannot be read: %w", err)
	}
	if len(onDisk) > 0 {
		return fmt.Errorf("%d journaled activation refusals are not yet in the state store", len(onDisk))
	}
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		return err
	}
	until, err := providerStoreRecoveryUntil(path, now)
	if err != nil {
		return fmt.Errorf("the recovery hold cannot be read: %w", err)
	}
	if now.Unix() < until {
		return fmt.Errorf("a recovery hold stands for %s", formatDuration(time.Duration(until-now.Unix())*time.Second))
	}
	return nil
}
