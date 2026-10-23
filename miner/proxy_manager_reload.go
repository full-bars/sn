package miner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// This file ports the v33 provider's cross-process proxy lock and reload
// trigger (proxy_reload.go, lock and trigger halves). The URLs fetcher, the
// reaper and the cleanup pass all write state under the lock and poke the
// trigger file so a running provider applies the change. The miner's provide
// loop reads the desired set at startup; the reload WATCHER that would apply
// trigger files live is P2 (hot reload), but the lock and trigger files are
// still the fork's cross-process contract and the ported tests assert their
// sequences.

func proxyReloadPath() (string, error) {
	return providerStatePath("proxy.reload")
}

func proxyLockPath() (string, error) {
	return providerStatePath("proxy.lock")
}

// readReloadSeq reads the current sequence number from the trigger file.
// Returns 0 if the file does not exist.
func readReloadSeq(path string) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		// Treat an unparseable trigger as seq 0 rather than failing the watcher.
		tlog("[proxy] warn: reload trigger file unparseable: %v\n", err)
		return 0, nil
	}
	return n, nil
}

// acquireProxyLock creates the lock file at the default path. Returns an error
// if a reload is already in progress (lock already held).
func acquireProxyLock() (func(), error) {
	path, err := proxyLockPath()
	if err != nil {
		return nil, err
	}
	return acquireProxyLockAt(path)
}

