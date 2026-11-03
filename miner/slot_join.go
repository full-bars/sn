package miner

import (
	"bytes"
	"context"
	"errors"
	"runtime/pprof"
	"sync"
	"time"
)

// slotJoin counts every worker the provide run is waiting on: the slots it
// launched at start, the slots a reload started later, and the retries it has
// scheduled for a worker that failed. The run ends when the count reaches zero
// and no reload is in flight.
//
// A plain sync.WaitGroup over the launch loop ends the run as soon as the
// original slots return, which discards a replacement a reload has just
// started and a retry that has not fired yet.
type slotJoin struct {
	mu     sync.Mutex
	n      int
	sealed bool
	wake   chan struct{}
}

func newSlotJoin() *slotJoin {
	return &slotJoin{wake: make(chan struct{}, 1)}
}

// add registers one worker. It reports false once the join has been released,
// when the run is already ending and nothing new may start.
func (j *slotJoin) add() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.sealed {
		return false
	}
	j.n++
	return true
}

// done releases one worker registered with add.
func (j *slotJoin) done() {
	j.mu.Lock()
	j.n--
	zero := j.n == 0
	j.mu.Unlock()
	if zero {
		select {
		case j.wake <- struct{}{}:
		default:
		}
	}
}

// wait blocks until every registered worker has finished. quiesce runs each
// time the count reaches zero and must return only when any reload that was
// already running has finished: that reload may be about to add the
// replacement for the worker that just returned, so zero is only believed
// after it. wait then seals the join, so a late add is refused.
func (j *slotJoin) wait(quiesce func()) {
	for {
		j.mu.Lock()
		n := j.n
		j.mu.Unlock()
		if n > 0 {
			<-j.wake
			continue
		}
		if quiesce != nil {
			quiesce()
		}
		j.mu.Lock()
		if j.n == 0 {
			j.sealed = true
			j.mu.Unlock()
			return
		}
		j.mu.Unlock()
	}
}

// providerShutdownGrace is how long the run waits for its workers after the run
// context is cancelled. A worker that ignores cancellation would otherwise pin
// the process in a half-stopped state forever: the supervisor's stop becomes a
// SIGKILL of a process that was still draining, or nothing at all. A variable
// so tests can shorten it.
//
// 35s, not the 90s systemd default stop timeout: a clean drain finishes well
// inside it, and the expiry path (grace plus providerShutdownExitBound, 40s in
// total) still ends the process before systemd would SIGKILL it.
var providerShutdownGrace = 35 * time.Second

// providerShutdownExitBound is how long after the grace expires the process is
// ended whatever run's deferred calls are doing. It bounds the defer chain, not
// only the worker wait: see providerShutdownExpire.
var providerShutdownExitBound = 5 * time.Second

// providerShutdownWatchdog arms the process exit after the grace has expired.
// A variable so tests can observe the arming without taking the test binary
// down.
var providerShutdownWatchdog = func(after time.Duration, code int) {
	time.AfterFunc(after, func() { exitProviderRun(code) })
}

// providerShutdownTimeoutExitCode ends a run whose workers did not drain within
// providerShutdownGrace. It is a failure, so a supervisor (systemd
// Restart=on-failure, the image's own supervisor) restarts a clean process.
const providerShutdownTimeoutExitCode = 1

// errProviderShutdownTimeout marks a run that gave up waiting for its workers.
// It only selects the exit code; see providerRunExitCode.
var errProviderShutdownTimeout = errors.New("provider shutdown did not complete")

// providerShutdownDumpMarker starts the goroutine dump a timed-out shutdown
// writes, so the next incident is diagnosable from the log alone.
const providerShutdownDumpMarker = "[provider] shutdown incomplete: goroutine dump follows"

// waitBounded is wait with a deadline that starts only once ctx is cancelled:
// a live run waits as long as it always did. When the grace expires with
// workers still registered it writes the goroutine dump through logf and
// returns errProviderShutdownTimeout; the blocked wait is abandoned, which is
// safe because the caller exits the process.
func (j *slotJoin) waitBounded(ctx context.Context, quiesce func(), grace time.Duration, logf func(format string, args ...any)) error {
	drained := make(chan struct{})
	go func() {
		j.wait(quiesce)
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-drained:
		return nil
	case <-timer.C:
	}
	j.mu.Lock()
	remaining := j.n
	j.mu.Unlock()
	logf("%s (%d workers still running after %s)\n", providerShutdownDumpMarker, remaining, grace)
	var buf bytes.Buffer
	if p := pprof.Lookup("goroutine"); p != nil {
		// Grouped by stack with counts: a fleet box holds tens of thousands
		// of goroutines, and the full per-goroutine form is too large to log.
		_ = p.WriteTo(&buf, 1)
	}
	logf("%s\n", buf.String())
	return errProviderShutdownTimeout
}

// providerShutdownExpire runs on the timeout path, once the grace has expired
// and before run's deferred calls. It arms the exit watchdog first, then writes
// the incident bundle, whose goroutines.txt is the dump that matters.
//
// Why the watchdog comes first: the bundle is captured in run's caller only
// after run has returned, and run returns through every defer it registered.
// Those can block. The ones that matter here are reloader.stop (takes r.mu,
// which a reload stuck on a worker holds), <-reloadWatchDone,
// controlSocket.finish, bringUp.shutdown, joinRenewal, keyOwner.Close,
// status.close and progress.close. None of them is skipped. Skipping one would
// drop its persistence (keys, control state, status), and a clean one still
// needs to run. The watchdog ends the process with
// providerShutdownTimeoutExitCode at providerShutdownExitBound whether or not
// the chain has finished, so no defer can keep this process alive past the
// grace. flushPersistentErrorsAtExit and waitExtenderRefusalsDurable were
// already bounded by their own timers.
func providerShutdownExpire(detail string) {
	providerShutdownWatchdog(providerShutdownExitBound, providerShutdownTimeoutExitCode)
	captureIncidentBundle(incidentTriggerShutdown, detail, nil)
}
