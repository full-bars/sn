package miner

// state_store.go is the provider's one persistent store for machine state.
//
// Why a store and not a file per feature: the state directory had grown one
// small json file per feature (backoff streaks, budgets, counters, markers),
// each with its own atomic-rename dance, none of them visible to anything else.
// One bbolt file gives the provider a single place to look, transactional
// writes that survive a power cut mid-update, and incremental page writes
// instead of rewriting a whole file for one counter.
//
// What stays out of it, deliberately:
//   - provider_state.json, the operator's settings. It is meant to be read and
//     edited by hand and written through the control socket; a database would
//     take that away. It stays the .json half.
//   - credentials (.client_jwts.json, .provider.key) and the proxy table. They
//     have their own rotation, backup and (later) at-rest encryption rules.
//   - append-only ledgers and logs. Sequential writes do not belong in a btree.
//
// The file is opened once per process. The provider process is the only writer
// (urnet-tools goes through the control socket), so a process-wide mutex plus
// bbolt's own single-writer transaction is all the serialization needed.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// errProviderStoreReleased reports that this process gave provider_state.db to
// a hotswap candidate. The candidate owns the file now: the parent neither
// reopens it, because that open would take the bbolt lock the candidate's next
// write waits on, nor reports a write as landed when it did not. Only an
// aborted handoff, which leaves the parent serving, takes the file back
// (restoreProviderStoreAfterAbortedHandoff).
var errProviderStoreReleased = errors.New("provider state store released to the hotswap candidate")

// errProviderStoreClosed reports a write on a handle that was closed without a
// handoff (a moved state directory, a test reset). The write did not land, and
// a caller that treats nil as success would drop a budget record.
var errProviderStoreClosed = errors.New("provider state store is closed")

const (
	providerStoreFileName = "provider_state.db"
	// The bucket for small keyed records. Each record is one JSON value.
	providerStoreBucketState = "state"
	// The record keys the provider keeps today.
	providerStoreKeyExtenderActivation = "extender_activation"
	// How long to wait for another process's lock before giving up. The
	// provider is the only writer, so a wait means a stale process is holding
	// the file and the error should say so rather than hang.
	providerStoreOpenTimeout = 2 * time.Second
)

type providerStore struct {
	mu sync.Mutex
	db *bbolt.DB
	// path is kept for error messages and for a caller that needs to report
	// where the state went.
	path string
	// released records that this process gave the file up for a hotswap
	// candidate. A save still queued on this handle reports the loss instead
	// of returning nil as if it had written.
	released bool
}

func openProviderStore(path string) (*providerStore, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// A file that did not exist before this open is a new entry in its
	// parent directory. bbolt syncs the database pages it writes, but not
	// the directory block that names the file, so a power cut just after
	// creation can leave the file unlinked or zero length.
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	// callProviderStore returns no value when fn panics, so the handle is kept
	// in opened as soon as bbolt returns it. Otherwise a panic after the open
	// would leave the file lock held by a handle nobody can close.
	var opened *bbolt.DB
	db, err := callProviderStore(func() (*bbolt.DB, error) {
		d, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: providerStoreOpenTimeout})
		if err != nil {
			return nil, err
		}
		opened = d
		if providerStoreOpenedHook != nil {
			providerStoreOpenedHook(d)
		}
		return d, nil
	})
	if err != nil {
		if opened != nil {
			_ = opened.Close()
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Deliberately no tx.Check() here. bbolt's Check runs the page walk in its
	// own goroutine, so a page-level panic inside it never passes through the
	// caller's recover and takes the process down; on a damaged-but-openable
	// file that turns a quarantine into a crash. The store relies on open-time
	// validation, on read errors, and on the same-goroutine panic containment
	// in callProviderStore instead.
	if created {
		if syncErr := providerStoreSyncDir(dir); syncErr != nil {
			// The database is open and its own pages are synced, so the
			// process can use the store; what is missing is the guarantee
			// that the directory entry survives a crash. That is worth a
			// line, not refusing a store that works.
			tlog("[state] created %s but could not sync %s: %s\n", path, dir, syncErr)
		}
	}
	store := &providerStore{db: db, path: path}
	return store, nil
}

var errProviderStoreCorrupt = errors.New("provider state store is corrupt")

// providerStoreOpenedHook runs inside the open closure once bbolt.Open has
// returned a handle. Tests use it to force a panic after the file lock is held.
var providerStoreOpenedHook func(*bbolt.DB)

func callProviderStore[T any](fn func() (T, error)) (value T, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("provider state store panic: %v", recovered)
		}
	}()
	return fn()
}