// acquireProxyLockWithRetry attempts to acquire the proxy lock with backoff.
func acquireProxyLockWithRetry() (func(), error) {
	var release func()
	var err error
	for i := 0; i < 5; i++ {
		release, err = acquireProxyLock()
		if err == nil {
			return release, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}

// acquireProxyLockAt is the path-explicit form, for testing.
func acquireProxyLockAt(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if isLockStale(existing) {
			// An empty or unparseable lock younger than the write grace is
			// likely an acquirer that has created the file but not yet
			// written its pid+timestamp (create and write are not atomic).
			// Stealing it would admit two holders at once; falling through
			// to O_EXCL below fails safely instead.
			steal := true
			if _, _, ok := parseLockContent(existing); !ok {
				if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) < proxyLockWriteGrace {
					steal = false
				}
			}
			if steal {
				// Remove only the file we actually judged stale: two racing
				// stealers could otherwise have the slower one delete the
				// faster one's fresh lock. The re-read narrows the window;
				// POSIX has no compare-and-delete, so a microsecond-sized
				// window remains - a fresh lock installed inside it fails
				// our O_EXCL below, which is the safe outcome.
				if current, err := os.ReadFile(path); err == nil && string(current) == string(existing) {
					os.Remove(path)
				}
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("reload already in progress - try again in a moment")
		}
		return nil, err
	}
	nonce := atomic.AddUint64(&proxyLockNonce, 1)
	content := fmt.Sprintf("%d\n%d\n%d\n", os.Getpid(), time.Now().Unix(), nonce)
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	// Release removes the lock ONLY while the file is still ours. A stale
	// steal can replace it with a NEW holder's lock (isLockStale admits a
	// live holder's lock once it ages past the PID-reuse bound), and an
	// unconditional remove would delete THEIR lock and let a third acquirer
	// in while they are still working. The read-compare-remove cannot be
	// atomic on POSIX; the residual window is accepted - closing it needs
	// flock, which is not portable to the Windows build.
	var once sync.Once
	return func() {
		once.Do(func() {
			if current, err := os.ReadFile(path); err == nil && string(current) == content {
				os.Remove(path)
			}
		})
	}, nil
}

const proxyLockStaleAge = 5 * time.Minute

// proxyLockNonce disambiguates lock content across acquisitions within one
// process: with second-granularity timestamps two acquisitions by this
// process inside the same second would otherwise carry byte-identical
// content, and a late release of the first would remove the second holder's
// lock.
var proxyLockNonce uint64

// proxyLockWriteGrace covers the non-atomic create-then-write window of a
// lock acquisition: lock content that does not parse and is younger than
// this is assumed mid-write, not stale.
var proxyLockWriteGrace = 5 * time.Second

// processStart approximates this process's start time. A hot swap execs in
// place: the pid survives and deferred cleanup never runs, so a proxy.lock
// written by the previous image carries our own pid with an old timestamp
// and would read as "held by a live holder" until proxyLockMaxAge ages it
// out, blocking every lock consumer in the fresh image for up to an hour.
var processStart = time.Now()

// cleanStaleSelfProxyLock removes a proxy.lock that carries this process's
// own pid but predates this process's start. Run it at startup, before the
// first lock consumer, so an escalated hot restart is not greeted by its
// own old lock.
func cleanStaleSelfProxyLock() {
	path, err := proxyLockPath()
	if err != nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, ts, ok := parseLockContent(b)
	if !ok || pid != os.Getpid() {
		return
	}
	// Inclusive bound: a lock written by the previous image in the same
	// integer second as this process's start must be cleaned too, and at
	// cleanup time this image cannot yet hold a lock of its own (it runs
	// before any local consumer).
	if ts <= processStart.Unix() {
		// Compare before removing, like the steal path: another holder
		// could have replaced the file between the read and the remove.
		if current, err := os.ReadFile(path); err != nil || string(current) != string(b) {
			return
		}
		if os.Remove(path) == nil {
			tlog("[proxy] removed a proxy.lock left behind by a previous image with this pid\n")
		}
	}
}

// proxyLockMaxAge is the outer bound: even if the holder appears alive
// (Signal(0) succeeds), a lock older than this is unconditionally stale.
// This prevents a PID-reuse scenario (OS reissues the same PID to an
// unrelated process) from wedging the proxy lock forever.
const proxyLockMaxAge = 1 * time.Hour

// writeReloadTriggerDebounce is the minimum interval between consecutive
// trigger writes. Callers inside fetch/reaper loops may fire hundreds of
// times per cycle; this ensures the watcher only picks up one trigger
// per debounce window instead of spawning overlapping reloads.
var writeReloadTriggerDebounce = 30 * time.Second

var lastReloadTriggerTime struct {
	sync.Mutex
	ts      time.Time
	pending bool // true if a suppressed call is waiting for trailing write
}

func writeReloadTrigger(path string) error {
	now := time.Now()
	lastReloadTriggerTime.Lock()
	elapsed := now.Sub(lastReloadTriggerTime.ts)
	if elapsed < writeReloadTriggerDebounce {
		if !lastReloadTriggerTime.pending {
			lastReloadTriggerTime.pending = true
			remaining := writeReloadTriggerDebounce - elapsed
			time.AfterFunc(remaining, func() { doWriteReloadTrigger(path) })
		}
		lastReloadTriggerTime.Unlock()
		return nil
	}
	lastReloadTriggerTime.ts = now
	lastReloadTriggerTime.Unlock()
	return doWriteReloadTrigger(path)
}

var reloadTriggerMutex sync.Mutex

func doWriteReloadTrigger(path string) error {
	reloadTriggerMutex.Lock()
	defer reloadTriggerMutex.Unlock()

	lastReloadTriggerTime.Lock()
	lastReloadTriggerTime.pending = false
	lastReloadTriggerTime.ts = time.Now()
	lastReloadTriggerTime.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	seq, _ := readReloadSeq(path)
	seq++
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(seq)), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// parseLockContent extracts the pid and timestamp from lock content. ok is
// false for empty, truncated or unparseable content - which must NOT be
// treated as stale on sight while an acquirer may still be mid-write (see
// the write-grace guard in acquireProxyLockAt).
func parseLockContent(data []byte) (pid int, ts int64, ok bool) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, 0, false
	}
	ts, err = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return pid, ts, true
}

func isLockStale(data []byte) bool {
	// Only the first two lines (pid, timestamp) are interpreted; the third
	// is a per-acquisition nonce, and any trailing content must not confuse
	// the parse.
	pid, ts, ok := parseLockContent(data)
	if !ok {
		return true
	}

	// Liveness is the authoritative signal: a lock held by a LIVE process is
	// never stale, however old, because a legitimately slow reload (>5min on
	// a large fleet) must not be stolen by a second acquirer. Only treat as
	// stale when the holder is conclusively gone.
	process, err := os.FindProcess(pid)
	if err == nil {
		err = process.Signal(syscall.Signal(0))
	}
	if err == nil {
		// Holder alive and working - stale ONLY if the lock is
		// unreasonably old (PID-reuse bound; proxyLockMaxAge).
		return time.Since(time.Unix(ts, 0)) > proxyLockMaxAge
	}
	if errors.Is(err, os.ErrProcessDone) ||
		strings.Contains(err.Error(), "no such process") ||
		strings.Contains(err.Error(), "process already finished") {
		// Holder conclusively gone - stale.
		return true
	}
	// Inconclusive (e.g. EPERM: another user's live process we can't
	// signal). Use age as the tiebreaker; very old + unverifiable = stale.
	return time.Since(time.Unix(ts, 0)) > proxyLockStaleAge
}
