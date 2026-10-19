//go:build linux

// Descendant regressions own a private subreaper process. None may send a
// child it adopts, intended or leaked, to the enclosing release gate.
package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Subreaper state belongs to a dedicated test subprocess, never the shared
// parallel test process. Cleanup kills the fixture first, then reaps here
// exactly the descendants that survive their parents: zero proves that the
// fixture's parents waited for every process they started.
func runSupervisorDescendantOwnerTest(t *testing.T, surviving int, timeout time.Duration, run func(*testing.T)) {
	t.Helper()
	const ownerKey = "SN_SUPERVISOR_DESCENDANT_TEST_OWNER"
	if os.Getenv(ownerKey) != t.Name() {
		ctx, cancel := context.WithTimeout(t.Context(), timeout+5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.timeout="+timeout.String())
		command.Env = append(os.Environ(), ownerKey+"="+t.Name())
		command.WaitDelay = 5 * time.Second
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated descendant owner failed: %v\n%s", err, output)
		}
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reaped := 0
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, 0, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.ECHILD) {
				break
			}
			if err != nil {
				t.Errorf("private descendant reap failed: %v", err)
				break
			}
			reaped++
			if pid <= 1 || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Errorf("unexpected descendant wait receipt: pid=%d status=%v", pid, status)
			}
		}
		if reaped != surviving {
			t.Errorf("private fixture must reap exactly %d surviving descendants, got %d", surviving, reaped)
		}
	})
	run(t)
}