func runProviderStoreView(db *bbolt.DB, fn func(*bbolt.Tx) error) error {
	_, err := callProviderStore(func() (struct{}, error) { return struct{}{}, db.View(fn) })
	return err
}

func runProviderStoreUpdate(db *bbolt.DB, fn func(*bbolt.Tx) error) error {
	_, err := callProviderStore(func() (struct{}, error) { return struct{}{}, db.Update(fn) })
	return err
}

func (s *providerStore) write(fn func(*bbolt.Tx) error) error {
	if s.db == nil {
		if s.released {
			return errProviderStoreReleased
		}
		return errProviderStoreClosed
	}
	err := runProviderStoreUpdate(s.db, fn)
	if err != nil && !errors.Is(err, errProviderStoreReleased) {
		IncrPersistentError(providerStoreWriteFailedCategory)
	}
	return err
}

// providerStoreSyncDir flushes one directory entry, which is what makes a
// freshly created database file durable before its first write. A variable so
// a test can observe the sync and its failure without a crash.
var providerStoreSyncDir = func(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func (s *providerStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// release closes the file for a hotswap candidate and marks the handle, so a
// save that arrives behind it says the write was lost. The handle is terminal:
// nothing reopens it, and a parent whose handoff aborts opens a fresh one
// (restoreProviderStoreAfterAbortedHandoff).
func (s *providerStore) release() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = true
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// load decodes one record.
//
// The two results separate "never written" from "could not be read", because
// the callers act on them differently: an absent record starts empty and is
// saved, while a record that could not be read must not be replaced. A single
// false for both made a damaged or unavailable store look like an empty one,
// and the caller then wrote its empty record over state the store still held.
//
//	(true, nil)  the record exists and decoded
//	(false, nil) the bucket or key was never written
//	(false, err) the store is unavailable, the read failed, or the value
//	             did not decode
func (s *providerStore) load(bucket, key string, out any) (bool, error) {
	if s == nil {
		return false, errProviderStoreClosed
	}
	if out == nil {
		return false, errors.New("provider store: load into a nil destination")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.released {
			return false, errProviderStoreReleased
		}
		return false, errProviderStoreClosed
	}
	var raw []byte
	err := runProviderStoreView(s.db, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		if val := b.Get([]byte(key)); val != nil {
			raw = append([]byte(nil), val...)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if len(raw) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// Same reason as the mutation path: an unreadable value must be visible
		// as a counted failure, not just as a retry loop in a local log.
		IncrPersistentError(providerStoreDecodeFailedCategory)
		return false, fmt.Errorf("decode record %s/%s: %w", bucket, key, err)
	}
	return true, nil
}

// save writes one record in a transaction. A crash either leaves the previous
// value or this one, never a mixture. A handle that can no longer write reports
// why: a dropped budget record must not look persisted to its caller.
func (s *providerStore) save(bucket, key string, value any) error {
	if s == nil {
		return errProviderStoreClosed
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.released {
			return errProviderStoreReleased
		}
		return errProviderStoreClosed
	}
	return s.write(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), raw)
	})
}

