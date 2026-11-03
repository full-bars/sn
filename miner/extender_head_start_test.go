package miner

// Tests for the bounded extender head start (extender_head_start.go).
//
// The four required behaviours are one test each: a completed attempt releases
// at once, the cap releases with no attempt, a start whose legacy import did
// real work is not held, and a run with no direct slot is not held. The STATUS=
// parity test keeps the hold's line and the startup phase from disagreeing.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// shortHeadStart makes the hold's cap and poll fast for a test.
func shortHeadStart(t *testing.T, cap, poll time.Duration) {
	t.Helper()
	previousCap, previousPoll := extenderHeadStartCap, extenderHeadStartPoll
	extenderHeadStartCap, extenderHeadStartPoll = cap, poll
	t.Cleanup(func() {
		extenderHeadStartCap, extenderHeadStartPoll = previousCap, previousPoll
	})
}

func resetExtenderHeadStartForTest(t *testing.T) {
	t.Helper()
	resetExtenderHeadStart()
	t.Cleanup(resetExtenderHeadStart)
}

// directHeadStartSources is a start that would hold: a serving direct slot and
// proxy slots waiting behind it.
func directHeadStartSources() extenderHeadStartSources {
	return extenderHeadStartSources{directSlot: true, directEnabled: true, proxies: 3}
}

// One completed attempt releases the proxies immediately, not at the cap.
func TestExtenderHeadStartReleasedByACompletedAttempt(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	go func() {
		time.Sleep(30 * time.Millisecond)
		publishProviderExtenderObservation(providerExtenderObservation{
			Known: true, Enabled: true, ActivatedV4: true, ActivationTime: 1,
		})
	}()

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), directHeadStartSources())
	elapsed := time.Since(start)

	if result.end != extenderHeadStartAttempted {
		t.Fatalf("end = %v, want attempted", result.end)
	}
	if !result.activatedV4 {
		t.Fatal("the result must say v4 activated")
	}
	if 2*time.Second < elapsed {
		t.Fatalf("a completed attempt held the proxies for %s", elapsed)
	}
	if line := result.line(); !strings.Contains(line, "activated v4, releasing proxies") {
		t.Fatalf("log line = %q, want it to name the activation", line)
	}
	if extenderHeadStartHeld.Load() {
		t.Fatal("the hold is still published after the attempt released it")
	}
}

// The cap is absolute: with no attempt at all the hold ends at the cap and
// releases the proxies.
func TestExtenderHeadStartCapReleasesWithoutAnAttempt(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 100*time.Millisecond, 5*time.Millisecond)

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), directHeadStartSources())
	elapsed := time.Since(start)

	if result.end != extenderHeadStartCapped {
		t.Fatalf("end = %v, want capped", result.end)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("the hold ended before the cap: %s", elapsed)
	}
	if 5*time.Second < elapsed {
		t.Fatalf("the hold ran past the cap: %s", elapsed)
	}
	if line := result.line(); !strings.Contains(line, "no attempt yet, releasing proxies") {
		t.Fatalf("log line = %q, want it to say the cap released the proxies", line)
	}
	if extenderHeadStartHeld.Load() {
		t.Fatal("the hold is still published after the cap")
	}
}

// A start whose legacy import adopted identities already did real work (a
// 3.23-fix to sn transition) and must not be given a hold.
func TestExtenderHeadStartSkippedWhenTheLegacyImportAdoptedIdentities(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	sources := directHeadStartSources()
	sources.legacyAdopted = 12

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), sources)

	if result.end != extenderHeadStartSkippedSlowStartup {
		t.Fatalf("end = %v, want skipped-for-slow-startup", result.end)
	}
	if elapsed := time.Since(start); 500*time.Millisecond < elapsed {
		t.Fatalf("a skip waited %s", elapsed)
	}
	want := "extender head start skipped: legacy import adopted 12 identities"
	if got := result.line(); got != want {
		t.Fatalf("log line = %q, want %q", got, want)
	}
	if extenderHeadStartHeld.Load() {
		t.Fatal("a skipped start published the hold")
	}
}

// A start whose provider store took longer than the limit is skipped too.
func TestExtenderHeadStartSkippedWhenTheStoreOpenWasSlow(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	sources := directHeadStartSources()
	sources.storeOpen = extenderHeadStartSlowStore + time.Second

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), sources)

	if result.end != extenderHeadStartSkippedSlowStartup {
		t.Fatalf("end = %v, want skipped-for-slow-startup", result.end)
	}
	if elapsed := time.Since(start); 500*time.Millisecond < elapsed {
		t.Fatalf("a skip waited %s", elapsed)
	}
	if line := result.line(); !strings.Contains(line, "provider store took 11.0s to open") {
		t.Fatalf("log line = %q, want it to name the store open", line)
	}
}

// No direct slot means no extender role in this run, so nothing is held.
func TestExtenderHeadStartWithoutADirectSlot(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	sources := directHeadStartSources()
	sources.directSlot = false

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), sources)

	if result.end != extenderHeadStartDisabled {
		t.Fatalf("end = %v, want disabled", result.end)
	}
	if elapsed := time.Since(start); 500*time.Millisecond < elapsed {
		t.Fatalf("a run with no direct slot waited %s", elapsed)
	}
	if extenderHeadStartHeld.Load() {
		t.Fatal("a run with no direct slot published the hold")
	}
	if line := result.line(); line != "extender head start skipped: no direct slot" {
		t.Fatalf("log line = %q", line)
	}
}

