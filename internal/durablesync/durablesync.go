// Package durablesync orders a directory entry after a rename, on the
// platforms that can do so. Windows has no directory flush, so the one syscall
// that orders a rename on Unix has no counterpart there; that absence is
// absorbed so a caller can proceed, while every real permission or I/O failure
// still fails.
//
// What each call assures:
//
//   - Sync, SyncDir and SyncParent make a completed rename or creation in the
//     directory durable on Unix. On Windows they assure nothing: a nil return
//     there only means no real failure was seen.
//   - Rename publishes a file under its new name durably on both: on Unix by
//     renaming and then flushing the directories involved, on Windows by
//     MoveFileEx with MOVEFILE_WRITE_THROUGH, which does not return until the
//     move is on disk.
//
// A caller whose correctness depends on a name surviving a crash on every
// platform publishes it with Rename. Creating or removing a name and then
// calling SyncDir is durable on Unix only.
//
// The directory requirement and the verdict are shared. The platform files
// differ only in the flush itself, which is where the platforms actually
// differ.
package durablesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotDirectory reports a descriptor that is not a directory.
//
// A rename is only as durable as the directory entry naming it, so flushing a
// file would answer a question the caller did not ask. On Unix that flush
// would succeed, so without this precondition the two platforms would disagree
// about what a completed call means.
var ErrNotDirectory = errors.New("durablesync: not a directory")

// Sync orders the metadata of a directory descriptor the caller retains for
// later use. The descriptor stays open and owned by the caller; only its
// already-committed rename is being made durable.
//
// The descriptor must be a directory. Anything else is refused with
// ErrNotDirectory before any flush is attempted, so no platform can report a
// completed ordering for a descriptor that could never carry one.
func Sync(directory *os.File) error {
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s", ErrNotDirectory, directory.Name())
	}
	return syncDirectory(directory)
}

// SyncDir opens a directory by name, orders it and closes it. Callers that
// hold only a pathname get the same verdict as one that holds a descriptor.
func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(Sync(directory), directory.Close())
}

// SyncParent orders the directory that retains path itself, which is the one
// whose metadata a rename of path changes. A file published by rename is only
// as durable as the entry naming it.
func SyncParent(path string) error {
	return SyncDir(filepath.Dir(path))
}

// Rename moves oldpath to newpath, replacing newpath, and returns once the move
// is durable. See the package comment for what that means per platform.
func Rename(oldpath, newpath string) error {
	return renameDurably(oldpath, newpath)
}
