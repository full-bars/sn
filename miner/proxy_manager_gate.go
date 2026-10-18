package miner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// proxy_manager_gate.go holds the startup grading gate: the one decision that
// keeps the proxy manager's active probe loops away from a box whose provider
// workers have not come up yet.
//
// The manager starts from main, long before the provide run has imported the
// 3.23-fix identities, opened the client key and launched a single provider
// worker. Its probe loops (the paid/file grader, the URL fetch + stage-1
// admission probe cycles, and the stale-reaper re-probe/demotion pass)
// describe what the box SERVES, and started at t=0 they graded an empty pool
// on a real box and wrote findings into proxy.state. The gate holds exactly
// those loops until the pool is actually up.
//
// The gate is automatic and has NO user-facing surface: no control key,
// environment variable, CLI flag or documented setting reads or writes it. Its
// thresholds are the private constants below; the decided reason is logged
// once, and after it opens it never closes. Launching, admission, the control
// socket and the 3.23-fix identity import never consult it, and the stored
// grade summary and the warmth/earnings launch ordering keep running un-gated.
//
// The gate decides from the accounting that already exists:
//   - desired: the provider slots the manager launches (its desired() set);
//   - running: the workers the provide run publishes as connected through the
//     provider progress owner (controlRuntime), read by gradingGateRunning.
//
// A direct-only or empty pool (desired == 0) opens immediately: there are no
// workers to wait for.

const (
	// gradingGateRunningPercent is the share of the desired pool that must be
	// running to open the gate ahead of its fallbacks.
	gradingGateRunningPercent = 90
	// gradingGatePlateau is how long a non-zero running count must hold still,
	// after gradingGateLaunchFloor of launches, before stragglers stop
	// blocking the gate.
	gradingGatePlateau = 60 * time.Second
	// gradingGateLaunchFloor is the minimum time since the first worker launch
	// before the plateau may fire: a pool still ramping is not a plateau.
	gradingGateLaunchFloor = 30 * time.Second
	// gradingGateDeadline is the absolute wait cap measured from the first
	// worker launch: the gate never waits forever, whatever the counts say.
	gradingGateDeadline = 10 * time.Minute
	// gradingGateWaitLogEvery paces the while-held log line.
	gradingGateWaitLogEvery = 10 * time.Second
	// gradingGatePollEvery paces the running-accounting polls.
	gradingGatePollEvery = time.Second
)

// The three open reasons, named in the single opened line.
const (
	gradingGateReasonRunning  = "running"
	gradingGateReasonPlateau  = "plateau"
	gradingGateReasonDeadline = "deadline"
)

// gradingGateRunning reads the running half of the existing per-slot
// accounting the provide run publishes (controlRuntime's provider progress
// owner, the same accounting the control report summarizes as connected
// slots): how many proxy workers are up, i.e. whose provider session is
// connected. "Running means connected" is the accounting the swarm status
// already uses (swarm.go status counts connected instances as running). The
// direct slot is not a proxy and is never counted; before the run publishes
// its accounting (or after it closes) nothing is up.
func gradingGateRunning() int {
	controlRuntime.mu.Lock()
	progress := controlRuntime.progress
	controlRuntime.mu.Unlock()
	if progress == nil {
		return 0
	}
	snapshot, err := progress.snapshot(context.Background())
	if err != nil || snapshot == nil {
		return 0
	}
	running := 0
	for _, member := range snapshot.Members {
		// providerRegistrationSlot(nil) is the direct slot's name; it lives in
		// the accounting but is not one of the desired proxy workers.
		if member.Slot == directProxyKey {
			continue
		}
		if member.Connected {
			running++
		}
	}
	return running
}

// gradingGate is one run's gate. It is created when the manager starts, opened
// at most once, and never closed.
type gradingGate struct {
	desired int

	// runningCount reads how many of the desired workers are up. Production
	// wires it to gradingGateRunning; tests drive their own.
	runningCount func() int
	// now is the clock. Tests replace it to exercise the plateau and the
	// deadline without sleeping them out.
	now func() time.Time

	mu sync.Mutex
	// running is the last observed count; lastRunningChange is when it last
	// moved (the plateau reads both).
	running           int
	lastRunningChange time.Time
	// launched is set by the provide run's readiness hook; launchedAt is the
	// first worker launch.
	launched   bool
	launchedAt time.Time
	// waitingSince is when the gate started holding; lastWaitLog paces the
	// while-held line. Both are diagnostics.
	waitingSince time.Time
	lastWaitLog  time.Time
	opened       bool
	openReason   string

	// done is closed when the gate opens, so waiters wake at once.
	done chan struct{}
}

// newGradingGate arms one gate for a pool of desired proxy workers.
func newGradingGate(desired int) *gradingGate {
	g := &gradingGate{
		desired:      desired,
		runningCount: gradingGateRunning,
		now:          time.Now,
		waitingSince: time.Now(),
		done:         make(chan struct{}),
	}
	if desired <= 0 {
		// Direct-only providing or an empty pool: there is nothing to wait
		// for, so the gate opens as it is armed.
		g.opened = true
		g.openReason = gradingGateCounts(0, desired)
		g.logOpened()
	}
	return g
}

