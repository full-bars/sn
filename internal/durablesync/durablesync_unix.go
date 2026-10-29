//go:build !windows

package durablesync

import (
	"os"
	"path/filepath"
)

// Nothing is absorbed on Unix. fsync(2) on a directory descriptor is the only
// thing that makes a rename durable there, so every refusal carries a real
// durability verdict and must reach the caller.
//
// Go retries an interrupted call, and on Darwin retries an ENOTSUP from
// F_FULLFSYNC with fsync(2). Those are retries of the same operation rather
// than absorptions: a refusal that survives them is reported.
func syncDirectory(directory *os.File) error {
	return directory.Sync()
}

// The rename changes the directory that gains newpath and, when it differs,
// the one that loses oldpath, so both are flushed.
func renameDurably(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); err != nil {
		return err
	}
	if err := SyncParent(newpath); err != nil {
		return err
	}
	if filepath.Dir(oldpath) != filepath.Dir(newpath) {
		return SyncParent(oldpath)
	}
	return nil
}
