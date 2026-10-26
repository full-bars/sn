package miner

// incident_capture.go ports the provider's incident evidence writer
// (provider/incident_capture.go) onto the miner's failure and action paths.
//
// The provider samples goroutine count and heap against its soft limit every
// ten seconds and captures when either shows a build-up: a provider heading into
// a memory spiral stops answering within minutes, so the profile has to be taken
// while it can still run. The miner carries both of the subsystems the provider
// also has - the OOM-aware start cap (proxy_manager_oom_cap.go) and the
// swap-thrash watchdog (thrash_escalation.go / thrash_watchdog.go), both ported
// from their provider counterparts - and both take an action at a known instant.
// That instant is a better place to take evidence than a sampler's guess, so this
// port captures on:
//
//   - a recovered provider-worker panic,
//   - a fatal exit after the run,
//   - a hotswap restart that could not hand over,
//   - an OOM-cap action at startup: reduce, frozen, relax or clear, decided by
//     oomCapDecideDetailed and reported by decideOOMCapStartup, and
//   - a swap-thrash escalation: the supervised restart that ends this process,
//     and the alert that refuses one.
//
// The last two are the miner's own action taxonomy and the reason this bundle
// exists at all: the cap says the next pool will be smaller, and the escalation
// says this process is about to exit 75. Neither survives to be read afterwards
// on its own, because events.log rotates under 1 MiB and the ramlog floods in
// about a minute.
//
// NOT ported, deliberately:
//
//   - The 10-second sampler and its 30-minute build-up window. Its signal is
//     calibrated on the provider's own per-proxy goroutine ramp (14k to 21k over
//     40 minutes, hence the 45-minute startup-ramp suppression and the 20k
//     floor), and the miner's thrash watchdog already samples PSI, swap and heap
//     on its own interval and makes the same "the box is going into a spiral"
//     call. A second sampler would double-read /proc and re-decide what the
//     watchdog owns, and the decisions above are the moments that matter.
//   - The heap profile (heap.pb.gz). pprof's heap writer runs a GC before it
//     samples, and every path here is a memory-pressure path; a forced GC on a
//     box that just thrashed is the one pause it cannot afford. The four
//     runtime/metrics numbers are in meta.txt instead (see incidentHeapSample).
//
// Bounds, because the node is already in trouble: a byte cap for the whole
// bundle, a per-file cap, and a wall-clock budget. Every artifact checks the
// deadline before it is written; a capture that cannot finish leaves what it
// has plus incomplete.txt naming what it skipped, and a capture that cannot
// even get a directory says so through the critical log and its returned error.
// The whole write runs on its own goroutine behind a hard outer stop, so a read
// stuck on a dying volume cannot hold the failing path past the budget. The
// thrash restart is the one caller that owes someone else an exit status, so it
// asks for a shorter ceiling (incidentThrashWait) and logs its own lines to
// stderr instead of events.log.
//
// Privacy: the bundle outlives the incident and may be copied off the box, so
// credential-bearing state is never copied. Files that are wholly a secret are
// recorded as name/size/mode only (see incidentSecretStateName) even when a
// whitelist entry names one, and everything that is written passes
// incidentRedact - the panic stack included - which removes JWTs, bearer
// tokens, URL userinfo, PEM private keys, host:port:user:pass credentials
// wherever they sit in a line, secret-named assignments and long hex runs.
// URNETWORK_INCIDENT_CAPTURE=0 turns it off, matching the provider.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// incidentDirName is the bundle root under the state directory. The
	// provider keeps its bundles under its OOM cap directory; the miner has no
	// cap directory, so the state directory is the parent.
	incidentDirName = "incidents"
	// incidentKeep is how many bundles survive; a long incident leaves a
	// handful of directories, not a full disk.
	incidentKeep = 8
	// incidentSchema names the bundle layout so a reader can refuse a future
	// one instead of guessing.
	incidentSchema = "urnetwork-miner-incident-v1"
)

// The bounds are vars rather than consts so the tests can shrink them; nothing
// outside a test writes them.
var (
	incidentBundleMaxBytes    = int64(2 << 20) // the whole bundle
	incidentFileMaxBytes      = int64(512 << 10)
	incidentStateFileMaxBytes = int64(256 << 10)
	incidentLogTailBytes      = int64(96 << 10) // per log source
	incidentStackMaxBytes     = int64(64 << 10)
	incidentMaxFiles          = 48
	incidentBudget            = 2 * time.Second
	// incidentBudgetGrace is how long the caller waits past the budget before
	// abandoning the writer: the writer needs a moment after its deadline to
	// record what it skipped in incomplete.txt.
	incidentBudgetGrace = time.Second
	// incidentNoteTimeout bounds the capture's own line once the caller has
	// abandoned the writer. The line is a courtesy on a path that already owes
	// someone an exit status (the thrash restart: a 1s writer budget plus this
	// sink must stay near that budget, not double it), so a sink stuck on the
	// same hung volume must not add its own timeout to the ceiling.
	incidentNoteTimeout = 250 * time.Millisecond
	// incidentHookAfterFile runs after each artifact is written. TEST-ONLY: a
	// test uses it to push the capture past its budget deterministically.
	incidentHookAfterFile func(name string)
)

