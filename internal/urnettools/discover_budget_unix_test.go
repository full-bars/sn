//go:build linux

package urnettools

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordSubprocesses replaces the subprocess seam with fn and returns the
// commands it was asked to run, as "name arg arg ...". The seam is called from
// probe goroutines, so the record is guarded.
func recordSubprocesses(t *testing.T, fn func(ctx context.Context, name string, args ...string) ([]byte, error)) func() []string {
	t.Helper()
	orig := execCommandFn
	t.Cleanup(func() { execCommandFn = orig })
	var mu sync.Mutex
	var calls []string
	execCommandFn = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		mu.Unlock()
		return fn(ctx, name, args...)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// noOpenRC keeps the OpenRC discovery branch out of these tests: no init
// script, no rc-service, no /run/openrc. It also fails the test if any of
// those probes run through the subprocess seam, so the record stays complete.
func noOpenRC(t *testing.T) {
	t.Helper()
	origStat, origLook, origRun, origProbe := openrcStatFn, openrcLookPathFn, openrcRunFn, openrcProbeFn
	t.Cleanup(func() {
		openrcStatFn, openrcLookPathFn, openrcRunFn, openrcProbeFn = origStat, origLook, origRun, origProbe
	})
	openrcStatFn = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	openrcLookPathFn = func(string) (string, error) { return "", errors.New("not installed") }
	openrcRunFn = func(...string) error { return errors.New("not installed") }
	openrcProbeFn = func() bool { return false }
}

func stubSystemd(t *testing.T, present bool) {
	t.Helper()
	orig := systemdRunningFn
	t.Cleanup(func() { systemdRunningFn = orig })
	systemdRunningFn = func() bool { return present }
}

// Without systemd as init, discovery must not start a single subprocess: no
// systemctl, no getent, nothing. That is the whole fast path.
func TestDiscoverStoppedWithoutSystemdRunsNoSubprocess(t *testing.T) {
	stubSystemd(t, false)
	noOpenRC(t)
	calls := recordSubprocesses(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Error("no subprocess may run when systemd is not the init")
		return nil, errors.New("unexpected")
	})

	if out := discoverStopped(nil); len(out) != 0 {
		t.Fatalf("no units and no OpenRC service should yield nothing, got %+v", out)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("expected no subprocess calls, got %q", got)
	}
}

// The marker being present must keep the systemctl probes. The fast path
// must not quietly disable discovery on a systemd box.
func TestDiscoverStoppedWithSystemdStillRunsSystemctlProbes(t *testing.T) {
	stubSystemd(t, true)
	noOpenRC(t)
	// list-units succeeds with no units, which lets the list-unit-files probe
	// follow it, as it does on a real box.
	calls := recordSubprocesses(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && args[len(args)-1] == "--no-pager" && strings.Contains(strings.Join(args, " "), "list-units") {
			return nil, nil
		}
		return nil, errors.New("probe failed")
	})

	discoverStopped(nil)
	got := strings.Join(calls(), "\n")
	for _, want := range []string{
		"systemctl --plain list-units --all --no-legend --no-pager",
		"systemctl --plain list-unit-files --no-legend --no-pager",
		"getent passwd",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("systemd present: want subprocess %q, got:\n%s", want, got)
		}
	}
}

// A probe that never returns must not hold the pass past its budget. The
// call comes back while the probe is still blocked. Its own timeout is
// 5 seconds, so only the budget can end the pass this early. The failure
// guard is generous and fires only when the budget is broken.
func TestDiscoverSystemdReturnsWithinBudgetWhileAProbeHangs(t *testing.T) {
	origBudget := systemdDiscoveryBudget
	systemdDiscoveryBudget = 50 * time.Millisecond
	t.Cleanup(func() { systemdDiscoveryBudget = origBudget })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// entered is signalled by the hung call. The send orders the probe's read
	// of the seam before the test restores it, so the race detector is happy.
	entered := make(chan struct{}, 1)
	recordSubprocesses(t, func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "--plain" {
			entered <- struct{}{}
			<-release // ignores its context, like a wedged systemctl
			return nil, errors.New("released")
		}
		return nil, errors.New("fast failure")
	})

	done := make(chan []Provider, 1)
	go func() { done <- discoverSystemdUnits(nil) }()
	select {
	case out := <-done:
		if len(out) != 0 {
			t.Fatalf("a hung probe contributes nothing, got %+v", out)
		}
		<-entered
	case <-time.After(10 * time.Second):
		t.Fatal("discovery did not return within its budget while a probe hung")
	}
}
