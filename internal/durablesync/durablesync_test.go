package durablesync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Windows miner panicked in auth because this operation failed. The same
// call must succeed on every platform the release ships.
func TestSyncDirOrdersARealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jwt"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatal("sync directory:", err)
	}
}

// SyncParent is the operation a token write actually performs.
func TestSyncParentOrdersTheRetainingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncParent(path); err != nil {
		t.Fatal("sync parent:", err)
	}
}

// Seed custody keeps its directory descriptors for its whole life; ordering
// one must not take that ownership away.
func TestSyncRetainsABorrowedDescriptor(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := Sync(dir); err != nil {
		t.Fatal("sync retained descriptor:", err)
	}
	if _, err := dir.Stat(); err != nil {
		t.Fatal("Sync closed a descriptor it does not own:", err)
	}
}

// An absent directory is a real failure. Absorbing it would publish a
// credential whose rename nobody ordered.
func TestSyncDirReportsAnAbsentDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if err := SyncDir(missing); err == nil {
		t.Fatal("absent directory was reported as durable")
	}
	if err := SyncParent(filepath.Join(missing, "jwt")); err == nil {
		t.Fatal("absent parent directory was reported as durable")
	}
}

// A regular file is refused on every platform. This is the cross-platform
// guard for the shared precondition: on Unix a file flush succeeds, so without
// the precondition the two platforms would disagree about whether a completed
// call means anything, and this test would fail wherever it was run.
func TestSyncRefusesARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	err = Sync(file)
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("a regular file must be refused with ErrNotDirectory, got %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the refusal must name the descriptor it rejected, got %v", err)
	}
}

// SyncDir routes through the same precondition, so a mistaken pathname is
// refused rather than silently synced on the platforms where that succeeds.
func TestSyncDirRefusesARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(path); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("a regular file must be refused, got %v", err)
	}
	// The containing directory is still a directory, so the neighbour of that
	// refusal keeps working.
	if err := SyncParent(path); err != nil {
		t.Fatalf("SyncParent must order the containing directory, got %v", err)
	}
}

// A closed descriptor reports the descriptor, not the policy. This is the
// error a caller sees when a handle was released too early.
func TestSyncReportsAClosedDescriptor(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	err = Sync(directory)
	if err == nil {
		t.Fatal("a closed descriptor was reported as an ordered directory")
	}
	if errors.Is(err, ErrNotDirectory) {
		t.Fatalf("a closed descriptor is not a directory-kind refusal: %v", err)
	}
}

// Rename is the one call that assures a durable name on every platform, so it
// must replace what is there and leave nothing under the old name.
func TestRenamePublishesAndReplaces(t *testing.T) {
	if !crashDurableRename {
		t.Skip("this platform refuses Rename; see TestRenameRefusesWhereTheGuaranteeIsMissing")
	}
	dir := t.TempDir()
	from, to := filepath.Join(dir, ".tmp-marker"), filepath.Join(dir, "marker")
	if err := os.WriteFile(to, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(from, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(from, to); err != nil {
		t.Fatal("rename:", err)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "new" {
		t.Fatalf("published %q, %v; want the new contents", data, err)
	}
	if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old name still present: %v", err)
	}
	if err := Rename(from, to); err == nil {
		t.Fatal("renaming a missing file was reported as durable")
	}
}

// A platform without a crash-durable rename must fail the call before any
// move, so a caller that needs the guarantee fails closed instead of reading
// a nil as durable.
func TestRenameRefusesWhereTheGuaranteeIsMissing(t *testing.T) {
	moved := false
	err := rename("from", "to", false, func(string, string) error { moved = true; return nil })
	if !errors.Is(err, ErrNotCrashDurable) {
		t.Fatalf("rename without the guarantee = %v, want ErrNotCrashDurable", err)
	}
	if moved {
		t.Fatal("the refused rename still moved the file")
	}
	if err := rename("from", "to", true, func(string, string) error { moved = true; return nil }); err != nil || !moved {
		t.Fatalf("rename with the guarantee = %v (moved %t), want it performed", err, moved)
	}
}
