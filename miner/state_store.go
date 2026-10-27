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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

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
// value or this one, never a mixture.
func (s *providerStore) save(bucket, key string, value any) error {
	if s == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
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
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
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
	if s == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, seq)
		if err := b.Put(key, raw); err != nil {
			return err
		}
		if keep <= 0 || seq <= uint64(keep) {
			return nil
		}
		// This append created the oldest record worth keeping, so the one
		// before it is the only record that has to go.
		oldest := make([]byte, 8)
		binary.BigEndian.PutUint64(oldest, seq-uint64(keep))
		return b.Delete(oldest)
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
	err := s.db.View(func(tx *bbolt.Tx) error {
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
	// A failed open is not retried on every call. bbolt's open waits on the
	// file lock, so a second process or a damaged file would otherwise stall
	// every caller that asks for the store and repeat the same log line for as
	// long as the process lives. The failure is counted, and the open is tried
	// again on a timer or immediately when the state directory moves.
	globalProviderStoreFailedPath string
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
//
// The path is re-checked on every call because the state directory can move
// between a HotSwap parent and its candidate, and because tests point it
// somewhere new. A moved directory closes the old handle first.
func providerStoreInstance() *providerStore {
	globalProviderStoreMu.Lock()
	defer globalProviderStoreMu.Unlock()
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		return nil
	}
	if globalProviderStore != nil && globalProviderStorePath == path {
		return globalProviderStore
	}
	now := time.Now()
	if globalProviderStore == nil && globalProviderStoreFailedPath == path && now.Before(globalProviderStoreRetryAt) {
		return nil
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
		globalProviderStoreRetryAt = now.Add(providerStoreRetryInterval)
		return nil
	}
	globalProviderStoreFailedPath = ""
	globalProviderStore, globalProviderStorePath = store, path
	return store
}

// closeProviderStore releases the file. Safe to call when it was never opened.
func closeProviderStore() {
	globalProviderStoreMu.Lock()
	defer globalProviderStoreMu.Unlock()
	if globalProviderStore != nil {
		_ = globalProviderStore.Close()
		globalProviderStore = nil
	}
	// A deliberate close (hotswap, a moved state directory, a test) clears the
	// failure throttle: the next call should try the open rather than inherit
	// an interval from a failure that no longer applies.
	globalProviderStoreFailedPath = ""
	globalProviderStoreRetryAt = time.Time{}
}
