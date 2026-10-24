package miner

import (
	"sync"
	"time"
)

// metrics_errors.go ports the 3.23-fix v33 provider's persistent per-category
// error counts (provider/metrics_provider.go persistentErrors) and wires the
// miner's own failure events into them.
//
// What the provider could not do: its counter had no producer. v33 fed
// IncrPersistentError from connect's SetPersistentErrorFunc, and the pinned
// connect fork removed that hook, so the provider path now passes the function
// to a no-op stub (provider/metrics_stubs.go) and the store never moves. Porting
// the store alone would have shipped the same dead counter here, so the miner
// counts where it already classifies failure: providerDiagnostics.observeFile.
// Every code counted below is one the diagnostics stream emits at a real
// failure, so the series can only move for an event the node actually
// recorded.
//
// The file lives beside the other miner state files (<state>/error_counts.json)
// in the same checksummed envelope the provider's atomicWriteJSON wrote. That
// compatibility is format and filename, not history: 3.23-fix's counter had no
// producer (its hook was a stub), so it never accumulated counts, and the
// provider's state directory ignored URNETWORK_STATE_DIR, which this miner
// honours. A file the provider wrote therefore reloads by name and shape; the
// numbers those codes report start here. Writes are atomic (temp file + rename,
// with the previous copy kept as .bak), and a corrupt file is quarantined and
// replaced rather than blocking startup: these are operator-visible stats, not
// consensus state.

// persistentErrorFlushInterval is how often an idle-but-dirty store is written.
// Failure events are rare (a rejected sign-in, a refused key write), so the
// provider's 100-count immediate-flush trigger buys nothing here; a shutdown,
// hotswap or fatal-exit flush covers the exit paths.
const persistentErrorFlushInterval = 5 * time.Minute

type persistentErrors struct {
	mu sync.Mutex

	// counts is the per-category lifetime total. The category is the
	// diagnostics code, a fixed bounded set: it can never grow with proxy
	// count or client id, so the scrape's cardinality is bounded.
	counts map[string]uint64

	// path is the store file, or "" when no state directory is available. The
	// store then lives in memory only: the metric still reports this process's
	// errors instead of the family vanishing.
	path string

	dirty      bool
	lastFlush  time.Time
	flushEvery time.Duration
}

// persistentErrorsPath returns <state>/error_counts.json, or "" when no state
// directory is available.
func persistentErrorsPath() string {
	return mustStatePathQuiet("error_counts.json")
}

// loadPersistentErrors reads the persisted counts. A missing, corrupt or
// unreadable file starts a fresh store rather than blocking the miner.
func loadPersistentErrors(path string) *persistentErrors {
	pe := &persistentErrors{
		counts:     map[string]uint64{},
		path:       path,
		flushEvery: persistentErrorFlushInterval,
		lastFlush:  time.Now(),
	}
	if path == "" {
		return pe
	}
	// The same checksum envelope, backup and quarantine the audit ring uses, so
	// a truncated write cannot be read as a real (smaller) count.
	var loaded map[string]uint64
	if ok, err := loadJSONWithRecovery(path, &loaded); ok {
		for cat, n := range loaded {
			pe.counts[cat] = n
		}
	} else if err != nil {
		tlog("[metrics] error loading persistent error counts: %v\n", err)
	}
	return pe
}

// Incr counts one event in category cat. A nil store means "no counts", never a
// panic: increment, flush and snapshot all tolerate it, because the store is
// loaded lazily and the tick, the scrape and the exit path must not fall over a
// rig that holds none.
func (pe *persistentErrors) Incr(cat string) {
	if pe == nil || cat == "" {
		return
	}
	pe.mu.Lock()
	pe.counts[cat]++
	pe.dirty = true
	pe.mu.Unlock()
}

// MaybeFlush writes the store if it is dirty and the flush throttle has
// elapsed. Returns whether a write happened.
func (pe *persistentErrors) MaybeFlush(now time.Time) bool {
	if pe == nil {
		return false
	}
	pe.mu.Lock()
	defer pe.mu.Unlock()
	if !pe.dirty || pe.path == "" {
		return false
	}
	if !pe.lastFlush.IsZero() && now.Sub(pe.lastFlush) < pe.flushEvery {
		return false
	}
	if err := pe.writeLocked(); err != nil {
		tlog("[metrics] periodic error flush failed: %v\n", err)
		return false // keep dirty; retry on the next tick
	}
	pe.dirty = false
	pe.lastFlush = now
	return true
}

// Flush forces a write (shutdown, hotswap, and the fatal exit path). It keeps
// the dirty flag when the write fails so a later attempt can still persist.
func (pe *persistentErrors) Flush() {
	if pe == nil {
		return
	}
	pe.mu.Lock()
	defer pe.mu.Unlock()
	if !pe.dirty || pe.path == "" {
		return
	}
	if err := pe.writeLocked(); err != nil {
		tlog("[metrics] shutdown error flush failed: %v\n", err)
		return
	}
	pe.dirty = false
	pe.lastFlush = time.Now()
}

