//go:build linux

package urnettools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordSubprocesses replaces the subprocess seam with fn and returns the
// commands it was asked to run, as "name arg arg ...". The seam is called from
// probe goroutines, so the record is guarded.
func recordSubprocesses(t *testing.T, fn func(ctx context.Context, name string, args ...string) ([]byte, error)) func() []string {
	t.Helper()
	execMu.Lock()
	orig := execCommandFn
	execMu.Unlock()
	t.Cleanup(func() {
		execMu.Lock()
		execCommandFn = orig
		execMu.Unlock()
	})
	var mu sync.Mutex
	var calls []string
	wrapped := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		mu.Unlock()
		return fn(ctx, name, args...)
	}
	execMu.Lock()
	execCommandFn = wrapped
	execMu.Unlock()
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
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	stubSystemd(t, true)
	noOpenRC(t)
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
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release // ignores its context, like a wedged systemctl
			return nil, errors.New("released")
		}
		return nil, errors.New("fast failure")
	})

	done := make(chan []Provider, 1)
	go func() { done <- discoverStopped(nil) }()
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

// attachUnits cannot exceed the budget: a stubbed userUnitOwns that blocks
// longer than a shrunk systemdDiscoveryBudget must not extend discoverStopped
// past the budget, and the caller still gets the process records.
func TestDiscoverAttachUnitsCannotExceedBudget(t *testing.T) {
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	stubSystemd(t, true)
	noOpenRC(t)
	origBudget := systemdDiscoveryBudget
	systemdDiscoveryBudget = 50 * time.Millisecond
	t.Cleanup(func() { systemdDiscoveryBudget = origBudget })

	origCgroup := readCgroupFn
	t.Cleanup(func() { readCgroupFn = origCgroup })
	readCgroupFn = func(pid int) (string, error) {
		return userUnitCgroup, nil
	}

	origOwns := userUnitOwns
	t.Cleanup(func() { userUnitOwns = origOwns })
	userUnitOwns = func(ctx context.Context, p Provider, unit string) bool {
		select {
		case <-time.After(2 * time.Second):
			return true
		case <-ctx.Done():
			return false
		}
	}

	procs := []Provider{{PID: 99999, User: "alice", Binary: "/opt/urnetwork/bin/provider"}}
	done := make(chan []Provider, 1)
	go func() { done <- discoverStopped(procs) }()

	select {
	case <-done:
		// Completed within budget. Process records are preserved.
		if len(procs) != 1 || procs[0].PID != 99999 || procs[0].User != "alice" {
			t.Fatalf("caller process records lost: %+v", procs)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("attachUnits exceeded systemdDiscoveryBudget")
	}
}

// A cut-short pass is reported: after a timed-out discovery, the flag is set
// and the user-facing message says the scan did not finish.
func TestDiscoverCutShortPassReported(t *testing.T) {
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	stubSystemd(t, true)
	noOpenRC(t)
	origBudget := systemdDiscoveryBudget
	systemdDiscoveryBudget = 50 * time.Millisecond
	t.Cleanup(func() { systemdDiscoveryBudget = origBudget })

	recordSubprocesses(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	out := discoverStopped(nil)
	if len(out) != 0 {
		t.Fatalf("expected no units, got %+v", out)
	}
	if !systemdDiscoveryCutShort {
		t.Fatal("expected systemdDiscoveryCutShort to be true after a timed-out discovery")
	}

	// selectTarget with no providers must report the timeout
	_, err := selectTarget(nil, Target{})
	if err == nil {
		t.Fatal("expected selectTarget to fail with no providers")
	}
	msg := err.Error()
	if !strings.Contains(msg, "systemd could not be scanned in time") || !strings.Contains(msg, "stopped units may be missing") || !strings.Contains(msg, "scan did not finish") {
		t.Fatalf("expected message to say systemd could not be scanned in time and stopped units may be missing (scan did not finish), got %q", msg)
	}

	// cmdStatus must also report this error
	origDisc := discoverStatusFn
	t.Cleanup(func() { discoverStatusFn = origDisc })
	discoverStatusFn = func() []Provider { return nil }
	statusErr := cmdStatus(nil)
	if statusErr == nil || !strings.Contains(statusErr.Error(), "scan did not finish") {
		t.Fatalf("cmdStatus expected cut-short error, got %v", statusErr)
	}
}

// Many units stay inside the budget: with N units whose property lookups each
// take a fixed delay, discovery completes inside the budget for N that would
// fail when serialised. Concurrency is asserted structurally (concurrent calls
// overlap) and via a budget with enough headroom that host load cannot trip it,
// while remaining strictly below the serial execution floor.
func TestDiscoverManyUnitsStayInsideBudget(t *testing.T) {
	const n = 24
	const fixedDelay = 25 * time.Millisecond
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("urnetwork-node%d.service loaded active running", i))
	}
	text := strings.Join(lines, "\n")

	// Serial execution of N units would take at least n * fixedDelay (600ms).
	// Under 8 concurrent workers, ideal duration is 3 * fixedDelay (75ms).
	// Budget of 400ms provides >5x headroom for scheduling delay under host contention,
	// while remaining well below the 600ms serial bound so serial execution cannot pass.
	serialBound := time.Duration(n) * fixedDelay
	origBudget := systemdDiscoveryBudget
	systemdDiscoveryBudget = 400 * time.Millisecond
	t.Cleanup(func() { systemdDiscoveryBudget = origBudget })

	var active atomic.Int32
	var maxConcurrent atomic.Int32

	start := time.Now()
	providers := parseUnitLines(text, nil,
		func(unit string) string { return "user" },
		func(unit string) string {
			cur := active.Add(1)
			for {
				m := maxConcurrent.Load()
				if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
					break
				}
			}
			time.Sleep(fixedDelay)
			active.Add(-1)
			return "/opt/urnetwork/bin/urnetwork"
		},
	)
	elapsed := time.Since(start)

	if len(providers) != n {
		t.Fatalf("expected %d providers, got %d", n, len(providers))
	}
	if maxConcurrent.Load() < 2 {
		t.Fatalf("expected concurrent unit property lookups, max concurrency was %d", maxConcurrent.Load())
	}
	if elapsed >= serialBound {
		t.Fatalf("discovery took %v, exceeding serial bound %v", elapsed, serialBound)
	}
	if elapsed > systemdDiscoveryBudget {
		t.Fatalf("discovery took %v, exceeding budget %v (serial bound %v, concurrent ideal ~%v)",
			elapsed, systemdDiscoveryBudget, serialBound, time.Duration(n/parseUnitWorkerCap)*fixedDelay)
	}
}

