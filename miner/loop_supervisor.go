package miner

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// loop_supervisor.go is the miner's port of the provider's
// provider/loop_supervisor.go.
//
// The provider isolates its background control loops from each other: each one
// runs under superviseLoop, so a panic restarts that loop instead of unwinding
// past main. The miner started the manager's loops as bare
// `go func() { defer wg.Done(); runX() }()`, so one panic anywhere in them (a
// URL fetch cycle, a reaper pass, the pressure sampler, the pool controller, a
// grader, the audit, the baseline sampler) took the whole process down and with
// it every proxy slot. Only the per-slot workers in run.go had their own
// recover.
//
// Supervision here contains a panic to the loop that raised it: the loop is
// logged, counted, and restarted with a bounded backoff. The loop bodies are
// unchanged, and the manager's context still stops them: a return while the
// context is live is treated as a failure, a return after cancellation is a
// clean exit.
//
// A loop that runs its own worker pool must contain a worker panic too: the
// supervisor recovers on the loop's goroutine, not on a worker's, so an
// unrecovered worker panic still ends the process. The probe pools in
// proxy_manager_probe.go, proxy_manager_table_probe.go and
// proxy_manager_grade_paid.go call connect.HandleError around the worker body
// for exactly that reason.
const (
	loopBackoffBase = time.Second
	loopBackoffMax  = 5 * time.Minute
	// loopHealthyRun is how long a run must last before the backoff resets, so
	// a loop that works for a while and then fails starts again quickly.
	loopHealthyRun = 10 * time.Minute
)

// loopSleep waits d or until ctx ends; false means ctx ended. A seam for tests.
var loopSleep = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// loopStatus is what the metrics report about one supervised loop.
type loopStatus struct {
	Up       bool
	Restarts int64
}

var (
	loopStatusMu sync.Mutex
	loopStatuses = map[string]*loopStatus{}
)

// loopRef is one supervised loop's identity: the name the logs and the metrics
// use, and its registry entry. The entry is resolved by the caller
// (newLoopRef), before the loop's goroutine starts, so the loop's own goroutine
// never reads the supervisor's registry: the series exists before the loop can
// fail, and at shutdown nothing looks process-wide state up from the loop being
// stopped.
type loopRef struct {
	name   string
	status *loopStatus
}

// newLoopRef registers the named loop if it is not registered yet and returns
// its reference. Call it from the goroutine that starts the loop, never from
// the loop itself.
func newLoopRef(name string) loopRef {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	return loopRef{name: name, status: loopStatusLocked(name)}
}

// setUp publishes one status change for the loop.
func (ref loopRef) setUp(up bool, restart bool) {
	if ref.status == nil {
		// A reference built without newLoopRef has nothing registered to
		// report into. That must not panic on the loop's goroutine.
		return
	}
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	ref.status.Up = up
	if restart {
		ref.status.Restarts++
	}
}

// loopStatusOf returns a copy of the named loop's status, registering it.
func loopStatusOf(name string) loopStatus {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	return *loopStatusLocked(name)
}

func loopStatusLocked(name string) *loopStatus {
	if loopStatuses == nil {
		// A loop can be registered from another package-level initializer (the
		// global admission gate builds its dispatcher at init), which may run
		// before this file's map literal. Writing to a nil map would turn a
		// registration into a panic during package init, so make the map here
		// as well.
		loopStatuses = map[string]*loopStatus{}
	}
	st, ok := loopStatuses[name]
	if !ok {
		st = &loopStatus{}
		loopStatuses[name] = st
	}
	return st
}

// setLoopUp publishes a state change for the named loop.
func setLoopUp(name string, up bool, restart bool) {
	newLoopRef(name).setUp(up, restart)
}

// nextLoopBackoff doubles the previous backoff up to loopBackoffMax, and
// resets it after a run that lasted loopHealthyRun.
func nextLoopBackoff(prev, ranFor time.Duration) time.Duration {
	if prev <= 0 || ranFor >= loopHealthyRun {
		return loopBackoffBase
	}
	if next := prev * 2; next < loopBackoffMax {
		return next
	}
	return loopBackoffMax
}

// superviseLoop runs fn until ctx ends, restarting it with backoff when it
// panics or returns while ctx is still live. ref comes from newLoopRef on the
// caller's goroutine. connect.HandleError contains the panic and logs it with a
// stack, exactly as the provider's does. onFail (optional) runs after every
// failed run, before the backoff, so a loop's actuators can be put back to
// neutral instead of staying wherever the dead loop left them.
func superviseLoop(ctx context.Context, ref loopRef, fn func(), onFail func()) {
	var backoff time.Duration
	restartPending := false
	for {
		// Checked before the restart bookkeeping, not after the backoff: a
		// cancel that lands between the sleep returning and this point is a
		// shutdown, and it must neither start fn on a dead context nor count a
		// restart that never ran.
		if ctx.Err() != nil {
			ref.setUp(false, false)
			return
		}
		// Counted only once another run is really about to start, so a
		// shutdown during the backoff does not report a restart that never
		// happened.
		if restartPending {
			ref.setUp(false, true)
			restartPending = false
		}
		started := time.Now()
		ref.setUp(true, false)
		connect.HandleError(fn)
		ref.setUp(false, false)
		if ctx.Err() != nil {
			return
		}
		backoff = nextLoopBackoff(backoff, time.Since(started))
		tlog("[miner][supervisor] loop %s ended unexpectedly, restarting in %s\n", ref.name, backoff)
		if onFail != nil {
			connect.HandleError(onFail)
		}
		if !loopSleep(ctx, backoff) {
			return
		}
		restartPending = true
	}
}

// supervisedLoopSeam is the metrics seam body: a snapshot of every registered
// loop, sorted by the writer (writeSeamLoops, metrics_provider.go).
func supervisedLoopSeam() []seamLoop {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	loops := make([]seamLoop, 0, len(loopStatuses))
	for name, st := range loopStatuses {
		loops = append(loops, seamLoop{Name: name, Up: st.Up, Restarts: uint64(st.Restarts)})
	}
	return loops
}

// The miner owns the supervised-loop registry, so it fills the seam the
// metrics writer already reads (urnet_loop_up / urnet_loop_restarts_total).
// Guarded so a later owner of the same hook is not overwritten from here.
func init() {
	if metricsSeamLoops == nil {
		metricsSeamLoops = supervisedLoopSeam
	}
}