// updateJSON reads one record into out, calls mutate with whether it existed,
// then writes out back, all inside a single transaction. The value a decision
// was made from and the value it rewrites therefore cannot be separated by
// another writer, a handle swap or a release. mutate changes out in place; an
// error aborts the transaction and leaves the stored record untouched.
func (s *providerStore) updateJSON(bucket, key string, out any, mutate func(found bool) error) error {
	if s == nil {
		return errProviderStoreClosed
	}
	if out == nil || mutate == nil {
		return errors.New("provider store: update with a nil destination or mutate")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.released {
			return errProviderStoreReleased
		}
		return errProviderStoreClosed
	}
	return s.write(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		var found bool
		if val := b.Get([]byte(key)); val != nil {
			if err := json.Unmarshal(val, out); err != nil {
				// A value this build cannot decode holds the role forever, so the
				// retry loop would otherwise be the only trace. Count it so the
				// fleet sees a record no build here can read.
				IncrPersistentError(providerStoreDecodeFailedCategory)
				return fmt.Errorf("decode record %s/%s: %w", bucket, key, err)
			}
			found = true
		}
		if err := mutate(found); err != nil {
			return err
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), raw)
	})
}

func (s *providerStore) delete(bucket, key string) error {
	if s == nil {
		return errProviderStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.released {
			return errProviderStoreReleased
		}
		return errProviderStoreClosed
	}
	return s.write(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}

// appendBounded adds one record under a sequence key and drops the record keep
// places behind it, so the bucket holds at most keep values however long the
// process runs. Order is the bucket's own sequence, never the wall clock: two
// records in the same second must not collide on a key. The append and the
// drop share one transaction, so a reader never meets a key without its value
// and a crash cannot leave the bucket a record too long.
func (s *providerStore) appendBounded(bucket string, value any, keep int) error {
	return s.appendBoundedMany(bucket, []any{value}, keep)
}

// appendBoundedMany adds records in one transaction, with the same bound and
// key discipline as appendBounded. A start that adopts a whole pool writes them
// this way: one commit instead of one per slot, so the startup does not wait on
// a disk sync per identity before any worker runs.
func (s *providerStore) appendBoundedMany(bucket string, values []any, keep int) error {
	if s == nil {
		return errProviderStoreClosed
	}
	if len(values) == 0 {
		return nil
	}
	raw := make([][]byte, 0, len(values))
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		raw = append(raw, encoded)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		// The caller records outcomes and markers through this path, so a
		// dropped append has to say so: nil here would report a write that
		// never landed, the same way save and delete would.
		if s.released {
			return errProviderStoreReleased
		}
		return errProviderStoreClosed
	}
	return s.write(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		for _, value := range raw {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, seq)
			if err := b.Put(key, value); err != nil {
				return err
			}
			if keep <= 0 || seq <= uint64(keep) {
				continue
			}
			// This append created the oldest record worth keeping, so the one
			// before it is the only record that has to go.
			oldest := make([]byte, 8)
			binary.BigEndian.PutUint64(oldest, seq-uint64(keep))
			if err := b.Delete(oldest); err != nil {
				return err
			}
		}
		return nil
	})
}

// tail returns up to n records in insertion order, oldest first. A negative n
// is every record; a bucket that was never written is no records, not an
// error.
func (s *providerStore) tail(bucket string, n int) ([][]byte, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, nil
	}
	var values [][]byte
	err := runProviderStoreView(s.db, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		cursor := b.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			values = append(values, append([]byte(nil), value...))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if n >= 0 && len(values) > n {
		values = values[len(values)-n:]
	}
	return values, nil
}

var (
	globalProviderStoreMu   sync.Mutex
	globalProviderStore     *providerStore
	globalProviderStorePath string
	// globalProviderStoreReleased is set once provider_state.db is handed to a
	// hotswap candidate and stays set: the candidate owns the file now, so the
	// parent must not open it again. A lazy reopen in the drain would take the
	// bbolt lock the candidate's first write waits on. Only an aborted handoff
	// clears it, because the parent keeps serving and has to write its state.
	globalProviderStoreReleased bool
	// A failed open is not retried on every call. bbolt's open waits on the
	// file lock, so a second process or a damaged file would otherwise stall
	// every caller that asks for the store and repeat the same log line for as
	// long as the process lives. The failure is counted, and the open is tried
	// again on a timer or immediately when the state directory moves.
	globalProviderStoreFailedPath string
	globalProviderStoreFailedErr  error
	globalProviderStoreRetryAt    time.Time
)

