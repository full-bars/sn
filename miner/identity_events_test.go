package miner

// The startup summary is the only place an operator sees a whole start's
// identity outcomes, and the store is what is left after the log has scrolled.
// These tests pin both, plus the promise that a broken store costs nothing but
// the durable copy.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/clientauth"

	"github.com/urnetwork/connect"
)

// testProviderIdentityStore gives a test its own state directory and its own
// store handle, and closes the handle afterwards so the next test starts clean.
func testProviderIdentityStore(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	closeProviderStore()
	t.Cleanup(closeProviderStore)
}

// One start reports each of the four outcomes once, however often a slot
// reports it, and a slot that kept its identity reports nothing at all.
func TestProviderStartupSummaryCountsIdentityOutcomesOnce(t *testing.T) {
	testProviderIdentityStore(t)
	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(5)
	tracker.authReused(0, connect.NewId(), "direct")
	tracker.authNew(1, connect.NewId(), "proxy:10.0.0.1:1080")

	tracker.identityOutcome(1, "proxy:10.0.0.1:1080", clientauth.IdentityOutcomeMintedNew, "")
	// A retry that lands on the same decision is the same outcome, not a
	// second mint.
	tracker.identityOutcome(1, "proxy:10.0.0.1:1080", clientauth.IdentityOutcomeMintedNew, "")
	tracker.identityOutcome(2, "proxy:10.0.0.2:1080", clientauth.IdentityOutcomeMintedReplacement, "saved identity rejected")
	tracker.identityOutcome(3, "proxy:10.0.0.3:1080", clientauth.IdentityOutcomeRevoked, "revoked identity retained")
	tracker.adoptIdentities([]string{"proxy:10.0.0.4:1080"})
	tracker.emitStartupSummary()

	text := buffer.String()
	want := "identity: 1 minted new, 1 minted replacement, 1 adopted, 1 revoked"
	if !strings.Contains(text, want) {
		t.Fatalf("startup summary misses %q:\n%s", want, text)
	}
	// One summary line, not one per slot: the per-slot lines already exist.
	if count := strings.Count(text, "[startup]"); count != 1 {
		t.Fatalf("startup summary printed %d times:\n%s", count, text)
	}
	if count := strings.Count(text, "identity:"); count != 1 {
		t.Fatalf("identity segment printed %d times:\n%s", count, text)
	}

	// Four slots reached an outcome, so the store holds four records: the
	// healthy slot wrote nothing.
	events, err := providerIdentityEvents(-1)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("recorded %d events, want 4: %+v", len(events), events)
	}
}

// A wave of replacements is one warning line, at the threshold and not below
// it, and it names both causes in the order an operator should check them.
func TestProviderIdentityWaveWarningFiresOnlyAboveTheThreshold(t *testing.T) {
	cases := []struct {
		name         string
		outcomes     int
		replacements int
		threshold    int
		warn         bool
	}{
		// 5% of 100 slots is 5, and the floor of 3 never binds there.
		{"a few replacements", 100, 3, 5, false},
		{"at the threshold", 100, 5, 5, false},
		{"above the threshold", 100, 6, 5, true},
		// 5% of 10 is 0, so the floor of 3 decides.
		{"the floor holds a small pool", 10, 3, 3, false},
		{"just above the floor", 10, 4, 3, true},
		// Fewer outcomes than the floor can never be a wave.
		{"fewer outcomes than the floor", 2, 2, 3, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testProviderIdentityStore(t)
			buffer := captureProviderBringUpLog(t)
			tracker := newProviderBringUpTracker(testCase.outcomes)
			for slot := 0; slot < testCase.outcomes; slot++ {
				outcome := clientauth.IdentityOutcomeMintedNew
				if slot < testCase.replacements {
					outcome = clientauth.IdentityOutcomeMintedReplacement
				}
				tracker.identityOutcome(uint64(slot), fmt.Sprintf("proxy:10.0.%d.%d:1080", slot/250, slot%250), outcome, "saved identity rejected")
			}
			tracker.emitStartupSummary()

			text := buffer.String()
			if warned := strings.Contains(text, "⚠️ [identity]"); warned != testCase.warn {
				t.Fatalf("warning = %v, want %v:\n%s", warned, testCase.warn, text)
			}
			if !testCase.warn {
				return
			}
			line := fmt.Sprintf("⚠️ [identity] %d of %d slots minted a replacement identity this start (threshold %d)",
				testCase.replacements, testCase.outcomes, testCase.threshold)
			if !strings.Contains(text, line) {
				t.Fatalf("warning misses %q:\n%s", line, text)
			}
			for _, cause := range []string{"state directory is intact", "platform is refusing identities that used to work"} {
				if !strings.Contains(text, cause) {
					t.Fatalf("warning does not name %q:\n%s", cause, text)
				}
			}
			// One line for the wave, however many times the summary runs.
			tracker.emitStartupSummary()
			if count := strings.Count(buffer.String(), "⚠️ [identity]"); count != 1 {
				t.Fatalf("wave warning printed %d times:\n%s", count, buffer.String())
			}
		})
	}
}

