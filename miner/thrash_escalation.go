package miner

// thrash_escalation.go ports the escalation rung of the relay's swap-thrash
// watchdog (provider/thrash_watchdog.go, "Escalation"): the gate stack that
// decides whether a thrashing miner may restart itself, and the message it
// writes when it does.
//
// The action is a plain SUPERVISED restart (non-zero exit; systemd
// Restart=on-failure, a container's start script, or OpenRC's
// supervise-daemon), never a hot-swap: hot-swapping spawns a child next to the
// multi-GB-swapped parent and bypasses the CLI's memory-headroom preflight,
// exactly when headroom is gone.
//
// Difference from the relay: none in the record. Both write the action to the
// audit ledger (ledgerRecord), and this path additionally puts the same facts
// on the critical log (events.log) through the watch's critf seam - the durable
// line an operator can read even if the ledger write could not land.

import (
	"errors"
	"fmt"
	"os"
	"time"
)

var (
	// swapThrashRetryInterval throttles escalation attempts while thrashing
	// continues (denied attempts do not burn the cap; only restarts do).
	swapThrashRetryInterval = 10 * time.Minute
	// swapThrashPersistTimeout bounds the anti-loop state write: a blocked
	// flock must abort the escalation (alert), never stall the loop or exit
	// without the record.
	swapThrashPersistTimeout = 5 * time.Second
	// swapThrashRecordTimeout bounds the restart record's ledger and events.log
	// writes, so a blocked lock cannot stall the watchdog's decision.
	swapThrashRecordTimeout = 2 * time.Second

	// swapThrashExitFn is the process exit used for the supervised restart; a
	// test seam, production always os.Exit.
	swapThrashExitFn = os.Exit
	// thrashEnvFn and thrashFileExistsFn are the supervisor-detection seams.
	thrashEnvFn        = os.Getenv
	thrashFileExistsFn = defaultFileExists
	// thrashCheckCapDirWritableFn proves the state directory survives a
	// restart before a container's self-exit.
	thrashCheckCapDirWritableFn = defaultThrashCheckCapDirWritable
)

const (
	// swapThrashExitCode is the deliberate non-zero exit the service manager
	// restarts: EX_TEMPFAIL, the relay's thrashExitCode. The miner image's
	// start script (and the shipped provider units) treat 75 as a planned
	// restart; it is not a crash.
	swapThrashExitCode = 75
)

// swapThrashEscalation is the decision of one escalation attempt.
type swapThrashEscalation struct {
	Action   string // "restart" | "alert"
	Code     string // stable reason code for alert dedupe (reasons embed changing durations)
	Msg      string
	Reason   string // human denial reason for alerts
	Cap      int
	Running  int
	Restarts int // restarts in the last 24h after this one
}

func swapThrashEscalationAlert(code, reason string) swapThrashEscalation {
	return swapThrashEscalation{Action: "alert", Code: code, Reason: reason}
}

// escalate runs the full gate stack once. Pure aside from the cap file write;
// the caller owns the log + exit.
func (w *swapThrashWatch) escalate(now time.Time, rt swapThrashRates, rd swapThrashRead, attr string, share float64, shareOK bool) swapThrashEscalation {
	if !w.selfHeal() {
		return swapThrashEscalationAlert("self-heal-off", "self-heal is off (URNETWORK_SELF_HEAL / proxy_self_heal), so no automatic restart runs; the operator must act")
	}
	if w.hotSwapBusy() {
		return swapThrashEscalationAlert("hotswap", "a hot-swap is draining or mid-handoff in this process; not restarting")
	}
	switch attr {
	case "other":
		return swapThrashEscalationAlert("attributed-other", fmt.Sprintf("the swap belongs to another process: this provider holds %s of %s swapped (%.0f%%); never restarting someone else's thrash",
			fmtSwapThrashMiB(rd.unitSwapMiB), fmtSwapThrashMiB(rd.hostSwapUsedMiB), share*100))
	case "unknown":
		return swapThrashEscalationAlert("attributed-unknown", "the swap cannot be attributed to this provider and no per-unit PSI is readable; not restarting blind")
	}
	// A supervisor's presence is only half the restart contract: the unit's
	// restart policy must also restart on exit status 75. The shipped units
	// use Restart=on-failure, which covers it; an operator override
	// (Restart=no, Restart=on-success, or a policy that excludes status 75)
	// leaves the miner dead after a restart attempt.
	kind, restartsOn75 := w.supervisorKind()
	if !restartsOn75 {
		return swapThrashEscalationAlert("no-supervisor", "not running under a service supervisor (systemd, or a container whose start script restarts on exit 75), so a self-exit would not be restarted; not restarting")
	}
	if kind == "docker" {
		if err := w.capDirWritable(); err != nil {
			return swapThrashEscalationAlert("persist-failed", fmt.Sprintf("state directory holding thrash_cap.json is not writable (%v); not restarting, to avoid an unthrottled restart loop", err))
		}
	}
	st := readThrashCapState()
	allowed, code, reason, n := thrashCapEscalationAllowed(st, now)
	if !allowed {
		return swapThrashEscalationAlert(code, reason)
	}
	running := w.running()
	cap := thrashCapForNextStart(running)
	// The restart must not happen unless its anti-loop record is durable: with
	// a read-only state directory every fresh process would see an empty ring
	// and restart again with no spacing and no daily ceiling. The write runs in
	// a goroutine with a bounded wait so a blocked flock cannot stall the
	// escalation path (the watchdog never blocks on a lock).
	var persistErr error
	persistDone := make(chan error, 1)
	go func() { persistDone <- recordThrashEscalation(cap, now) }()
	select {
	case persistErr = <-persistDone:
	case <-time.After(swapThrashPersistTimeout):
		persistErr = errors.New("timed out persisting the restart record")
	}
	if persistErr != nil {
		return swapThrashEscalationAlert("persist-failed", fmt.Sprintf("cannot persist the anti-loop restart record (%v); not restarting, to avoid an unthrottled restart loop", persistErr))
	}
	// The restart is recorded twice, both fire-and-forget but bounded so a
	// blocked lock cannot stall the watchdog's decision: the action ledger line
	// (the same one the relay writes, so `urnet-tools autopilot log` shows this
	// restart) and the events.log line that survives the restart it describes.
	recordDone := make(chan struct{})
	go func() {
		ledgerRecord(ledgerEntry{
			Actor:  "thrash",
			Action: "restart",
			From:   running,
			To:     cap,
			Mode:   "on",
			Reason: swapThrashRestartReason(rt, rd),
		}, now)
		w.critf("[thrash] restart: %s (proxies %d -> %d)\n", swapThrashRestartReason(rt, rd), running, cap)
		close(recordDone)
	}()
	select {
	case <-recordDone:
	case <-time.After(swapThrashRecordTimeout):
	}
	return swapThrashEscalation{
		Action:   "restart",
		Code:     "restart",
		Msg:      swapThrashActionMsg(n+1, thrashMaxRestarts24h, running, cap),
		Cap:      cap,
		Running:  running,
		Restarts: n + 1,
	}
}