// Snapshot returns a copy of the counts for the scrape.
func (pe *persistentErrors) Snapshot() map[string]uint64 {
	if pe == nil {
		return nil
	}
	pe.mu.Lock()
	defer pe.mu.Unlock()
	out := make(map[string]uint64, len(pe.counts))
	for cat, n := range pe.counts {
		out[cat] = n
	}
	return out
}

// writeLocked persists the counts. Caller holds mu.
func (pe *persistentErrors) writeLocked() error {
	snapshot := make(map[string]uint64, len(pe.counts))
	for cat, n := range pe.counts {
		snapshot[cat] = n
	}
	return atomicWriteJSON(pe.path, snapshot)
}

// persistentErrStore is the process's lifetime per-category error store. The
// lifetime collector flushes it on its throttle; the graceful shutdown, the two
// hotswap exit points and the fatal exit path flush it unconditionally; the
// scrape reads it.
//
// It loads on first use, not at package initialisation. Loading resolves the
// state directory (URNETWORK_STATE_DIR, or the operator's own directory under
// --all-operators), reads it, and can rename a corrupt file there. None of that
// belongs in package init: every invocation would pay for it (provider --version
// and --help included), the path is only knowable once the flags and the
// environment are settled, and deferring the load is what lets a test point the
// store at a temporary directory before anything reads it.
var (
	persistentErrStore     *persistentErrors
	persistentErrStoreOnce sync.Once
)

// persistentErrorStore returns the store, loading it on the first call. Every
// package-level entry point goes through here, so the lazy value is published
// by the Once rather than read from a bare variable.
func persistentErrorStore() *persistentErrors {
	persistentErrStoreOnce.Do(func() {
		if persistentErrStore == nil {
			persistentErrStore = loadPersistentErrors(persistentErrorsPath())
		}
	})
	return persistentErrStore
}

func init() { metricsSeamErrors = persistentErrorsSeamSnapshot }

// persistentErrorsSeamSnapshot exposes the counts to the scrape
// (urnet_lifetime_errors_total{category}), the provider's metric name.
func persistentErrorsSeamSnapshot() map[string]uint64 {
	return persistentErrorStore().Snapshot()
}

// IncrPersistentError counts one categorized failure. The provider name is kept
// so the two stores read the same at the call site.
func IncrPersistentError(cat string) {
	persistentErrorStore().Incr(cat)
}

// FlushPersistentErrors writes the counts now (graceful shutdown, and the
// hotswap paths that exit without one). The fatal exit path calls
// flushPersistentErrorsAtExit, which bounds this same write.
func FlushPersistentErrors() {
	persistentErrorStore().Flush()
}

// persistentErrorExitFlushTimeout bounds the write on a fatal exit path.
//
// The store is a few hundred bytes and the write normally takes microseconds;
// the bound exists because the state disk can be the reason the node is dying
// (full, hung mount, I/O that never completes). fsync on such a disk blocks
// indefinitely, and a process that has already failed must still terminate: the
// supervisor restarts it, and the increments it could not write are lost either
// way. Recording the failure is the goal, so the write is attempted -- not
// skipped -- and only abandoned once it has clearly hung.
const persistentErrorExitFlushTimeout = 2 * time.Second

// flushPersistentErrorsAtExit writes the counts from a path that is about to
// terminate the process.
//
// os.Exit skips deferred calls, including finishControlRun's flush, so without
// this the categories counted while the run went down (startup refusal, worker
// panic, refused capture profile, quarantined sign-in) would never reach disk:
// the failures that killed the node are exactly the ones an investigation needs.
//
// The write runs on its own goroutine, under a bound, with the panic recovered.
// A stuck state disk or a panicking flush must not keep a dead process alive,
// and neither may it turn a failure exit into a hang or a crash.
func flushPersistentErrorsAtExit() {
	done := make(chan struct{})
	go func() {
		// Registered first so it runs last: the recover completes (and any
		// lock Flush held is unwound) before the waiter below is released.
		defer close(done)
		defer func() {
			if cause := recover(); cause != nil {
				tlog("[metrics] error flush on exit panicked: %v\n", cause)
			}
		}()
		FlushPersistentErrors()
	}()
	timer := time.NewTimer(persistentErrorExitFlushTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		tlog("[metrics] error flush on exit did not finish within %s; exiting anyway\n", persistentErrorExitFlushTimeout)
	}
}

// persistentErrorCategory reports whether a diagnostics code names a failure
// worth counting across restarts. It is deliberately an allow-list of the codes
// emitted at a real failure: retry-in-progress (retry_wait,
// network_sign_in_renewal_retry), success (identity_available, provider_started,
// status_started, observed, network_sign_in_renewed), informational
// (http_notice) and expected stops (network_sign_in_renewal_stopped, which an
// API key that never expires produces on every pass) must not be counted, or
// the series would report health as failure.
func persistentErrorCategory(code string) bool {
	switch code {
	case "jwt_save_failed",
		"rejection_save_failed",
		"rejected",
		"client_key_save_failed",
		"tls_key_save_failed",
		"extender_key_save_failed",
		"extender_identity_invalid",
		"provider_worker_failed",
		"status_failed",
		"startup_recovery_required",
		"whole_work_capture_required",
		"original_contract_capture_required",
		"network_sign_in_rejected",
		"network_sign_in_quarantined":
		return true
	}
	return false
}