// gradingGateCounts names the counts in every open reason: "running=X/Y".
func gradingGateCounts(running, desired int) string {
	return fmt.Sprintf("%s=%d/%d", gradingGateReasonRunning, running, desired)
}

// noteWorkerLaunches starts the gate's launch clock. The provide run calls it
// through noteProviderWorkerLaunches immediately before it starts its workers.
// The first call wins: a later restart of the launch loop never extends the
// deadline.
func (g *gradingGate) noteWorkerLaunches() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.launched {
		return
	}
	g.launched = true
	g.launchedAt = g.now()
}

// isOpen reports whether the gate has opened.
func (g *gradingGate) isOpen() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.opened
}

// wait blocks until the gate opens and reports whether it did; a context that
// ends first returns false. A nil gate (a caller that has no manager, e.g. a
// unit test driving a loop directly) is open.
func (g *gradingGate) wait(ctx context.Context) bool {
	if g == nil {
		return true
	}
	if g.isOpen() {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-g.done:
		return true
	}
}

// run polls the running accounting until the gate opens or ctx ends: the
// manager's own goroutine, one per process. While held it logs the counts every
// gradingGateWaitLogEvery; when it opens it logs the one line naming the
// reason.
func (g *gradingGate) run(ctx context.Context) {
	if g == nil || g.isOpen() {
		return
	}
	if g.observe() {
		return
	}
	ticker := time.NewTicker(gradingGatePollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if g.observe() {
			return
		}
	}
}

// observe applies one running observation: it opens the gate when a condition
// holds (logging the reason) or logs the paced while-held line. It reports
// whether the gate is open.
func (g *gradingGate) observe() bool {
	now := g.now()
	running := g.runningCount()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.opened {
		return true
	}
	if running != g.running {
		g.running = running
		g.lastRunningChange = now
	}
	if reason, open := g.decideLocked(now); open {
		g.opened = true
		g.openReason = reason
		close(g.done)
		g.logOpened()
		return true
	}
	g.logWaitingLocked(now)
	return false
}

// decideLocked is the first-of decision: the pool is up (running >= 90% of
// desired), the running count has plateaued after the launch floor, or the
// deadline since the first worker launch has passed.
func (g *gradingGate) decideLocked(now time.Time) (string, bool) {
	if g.desired > 0 && g.running*100 >= g.desired*gradingGateRunningPercent {
		return gradingGateCounts(g.running, g.desired), true
	}
	if g.launched && now.Sub(g.launchedAt) >= gradingGateLaunchFloor &&
		g.running > 0 && now.Sub(g.lastRunningChange) >= gradingGatePlateau {
		return gradingGateReasonPlateau + ", " + gradingGateCounts(g.running, g.desired), true
	}
	if g.launched && now.Sub(g.launchedAt) >= gradingGateDeadline {
		return gradingGateReasonDeadline + ", " + gradingGateCounts(g.running, g.desired), true
	}
	return "", false
}

// logOpened writes the one line that names why the gate opened. The deadline
// is the never-wait-forever exit, so it is written loudly (ramlog plus
// events.log), not only to the ramlog.
func (g *gradingGate) logOpened() {
	line := "grading gate opened: " + g.openReason + "\n"
	if strings.HasPrefix(g.openReason, gradingGateReasonDeadline) {
		importantLogf("%s", line)
		return
	}
	tlog("%s", line)
}

// logWaitingLocked writes the while-held line, paced to
// gradingGateWaitLogEvery. The elapsed time is how long the gate has been
// held, not the deadline clock (which starts at the first worker launch).
func (g *gradingGate) logWaitingLocked(now time.Time) {
	if now.Sub(g.lastWaitLog) < gradingGateWaitLogEvery {
		return
	}
	g.lastWaitLog = now
	tlog("grading gate waiting: running=%d/%d, elapsed=%s\n",
		g.running, g.desired, now.Sub(g.waitingSince).Round(time.Second))
}

// gradingLoopInterval is the tick cadence of the three gated active loops. It
// mirrors proxyReaperInterval; it is a variable only so the startup-gate tests
// can drive a real loop in milliseconds instead of waiting out the production
// cadence. Production never writes it.
var gradingLoopInterval = proxyReaperInterval

// The provide run's readiness hook. One process has one proxy manager, which
// registers its gate here when it starts; run.go calls
// noteProviderWorkerLaunches once, immediately before it starts the provider
// workers.
var processGradingGate atomic.Pointer[gradingGate]

// noteProviderWorkerLaunches starts the current gate's launch clock. It never
// blocks: the launches it announces are not held by the gate.
func noteProviderWorkerLaunches() {
	if gate := processGradingGate.Load(); gate != nil {
		gate.noteWorkerLaunches()
	}
}