// A launch the activation budget withheld never attempts, so its verdict
// releases the hold without waiting for the cap.
func TestExtenderHeadStartReleasedWhenTheBudgetWithheldTheLaunch(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	go func() {
		time.Sleep(20 * time.Millisecond)
		publishExtenderHeadStartRole(false)
	}()

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), directHeadStartSources())

	if result.end != extenderHeadStartDisabled {
		t.Fatalf("end = %v, want disabled", result.end)
	}
	if 2*time.Second < time.Since(start) {
		t.Fatal("a withheld launch held the proxies")
	}
	if extenderHeadStartHeld.Load() {
		t.Fatal("the hold is still published after the withheld verdict")
	}
}

// A provider shutdown during the hold releases the proxies at once.
func TestExtenderHeadStartReleasedByShutdown(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	result := extenderHeadStartWait(ctx, directHeadStartSources())

	if result.end != extenderHeadStartShutdown {
		t.Fatalf("end = %v, want shutdown", result.end)
	}
	if line := result.line(); !strings.Contains(line, "shutting down, releasing proxies") {
		t.Fatalf("log line = %q", line)
	}
	if extenderHeadStartHeld.Load() {
		t.Fatal("the hold is still published after the shutdown")
	}
}

// The startup phase and the systemd STATUS= line must never disagree. While
// the proxies are deliberately held the line names the hold and its cap, and
// never reads "resolving proxies"; when they are not held it never mentions
// the hold, so an operator cannot mistake a normal resolution for a wait.
func TestStartupPhaseNeverDisagreesWithTheSystemdLine(t *testing.T) {
	resetSystemdStatusState(t)
	resetExtenderHeadStartForTest(t)

	phases := []struct {
		name       string
		holding    bool
		total      int64
		resolution int32
	}{
		{"ready before resolution", false, 0, proxyResolutionPending},
		{"ready with proxies configured", false, 5, proxyResolutionOK},
		{"holding before resolution", true, 0, proxyResolutionPending},
		{"holding with proxies configured", true, 5, proxyResolutionOK},
		{"holding with proxies configured, resolution pending", true, 3, proxyResolutionPending},
	}
	for _, phase := range phases {
		t.Run(phase.name, func(t *testing.T) {
			resetSystemdStatusState(t)
			resetExtenderHeadStart()
			extenderHeadStartHeld.Store(phase.holding)
			proxiesConfigured.Store(phase.total)
			proxyResolutionStatus.Store(phase.resolution)

			line := systemdStatusLine()
			if phase.holding {
				if want := "starting: giving the extender one activation attempt (up to 20s)"; line != want {
					t.Fatalf("holding line = %q, want %q", line, want)
				}
				if strings.Contains(line, "resolving proxies") {
					t.Fatalf("holding line claims resolution: %q", line)
				}
				return
			}
			if strings.Contains(line, "extender") {
				t.Fatalf("a settled line mentions the hold: %q", line)
			}
			if phase.total == 0 && line != "starting: resolving proxies" {
				t.Fatalf("pending resolution line = %q", line)
			}
			if phase.total > 0 && !strings.Contains(line, "proxies authenticated") {
				t.Fatalf("configured proxies line = %q", line)
			}
		})
	}
}

// A role that reports off must release the hold at once. The published
// observation cannot carry that verdict — the status callback clears it so no
// stale enabled reading survives — so the hold reads the callback's own marker.
// Without that marker this test waits out the whole cap, which is the startup
// stall a box whose 443 is already taken used to pay on every launch.
func TestExtenderHeadStartReleasedWhenTheRoleReportsOff(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	listener := &providerExtenderStatusListener{}
	// The role is allowed, so only the disabled status can release the gate:
	// without this the withheld-role path would release it and the test would
	// pass without exercising the fix.
	publishExtenderHeadStartRole(true)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true})
	go func() {
		time.Sleep(30 * time.Millisecond)
		listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: false})
	}()

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), directHeadStartSources())
	elapsed := time.Since(start)

	if result.end != extenderHeadStartDisabled {
		t.Fatalf("end = %v, want disabled", result.end)
	}
	if 2*time.Second < elapsed {
		t.Fatalf("a role that reported off held the proxies for %s", elapsed)
	}
	// The verdict the gate acts on is the published one: it must say the role is
	// known and off, which is what makes the check in the gate fire.
	if latest := latestProviderExtenderObservation(); !latest.Known || latest.Enabled {
		t.Fatalf("the gate cannot see the off verdict: %+v", latest)
	}
}

// A listen that failed cannot lead to an activation, so it is an attempt as far
// as the hold is concerned: waiting for an activation time that will never come
// only spends the cap.
func TestExtenderHeadStartReleasedWhenTheBindFails(t *testing.T) {
	resetExtenderHeadStartForTest(t)
	shortHeadStart(t, 10*time.Second, 5*time.Millisecond)

	listener := &providerExtenderStatusListener{}
	publishExtenderHeadStartRole(true)
	go func() {
		time.Sleep(30 * time.Millisecond)
		listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{
			Enabled: true, Listening: false, ListenError: "bind: address already in use",
		})
	}()

	start := time.Now()
	result := extenderHeadStartWait(context.Background(), directHeadStartSources())
	elapsed := time.Since(start)

	if result.end != extenderHeadStartAttempted {
		t.Fatalf("end = %v, want attempted", result.end)
	}
	if 2*time.Second < elapsed {
		t.Fatalf("a failed bind held the proxies for %s", elapsed)
	}
}
