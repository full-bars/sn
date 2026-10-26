package miner

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// liveness_watchdog.go ports the relay's stall defense
// (provider/liveness_watchdog.go): a systemd watchdog the miner cannot satisfy
// while it is stalled.
//
// The failure it exists for: the Go runtime on a small box can spend nearly all
// of its time in the garbage collector (heap above its soft limit, part of it
// swapped out), so the process stays alive at 50 to 90 percent CPU while every
// goroutine, the self-heal loops included, makes no useful progress. systemd
// sees a running process, the thrash watchdog sees no sustained pressure
// reading, and nothing ever restarts it. An in-process responder cannot fix
// that, because it shares the starved runtime; a supervisor outside the process
// can. With WatchdogSec set, systemd kills and restarts a unit that stops
// sending WATCHDOG=1, so the miner sends it only while the loop that proves the
// process can still work keeps ticking.
//
// It is inert unless systemd sets WATCHDOG_USEC (a unit with WatchdogSec=), or
// the process sits under a supervisor with no watchdog of its own, where the
// action is the self-exit in liveness_selfexit_linux.go.

// livenessStaleAfter is how long the progress loop may stay silent before the
// ping is withheld. The miner's always-on sampler ticks every second, so this
// is hundreds of missed ticks: slow enough that a long GC pause or a briefly
// loaded box never trips it (a false restart costs a few minutes of earnings),
// fast enough that a wedged process is replaced in well under an hour.
const livenessStaleAfter = 10 * time.Minute

// livenessRecordTimeout bounds the lean-cap write: a blocked lock must not
// delay the watchdog's decision.
const livenessRecordTimeout = 5 * time.Second

// livenessStartupGrace is how long startup may take before the first tick of
// the progress loop. The sampler starts early, but a large proxy list can take
// minutes to launch; until the loop has ticked once, a process is judged
// against this, not against the stall budget. A start that takes longer than
// this is itself a stall.
const livenessStartupGrace = 30 * time.Minute

// livenessProgress records when the process last proved it can make progress.
type livenessProgress struct {
	nowFn  func() time.Time
	nano   atomic.Int64
	ticked atomic.Bool
}

func newLivenessProgress(nowFn func() time.Time) *livenessProgress {
	l := &livenessProgress{nowFn: nowFn}
	l.note() // starting up is progress
	return l
}

// note records progress now without claiming the progress loop has ticked.
func (self *livenessProgress) note() {
	self.nano.Store(self.nowFn().UnixNano())
}

// tick records a tick of the progress loop. After the first one the strict
// stall budget applies.
func (self *livenessProgress) tick() {
	self.ticked.Store(true)
	self.note()
}

// fresh reports whether progress was recorded recently enough: within
// staleAfter once the loop has ticked, within the startup grace before that.
func (self *livenessProgress) fresh(staleAfter time.Duration) bool {
	limit := staleAfter
	if !self.ticked.Load() {
		limit = max(limit, livenessStartupGrace)
	}
	return self.nowFn().Sub(time.Unix(0, self.nano.Load())) <= limit
}

// processLiveness is the one the miner's sampler loop feeds.
var processLiveness = newLivenessProgress(time.Now)

// noteLivenessProgress is called by the loop whose ticking proves the process
// can still schedule goroutines, take its locks and write its logs.
func noteLivenessProgress() {
	processLiveness.tick()
}

// minWatchdogEvery is the floor for a watchdog ping interval. It is deliberately
// far below any real timeout: a one-second floor made the interval equal the
// timeout for a unit with WatchdogSec=1s, so a single late tick was a kill.
// Only a pathological WATCHDOG_USEC reaches this clamp.
const minWatchdogEvery = 10 * time.Millisecond

// sdWatchdogInterval is how often to ping, derived from systemd's WATCHDOG_USEC:
// a third of the timeout, with no floor near the timeout itself. ok is false
// when the watchdog is not enabled for this process.
//
// WATCHDOG_PID is deliberately ignored. sd_watchdog_enabled() disables the
// watchdog in a process whose pid differs, but a hot-swap candidate becomes the
// main pid of the same unit and must keep the watchdog fed.
func sdWatchdogInterval(getenv func(string) string) (time.Duration, bool) {
	usec, err := strconv.ParseInt(getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0, false
	}
	interval := time.Duration(usec) * time.Microsecond / 3
	return max(interval, minWatchdogEvery), true
}

