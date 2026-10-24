package miner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetPersistentErrorStore makes the next accessor load the store again, so a
// test can observe the lazy load or install its own store as the one the
// package-level entry points use. The previous store is restored when the test
// ends.
func resetPersistentErrorStore(t *testing.T) {
	t.Helper()
	prev := persistentErrStore
	persistentErrStore = nil
	persistentErrStoreOnce = sync.Once{}
	t.Cleanup(func() {
		persistentErrStore = prev
		// Leave the Once spent rather than open: restoring the store must not
		// let a later test trigger a load from whatever directory the
		// environment names then (a developer's real ~/.urnetwork, for one).
		persistentErrStoreOnce = sync.Once{}
		persistentErrStoreOnce.Do(func() {})
	})
}

// withPersistentErrorStore points the process-wide error store at one in-memory
// instance for the duration of a test, so an assertion never depends on a
// previous test's counters.
func withPersistentErrorStore(t *testing.T) *persistentErrors {
	t.Helper()
	resetPersistentErrorStore(t)
	store := loadPersistentErrors("")
	persistentErrStore = store
	return store
}

// A counted failure is written by the flush and appears in the scrape under the
// provider's metric name, so a node that accumulates errors shows them.
func TestPersistentErrorsFlushEmitsTheCategory(t *testing.T) {
	withTempHome(t)
	resetPersistentErrorStore(t)
	path := persistentErrorsPath()
	if path == "" {
		t.Fatal("persistentErrorsPath must resolve inside the state directory")
	}
	persistentErrStore = loadPersistentErrors(path)

	IncrPersistentError("jwt_save_failed")
	IncrPersistentError("jwt_save_failed")
	IncrPersistentError("provider_worker_failed")
	FlushPersistentErrors()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("a flush must write the store: %v", err)
	}
	// The provider's envelope: the counts ride inside "data" so a node moving
	// from 3.23-fix keeps its history.
	for _, want := range []string{`"data"`, `"checksum"`, `"jwt_save_failed": 2`, `"provider_worker_failed": 1`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("persisted store is missing %s: %s", want, string(b))
		}
	}

	out := minerExtraMetrics()
	for _, want := range []string{
		"# TYPE urnet_lifetime_errors_total counter",
		`urnet_lifetime_errors_total{category="jwt_save_failed"} 2`,
		`urnet_lifetime_errors_total{category="provider_worker_failed"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("scrape is missing %q", want)
		}
	}
	// The added family must keep the whole scrape parseable.
	for _, err := range Lint(out) {
		t.Error(err)
	}
}

// The counts are all-time: a restart loads them back instead of starting at
// zero, which is the whole point of the persistence.
func TestPersistentErrorsSurviveRestart(t *testing.T) {
	withTempHome(t)
	path := persistentErrorsPath()
	store := loadPersistentErrors(path)
	store.Incr("network_sign_in_rejected")
	store.Flush()

	reloaded := loadPersistentErrors(path)
	counts := reloaded.Snapshot()
	if counts["network_sign_in_rejected"] != 1 {
		t.Fatalf("reloaded counts = %v, want network_sign_in_rejected=1", counts)
	}
}

// The store is empty until something is counted: an idle node must not invent a
// zero series.
func TestPersistentErrorsSeamIsEmptyUntilCounted(t *testing.T) {
	withPersistentErrorStore(t)
	if counts := persistentErrorsSeamSnapshot(); len(counts) != 0 {
		t.Fatalf("fresh store reported %v", counts)
	}
	if strings.Contains(minerExtraMetrics(), "urnet_lifetime_errors_total") {
		t.Fatal("an empty store must leave the family out of the scrape")
	}
}

// The throttle writes at most once per interval, and only when dirty.
func TestPersistentErrorsMaybeFlushHonorsThrottle(t *testing.T) {
	withTempHome(t)
	start := time.Now()
	store := loadPersistentErrors(persistentErrorsPath())
	store.lastFlush = start

	if store.MaybeFlush(start.Add(time.Minute)) {
		t.Fatal("an empty store must not write")
	}
	store.Incr("status_failed")
	if store.MaybeFlush(start.Add(time.Minute)) {
		t.Fatal("a flush inside the throttle window must wait")
	}
	if !store.MaybeFlush(start.Add(persistentErrorFlushInterval + time.Second)) {
		t.Fatal("the flush must write once the throttle window passes")
	}
	if store.MaybeFlush(start.Add(2 * persistentErrorFlushInterval)) {
		t.Fatal("a clean store must not rewrite")
	}
}

// Only failure codes are counted. This is the guard against a counter that
// moves for health: retry-in-progress, informational and expected stops must
// leave it alone, and the API-key stop is the one that would otherwise fire on
// every pass of a healthy node.
func TestDiagnosticsCountOnlyFailureEvents(t *testing.T) {
	store := withPersistentErrorStore(t)
	output, err := newProviderDiagnostics(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer output.close()

	output.observe(providerJwtSaveFailed, 0, true, os.ErrPermission, 0, nil)
	output.observe(providerNetworkSignInRejected, 0, false, os.ErrPermission, 0, nil)
	output.observeFile(providerNetworkSignInQuarantined, 0, false, os.ErrPermission, 0, nil, "jwt.rejected")

	// None of these is a failure: a retry in progress, a notice, a start, a
	// renewal that succeeded, and the API-key stop.
	output.observe(providerAuthenticationWait, 0, true, os.ErrPermission, time.Second, nil)
	output.observe(providerStatusNotice, 0, false, nil, 0, nil)
	output.observe(providerStarted, 0, true, nil, 0, nil)
	output.observe(providerNetworkTokenRenewed, 0, true, nil, 0, nil)
	output.observe(providerNetworkTokenRenewalStopped, 0, true, nil, 0, nil)

	counts := store.Snapshot()
	want := map[string]uint64{
		"jwt_save_failed":             1,
		"network_sign_in_rejected":    1,
		"network_sign_in_quarantined": 1,
	}
	if len(counts) != len(want) {
		t.Fatalf("counts = %v, want exactly %v", counts, want)
	}
	for cat, n := range want {
		if counts[cat] != n {
			t.Fatalf("counts[%q] = %d, want %d (all: %v)", cat, counts[cat], n, counts)
		}
	}
}

// The fatal exit is the path that matters most: os.Exit skips every deferred
// call, including finishControlRun's flush, so the failure counted while the run
// went down must be written by the exit path itself. This drives the seam
// provide() leaves through, not the flush, so a flush removed from the fatal
// path fails here even though FlushPersistentErrors still works.
func TestPersistentErrorsFatalExitFlushesPendingCounts(t *testing.T) {
	withTempHome(t)
	resetPersistentErrorStore(t)

	origExit := exitFunc
	t.Cleanup(func() { exitFunc = origExit })
	exitCode := -1
	exitFunc = func(code int) { exitCode = code }

	// What the quarantined-sign-in path counts while the run unwinds.
	IncrPersistentError("network_sign_in_quarantined")
	IncrPersistentError("network_sign_in_quarantined")

	exitProviderRun(providerQuarantinedExitCode)

	if exitCode != providerQuarantinedExitCode {
		t.Fatalf("exit code = %d, want %d", exitCode, providerQuarantinedExitCode)
	}
	reloaded := loadPersistentErrors(persistentErrorsPath())
	if counts := reloaded.Snapshot(); counts["network_sign_in_quarantined"] != 2 {
		t.Fatalf("counts written by the fatal exit = %v, want network_sign_in_quarantined=2", counts)
	}
}

// A flush that cannot write must not delay or block the exit: the node has
// already failed, and the supervisor needs the code.
func TestPersistentErrorsFatalExitSurvivesAWriteFailure(t *testing.T) {
	dir := withTempHome(t)
	resetPersistentErrorStore(t)

	// A regular file where the state directory should be makes every write
	// under it fail.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistentErrStore = loadPersistentErrors(filepath.Join(blocked, "error_counts.json"))

	origExit := exitFunc
	t.Cleanup(func() { exitFunc = origExit })
	exitCode := -1
	exitFunc = func(code int) { exitCode = code }

	IncrPersistentError("startup_recovery_required")
	exitProviderRun(providerStartupRefusedExitCode)

	if exitCode != providerStartupRefusedExitCode {
		t.Fatalf("exit code = %d, want %d: a failed flush must not swallow the exit", exitCode, providerStartupRefusedExitCode)
	}
}

// A nil store means "no counts", never a panic: the collector tick and the exit
// path both reach a store that a rig may hold none of.
func TestPersistentErrorsNilStoreIsInert(t *testing.T) {
	var store *persistentErrors
	store.Incr("status_failed")
	if store.MaybeFlush(time.Now()) {
		t.Fatal("a nil store must not report a write")
	}
	store.Flush()
	if counts := store.Snapshot(); len(counts) != 0 {
		t.Fatalf("a nil store snapshotted %v", counts)
	}
}

// Loading the store touches the state directory and can quarantine a corrupt
// file there, so it must happen on first use, not at package initialisation:
// the directory is only knowable once URNETWORK_STATE_DIR and the flags are
// settled, and an invocation that never reads the counts (provider --version)
// has no business rewriting anything.
func TestPersistentErrorStoreLoadsOnFirstUse(t *testing.T) {
	dir := withTempHome(t)
	resetPersistentErrorStore(t)

	path := filepath.Join(dir, "error_counts.json")
	if err := os.WriteFile(path, []byte("{ not the checksummed envelope"), 0o600); err != nil {
		t.Fatal(err)
	}
	quarantined := func() []string {
		matches, err := filepath.Glob(path + ".corrupt.*")
		if err != nil {
			t.Fatal(err)
		}
		return matches
	}
	if got := quarantined(); len(got) != 0 {
		t.Fatalf("the store was read before first use: %v", got)
	}

	// First use: the scrape. Reading is what quarantines the corrupt file.
	if counts := persistentErrorsSeamSnapshot(); len(counts) != 0 {
		t.Fatalf("a corrupt store must start empty, got %v", counts)
	}
	if got := quarantined(); len(got) != 1 {
		t.Fatalf("first use must load the store and quarantine the corrupt file, got %v", got)
	}
}
