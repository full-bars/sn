package miner

import (
	"runtime/debug"

	"github.com/urnetwork/connect"
)

// panic_guard.go is the miner's panic barrier for background goroutines that
// have no owner to restart them.
//
// A panic in a goroutine with no recover unwinds that goroutine and takes the
// whole process down: every proxy, the control socket and the in-flight claims
// go with it, and only the service supervisor's restart brings the node back.
// The manager loops and the miner's long-running loops are supervised
// (loop_supervisor.go), so a panic in one of those restarts that loop with
// backoff. Everything else runs one bounded piece of work — a request handler,
// a one-shot read, an immediate tick: the work is over by the time a panic
// could be reported, and a restart would have nothing to resume. Those get this
// barrier instead: the panic is recovered and logged with its stack by
// connect.HandleError (the helper the rest of this tree already uses), and only
// that goroutine ends.
//
// The tier follows the body's shape, not its name: a body that loops until the
// process or its context ends belongs under the supervisor so it comes back; a
// body that does one thing and returns is contained, because there is nothing
// left to restart.
//
// The barrier wraps the whole body, so the body's own defers still run while
// the panic unwinds: a lock, an admission slot or a channel it holds is
// released before the barrier returns. That is what keeps a contained panic
// from turning into a deadlock for whoever waits on the same object.

// runSafe runs fn on the calling goroutine behind the panic barrier.
func runSafe(name string, fn func()) {
	connect.HandleError(fn, func(err error) {
		if connect.IsDoneError(err) {
			// connect treats a context-cancellation panic as the standard
			// shutdown pattern and does not log it; a named line for every
			// teardown would only add noise.
			return
		}
		// HandleError has already logged the panic and its stack; this line
		// says which goroutine it was, which the stack cannot.
		importantLogf("[panic] recovered in %s: %v\n", name, err)
	})
}

// reportRecoveredPanic records a panic recovered in place, with the recovering
// goroutine's stack. It is for the one shape neither tier expresses: a body
// that owns a process-level side effect (the hotswap drain monitor releases the
// drain latch, cancels the run and exits non-zero) and must perform that
// cleanup itself rather than be restarted or merely ended.
func reportRecoveredPanic(name string, cause any) {
	importantLogf("[panic] recovered in %s: %v\n%s\n", name, cause, debug.Stack())
}

// goSafe starts fn on a new goroutine behind the panic barrier. It replaces a
// bare `go func() { ... }()` whose panic would otherwise kill the process, for
// one-shot and per-request bodies only: a long-running loop belongs under
// superviseLoop instead, or a contained panic leaves the feature down until the
// process restarts.
func goSafe(name string, fn func()) {
	go runSafe(name, fn)
}