// runSdWatchdogLoop pings on every tick while progress is fresh. While it is
// not, the gate decides: when it says no (self-heal is off, or the daily restart
// budget is spent) the ping keeps flowing and the line says what would have
// happened; when it says yes the ping is withheld, the stall is logged once and
// onStall runs once, and the undo it returns runs if progress resumes before
// systemd acts. gate is asked with episodeStart true only at the first stale
// tick of an episode: the budget it consults includes the entry onStall itself
// writes, so asking again mid-episode would feed the watchdog right back.
//
// idle, when set, runs each time the loop is about to wait for the next tick,
// so the previous tick has been handled completely. Production passes nil; a
// test uses it to know when a tick is done instead of guessing.
func runSdWatchdogLoop(ctx context.Context, tick <-chan time.Time, progress *livenessProgress, staleAfter time.Duration, ping func() error, logf func(string, ...any), gate func(episodeStart bool) (bool, string), onStall func() func(), idle func()) {
	passive, acting := false, false
	var undo func()
	for {
		if idle != nil {
			idle()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		if progress.fresh(staleAfter) {
			if passive || acting {
				passive, acting = false, false
				logf("[liveness] progress resumed; the systemd watchdog is being fed again\n")
			}
			_ = ping() // the ping first: nothing below may delay it
			if undo != nil {
				go undo() // the lock behind it can block, so it never sits in front of the ping
				undo = nil
			}
			continue
		}
		ok, why := gate(!acting)
		if !ok {
			if !passive {
				passive = true
				logf("[liveness] no progress for %s, but %s, so the systemd watchdog is still being fed (it would otherwise be withheld and systemd would restart the provider)\n", staleAfter, why)
			}
			_ = ping()
			continue
		}
		if !acting {
			acting = true
			logf("🚨 [liveness] no progress for %s: withholding the systemd watchdog ping, so systemd will restart the provider\n", staleAfter)
			if onStall != nil {
				undo = onStall()
			}
		}
	}
}

var livenessStartOnce sync.Once

// startLivenessWatchdog starts the stall defense once, as early as the miner is
// ready (not after the proxy list is loaded): systemd's watchdog timer runs
// from the start, and the loop has to be feeding it through the whole of
// startup. Where there is no timer to feed (a container's start script) the same
// detection runs with a self-exit as its action (see runSelfExitLivenessWatchdog).
func startLivenessWatchdog(ctx context.Context) {
	if !livenessApplies() {
		return
	}
	livenessStartOnce.Do(func() {
		go superviseLoop(ctx, newLoopRef("liveness_watchdog"), func() { runSdWatchdog(ctx, livenessGate) }, nil)
	})
}

// livenessApplies reports whether this process runs the stall detection at all:
// under systemd when the unit sets WatchdogSec= (there is a ping to withhold),
// or under a supervisor with no watchdog of its own, which can only be asked
// for a restart by the miner exiting (a container's start script).
func livenessApplies() bool {
	if _, enabled := sdWatchdogInterval(os.Getenv); enabled {
		return true
	}
	return livenessUnderSelfExitSupervisor()
}

// A systemd unit carrying WatchdogSec= is fed twice: startSystemdWatchdog
// (hotswap_unix.go) pets through the whole of startup, then the gated loop
// below takes over and may WITHHOLD the ping when the provider stalls, which is
// what makes systemd restart it. The ungated startup pinger must therefore stop
// as soon as the gated loop is feeding; two feeders, one ungated, would re-arm
// the timer forever and the stall would never reach systemd.
var (
	sdWatchdogStopMu sync.Mutex
	sdWatchdogStop   chan struct{}
	// sdWatchdogUngated is true while the startup pinger is still running. It
	// exists so a test can prove the handover happened, not just the wiring.
	sdWatchdogUngated atomic.Bool
)

// stopSystemdWatchdog hands the pings over to the gated liveness loop. Called
// only after that loop has sent its own first ping, so the unit is never left
// unpetted in between. A no-op when the startup pinger never ran.
func stopSystemdWatchdog() {
	sdWatchdogStopMu.Lock()
	stop := sdWatchdogStop
	sdWatchdogStop = nil
	sdWatchdogStopMu.Unlock()
	if stop != nil {
		close(stop)
	}
	// Cleared here rather than only in the goroutine's defer so callers (and
	// tests) observe the handover as soon as the stop is requested; the
	// goroutine's own defer is idempotent.
	sdWatchdogUngated.Store(false)
}

// runSdWatchdog is the feed itself. It logs through the critical log, not the
// ramlog pipe: the ramlog reader may be the thing that is starved, and a log
// call that blocks must not stop the ping decision.
func runSdWatchdog(ctx context.Context, gate func(episodeStart bool) (bool, string)) {
	interval, ok := sdWatchdogInterval(os.Getenv)
	if !ok {
		// No systemd watchdog. Under a supervisor that restarts on exit 75 but
		// has no watchdog of its own (a container's start script), the same
		// stall detection still protects the box: its action is a self-exit
		// instead of a withheld ping.
		if livenessApplies() {
			runSelfExitLivenessWatchdog(ctx, gate)
		}
		return
	}
	if os.Getenv("NOTIFY_SOCKET") == "" {
		// WatchdogSec= is set but systemd gave this process no notify socket
		// (NotifyAccess=none), so no ping can ever reach it and systemd will
		// restart the unit every interval whatever this process does. Say so
		// once and stay put: returning would have the supervisor restart this
		// loop for ever.
		critLog("⚠️ [liveness] WATCHDOG_USEC is set but NOTIFY_SOCKET is not: the unit needs NotifyAccess=main or all, or systemd will restart the provider every %s\n", interval*3)
		<-ctx.Done()
		return
	}
	processLiveness.note()      // the start is progress
	_ = notifySystemdWatchdog() // first feed right away
	// The gated loop owns the pings from here, so the ungated startup pinger
	// must stop: left running it would re-arm systemd's timer forever and the
	// withhold below could never fire.
	stopSystemdWatchdog()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runSdWatchdogLoop(ctx, ticker.C, processLiveness, livenessStaleAfter, notifySystemdWatchdog, critLog, gate, func() func() { return recordLivenessStall(critLog) }, nil)
}