// providerStoreRetryInterval bounds how often a failed open is retried.
const providerStoreRetryInterval = 30 * time.Second

// providerStoreUnavailableCategory is the persistent error category the scrape
// exposes for a store that would not open, so a host running without its store
// is visible instead of only logged.
const providerStoreUnavailableCategory = "state_store_unavailable"
const providerStoreCorruptCategory = "state_store_corrupt"
const providerStoreWriteFailedCategory = "state_store_write_failed"
const providerStoreDecodeFailedCategory = "state_store_decode_failed"

// providerStoreInstance returns the process-wide store, opening it on first
// use. A nil store is not fatal: callers fall back to their previous storage
// and the provider keeps running.
func providerStoreInstance() *providerStore {
	store, _ := openProviderStoreForProcess()
	return store
}

// openProviderStoreForProcess is providerStoreInstance with the reason it
// refused, so a caller can tell a file this process released to a hotswap
// candidate (errProviderStoreReleased) from one that would not open.
//
// The path is re-checked on every call because the state directory can move
// between a HotSwap parent and its candidate, and because tests point it
// somewhere new. A moved directory closes the old handle first.
func openProviderStoreForProcess() (*providerStore, error) {
	return openProviderStoreForProcessWith(true)
}

// openProviderStoreForProcessWith is openProviderStoreForProcess with the
// quarantine decision made by the caller. The restore path passes false: a
// file it finds damaged was last written by a candidate, and moving it aside
// would start an empty store, which resets the extender activation budget. The
// damaged file stays where it is for the next normal open to quarantine.
func openProviderStoreForProcessWith(allowQuarantine bool) (*providerStore, error) {
	globalProviderStoreMu.Lock()
	defer globalProviderStoreMu.Unlock()
	if globalProviderStoreReleased {
		return nil, errProviderStoreReleased
	}
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		return nil, err
	}
	if globalProviderStore != nil && globalProviderStorePath == path {
		return globalProviderStore, nil
	}
	now := time.Now()
	if globalProviderStore == nil && globalProviderStoreFailedPath == path && now.Before(globalProviderStoreRetryAt) {
		// Inside the retry interval: report the open that produced the
		// throttle rather than waiting on the lock again.
		return nil, globalProviderStoreFailedErr
	}
	if globalProviderStore != nil {
		_ = globalProviderStore.Close()
		globalProviderStore = nil
	}
	store, err := openProviderStore(path)
	if isProviderStoreCorrupt(err) {
		IncrPersistentError(providerStoreCorruptCategory)
	}
	if err != nil {
		// A database bbolt can never load is moved aside once, and the run
		// starts from an empty store, rather than warning about the same file
		// every thirty seconds for the life of the process.
		if !allowQuarantine {
			tlog("[state] %s would not open (%v); left in place, the next normal open quarantines it\n", path, err)
		} else if moved, quarantined := quarantineCorruptStore(path, err); quarantined {
			tlog("[state] %s would not open (%v); moved it to %s and started a fresh store\n", path, err, moved)
			store, err = openProviderStore(path)
		}
	}
	if err != nil {
		tlog("[state] store unavailable: %s\n", err)
		IncrPersistentError(providerStoreUnavailableCategory)
		globalProviderStoreFailedPath = path
		globalProviderStoreFailedErr = err
		globalProviderStoreRetryAt = now.Add(providerStoreRetryInterval)
		return nil, err
	}
	globalProviderStoreFailedPath = ""
	globalProviderStoreFailedErr = nil
	globalProviderStore, globalProviderStorePath = store, path
	return store, nil
}