// swapThrashHotSwapBusy reports whether a hot-swap is currently draining or
// mid-handoff in THIS process. Env-based detection is wrong: the candidate
// child inherits EnvHotSwap for its whole life (nothing unsets it after the
// handoff), so an env gate would silently disable the restart rung forever on
// every updated box — while the draining parent, the one that must NOT exit,
// never has it set at all.
func swapThrashHotSwapBusy() bool {
	if isHotSwapDraining.Load() {
		return true
	}
	// The hot-swap trigger holds this lock for the whole handoff; a free lock
	// means nothing is in flight.
	if hotSwapLock.TryLock() {
		hotSwapLock.Unlock()
		return false
	}
	return true
}

// swapThrashRestartReason is the recorded reason for a restart: a heap runaway
// says so instead of reading as a swap storm with no stalls.
func swapThrashRestartReason(rt swapThrashRates, rd swapThrashRead) string {
	if swapThrashHeapRunaway(rd) && !(rt.fullOK && rt.fullFrac >= swapThrashSevereFrac) {
		return fmt.Sprintf("heap runaway: %.1fx its soft limit with %s RAM free", rd.heapFrac, fmtSwapThrashMiB(rd.ramAvailMiB))
	}
	stall := 0.0
	if v, ok := swapThrashStallFrac(rt); ok {
		stall = v
	}
	swapClause := "swap activity unreadable"
	if rt.swapOK {
		swapClause = "swapping " + fmtSwapThrashMBs(rt.swapInPS+rt.swapOutPS)
	}
	return fmt.Sprintf("swap thrash: %.0f%% of wall time stalled on memory, %s", stall*100, swapClause)
}

func swapThrashActionMsg(restartN, maxN, running, cap int) string {
	capTxt := "without a proxy cap change"
	if cap > 0 {
		capTxt = fmt.Sprintf("with the proxy cap reduced %d -> %d", running, cap)
	}
	return fmt.Sprintf("🚨 [memory] This will not recover on its own. Restarting the provider now (restart %d of max %d per day) %s so the next run fits in RAM.",
		restartN, maxN, capTxt)
}

func defaultFileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// thrashCapDirWritableBounded runs the writability probe in a goroutine with
// the same bounded wait as the cap write, so a hung volume aborts the
// escalation (alert) instead of stalling the watchdog loop.
func thrashCapDirWritableBounded() error {
	done := make(chan error, 1)
	probe := thrashCheckCapDirWritableFn // read here, not in the goroutine
	go func() { done <- probe() }()
	select {
	case err := <-done:
		return err
	case <-time.After(swapThrashPersistTimeout):
		return errors.New("timed out probing the state directory")
	}
}

func defaultThrashCheckCapDirWritable() error {
	dir, err := oomCapDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".thrash_cap_persist_probe_*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	probeData := []byte("persistent-check")
	if _, err := f.Write(probeData); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	readBack, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if string(readBack) != string(probeData) {
		return errors.New("readback content mismatch")
	}
	return nil
}

// thrashSupervisorKind inspects environment variables and the filesystem to
// identify the active supervisor and report whether it will restart the miner
// on an exit status of 75. Returns "systemd", "docker", or "none".
//
// Ported as the relay has it: OpenRC is NOT recognised. Nothing in the relay's
// detection marks it, even though the sn installer exports URNETWORK_INIT=openrc
// for supervise-daemon; on an OpenRC host the escalation therefore refuses with
// "no-supervisor" exactly as the relay does. Widening the detection is a
// separate decision, not part of this port.
func thrashSupervisorKind(env func(string) string, fileExists func(string) bool) (kind string, restartsOn75 bool) {
	if env == nil {
		env = os.Getenv
	}
	if fileExists == nil {
		fileExists = defaultFileExists
	}
	if env("INVOCATION_ID") != "" || env("NOTIFY_SOCKET") != "" {
		return "systemd", true
	}
	// A container's start script (the miner image's entrypoint) treats exit 75
	// as a planned restart and loops, so a container is a supervisor with no
	// opt-in. The writable-state-dir guard still applies: without a durable
	// restart record a self-exit could loop without its throttle.
	if fileExists("/.dockerenv") || env("URNETWORK_CONTAINER") == "1" {
		return "docker", true
	}
	return "none", false
}
