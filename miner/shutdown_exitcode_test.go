package miner

import (
	"testing"
	"time"
)

// A drain timeout on a quarantined run must still exit 75. Exiting 1 makes the
// supervisor treat a deliberate quarantine as a generic crash and restart on the
// wrong policy; the earlier test only exercised providerRunExitCode with a
// pre-joined error, so it never reached this decision.
func TestProviderShutdownExitCodeKeepsQuarantine(t *testing.T) {
	if got := providerShutdownExitCode(true); got != providerQuarantinedExitCode {
		t.Fatalf("quarantined timeout exit code = %d, want %d", got, providerQuarantinedExitCode)
	}
	if got := providerShutdownExitCode(false); got != providerShutdownTimeoutExitCode {
		t.Fatalf("plain timeout exit code = %d, want %d", got, providerShutdownTimeoutExitCode)
	}
}

func TestShutdownExpireArmsTheGivenExitCode(t *testing.T) {
	origWatchdog := providerShutdownWatchdog
	t.Cleanup(func() { providerShutdownWatchdog = origWatchdog })

	// providerShutdownExpire also captures an incident bundle, and that capture
	// stamps process-wide pacing (a total budget plus a per-trigger interval).
	// Give both back, or a later test that asserts on its own bundle is either
	// starved or paced out by this one.
	incidentGateMu.Lock()
	beforeCapture := incidentTotalCapture
	beforePacing := make(map[incidentTrigger]time.Time, len(incidentLastByTrigger))
	for k, v := range incidentLastByTrigger {
		beforePacing[k] = v
	}
	incidentGateMu.Unlock()
	t.Cleanup(func() {
		incidentGateMu.Lock()
		incidentTotalCapture = beforeCapture
		incidentLastByTrigger = beforePacing
		incidentGateMu.Unlock()
	})

	gotAfter, gotCode := time.Duration(-1), -1
	providerShutdownWatchdog = func(after time.Duration, code int) {
		gotAfter, gotCode = after, code
	}

	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	providerShutdownArmWatchdog(providerQuarantinedExitCode)
	providerShutdownCapture("workers did not drain")

	if gotCode != providerQuarantinedExitCode {
		t.Fatalf("watchdog armed with code %d, want %d", gotCode, providerQuarantinedExitCode)
	}
	if gotAfter != providerShutdownExitBound {
		t.Fatalf("watchdog armed for %s, want %s", gotAfter, providerShutdownExitBound)
	}
}

// The bound must be the whole ceiling: the immediate exit must not wait on the
// persistent-error flush, which adds up to persistentErrorExitFlushTimeout.
func TestShutdownImmediateExitDoesNotWaitOnFlush(t *testing.T) {
	origExit := exitFunc
	t.Cleanup(func() { exitFunc = origExit })

	got := -1
	exitFunc = func(code int) { got = code }

	start := time.Now()
	exitProviderRunImmediate(providerQuarantinedExitCode)
	elapsed := time.Since(start)

	if got != providerQuarantinedExitCode {
		t.Fatalf("exit code = %d, want %d", got, providerQuarantinedExitCode)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("immediate exit took %s; it must not wait on the error flush", elapsed)
	}
}
