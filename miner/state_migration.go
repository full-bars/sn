package miner

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"go.etcd.io/bbolt"
)

const providerMigrationBucket = "migration"
const providerMigrationRecordsBucket = "migration_records"
const providerMigrationVersion = 1

type migrationMode string

const (
	migrationUninitialized migrationMode = "uninitialized"
	migrationShadow        migrationMode = "shadow"
	migrationVerified      migrationMode = "verified"
	migrationReadDB        migrationMode = "read-db"
)

type migrationOperationState string

const (
	migrationPending   migrationOperationState = "pending"
	migrationCommitted migrationOperationState = "committed"
)

// migrationRecord retains exact bytes. Format is an explicit compatibility
// label; it does not imply that two different encodings compare equal.
type migrationRecord struct {
	Format string `json:"format"`
	Bytes  []byte `json:"bytes"`
}

type migrationStoredRecord struct {
	Version int             `json:"version"`
	Record  migrationRecord `json:"record"`
}

// readMigrationRecord copies the bytes out of the transaction before it ends.
func readMigrationRecord(tx *bbolt.Tx, key string) (migrationRecord, bool, error) {
	var stored migrationStoredRecord
	b := tx.Bucket([]byte(providerMigrationRecordsBucket))
	if b == nil || b.Get([]byte(key)) == nil {
		return migrationRecord{}, false, nil
	}
	if err := json.Unmarshal(b.Get([]byte(key)), &stored); err != nil {
		return migrationRecord{}, true, fmt.Errorf("invalid migration raw record: %w", err)
	}
	if stored.Version != providerMigrationVersion || stored.Record.Format == "" {
		return migrationRecord{}, true, errors.New("unsupported or invalid migration raw record")
	}
	return stored.Record, true, nil
}

func writeMigrationRecord(tx *bbolt.Tx, key string, value migrationRecord) error {
	if key == "" || value.Format == "" {
		return errors.New("invalid migration raw record key or format")
	}
	if _, _, err := readMigrationRecord(tx, key); err != nil {
		return err
	}
	raw, err := json.Marshal(migrationStoredRecord{Version: providerMigrationVersion, Record: value})
	if err != nil {
		return err
	}
	b, err := tx.CreateBucketIfNotExists([]byte(providerMigrationRecordsBucket))
	if err != nil {
		return err
	}
	return b.Put([]byte(key), raw)
}

type migrationOperation struct {
	Generation uint64                  `json:"generation"`
	Hash       [sha256.Size]byte       `json:"hash"`
	State      migrationOperationState `json:"state"`
}

// A committed DB entry is atomic with other changes in its bbolt transaction.
// It cannot seal a filesystem rename, prove file durability, or establish that
// an independently writable file has not changed since the transaction.
type migrationFamily struct {
	Version   int                 `json:"version"`
	Mode      migrationMode       `json:"mode"`
	Operation *migrationOperation `json:"operation,omitempty"`
}

func (f migrationFamily) validate() error {
	if f.Version != providerMigrationVersion {
		return errors.New("unsupported migration record version")
	}
	switch f.Mode {
	case migrationUninitialized, migrationShadow, migrationVerified, migrationReadDB:
	default:
		return errors.New("invalid migration mode")
	}
	if f.Operation != nil {
		if f.Operation.Generation == 0 || (f.Operation.State != migrationPending && f.Operation.State != migrationCommitted) {
			return errors.New("invalid migration operation")
		}
	}
	return nil
}

// migrationView and migrationUpdate never open or stamp a store. Both check the
// existing schema in the same transaction used to inspect or mutate records.
func migrationSchema(tx *bbolt.Tx) error {
	b := tx.Bucket([]byte(providerStoreBucketMeta))
	if b == nil || b.Get([]byte(providerStoreKeySchema)) == nil {
		return errors.New("schema stamp missing")
	}
	return providerStoreStampVerdict(b.Get([]byte(providerStoreKeySchema)), "")
}

func migrationView(s *providerStore, fn func(*bbolt.Tx) error) error {
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
	return runProviderStoreView(s.db, func(tx *bbolt.Tx) error {
		if err := migrationSchema(tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

func migrationUpdate(s *providerStore, fn func(*bbolt.Tx) error) error {
	if s == nil {
		return errProviderStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(func(tx *bbolt.Tx) error {
		if err := migrationSchema(tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

func readMigrationFamily(tx *bbolt.Tx, family string) (migrationFamily, bool, error) {
	var value migrationFamily
	b := tx.Bucket([]byte(providerMigrationBucket))
	if b == nil || b.Get([]byte(family)) == nil {
		return value, false, nil
	}
	if err := json.Unmarshal(b.Get([]byte(family)), &value); err != nil {
		return value, true, fmt.Errorf("invalid migration record: %w", err)
	}
	return value, true, value.validate()
}

func writeMigrationFamily(tx *bbolt.Tx, family string, value migrationFamily) error {
	if family == "" {
		return errors.New("empty migration family")
	}
	if err := value.validate(); err != nil {
		return err
	}
	// Refuse to overwrite a damaged or newer record.
	if _, _, err := readMigrationFamily(tx, family); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b, err := tx.CreateBucketIfNotExists([]byte(providerMigrationBucket))
	if err != nil {
		return err
	}
	return b.Put([]byte(family), raw)
}
