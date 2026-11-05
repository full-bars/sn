package miner

import (
	"bytes"
	"errors"
	"testing"
)

func TestReconcileClassificationsAndStability(t *testing.T) {
	file := migrationSide{Present: true, Record: migrationRecord{Format: "json", Bytes: []byte("{\"x\":1}")}}
	equal := migrationSide{Present: true, Record: migrationRecord{Format: "json", Bytes: append([]byte(nil), file.Record.Bytes...)}}
	conflict := migrationSide{Present: true, Record: migrationRecord{Format: "json", Bytes: []byte("{\"x\":2}")}}
	differentFormat := migrationSide{Present: true, Record: migrationRecord{Format: "other", Bytes: append([]byte(nil), file.Record.Bytes...)}}
	interrupted := migrationSide{Err: errors.New("injected interruption")}
	cases := []struct {
		name    string
		a, b    migrationSide
		pending bool
		want    migrationComparison
	}{
		{"equal", file, equal, false, migrationEqual},
		{"only-file", file, migrationSide{}, false, migrationOnlyFile},
		{"only-store", migrationSide{}, equal, false, migrationOnlyStore},
		{"conflict", file, conflict, false, migrationConflict},
		{"unreadable", file, interrupted, false, migrationUnreadable},
		{"pending", file, equal, true, migrationUnresolved},
		{"both absent", migrationSide{}, migrationSide{}, false, migrationUnresolved},
		{"different format", file, differentFormat, false, migrationUnresolved},
	}
	before := append([]byte(nil), file.Record.Bytes...)
	storeBefore := append([]byte(nil), equal.Record.Bytes...)
	writesBefore := providerStoreWrites.Load()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				if got := compareMigrationRecords(tc.a, tc.b, tc.pending); got != tc.want {
					t.Fatalf("scan %d = %s, want %s", i, got, tc.want)
				}
			}
		})
	}
	if !bytes.Equal(file.Record.Bytes, before) || !bytes.Equal(equal.Record.Bytes, storeBefore) {
		t.Fatal("comparison mutated source or store bytes")
	}
	if providerStoreWrites.Load() != writesBefore {
		t.Fatal("comparison attempted a store write")
	}
}
