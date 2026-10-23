package miner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cleanStaleSelfProxyLock removes only a lock carrying this process's own
// pid with a timestamp at or before this process's start: other-pid and
// future self locks are left alone.
func TestCleanStaleSelfProxyLock(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	path, err := proxyLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// Pin processStart so the same-integer-second boundary is deterministic.
	oldStart := processStart
	processStart = time.Unix(1700000000, 0)
	t.Cleanup(func() { processStart = oldStart })

	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Another pid is never touched.
	write("424242\n1699999999\n7\n")
	cleanStaleSelfProxyLock()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a lock with another pid must be left alone")
	}
	// A self lock from after this process's start is its own and stays.
	write(fmt.Sprintf("%d\n%d\n8\n", os.Getpid(), processStart.Unix()+1))
	cleanStaleSelfProxyLock()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a self lock from after the start must be left alone")
	}
	// Same integer second as the start: a previous image's remnant (this
	// image runs cleanup before any local consumer can hold the lock).
	write(fmt.Sprintf("%d\n%d\n9\n", os.Getpid(), processStart.Unix()))
	cleanStaleSelfProxyLock()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a same-second self lock must be removed, stat err = %v", err)
	}
	// An old self lock is removed.
	write(fmt.Sprintf("%d\n%d\n10\n", os.Getpid(), processStart.Unix()-3600))
	cleanStaleSelfProxyLock()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an old self lock must be removed, stat err = %v", err)
	}
}