// quarantineCorruptStore moves a database bbolt refuses to open out of the way,
// once, so the next open starts from an empty file instead of retrying forever
// against bytes that can never load. Only the corrupt-file errors qualify: a
// lock timeout means another process holds a healthy file, and a permission
// error is not the file's fault, so neither is moved.
func quarantineCorruptStore(path string, err error) (string, bool) {
	if !isProviderStoreCorrupt(err) &&
		!errors.Is(err, bbolt.ErrChecksum) &&
		!errors.Is(err, bbolt.ErrVersionMismatch) {
		return "", false
	}
	dst := fmt.Sprintf("%s.corrupt.%d", path, time.Now().Unix())
	if renameErr := os.Rename(path, dst); renameErr != nil {
		tlog("[state] store %s is corrupt (%v) but could not be moved aside: %v\n", path, err, renameErr)
		return "", false
	}
	return dst, true
}

func isProviderStoreCorrupt(err error) bool {
	return errors.Is(err, errProviderStoreCorrupt) || errors.Is(err, bbolt.ErrInvalid) ||
		errors.Is(err, bbolt.ErrChecksum) || errors.Is(err, bbolt.ErrVersionMismatch)
}

// releaseProviderStore gives provider_state.db to the hotswap candidate for the
// rest of this process's life. The close is marked on the handle, so a save a
// slot still had queued during the drain reports the lost write, and the
// process-wide flag refuses every later open: the candidate's open and its
// writes must not meet a lock this parent took back. A handoff that aborts
// while the parent keeps serving takes the file back
// (restoreProviderStoreAfterAbortedHandoff); a handoff that succeeds leaves it
// released.
func releaseProviderStore() {
	globalProviderStoreMu.Lock()
	defer globalProviderStoreMu.Unlock()
	globalProviderStoreReleased = true
	if globalProviderStore != nil {
		_ = globalProviderStore.release()
		globalProviderStore = nil
	}
	globalProviderStorePath = ""
	globalProviderStoreFailedPath = ""
	globalProviderStoreFailedErr = nil
	globalProviderStoreRetryAt = time.Time{}
}

// restoreProviderStoreAfterAbortedHandoff takes provider_state.db back when a
// handoff aborts while this process keeps running. The release is right on the
// success path because the candidate owns the file there, but an abort leaves
// the parent serving, and a parent whose released flag stayed set would fail
// every state write for the rest of its life, warning once as if the file had
// gone to a successor: activation records, host capability and every marker
// would be dropped with no way back.
//
// The release already dropped the handle and the failed-open throttle, so this
// clears the refusal and asks for the open again. A reopen that fails is
// reported, not fatal: the candidate may hold the bbolt lock, and then the
// parent keeps serving with the failure counted and every state write reporting
// the loss, which is louder than a write that looks landed.
func restoreProviderStoreAfterAbortedHandoff() error {
	globalProviderStoreMu.Lock()
	globalProviderStoreReleased = false
	globalProviderStoreMu.Unlock()

	if _, err := openProviderStoreForProcessWith(false); err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			hotswapLog("⚠️ [hotswap] Handoff aborted, but the candidate still holds %s: the parent keeps serving and its state writes fail until the candidate exits\n", providerStoreFileName)
		} else {
			hotswapLog("⚠️ [hotswap] Handoff aborted and %s did not reopen (%v): the parent keeps serving without its state store\n", providerStoreFileName, err)
		}
		return err
	}
	hotswapLog("⚡ [hotswap] Handoff aborted: reopened %s for the parent\n", providerStoreFileName)
	return nil
}

// closeProviderStore releases the file. Safe to call when it was never opened.
// This is the reset path (a moved state directory, a test); a handoff uses
// releaseProviderStore, which hands the file to the candidate and is terminal
// unless the handoff aborts.
func closeProviderStore() {
	globalProviderStoreMu.Lock()
	defer globalProviderStoreMu.Unlock()
	if globalProviderStore != nil {
		_ = globalProviderStore.Close()
		globalProviderStore = nil
	}
	globalProviderStoreReleased = false
	// A deliberate close clears the failure throttle: the next call should try
	// the open rather than inherit an interval from a failure that no longer
	// applies.
	globalProviderStoreFailedPath = ""
	globalProviderStoreFailedErr = nil
	globalProviderStoreRetryAt = time.Time{}
}
