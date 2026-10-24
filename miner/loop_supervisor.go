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

// loopStatusOf returns a copy of the named loop's status, registering it.
func loopStatusOf(name string) loopStatus {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	return *loopStatusLocked(name)
}

func loopStatusLocked(name string) *loopStatus {
	st, ok := loopStatuses[name]
	if !ok {
		st = &loopStatus{}
		loopStatuses[name] = st
	}
	return st
}

func setLoopUp(name string, up bool, restart bool) {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	st := loopStatusLocked(name)
	st.Up = up
	if restart {
		st.Restarts++
	}
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
// panics or returns while ctx is still live. connect.HandleError contains the
// panic and logs it with a stack, exactly as the provider's does. onFail
// (optional) runs after every failed run, before the backoff, so a loop's
// actuators can be put back to neutral instead of staying wherever the dead
// loop left them.
func superviseLoop(ctx context.Context, name string, fn func(), onFail func()) {
	var backoff time.Duration
	loopStatusOf(name) // register so the series exists before the first failure
	for {
		started := time.Now()
		setLoopUp(name, true, false)
		connect.HandleError(fn)
		setLoopUp(name, false, false)
		if ctx.Err() != nil {
			return
		}
		backoff = nextLoopBackoff(backoff, time.Since(started))
		tlog("[miner][supervisor] loop %s ended unexpectedly, restarting in %s\n", name, backoff)
		if onFail != nil {
			connect.HandleError(onFail)
		}
		if !loopSleep(ctx, backoff) || ctx.Err() != nil {
			return
		}
		// Counted only once another run is about to start, so a shutdown
		// during the backoff does not report a restart that never happened.
		setLoopUp(name, false, true)
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