// incidentProcessStart is the uptime origin the bundle reports.
var incidentProcessStart = time.Now()

// incidentTrigger is why a bundle was taken. Fixed names, because they are part
// of the directory name and of meta.txt.
type incidentTrigger string

const (
	incidentTriggerPanic   incidentTrigger = "panic"
	incidentTriggerFatal   incidentTrigger = "fatal"
	incidentTriggerRestart incidentTrigger = "restart"
	// incidentTriggerOOMCap is one OOM-cap action at startup: the automatic
	// start cap moved because the box was OOM-killed, or relaxed/cleared after
	// a clean window. The provider's sampler covers this ground from the other
	// side (heap near/over its limit); the miner has the decision itself.
	incidentTriggerOOMCap incidentTrigger = "oomcap"
	// incidentTriggerThrash is one swap-thrash escalation: either the
	// supervised restart that exits 75, or the denial that refuses to restart.
	incidentTriggerThrash incidentTrigger = "thrash"
	// incidentTriggerAuthStop is the terminal stop after repeated
	// authentication refusals: one bundle records what the node was doing
	// when it parked.
	incidentTriggerAuthStop incidentTrigger = "auth_stop"
)

// Pacing, from the provider: one capture per trigger per cooldown, and a
// process ceiling. A panic that hits every slot is one incident, not twenty
// bundles, and each bundle costs a disk sync on a box that is already failing.
var (
	incidentReasonCooldown = 30 * time.Second
	incidentMaxPerProcess  = 6
)

var (
	incidentGateMu        sync.Mutex
	incidentLastByTrigger = map[incidentTrigger]time.Time{}
	incidentTotalCapture  int
)

// incidentAdmit applies that pacing. The first capture of a process is always
// admitted: the trigger itself must never be the thing that suppresses the
// evidence.
func incidentAdmit(now time.Time, trigger incidentTrigger) bool {
	incidentGateMu.Lock()
	defer incidentGateMu.Unlock()
	if incidentTotalCapture >= incidentMaxPerProcess {
		return false
	}
	if at, ok := incidentLastByTrigger[trigger]; ok && now.Sub(at) < incidentReasonCooldown {
		return false
	}
	incidentLastByTrigger[trigger] = now
	incidentTotalCapture++
	return true
}

// incidentResetGate is the test seam for the pacing state.
func incidentResetGate() {
	incidentGateMu.Lock()
	defer incidentGateMu.Unlock()
	incidentLastByTrigger = map[incidentTrigger]time.Time{}
	incidentTotalCapture = 0
}

func incidentCaptureEnabled() bool {
	return os.Getenv("URNETWORK_INCIDENT_CAPTURE") != "0"
}

func incidentRootDir() (string, error) {
	base, err := providerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, incidentDirName), nil
}

// captureIncidentBundle writes a bounded, redacted bundle for one failure and
// returns its directory. It never panics, never waits past
// incidentBudget+incidentBudgetGrace, and reports an incomplete capture both
// through the returned error and through the critical log.
//
// stack may be nil (the fatal and restart paths have no panic stack); detail is
// one line of context and is redacted before it is written.
func captureIncidentBundle(trigger incidentTrigger, detail string, stack []byte) (string, error) {
	return captureIncidentBundleWithin(trigger, detail, stack, incidentBudget+incidentBudgetGrace)
}

// captureIncidentBundleWithin is captureIncidentBundle with an explicit ceiling
// on how long the caller may be held. The writer's own deadline is the shorter
// of that ceiling and incidentBudget, so a caller that must not linger (the
// thrash restart, which owes the supervisor an exit status) abandons a bundle
// that stopped at its own deadline instead of one cut off mid-file: meta.txt
// goes first, so even a 1s bundle says what it was for.
func captureIncidentBundleWithin(trigger incidentTrigger, detail string, stack []byte, wait time.Duration) (string, error) {
	return captureIncidentBundleWithinLog(trigger, detail, stack, wait, critLog)
}

// captureIncidentBundleWithinLog is captureIncidentBundleWithin with the sink for
// the capture's own lines. critLog is right for a failure path: it lands in
// events.log next to the incident. The thrash restart passes the watchdog's
// stderr note instead, because critLog fsyncs events.log in the state directory
// whose hang that escalation exists for, and the exit must not wait on it
// (thrash_exit_note.go).
func captureIncidentBundleWithinLog(trigger incidentTrigger, detail string, stack []byte, wait time.Duration, logf func(format string, args ...any)) (string, error) {
	if logf == nil {
		logf = critLog
	}
	if !incidentCaptureEnabled() {
		return "", nil
	}
	if !incidentAdmit(time.Now(), trigger) {
		logf("[incident] %s: no bundle taken (paced: %s per trigger, %d per process)", trigger, incidentReasonCooldown, incidentMaxPerProcess)
		return "", nil
	}
	budget := incidentBudget
	if wait < budget {
		budget = wait
	}
	type outcome struct {
		dir string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		dir, err := writeIncidentBundle(time.Now(), trigger, detail, stack, budget)
		done <- outcome{dir: dir, err: err}
	}()

	// The outer stop is the last line of defence. A read on a dying volume can
	// block forever; the path that is already in trouble must not.
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case result := <-done:
		if result.err != nil {
			logf("[incident] %s bundle at %s is incomplete: %v", trigger, result.dir, result.err)
		} else {
			logf("[incident] %s: evidence in %s", trigger, result.dir)
		}
		return result.dir, result.err
	case <-timer.C:
		incidentNoteWithin(logf, incidentNoteTimeout, "[incident] %s bundle abandoned: still writing after %s", trigger, wait)
		return "", fmt.Errorf("incident bundle abandoned after %s", wait)
	}
}

