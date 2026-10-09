package provider

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// A systemd watchdog the provider cannot satisfy while it is stalled.
//
// The failure it exists for: the Go runtime on a small box can spend nearly all
// of its time in the garbage collector (heap above its soft limit, part of it
// swapped out), so the process stays alive at 50 to 90 percent CPU while every
// goroutine, the self-heal loops included, makes no useful progress. systemd
// sees a running process, the thrash watchdog sees no sustained pressure
// reading, and nothing ever restarts it. An in-process responder cannot fix
// that, because it shares the starved runtime; a supervisor outside the process
// can. With WatchdogSec set, systemd kills and restarts a unit that stops
// sending WATCHDOG=1, so the provider sends it only while the loop that proves
// the process can still work keeps ticking.
//
// It is inert unless systemd sets WATCHDOG_USEC (a unit with WatchdogSec=).

// livenessStaleAfter is how long the progress loop may stay silent before the
// ping is withheld. The pressure monitor ticks every 30 seconds, so this is
// twenty missed ticks: slow enough that a long GC pause or a briefly loaded box
// never trips it (a false restart costs a few minutes of earnings), fast enough
// that a wedged process is replaced in well under an hour.
const livenessStaleAfter = 10 * time.Minute

// livenessRecordTimeout bounds the lean-cap write: a blocked lock must not
// delay the watchdog's decision.
const livenessRecordTimeout = 5 * time.Second

// livenessStartupGrace is how long startup may take before the first tick of
// the progress loop. The loop starts only after the proxy list is loaded and
// launched, which on a large list takes minutes; until it has ticked once, a
// process is judged against this, not against the stall budget. A start that
// takes longer than this is itself a stall.
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

// processLiveness is the one the pressure monitor feeds.
var processLiveness = newLivenessProgress(time.Now)

// noteLivenessProgress is called by the loop whose ticking proves the process
// can still schedule goroutines, take its locks and write its logs.
func noteLivenessProgress() {
	processLiveness.tick()
}

// sdWatchdogInterval is how often to ping, derived from systemd's WATCHDOG_USEC:
// a third of the timeout, never under one second. ok is false when the watchdog
// is not enabled for this process.
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
	return max(interval, time.Second), true
}

// runSdWatchdogLoop pings on every tick while progress is fresh. While it is
// not, the gate decides: when it says no (self-heal is off, or the daily restart
// budget is spent) the ping keeps flowing and the line says what would have
// happened; when it says yes the ping is withheld, the stall is logged once and
// onStall runs once, and the undo it returns runs if progress resumes before
// systemd acts. gate is asked with episodeStart true only at the first stale
// tick of an episode: the budget it consults includes the entry onStall itself
// writes, so asking again mid-episode would feed the watchdog right back.
func runSdWatchdogLoop(ctx context.Context, tick <-chan time.Time, progress *livenessProgress, staleAfter time.Duration, ping func() error, logf func(string, ...any), gate func(episodeStart bool) (bool, string), onStall func() func()) {
	passive, acting := false, false
	var undo func()
	for {
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

// startLivenessWatchdog starts the feed once, as early as the provider is ready
// (not after the proxy list is loaded): systemd's watchdog timer runs from the
// start, and the loop has to be feeding it through the whole of startup.
func startLivenessWatchdog(ctx context.Context) {
	if _, enabled := sdWatchdogInterval(os.Getenv); !enabled {
		return
	}
	livenessStartOnce.Do(func() {
		go superviseLoop(ctx, "liveness_watchdog", func() { runSdWatchdog(ctx, livenessGate) }, nil)
	})
}

// runSdWatchdog is the feed itself. It logs through the disk event log, not the
// ramlog pipe: the ramlog reader may be the thing that is starved, and a log
// call that blocks must not stop the ping decision.
func runSdWatchdog(ctx context.Context, gate func(episodeStart bool) (bool, string)) {
	interval, ok := sdWatchdogInterval(os.Getenv)
	if !ok {
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
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runSdWatchdogLoop(ctx, ticker.C, processLiveness, livenessStaleAfter, notifySystemdWatchdog, critLog, gate, recordLivenessStall)
}
