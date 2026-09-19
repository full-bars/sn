//go:build !windows

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquirePendingOverridesLock obtains a blocking, exclusive inter-process
// lock on stateDir's pending_overrides.json so two concurrent `urnet-tools`
// invocations (e.g. two operators, or a script looping `set` calls) can't
// each read the same queue, append their own op, and let the last
// os.Rename in queuePendingOverride silently discard the other's update.
// Returns a release function that must be called when done.
func acquirePendingOverridesLock(queueFile string) (func(), error) {
	release, err := acquireExclusiveLock(queueFile + ".lock")
	if err != nil {
		return nil, fmt.Errorf("pending-overrides: %w", err)
	}
	// A freshly created lock file must belong to the state-dir owner, or the
	// unprivileged provider's own merge cannot open it and queued overrides
	// are silently never applied. Best-effort: do not fail the command
	// over chown; the operator-visible symptom (lock owned by root) is at
	// least no worse than the pre-fix state.
	_ = chownLockFileLikeStateOwner(filepath.Dir(queueFile), queueFile+".lock")
	return release, nil
}

// acquireExclusiveLock obtains a blocking, exclusive inter-process lock on
// lockPath and returns a release function. Blocking rather than try-lock on
// purpose: a caller that loses the race should wait its turn, not fail.
//
// flock(2) is held by the open file description and the kernel drops it when
// the process exits, however it exits, so a crash cannot leave a stale lock
// that wedges the fleet. That is the reason for flock over a pidfile.
func acquireExclusiveLock(lockPath string) (func(), error) {
	// O_NOFOLLOW: a planted symlink at the lock path must not make root
	// create the symlink TARGET (e.g. a planted ...json.lock -> /etc/nologin
	// would block non-root logins once root O_CREATEs it) — H2.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// chownLockFileLikeStateOwner hands a freshly-created lock file to the state
// dir's owner. Without this the lock stays root-owned 0600 after a root-run
// `urnet-tools set/report/rename/profile/fast-auth`, the unprivileged
// provider's own merge then hits EACCES opening the same lock, and queued
// overrides are silently never applied. Call AFTER creating the file
// with the state dir known; best-effort (returns the chown error when the
// caller wants to surface it).
func chownLockFileLikeStateOwner(stateDir, lockPath string) error {
	if stateDir == "" {
		return nil
	}
	return chownLikeStateOwner(stateDir, lockPath)
}
