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
