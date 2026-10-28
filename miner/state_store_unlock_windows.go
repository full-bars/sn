//go:build windows

package miner

import (
	"os"

	"golang.org/x/sys/windows"
)

// releaseProviderStoreFile gives up a database file that bbolt locked and then
// panicked on before it returned a handle. The lock is a byte range on the
// handle, and bbolt's mapping of the file can outlive a close, so the range is
// unlocked explicitly first.
func releaseProviderStoreFile(file *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
	_ = file.Close()
}