// A cancelled pass leaves no subprocess running: after the budget expires,
// the subprocess count returns to zero rather than lingering.
func TestDiscoverCancelledPassLeavesNoSubprocessRunning(t *testing.T) {
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	stubSystemd(t, true)
	noOpenRC(t)
	origBudget := systemdDiscoveryBudget
	systemdDiscoveryBudget = 50 * time.Millisecond
	t.Cleanup(func() { systemdDiscoveryBudget = origBudget })

	var activeSubprocesses atomic.Int32
	recordSubprocesses(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		activeSubprocesses.Add(1)
		defer activeSubprocesses.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	discoverStopped(nil)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && activeSubprocesses.Load() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if remaining := activeSubprocesses.Load(); remaining != 0 {
		t.Fatalf("expected 0 running subprocesses after cancellation, got %d lingering", remaining)
	}
}

// A cut pass must not return half-built entries as normal rows. When unitProperty
// hits a budget expiry on an exact-name unit, the degraded entry with empty binary
// and empty user is dropped, and the cut-short flag is set.
func TestDiscoverDropsDegradedEntryOnBudgetExpiry(t *testing.T) {
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	stubSystemd(t, true)
	noOpenRC(t)
	origBudget := systemdDiscoveryBudget
	systemdDiscoveryBudget = 50 * time.Millisecond
	t.Cleanup(func() { systemdDiscoveryBudget = origBudget })

	recordSubprocesses(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 {
			if args[0] == "--plain" && len(args) > 1 && args[1] == "list-units" {
				return []byte("urnetwork.service loaded active running URnetwork Provider\n"), nil
			}
			if args[0] == "show" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("not matched")
	})

	out := discoverStopped(nil)
	if len(out) != 0 {
		t.Fatalf("expected degraded entry to be dropped, got: %+v", out)
	}
	if !systemdDiscoveryCutShort {
		t.Fatal("expected systemdDiscoveryCutShort to be true after property budget expiry")
	}
}

// attachUnits sets systemdDiscoveryCutShort when the budget expires during attach.
func TestAttachUnitsSetsCutShortOnBudgetExpiry(t *testing.T) {
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired

	procs := []Provider{{PID: 1234, User: "u", Binary: "/opt/urnetwork/bin/urnetwork"}}
	attachUnits(ctx, procs)

	if !systemdDiscoveryCutShort {
		t.Fatal("expected systemdDiscoveryCutShort to be true after attachUnits with expired ctx")
	}
	if procs[0].Unit != "" {
		t.Fatalf("unit should not be attached on expired ctx, got %q", procs[0].Unit)
	}
}

// Attach and probe share a single budget: if attach consumes most of the budget,
// the probe stage must be cut rather than receiving a fresh full budget.
func TestDiscoverAttachAndProbeShareSingleBudget(t *testing.T) {
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	stubSystemd(t, true)
	noOpenRC(t)
	origBudget := systemdDiscoveryBudget
	systemdDiscoveryBudget = 100 * time.Millisecond
	t.Cleanup(func() { systemdDiscoveryBudget = origBudget })

	origCgroup := readCgroupFn
	t.Cleanup(func() { readCgroupFn = origCgroup })
	readCgroupFn = func(pid int) (string, error) {
		return userUnitCgroup, nil
	}

	origOwns := userUnitOwns
	t.Cleanup(func() { userUnitOwns = origOwns })
	// Attach consumes 60ms of the 100ms budget (~60%).
	userUnitOwns = func(ctx context.Context, p Provider, unit string) bool {
		select {
		case <-time.After(60 * time.Millisecond):
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Probe stage wants 60ms. With a shared 100ms clock only ~40ms remains,
	// so it must be cut by ctx.Done() rather than finishing cleanly under
	// a fresh 100ms clock.
	var probeOnce sync.Once
	recordSubprocesses(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		probeOnce.Do(func() {
			select {
			case <-time.After(60 * time.Millisecond):
			case <-ctx.Done():
			}
		})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if name == "systemctl" && len(args) > 1 && args[1] == "list-units" {
			return []byte("urnetwork.service loaded active running URnetwork\n"), nil
		}
		return nil, nil
	})

	procs := []Provider{{PID: 99999, User: "alice", Binary: "/opt/urnetwork/bin/provider"}}
	out := discoverStopped(procs)

	if !systemdDiscoveryCutShort {
		t.Fatal("expected systemdDiscoveryCutShort to be true when attach and probe exceed shared budget")
	}
	if len(out) != 0 {
		t.Fatalf("expected probe stage to be cut and return no stopped units, got %+v", out)
	}
}


