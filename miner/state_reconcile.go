package miner

import "bytes"

type migrationComparison string

const (
	migrationEqual      migrationComparison = "equal"
	migrationOnlyFile   migrationComparison = "only-file"
	migrationOnlyStore  migrationComparison = "only-store"
	migrationConflict   migrationComparison = "conflict"
	migrationUnreadable migrationComparison = "unreadable"
	migrationUnresolved migrationComparison = "pending"
)

// migrationSide is a snapshot supplied by the caller. Err or an unknown format
// cannot be treated as absence or equality. No source is read or changed here.
type migrationSide struct {
	Present bool
	Record  migrationRecord
	Err     error
}

func compareMigrationRecords(file, store migrationSide, pending bool) migrationComparison {
	if file.Err != nil || store.Err != nil {
		return migrationUnreadable
	}
	if pending {
		return migrationUnresolved
	}
	switch {
	case file.Present && !store.Present:
		return migrationOnlyFile
	case !file.Present && store.Present:
		return migrationOnlyStore
	case !file.Present && !store.Present:
		// Neither side supplies evidence of a completed copy.
		return migrationUnresolved
	case file.Record.Format == "" || store.Record.Format == "":
		return migrationUnreadable
	case file.Record.Format != store.Record.Format:
		return migrationUnresolved
	case bytes.Equal(file.Record.Bytes, store.Record.Bytes):
		return migrationEqual
	default:
		return migrationConflict
	}
}
