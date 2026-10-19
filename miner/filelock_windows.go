//go:build windows

package miner

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// filelock_windows.go is the Windows arm of the miner's inter-process file
// lock (see filelock_unix.go). Windows has no flock; the port uses LockFileEx
// on the same path+".lock" file, which gives the same blocking
// acquire/release contract and is released when the process dies.

// fileLockSuffix is appended to the target path to make the lock file name.
const fileLockSuffix = ".lock"

// acquireFileLock takes an exclusive lock on path's lock file.
func acquireFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path+fileLockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open file lock: %w", err)
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock file: %w", err)
	}
	return func() {
		windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
		f.Close()
	}, nil
}
