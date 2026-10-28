//go:build !windows

package miner

import (
	"os"

	"golang.org/x/sys/unix"
)

// releaseProviderStoreFile gives up a database file that bbolt locked and then
// panicked on before it returned a handle. The flock belongs to the open file
// description, and bbolt's mapping of the file holds that description past a
// close, so the lock is released explicitly first.
func releaseProviderStoreFile(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}
