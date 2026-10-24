//go:build linux

package miner

// Tests for the self-exit stall path (provider/liveness_selfexit_linux.go): a
// stalled miner under a supervisor with no watchdog exits 75 and leaves the
// lean start cap; the same stall is refused when there is no supervisor to
// restart it, when the state dir would not survive the restart, and when
// self-heal is off.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// livenessSelfExitRig points the supervisor seams at the given environment,
// drives the tick fast, records any exit code and leaves progress well past the
// stall budget: a miner that is alive but making no progress.
func livenessSelfExitRig(t *testing.T, env map[string]string) chan int {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	prevEnv, prevFiles, prevExit, prevTick := thrashEnvFn, thrashFileExistsFn, swapThrashExitFn, livenessSelfExitTick
	prevWritable := thrashCheckCapDirWritableFn
	prevTicked, prevNano := processLiveness.ticked.Load(), processLiveness.nano.Load()
	t.Cleanup(func() {
		thrashEnvFn, thrashFileExistsFn, swapThrashExitFn, livenessSelfExitTick = prevEnv, prevFiles, prevExit, prevTick
		thrashCheckCapDirWritableFn = prevWritable
		processLiveness.ticked.Store(prevTicked)
		processLiveness.nano.Store(prevNano)
	})
	thrashEnvFn = func(k string) string { return env[k] }
	thrashFileExistsFn = func(string) bool { return false }
	livenessSelfExitTick = 5 * time.Millisecond

	exited := make(chan int, 1)
	swapThrashExitFn = func(c int) { exited <- c }

	prevRunning := oomCapRunningProxies.Load()
	t.Cleanup(func() { oomCapRunningProxies.Store(prevRunning) })
	oomCapRunningProxies.Store(500)

	processLiveness.nano.Store(time.Now().Add(-livenessStaleAfter - time.Minute).UnixNano())
	processLiveness.ticked.Store(true)
	return exited
}

// waitSelfExit waits for the recorded exit and pins the code.
func waitSelfExit(t *testing.T, exited chan int, what string) {
	t.Helper()
	select {
	case code := <-exited:
		if code != swapThrashExitCode {
			t.Fatalf("exit code = %d, want swapThrashExitCode (%d)", code, swapThrashExitCode)
		}
		if code != 75 {
			t.Fatalf("the provider's planned-restart code is 75, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s must exit so its supervisor restarts it", what)
	}
}

// assertNoSelfExit runs the loop for a short window and asserts no exit and no
// restart entry, the shape every refusal here must have.
func assertNoSelfExit(t *testing.T, exited chan int, run func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		run(ctx)
		close(done)
	}()
	select {
	case code := <-exited:
		t.Fatalf("a refused self-exit must not exit, got %d", code)
	case <-done:
	}
	if len(readThrashCapState().Restarts) != 0 {
		t.Fatal("a refused self-exit must not leave a restart entry behind")
	}
}

// A stalled miner in a container exits 75 and leaves the same lean start cap a
// thrash escape leaves.
func TestLivenessSelfExitExitsOnStallInAContainer(t *testing.T) {
	exited := livenessSelfExitRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go runSelfExitLivenessWatchdog(ctx, livenessGate)

	waitSelfExit(t, exited, "a stalled miner in a container")
	if cap, ok := activeThrashCap(time.Now()); !ok || cap != 300 {
		t.Fatalf("the exit must leave the lean start cap (300 of 500), got %d ok=%v", cap, ok)
	}
}

// The requirement the parent task pins: with no supervisor to restart it, the
// self-exit path must be refused. The production entry point is driven with a
// stalled process, self-heal on, and an ordinary host: nothing exits and
// nothing is recorded.
func TestLivenessSelfExitRefusedWithoutSupervisor(t *testing.T) {
	exited := livenessSelfExitRig(t, map[string]string{})
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	t.Setenv("WATCHDOG_USEC", "")
	t.Setenv("NOTIFY_SOCKET", "")

	assertNoSelfExit(t, exited, func(ctx context.Context) { runSdWatchdog(ctx, livenessGate) })
}

