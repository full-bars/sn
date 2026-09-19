//go:build !windows

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// pendingOverridesLockWait bounds how long a queue update waits for the lock.
// The critical section is a small JSON read-modify-write, so a wait this long
// means the holder is stuck or hostile. The lock lives in a directory the
// provider user owns, so that user can flock it and never let go; without a
// bound a root-run `urnet-tools set` would hang forever.
const pendingOverridesLockWait = 30 * time.Second

// acquirePendingOverridesLock obtains a blocking, exclusive inter-process
// lock on stateDir's pending_overrides.json so two concurrent `urnet-tools`
// invocations (e.g. two operators, or a script looping `set` calls) can't
// each read the same queue, append their own op, and let the last
// os.Rename in queuePendingOverride silently discard the other's update.
// Returns a release function that must be called when done.
func acquirePendingOverridesLock(queueFile string) (func(), error) {
	// A freshly created lock file must belong to the state-dir owner, or the
	// unprivileged provider's own merge cannot open it and queued overrides
	// are silently never applied. The ownership change is made on the OPEN
	// descriptor inside acquireExclusiveLockOwned (best-effort): re-resolving
	// the pathname afterwards would let a local user swap it before root's
	// chown.
	release, err := acquireExclusiveLockOwned(queueFile+".lock", filepath.Dir(queueFile), pendingOverridesLockWait)
	if err != nil {
		return nil, fmt.Errorf("pending-overrides: %w", err)
	}
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
	return acquireExclusiveLockOwned(lockPath, "", 0)
}

// acquireExclusiveLockOwned is acquireExclusiveLock that also hands the lock
// file to the owner of ownerDir (when non-empty) via fchown on the descriptor
// it just opened. timeout <= 0 blocks until the lock is free; a positive
// timeout gives up with an error once it elapses.
//
// A chown failure is not a lock failure, but it is not silent either: a lock
// file left owned by root (0600) cannot be opened by the unprivileged
// provider, so its own merge of queued overrides would never run and nothing
// would say why.
func acquireExclusiveLockOwned(lockPath, ownerDir string, timeout time.Duration) (func(), error) {
	// O_NOFOLLOW: a planted symlink at the lock path must not make root
	// create the symlink TARGET (e.g. a planted ...json.lock -> /etc/nologin
	// would block non-root logins once root O_CREATEs it) — H2.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if ownerDir != "" {
		// Only a plain, singly-linked file is handed over: fchown acts on the
		// inode, so a hardlink the directory owner planted to some other file
		// would change that file's owner.
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			f.Close()
			return nil, fmt.Errorf("fstat lock file %s: %w", lockPath, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink > 1 {
			f.Close()
			return nil, fmt.Errorf("refusing lock file %s: not a regular file with a single link", lockPath)
		}
		if err := chownFdLikeStateOwner(ownerDir, int(f.Fd())); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not hand %s to the state dir owner: %v (the provider may be unable to read it)\n", lockPath, err)
		}
	}
	if err := flockWithTimeout(int(f.Fd()), timeout); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// flockWithTimeout takes an exclusive flock on fd. With timeout <= 0 it
// blocks; otherwise it polls LOCK_NB until the deadline.
func flockWithTimeout(fd int, timeout time.Duration) error {
	if timeout <= 0 {
		return unix.Flock(fd, unix.LOCK_EX)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for another process to release it", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