// incidentNoteWithin writes one capture line, giving up after limit. It is only
// for lines written after the capture itself has been abandoned: the caller
// asked for a ceiling because something else is waiting on it (an exit status,
// on the thrash restart), and the sink is a log that may be stuck on the same
// volume the capture was. The line is a courtesy, the ceiling is the contract.
func incidentNoteWithin(logf func(format string, args ...any), limit time.Duration, format string, args ...any) {
	done := make(chan struct{}, 1)
	go func() {
		logf(format, args...)
		done <- struct{}{}
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// incidentThrashWait is how long the thrash restart may be held for evidence.
// The escalation owes the supervisor an exit status, and the state directory it
// is describing may be the very thing that hung, so this ceiling is shorter than
// a failure capture's: meta.txt goes first, and a bundle that stops at this
// deadline still says what the restart was for.
var incidentThrashWait = time.Second

// captureOOMCapIncident records one OOM-cap action at startup. It is called from
// decideOOMCapStartup, once per process, before the launch selection: the
// decision it describes is the one that sizes this very start's pool, and
// oom_cap.json/run.marker/thrash_cap.json in the bundle are what the next start
// will read.
//
// An empty or "none" decision writes nothing. The normal no-OOM start is not an
// incident, and a bundle on every start would bury the ones that matter.
func captureOOMCapIncident(d oomCapStartupDecision) (string, error) {
	if d.Decision.Action == "" || d.Decision.Action == "none" {
		return "", nil
	}
	return captureIncidentBundle(incidentTriggerOOMCap, incidentOOMCapDetail(d), nil)
}

// incidentOOMCapDetail words the decision the way meta.txt should read: the
// action, the move, the mode that decided it and the condition it was made under.
// Mode-neutral on purpose (not "would reduce"/"reduced"): the bundle records what
// the cap did to the next pool, and env.txt already carries URNETWORK_OOM_CAP.
func incidentOOMCapDetail(d oomCapStartupDecision) string {
	from, to := "none", "none"
	if d.Decision.From > 0 {
		from = strconv.Itoa(d.Decision.From)
	}
	if d.Decision.To > 0 {
		to = strconv.Itoa(d.Decision.To)
	}
	return "oomcap " + d.Decision.Action + ": automatic start cap " + from + " -> " + to +
		", mode " + oomCapModeName(oomCapMode()) + ", " + d.Condition
}

// captureThrashIncident records one swap-thrash escalation: the supervised
// restart that is about to end this process (exit 75), or the alert that refused
// one. The watchdog calls it from its sample loop through the evidenceFn seam,
// after the escalation gate and before exitOnRestart, so the bundle exists while
// the process that holds the sensors still does.
//
// notef is the watchdog's stderr note, and it is also the bundle's log sink: this
// path may be inside the state-directory hang the escalation exists for, so
// nothing here may fsync events.log before the exit (see thrash_exit_note.go).
//
// The restart path is the one capture that delays an exit, so it uses
// incidentThrashWait; the alert path is not exiting and takes the normal budget.
func captureThrashIncident(esc swapThrashEscalation, rt swapThrashRates, rd swapThrashRead, attr string, running int, notef func(format string, args ...any)) (string, error) {
	wait := incidentBudget + incidentBudgetGrace
	var detail string
	if esc.Action == "restart" {
		detail = "restart (exit " + strconv.Itoa(swapThrashExitCode) + "): " + esc.Msg
		wait = incidentThrashWait
	} else {
		detail = "not restarting (" + esc.Code + "): " + esc.Reason
	}
	detail += " | proxies " + strconv.Itoa(esc.Running) + " -> " + strconv.Itoa(esc.Cap)
	if esc.Restarts > 0 {
		detail += ", restarts24h " + strconv.Itoa(esc.Restarts)
	}
	detail += " | " + swapThrashDetailLine(rt, rd, attr)
	if rd.unitSwapOK {
		detail += ", unit swap " + fmtSwapThrashMiB(rd.unitSwapMiB)
	}
	if rd.hostSwapOK {
		detail += ", host swap " + fmtSwapThrashMiB(rd.hostSwapUsedMiB) + "/" + fmtSwapThrashMiB(rd.hostSwapTotalMiB)
	}
	return captureIncidentBundleWithinLog(incidentTriggerThrash, detail, nil, wait, notef)
}

// incidentBundle is one directory being filled under a deadline and a byte cap.
type incidentBundle struct {
	dir      string
	deadline time.Time
	// budget is the wall-clock allowance this bundle was given. It is not
	// always incidentBudget: the thrash restart asks for a shorter one, and
	// incomplete.txt has to name the budget that was actually spent.
	budget   time.Duration
	maxBytes int64
	written  int64
	files    int
	notes    []string
	failed   error
}

func newIncidentBundle(root string, now time.Time, trigger incidentTrigger, budget time.Duration) (*incidentBundle, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	name := now.UTC().Format("20060102T150405Z") + "-" + string(trigger)
	for i := 0; i < 16; i++ {
		candidate := filepath.Join(root, name)
		if i > 0 {
			candidate = filepath.Join(root, fmt.Sprintf("%s-%d", name, i))
		}
		err := os.Mkdir(candidate, 0o700)
		if err == nil {
			return &incidentBundle{
				dir:      candidate,
				deadline: now.Add(budget),
				budget:   budget,
				maxBytes: incidentBundleMaxBytes,
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no free incident directory under %s", root)
}

// write stores one artifact inside the caps. A file that cannot fit is cut and
// named in incomplete.txt; once the deadline passes nothing further is written
// and the remaining artifacts are named as skipped.
func (b *incidentBundle) write(name string, content []byte) {
	if b.failed != nil {
		return
	}
	if time.Now().After(b.deadline) {
		b.notes = append(b.notes, "budget of "+b.budget.String()+" spent before "+name)
		return
	}
	if b.files >= incidentMaxFiles {
		b.notes = append(b.notes, "file cap of "+strconv.Itoa(incidentMaxFiles)+" reached before "+name)
		return
	}
	remaining := b.maxBytes - b.written
	if remaining <= 0 {
		b.notes = append(b.notes, "size cap of "+strconv.FormatInt(b.maxBytes, 10)+" bytes reached before "+name)
		return
	}
	limit := remaining
	if limit > incidentFileMaxBytes {
		limit = incidentFileMaxBytes
	}
	body, truncated := incidentCut(content, limit)
	if truncated {
		b.notes = append(b.notes, name+" truncated at "+strconv.FormatInt(limit, 10)+" bytes")
	}
	path := filepath.Join(b.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		b.failed = err
		return
	}
	if err := writeIncidentFile(path, body); err != nil {
		b.failed = err
		return
	}
	b.written += int64(len(body))
	b.files++
	if incidentHookAfterFile != nil {
		incidentHookAfterFile(name)
	}
}

// finish records what was skipped and reports the capture as incomplete if any
// bound bit. incomplete.txt is deliberately outside the caps: it is the one
// small file that says the bundle is not the whole story.
func (b *incidentBundle) finish() error {
	if len(b.notes) > 0 {
		body := "incomplete: a bound stopped this bundle before every artifact was written\n" +
			strings.Join(b.notes, "\n") + "\n"
		if err := writeIncidentFile(filepath.Join(b.dir, "incomplete.txt"), []byte(body)); err != nil && b.failed == nil {
			b.failed = err
		}
	}
	if b.failed != nil {
		return fmt.Errorf("bundling %s failed: %w", b.dir, b.failed)
	}
	if len(b.notes) > 0 {
		return fmt.Errorf("incomplete: %s", strings.Join(b.notes, "; "))
	}
	return nil
}

func writeIncidentFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// incidentCut trims content to limit bytes on a line boundary where it can, and
// appends a marker so a shortened artifact cannot read as a complete one.
func incidentCut(content []byte, limit int64) ([]byte, bool) {
	if int64(len(content)) <= limit {
		return content, false
	}
	if limit < 64 {
		return nil, true
	}
	cut := int(limit)
	if i := bytes.LastIndexByte(content[:cut], '\n'); i > 0 && i > cut-4096 {
		cut = i + 1
	}
	marker := fmt.Sprintf("[truncated at %d bytes by the incident bundle cap]\n", limit)
	if cut > int(limit)-len(marker) {
		cut = int(limit) - len(marker)
	}
	if cut < 0 {
		cut = 0
	}
	out := make([]byte, 0, cut+len(marker))
	out = append(out, content[:cut]...)
	out = append(out, marker...)
	return out, true
}

// writeIncidentBundle is the whole capture: meta first, so a bundle cut short
// still says what it was for, then the dump, the logs, the conditions in force
// and the state inventory.
func writeIncidentBundle(now time.Time, trigger incidentTrigger, detail string, stack []byte, budget time.Duration) (string, error) {
	root, err := incidentRootDir()
	if err != nil {
		return "", err
	}
	b, err := newIncidentBundle(root, now, trigger, budget)
	if err != nil {
		return "", err
	}

	b.write("meta.txt", incidentMeta(now, trigger, detail))
	if len(stack) > 0 {
		body, _ := incidentCut(stack, incidentStackMaxBytes)
		// The same rule as every other artifact: a panic value can be a
		// header, a URL with userinfo, or a token formatted into the panic
		// message, and this file outlives the incident like the rest.
		b.write("stack.txt", []byte(incidentRedact(string(body))))
	}
	profile, profileCut := incidentGoroutineProfile(incidentFileMaxBytes)
	if profileCut {
		b.notes = append(b.notes, "goroutines.txt truncated at "+strconv.FormatInt(incidentFileMaxBytes, 10)+" bytes")
	}
	b.write("goroutines.txt", []byte(incidentRedact(profile)))
	b.write("goroutines-summary.txt", incidentGoroutineSummary(profile, 15))
	b.write("logs.txt", incidentLogTail())
	b.write("ramlog.txt", incidentRamlogTail())
	b.write("env.txt", incidentEnv())
	incidentCopyState(b)
	b.write("redactions.txt", []byte(incidentRedactionNote))

	pruneIncidentBundles(root, incidentKeep)
	return b.dir, b.finish()
}

func incidentMeta(now time.Time, trigger incidentTrigger, detail string) []byte {
	objects, inuse, sys, cycles := incidentHeapSample()
	limit := debug.SetMemoryLimit(-1)
	hostname, _ := os.Hostname()
	frac := 0.0
	if limit > 0 && limit != math.MaxInt64 {
		frac = float64(objects) / float64(limit)
	}
	limitMiB := int64(0)
	if limit > 0 && limit != math.MaxInt64 {
		limitMiB = limit >> 20
	}
	var b strings.Builder
	fmt.Fprintf(&b, "schema=%s\n", incidentSchema)
	fmt.Fprintf(&b, "time=%s\n", now.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "trigger=%s\n", trigger)
	fmt.Fprintf(&b, "detail=%s\n", incidentLine(detail, 512))
	fmt.Fprintf(&b, "version=%s\n", RequireVersion())
	fmt.Fprintf(&b, "go_version=%s\n", runtime.Version())
	fmt.Fprintf(&b, "goos=%s\n", runtime.GOOS)
	fmt.Fprintf(&b, "goarch=%s\n", runtime.GOARCH)
	fmt.Fprintf(&b, "pid=%d\n", os.Getpid())
	fmt.Fprintf(&b, "ppid=%d\n", os.Getppid())
	fmt.Fprintf(&b, "hostname=%s\n", hostname)
	fmt.Fprintf(&b, "uptime_s=%d\n", int64(now.Sub(incidentProcessStart).Seconds()))
	fmt.Fprintf(&b, "goroutines=%d\n", runtime.NumGoroutine())
	fmt.Fprintf(&b, "gomaxprocs=%d\n", runtime.GOMAXPROCS(0))
	fmt.Fprintf(&b, "heap_objects_mib=%d\n", objects>>20)
	fmt.Fprintf(&b, "heap_inuse_mib=%d\n", inuse>>20)
	fmt.Fprintf(&b, "sys_mib=%d\n", sys>>20)
	fmt.Fprintf(&b, "gc_cycles=%d\n", cycles)
	fmt.Fprintf(&b, "mem_limit_mib=%d\n", limitMiB)
	fmt.Fprintf(&b, "heap_frac=%.3f\n", frac)
	return []byte(incidentRedact(b.String()))
}

// incidentHeapSample reads the four memory numbers from runtime/metrics, never
// from runtime.ReadMemStats: ReadMemStats stops the world, and a second pause on
// a process that is already failing is exactly what it cannot afford. The same
// reason is why no heap profile is written here - pprof's heap writer runs a GC
// before it samples. The numbers are the evidence; the pause is not affordable.
func incidentHeapSample() (objects, inuse, sys, cycles uint64) {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/in-use:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(samples)
	value := func(i int) uint64 {
		if samples[i].Value.Kind() == metrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	return value(0), value(1), value(2), value(3)
}

// incidentGoroutineProfile is the debug=1 profile: stack traces without
// function arguments. debug=2 prints the arguments, and an argument can be a
// key, a token or a password; the dump must not carry what the redactor cannot
// recognise as a credential.
//
// The writer stops accepting bytes at limit. pprof formats a record per
// goroutine, so at 60k that is tens of MiB the failing process must not spend;
// a truncated dump still carries the biggest groups, which is the answer.
func incidentGoroutineProfile(limit int64) (string, bool) {
	w := &incidentLimitWriter{limit: limit}
	if p := pprof.Lookup("goroutine"); p != nil {
		if err := p.WriteTo(w, 1); err != nil {
			return "goroutine profile unavailable: " + err.Error() + "\n", false
		}
	}
	return w.buf.String(), w.cut
}

// incidentLimitWriter keeps at most limit bytes and discards the rest while
// still reporting success, so pprof finishes rather than failing the dump.
type incidentLimitWriter struct {
	limit int64
	buf   bytes.Buffer
	cut   bool
}

func (w *incidentLimitWriter) Write(p []byte) (int, error) {
	remaining := w.limit - int64(w.buf.Len())
	if remaining <= 0 {
		w.cut = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		w.buf.Write(p[:remaining])
		w.cut = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// incidentGoroutineSummary ranks the profile by the function the goroutines are
// parked in, which is usually the whole answer to "what piled up". It reuses the
// control socket's parser rather than growing a second one.
func incidentGoroutineSummary(profile string, n int) []byte {
	groups := parseGoroutineProfile(profile, n)
	var b strings.Builder
	fmt.Fprintf(&b, "total=%d groups=%d\n\n", groups.Total, len(groups.Groups))
	for _, g := range groups.Groups {
		fmt.Fprintf(&b, "%6d  %s\n", g.Count, g.Func)
	}
	return []byte(b.String())
}

// incidentLogTail reads the tail of the two state-directory logs. events.log is
// the critical-event log (1 MiB, rotated in half) and important.log the small
// high-value mirror; both are far too small to answer "what led here" on their
// own, which is the point of copying their tails into the bundle.
func incidentLogTail() []byte {
	var b bytes.Buffer
	for _, name := range []string{"events.log", "important.log"} {
		path, err := providerStatePath(name)
		if err != nil {
			continue
		}
		tail, err := incidentTail(path, incidentLogTailBytes)
		if err != nil || len(tail) == 0 {
			continue
		}
		fmt.Fprintf(&b, "===== %s =====\n", name)
		b.Write(tail)
		if tail[len(tail)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return []byte("no state-directory log was readable\n")
	}
	return []byte(incidentRedact(b.String()))
}

// incidentRamlogTail copies the tail of the shared RAM log when this host has
// one. It is the only place the per-proxy and per-connection lines survive.
func incidentRamlogTail() []byte {
	tail, err := incidentTail(minerSHMLogPath, incidentLogTailBytes)
	if err != nil || len(tail) == 0 {
		return []byte("no ramlog at " + minerSHMLogPath + "\n")
	}
	return []byte(incidentRedact(string(tail)))
}

// incidentTail reads at most n bytes from the end of path, starting at a line
// boundary so the first line of the tail is whole.
func incidentTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := int64(0)
	if size > n {
		offset = size - n
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, size-offset)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	buf = buf[:read]
	if offset > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 && i+1 < len(buf) {
			buf = buf[i+1:]
		}
	}
	return buf, nil
}

// incidentEnv records the conditions the run was under: the URNETWORK_* knobs,
// the Go runtime knobs and the supervisor. Values whose name suggests a
// credential are masked outright rather than pattern-matched.
func incidentEnv() []byte {
	var b strings.Builder
	b.WriteString("# URNETWORK_* and Go runtime variables in force; credential-named values are masked\n")
	env := os.Environ()
	sort.Strings(env)
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "URNETWORK_") && name != "GOGC" && name != "GOMEMLIMIT" && name != "GOMAXPROCS" {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", name, incidentEnvValue(name, value))
	}
	fmt.Fprintf(&b, "supervisor=%s\n", incidentSupervisor())
	if dir, err := providerStateDir(); err == nil {
		fmt.Fprintf(&b, "state_dir=%s\n", dir)
	}
	return []byte(incidentRedact(b.String()))
}

func incidentEnvValue(name, value string) string {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "KEY", "PASS", "JWT", "AUTH", "CRED", "CODE"} {
		if strings.Contains(upper, marker) {
			return "<redacted>"
		}
	}
	return value
}

func incidentSupervisor() string {
	switch {
	case os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "":
		return "systemd"
	case os.Getenv("URNETWORK_CONTAINER") == "1":
		return "docker"
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	return "none"
}

// incidentEvidenceStateFiles is the copy whitelist: state that is useful
// evidence and carries no credential. A whitelist, not a denylist, so a
// credential file the miner grows later is left out by default instead of
// copied by mistake. Each entry still passes incidentRedact, and an entry whose
// name incidentSecretStateName calls a credential is withheld regardless (see
// incidentCopyState): the list says "useful", the name says "secret", and the
// name wins.
var incidentEvidenceStateFiles = []string{
	"provider_state.json",
	"proxy_grades.json",
	"proxy_probe.json",
	"proxy_table_probe.json",
	"direct_grade.json",
	"pending_overrides.json",
	// The OOM cap's own state (proxy_manager_oom_cap.go): the automatic start
	// cap in force, the reduction ring that decides when it freezes, and the
	// previous start's kill baseline. Counts, boot ids and unix times only.
	"oom_cap.json",
	"run.marker",
	// The thrash escape's state (thrash_cap.go): the reduced next-start cap,
	// its expiry, and the restart ring the anti-loop gate judges. Counts and
	// unix times only.
	"thrash_cap.json",
	// The watchdog's published snapshot (thrash_watchdog.go): the sensor
	// readings and attribution behind the escalation. Numbers and generated
	// sentences; no proxy, no key.
	"thrash_status",
}

// incidentSecretStateName reports a state file that is wholly or largely a
// credential: the network jwt, the per-slot provider and client JWTs, the
// provider and extender private keys and cert, the proxy config with its Auths
// block, proxy.state, whose identity keys carry the proxy username, and
// proxy_url.json, whose cache holds each URL-sourced proxy's user and password
// in plaintext. These are recorded by name, size and mode only. Redacting part
// of a JWT or a 32-byte private key leaves either the secret or nothing; the
// whole file is the credential.
//
// The matcher is checked against every credential-shaped file the miner writes
// (TestIncidentSecretStateName): "jwt" covers the whole family, bare and
// dotted, the .client_jwts.json store and its timestamped .bak snapshots,
// because the marker is the substring, not a ".jwt" suffix.
func incidentSecretStateName(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "proxy", "proxy.lock", "proxy.state", "proxy_url.json":
		return true
	}
	for _, marker := range []string{"jwt", ".key", ".cert", "token", "credential", "secret"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// incidentCopyState copies the whitelisted evidence and writes an inventory of
// everything else that was present, so the bundle still answers "what state
// existed" without carrying any of it.
//
// The copy set is the static whitelist plus this process's own thrash exit
// marker, which is named per pid (thrash_exit.<pid>) and so cannot be listed
// statically. Only this pid: a marker left by another process is a stale
// leftover whose exit this bundle did not witness, and copying it would read as
// evidence for a restart that is not this one. The restart writes its own
// marker at the exit, after this capture, so a restart bundle usually carries
// none - the inventory says so rather than substituting a stranger's.
//
// A whitelisted name that incidentSecretStateName calls a credential is still
// withheld: proxy.state and proxy carry the proxy keys, and no whitelist entry
// may ever copy them.
//
// Three outcomes are kept apart in inventory.txt, because "the file was not
// evidence", "the file was a symlink", and "the file could not be read" mean
// different things to the operator: a read failure and a truncated read are
// named in incomplete.txt as well.
func incidentCopyState(b *incidentBundle) {
	dir, err := providerStateDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.write("state/inventory.txt", []byte("state directory unreadable: "+err.Error()+"\n"))
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	wanted := append([]string(nil), incidentEvidenceStateFiles...)
	wanted = append(wanted, thrashExitMarkerPrefix+strconv.Itoa(os.Getpid()))
	status := map[string]string{}
	for _, name := range wanted {
		if _, seen := status[name]; seen {
			continue
		}
		if incidentSecretStateName(name) {
			status[name] = "withheld:credential-file"
			continue
		}
		data, truncated, err := readCapped(filepath.Join(dir, name), incidentStateFileMaxBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// A whitelist names what may be evidence, not what exists.
				continue
			}
			if errors.Is(err, errIncidentStateSymlink) {
				status[name] = "withheld:symlink"
				continue
			}
			// A read failure is not a deliberate withhold, and the
			// inventory must not let it look like one.
			status[name] = "error:read-failed"
			b.notes = append(b.notes, name+" could not be read: "+incidentLine(err.Error(), 160))
			continue
		}
		status[name] = "copied:redacted"
		if truncated {
			b.notes = append(b.notes, name+" truncated at "+strconv.FormatInt(incidentStateFileMaxBytes, 10)+" bytes")
		}
		b.write(filepath.Join("state", name), []byte(incidentRedact(string(data))))
	}

	var inventory strings.Builder
	inventory.WriteString("# every file in the state directory, and whether this bundle carries it\n")
	for _, name := range names {
		// Lstat, not Stat: the inventory describes the entry that is here, and
		// a symlink's target is not in this directory.
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		line := "withheld:not-evidence"
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// Never copied under any name, so it is never evidence here.
			line = "withheld:symlink"
		case incidentSecretStateName(name):
			line = "withheld:credential-file"
		}
		if s, ok := status[name]; ok {
			line = s
		}
		fmt.Fprintf(&inventory, "%s bytes=%d mode=%s %s\n", name, info.Size(), info.Mode().Perm(), line)
	}
	b.write("state/inventory.txt", []byte(inventory.String()))
}

// errIncidentStateSymlink marks a state file the copy refused because it is a
// symlink. readCapped never follows one: the state directory's own file is the
// evidence, and a link is a path out of it, so the bundle records the name and
// moves on.
var errIncidentStateSymlink = errors.New("state file is a symlink")

// readCapped reads at most limit bytes, so one large state file cannot become
// an allocation the failing process regrets. A file bigger than limit is
// reported as truncated: io.ReadFull returns the first limit bytes with no
// error, and a partial copy that reads as complete is worse than a short one
// that says it is short.
//
// The symlink check is Lstat before the open, so a link swapped in between the
// two calls is still followed. That window is not worth a platform-specific
// O_NOFOLLOW here: the state directory is the miner's own, and the failure it
// guards against is a misnamed evidence entry, not an attacker who can already
// write there.
func readCapped(path string, limit int64) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errIncidentStateSymlink
	}
	size := info.Size()
	truncated := size > limit
	if size > limit {
		size = limit
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	buf := make([]byte, size)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, false, err
	}
	return buf[:read], truncated, nil
}

// incidentLine folds detail to one line, redacts it and caps it. A panic cause
// can embed a whole request, and the bundle must not become the leak.
func incidentLine(s string, max int) string {
	s = incidentSpace(s)
	s = incidentRedact(s)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

func incidentSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var (
	// A JWT starts with the base64url of {" - always "eyJ" - and has three
	// dot-separated parts.
	incidentJWTRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	// Authorization: Bearer <token> / Basic <base64>.
	incidentBearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// scheme://user:pass@host -> scheme://<redacted>@host.
	incidentUserinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)
	// A PEM private key block, header to footer.
	incidentPEMRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	// host:port:user:pass, the proxy list format, keeps the address only.
	//
	// Both credential fields are required and neither may contain whitespace,
	// a colon, a comma, a semicolon or a quote, so an ordinary "host:port"
	// pair, a "host:port: word" log line and a "file.go:12:3: msg" reference
	// are not mistaken for an entry. There is no end-of-line anchor on
	// purpose: the same credentials appear mid-line after an error suffix
	// ("...: connection refused"), inside a JSON string, and in a
	// comma-separated env list, and every one of those shapes has to be
	// redacted. Being blunt here can cost a little context - a run of four
	// colon-separated fields that is not a proxy entry is redacted too - and
	// that is the deliberate trade this file makes everywhere.
	incidentProxyLineRe = regexp.MustCompile(`(\[[0-9A-Fa-f:.]+\]:\d+|[A-Za-z0-9_.-]+:\d+):[^\s:,;"']+:[^\s:,;"']+`)
	// "password": "..." and friends in JSON.
	incidentAssignQuotedRe = regexp.MustCompile(`(?i)("[a-z0-9_.-]*(?:jwt|token|secret|password|passwd|private[_-]?key|api[_-]?key|auth[_-]?code|credential)[a-z0-9_.-]*"\s*:\s*)"[^"]*"`)
	// password=... and friends in key=value form.
	incidentAssignRe = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:jwt|token|secret|password|passwd|private[_-]?key|api[_-]?key|auth[_-]?code|credential)[a-z0-9_.-]*=)[^\s&,;]+`)
	// A raw 32-byte key (or a digest) in hex.
	incidentHexRe = regexp.MustCompile(`\b[0-9a-fA-F]{64,}\b`)
)

// incidentRedact removes credential material from text on its way into the
// bundle. It is deliberately blunt: over-redacting a hash costs a little
// context, under-redacting a key hands over the node's identity.
func incidentRedact(text string) string {
	if text == "" {
		return text
	}
	text = incidentPEMRe.ReplaceAllString(text, "<redacted-private-key>")
	text = incidentJWTRe.ReplaceAllString(text, "<redacted-jwt>")
	text = incidentBearerRe.ReplaceAllString(text, "$1 <redacted>")
	text = incidentUserinfoRe.ReplaceAllString(text, "$1<redacted>@")
	text = incidentProxyLineRe.ReplaceAllString(text, "$1:[redacted]")
	text = incidentAssignQuotedRe.ReplaceAllString(text, `$1"<redacted>"`)
	text = incidentAssignRe.ReplaceAllString(text, "$1<redacted>")
	text = incidentHexRe.ReplaceAllString(text, "<redacted-hex>")
	return text
}

// incidentRedactionNote is written into every bundle. An operator reading a
// redacted artifact has to know what is missing and why, or the absence reads
// as evidence of absence.
const incidentRedactionNote = `schema=` + incidentSchema + `

Copied: the trigger and its detail, the versions, the goroutine dump and its
summary, the tails of events.log and important.log (and the RAM log when this
host has one), the URNETWORK_* and Go runtime conditions in force, and the
evidence state files listed in state/inventory.txt as copied:redacted. For an
oomcap or thrash bundle that includes the state those subsystems own:
oom_cap.json (the automatic start cap, its reduction ring and the clean-day
clock), run.marker (the previous start's OOM counter baseline), thrash_cap.json
(the reduced next-start cap, its expiry and the restart ring), thrash_status
(the watchdog's published sensor snapshot), and this pid's thrash_exit.<pid>
marker when it is already on disk. The restart writes its own marker at the
exit, just after this capture, so a restart bundle usually carries none; a
marker left by another pid is never copied, because it describes an exit this
bundle did not witness.

Withheld entirely, by name/size/mode only:
  * jwt and every .provider*.jwt - the whole file is a bearer credential, so
    there is nothing safe to keep of it.
  * .client_jwts.json and its timestamped .bak snapshots - the per-slot client
    JWTs, one per configured proxy identity.
  * .provider.key, .provider.extender.key, .provider.extender-*.key,
    .provider.extender.measurement.key, .provider.cert - private key material;
    the same argument.
  * proxy and proxy.state - proxy credentials, and identity keys that carry
    the proxy username.
  * proxy_url.json - the URL-sourced proxy cache stores each entry's user and
    password in plaintext.
  A name matching this rule is withheld even if a future whitelist entry names
  it: the whitelist says "useful", this rule says "secret", and the rule wins.

Removed from everything that is written, stack.txt included:
  * JWTs (eyJ... three-part tokens) and bearer/basic authorization headers.
  * URL userinfo (scheme://user:pass@host) and proxy credentials in
    host:port:user:pass form, wherever they appear: the end of a log line, the
    middle of one with an error suffix, inside a JSON value, or in a
    comma-separated list. Addresses without both credential fields
    (host:port) are left alone.
  * PEM private key blocks and hex runs of 64+ characters (a 32-byte key or a
    digest; cheaper to lose than to leak).
  * secret-named assignments, quoted or key=value.

The provider's incident_capture.go writes profiles and meta only, so it has
nothing to redact; this port copies state and logs, so it does. The goroutine
profile is debug=1 for the same reason: debug=2 prints function arguments, and
an argument can be a token.
`

// pruneIncidentBundles keeps the newest keep directories. The names sort by
// UTC timestamp, so lexical order is age order.
func pruneIncidentBundles(root string, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	for len(dirs) > keep {
		_ = os.RemoveAll(filepath.Join(root, dirs[0]))
		dirs = dirs[1:]
	}
}