// The events survive a reopen, carry the slot, outcome, reason and a UTC
// instant, and the bucket keeps only the newest records.
func TestProviderIdentityEventsRoundTripAndStayBounded(t *testing.T) {
	testProviderIdentityStore(t)
	previousKeep := identityEventKeep
	identityEventKeep = 4
	t.Cleanup(func() { identityEventKeep = previousKeep })

	for slot := 0; slot < 7; slot++ {
		recordProviderIdentityEvent(fmt.Sprintf("proxy:10.0.0.%d:1080", slot), identityOutcomeMintedReplacement, "saved identity rejected")
	}
	events, err := providerIdentityEvents(-1)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != identityEventKeep {
		t.Fatalf("kept %d events, want %d: %+v", len(events), identityEventKeep, events)
	}
	for index, event := range events {
		if want := fmt.Sprintf("proxy:10.0.0.%d:1080", index+3); event.Slot != want {
			t.Fatalf("event %d names %q, want the newest four starting at %q", index, event.Slot, want)
		}
		if event.Outcome != string(identityOutcomeMintedReplacement) || event.Reason != "saved identity rejected" {
			t.Fatalf("event %d = %+v, want a replacement with its reason", index, event)
		}
		if event.At.IsZero() || event.At.Location() != time.UTC {
			t.Fatalf("event %d timestamp = %v, want a UTC instant", index, event.At)
		}
	}
	// The file is what grows, so the bound has to survive a restart: the
	// sequence continues where the last process left it.
	closeProviderStore()
	recordProviderIdentityEvent("proxy:10.0.1.0:1080", identityOutcomeMintedNew, "")
	recordProviderIdentityEvent("proxy:10.0.1.1:1080", identityOutcomeRevoked, "revoked identity retained")
	recent, err := providerIdentityEvents(2)
	if err != nil {
		t.Fatalf("read events after reopen: %v", err)
	}
	if len(recent) != 2 || recent[0].Slot != "proxy:10.0.1.0:1080" || recent[1].Slot != "proxy:10.0.1.1:1080" {
		t.Fatalf("newest events after a restart = %+v", recent)
	}
	all, err := providerIdentityEvents(-1)
	if err != nil {
		t.Fatalf("read events after reopen: %v", err)
	}
	if len(all) != identityEventKeep {
		t.Fatalf("bucket holds %d events after a restart, want %d", len(all), identityEventKeep)
	}
}

// A store that will not open must cost the durable copy and nothing else: the
// start still counts, still reports, and reports the failure once instead of
// once per slot.
func TestProviderIdentityOutcomesSurviveAnUnavailableStore(t *testing.T) {
	dir := t.TempDir()
	// A directory where the database file belongs makes bbolt's open fail
	// deterministically, which is the shape of a wrong mount or a permissions
	// change.
	if err := os.MkdirAll(filepath.Join(dir, providerStoreFileName), 0o700); err != nil {
		t.Fatalf("seed: %s", err)
	}
	t.Setenv("URNETWORK_STATE_DIR", dir)
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	FlushPersistentErrors()
	before := persistentErrorsSeamSnapshot()[providerStoreUnavailableCategory]

	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(3)
	tracker.identityOutcome(0, "proxy:10.0.0.1:1080", clientauth.IdentityOutcomeMintedReplacement, "saved identity rejected")
	tracker.identityOutcome(1, "proxy:10.0.0.2:1080", clientauth.IdentityOutcomeMintedReplacement, "saved identity rejected")
	tracker.identityOutcome(2, "proxy:10.0.0.3:1080", clientauth.IdentityOutcomeRevoked, "revoked identity retained")
	tracker.emitStartupSummary()

	FlushPersistentErrors()
	if after := persistentErrorsSeamSnapshot()[providerStoreUnavailableCategory]; after != before+1 {
		t.Fatalf("unavailable store counted %d times, want once", after-before)
	}
	text := buffer.String()
	if !strings.Contains(text, "identity: 0 minted new, 2 minted replacement, 0 adopted, 1 revoked") {
		t.Fatalf("the start did not report its outcomes without a store:\n%s", text)
	}
	if events, err := providerIdentityEvents(-1); err != nil || len(events) != 0 {
		t.Fatalf("unavailable store returned events=%d err=%v, want none", len(events), err)
	}
}

// The slot wiring, not just the tracker: the run's own mint path feeds the
// report, so a start that mints for a slot with no saved identity records it
// without anyone reading a log line.
func TestProviderRunRecordsAFirstStartMint(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	stop := errors.New("synthetic stop after authenticated provider")
	if err := fixture.run(t.Context(), true, providerRegistrationHooks{afterAuthenticated: func(string, connect.Id, []byte) error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("provider first start: %v", err)
	}
	events, err := providerIdentityEvents(-1)
	if err != nil || len(events) != 1 {
		t.Fatalf("recorded %d events (err=%v), want one", len(events), err)
	}
	if events[0].Slot != "direct" || events[0].Outcome != string(identityOutcomeMintedNew) {
		t.Fatalf("first start recorded %+v, want a minted new identity for the direct slot", events[0])
	}
}