// Self-heal off: the miner keeps running and leaves no restart entry.
func TestLivenessSelfExitRespectsTheSelfHealSwitch(t *testing.T) {
	exited := livenessSelfExitRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "")

	assertNoSelfExit(t, exited, func(ctx context.Context) { runSelfExitLivenessWatchdog(ctx, livenessGate) })
}

// A container's state directory can be a volume that does not survive the
// restart; without a durable throttle record a self-exit could loop, so it is
// refused.
func TestLivenessSelfExitRefusedWhenTheStateDirIsNotDurable(t *testing.T) {
	exited := livenessSelfExitRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	thrashCheckCapDirWritableFn = func() error { return context.DeadlineExceeded }

	assertNoSelfExit(t, exited, func(ctx context.Context) { runSelfExitLivenessWatchdog(ctx, livenessGate) })
}

// The exit is only safe with its throttle record on disk: a write that cannot
// land must refuse the exit rather than restart unthrottled.
func TestLivenessSelfExitRefusedWhenTheRestartCannotBeRecorded(t *testing.T) {
	exited := livenessSelfExitRig(t, map[string]string{"URNETWORK_CONTAINER": "1"})
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	thrashCheckCapDirWritableFn = func() error { return nil } // the probe is satisfied

	// a directory where the cap file belongs: reading sees no entries, writing fails
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, thrashCapFileName), 0o700); err != nil {
		t.Fatal(err)
	}

	assertNoSelfExit(t, exited, func(ctx context.Context) { runSelfExitLivenessWatchdog(ctx, livenessGate) })
}

// The gate itself: self-heal must be on, and at the start of an episode the
// daily ring must allow another restart.
func TestLivenessGate(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	t.Setenv("URNETWORK_SELF_HEAL", "1")
	if ok, _ := livenessGate(true); !ok {
		t.Fatal("self-heal on and a fresh budget: the gate is open")
	}

	t.Setenv("URNETWORK_SELF_HEAL", "")
	if ok, why := livenessGate(true); ok || !strings.Contains(why, "self-heal is off") {
		t.Fatalf("self-heal off closes the gate, got ok=%v %q", ok, why)
	}
	t.Setenv("URNETWORK_SELF_HEAL", "1")

	// spend the daily ring: three recent restarts
	now := time.Now()
	for i := 0; i < thrashMaxRestarts24h; i++ {
		if err := recordThrashEscalation(0, now.Add(-time.Duration(i+1)*3*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if ok, why := livenessGate(true); ok || !strings.Contains(why, "restart budget is spent") {
		t.Fatalf("a spent ring closes the gate at the start of an episode, got ok=%v %q", ok, why)
	}
	if ok, _ := livenessGate(false); !ok {
		t.Fatal("mid-episode the budget is not asked again (it includes the episode's own entry)")
	}
}

// A stall under an already-standing cap keeps it instead of compounding down
// from the capped count, and the undo removes only the entry it added.
func TestLivenessRecordStallKeepsAnExistingCap(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	prevRunning := oomCapRunningProxies.Load()
	t.Cleanup(func() { oomCapRunningProxies.Store(prevRunning) })
	oomCapRunningProxies.Store(500)

	recordLivenessStall()
	if cap, ok := activeThrashCap(time.Now()); !ok || cap != 300 {
		t.Fatalf("first stall: cap 300 expected, got %d ok=%v", cap, ok)
	}
	oomCapRunningProxies.Store(300)
	time.Sleep(1100 * time.Millisecond) // a distinct ring timestamp
	recordLivenessStall()
	if cap, _ := activeThrashCap(time.Now()); cap != 300 {
		t.Fatalf("a stall under a standing cap must keep it, not compound to %d", cap)
	}
}
