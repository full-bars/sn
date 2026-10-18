//go:build !windows

package miner

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// filelock_unix.go is the miner's inter-process file lock, the port of the
// v33 provider's acquireJWTStoreLock (provider/jwt_store_lock_unix.go). The
// miner's file writers that more than one process can reach - the operator
// baseline mark, the action ledger and the OOM/thrash cap state - all take it
// so a hot-swap parent and candidate, or a restart that has not fully drained,
// cannot interleave an append with a rotation.
//
// The lock lives beside the file at path+".lock" and is a blocking flock,
// released when the holder dies, so there is no stale-lock case to handle and
// a contended writer waits instead of being dropped.

// fileLockSuffix is appended to the target path to make the lock file name.
const fileLockSuffix = ".lock"

// acquireFileLock takes an exclusive lock on path's lock file. The returned
// release must be called when the caller is done (idempotently safe: flock
// LOCK_UN on a closed descriptor is an error, so callers call it once).
func acquireFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path+fileLockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open file lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
