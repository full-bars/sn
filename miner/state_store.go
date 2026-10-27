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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: providerStoreOpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	store := &providerStore{db: db, path: path}
	return store, nil
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

// load decodes one record. false means it has never been written.
func (s *providerStore) load(bucket, key string, out any) bool {
	if s == nil || out == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false
	}
	var raw []byte
	_ = s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		raw = append(raw, b.Get([]byte(key))...)
		return nil
	})
	if len(raw) == 0 {
		return false
	}
	return json.Unmarshal(raw, out) == nil
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
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
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
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
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

	if _, err := openProviderStoreForProcess(); err != nil {
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
