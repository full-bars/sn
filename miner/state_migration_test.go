package miner

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"
)

func TestMigrationFamilyTransactions(t *testing.T) {
	dir := t.TempDir()
	original := []byte("retained source bytes")
	s, err := openProviderStore(filepath.Join(dir, providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := checkProviderStoreSchema(s); err != nil {
		t.Fatal(err)
	}
	if err := migrationView(s, func(tx *bbolt.Tx) error {
		_, found, err := readMigrationFamily(tx, "identities")
		if found || err != nil {
			t.Fatalf("absent: found=%v err=%v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	value := migrationFamily{Version: 1, Mode: migrationShadow, Operation: &migrationOperation{Generation: 1, Hash: sha256.Sum256(original), State: migrationPending}}
	record := migrationRecord{Format: "opaque", Bytes: original}
	abort := errors.New("injected abort")
	if err := migrationUpdate(s, func(tx *bbolt.Tx) error {
		if err := writeMigrationFamily(tx, "identities", value); err != nil {
			return err
		}
		if err := writeMigrationRecord(tx, "identities/key", record); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("abort: %v", err)
	}
	if err := migrationView(s, func(tx *bbolt.Tx) error {
		_, found, err := readMigrationFamily(tx, "identities")
		if found || err != nil {
			t.Fatalf("aborted write persisted: %v %v", found, err)
		}
		_, found, err = readMigrationRecord(tx, "identities/key")
		if found || err != nil {
			t.Fatalf("aborted raw write persisted: %v %v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := migrationUpdate(s, func(tx *bbolt.Tx) error {
		if err := writeMigrationFamily(tx, "identities", value); err != nil {
			return err
		}
		return writeMigrationRecord(tx, "identities/key", record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := migrationView(s, func(tx *bbolt.Tx) error {
		got, found, err := readMigrationFamily(tx, "identities")
		if err != nil || !found || got.Mode != migrationShadow || got.Operation == nil || got.Operation.State != migrationPending || got.Operation.Hash != sha256.Sum256(original) || got.Operation.Generation != 1 {
			t.Fatalf("retry: %+v %v %v", got, found, err)
		}
		raw, found, err := readMigrationRecord(tx, "identities/key")
		if err != nil || !found || raw.Format != record.Format || !bytes.Equal(raw.Bytes, original) {
			t.Fatalf("raw retry: found=%v err=%v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	value.Operation.State = migrationCommitted
	value.Mode = migrationVerified
	if err := migrationUpdate(s, func(tx *bbolt.Tx) error { return writeMigrationFamily(tx, "identities", value) }); err != nil {
		t.Fatal(err)
	}
	if err := migrationView(s, func(tx *bbolt.Tx) error {
		got, found, err := readMigrationFamily(tx, "identities")
		if err != nil || !found || got.Mode != migrationVerified || got.Operation == nil || got.Operation.State != migrationCommitted {
			t.Fatalf("committed: %+v %v %v", got, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"{broken", `{"version":2,"mode":"shadow"}`} {
		if err := s.write(func(tx *bbolt.Tx) error {
			return tx.Bucket([]byte(providerMigrationBucket)).Put([]byte("identities"), []byte(raw))
		}); err != nil {
			t.Fatal(err)
		}
		if err := migrationUpdate(s, func(tx *bbolt.Tx) error { return writeMigrationFamily(tx, "identities", value) }); err == nil {
			t.Fatal("overwrote undecodable or newer record")
		}
	}
	if err := s.release(); err != nil {
		t.Fatal(err)
	}
	if err := migrationView(s, func(*bbolt.Tx) error { return nil }); !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("released read: %v", err)
	}
	if err := migrationUpdate(s, func(*bbolt.Tx) error { return nil }); !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("released write: %v", err)
	}
}

// The family only moves forward: a mode never regresses, a generation never
// decreases, and a committed operation is final. A refused write leaves the
// stored bytes exactly as they were.
func TestMigrationFamilyRefusesIllegalTransitions(t *testing.T) {
	op := func(gen uint64, state migrationOperationState, hash byte) *migrationOperation {
		return &migrationOperation{Generation: gen, State: state, Hash: [sha256.Size]byte{hash}}
	}
	fam := func(mode migrationMode, o *migrationOperation) migrationFamily {
		return migrationFamily{Version: 1, Mode: mode, Operation: o}
	}
	cases := []struct {
		name     string
		start    migrationFamily
		next     migrationFamily
		wantFail bool
	}{
		{"forward mode and commit", fam(migrationShadow, op(1, migrationPending, 1)), fam(migrationVerified, op(1, migrationCommitted, 1)), false},
		{"same pending retry", fam(migrationShadow, op(1, migrationPending, 1)), fam(migrationShadow, op(1, migrationPending, 1)), false},
		{"new higher generation", fam(migrationVerified, op(1, migrationCommitted, 1)), fam(migrationVerified, op(2, migrationPending, 2)), false},
		{"read-db back to uninitialized", fam(migrationReadDB, op(3, migrationCommitted, 1)), fam(migrationUninitialized, nil), true},
		{"verified back to shadow", fam(migrationVerified, op(1, migrationCommitted, 1)), fam(migrationShadow, op(1, migrationCommitted, 1)), true},
		{"generation decreases", fam(migrationShadow, op(5, migrationPending, 1)), fam(migrationShadow, op(4, migrationPending, 1)), true},
		{"generation decreases with higher mode", fam(migrationShadow, op(5, migrationPending, 1)), fam(migrationVerified, op(4, migrationCommitted, 1)), true},
		{"committed back to pending", fam(migrationVerified, op(2, migrationCommitted, 1)), fam(migrationVerified, op(2, migrationPending, 1)), true},
		{"replay of a committed operation", fam(migrationVerified, op(2, migrationCommitted, 1)), fam(migrationVerified, op(2, migrationCommitted, 1)), true},
		{"operation dropped", fam(migrationShadow, op(1, migrationPending, 1)), fam(migrationShadow, nil), true},
		{"same generation different hash", fam(migrationShadow, op(1, migrationPending, 1)), fam(migrationShadow, op(1, migrationPending, 2)), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := checkProviderStoreSchema(s); err != nil {
				t.Fatal(err)
			}
			if err := migrationUpdate(s, func(tx *bbolt.Tx) error { return writeMigrationFamily(tx, "identities", c.start) }); err != nil {
				t.Fatalf("seed: %v", err)
			}
			var before []byte
			read := func() []byte {
				var raw []byte
				if err := migrationView(s, func(tx *bbolt.Tx) error {
					raw = append([]byte(nil), tx.Bucket([]byte(providerMigrationBucket)).Get([]byte("identities"))...)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			before = read()
			err = migrationUpdate(s, func(tx *bbolt.Tx) error { return writeMigrationFamily(tx, "identities", c.next) })
			if !c.wantFail {
				if err != nil {
					t.Fatalf("legal transition refused: %v", err)
				}
				return
			}
			if !errors.Is(err, errMigrationTransition) {
				t.Fatalf("want errMigrationTransition, got %v", err)
			}
			if after := read(); !bytes.Equal(before, after) {
				t.Fatalf("refused write changed the record: %s -> %s", before, after)
			}
		})
	}
}
