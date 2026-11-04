package miner

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func startupTestHarness(t *testing.T) *settingsHarness {
	t.Helper()
	h := newSettingsHarness(t)
	resetProviderStoreDegradedForTest()
	t.Cleanup(resetProviderStoreDegradedForTest)
	return h
}

// A store that opens with a newer stamp refuses the start: non-zero exit, the
// operator's message, no degraded flag (the schema WAS checked).
func TestStateStartupRefusesAFutureStamp(t *testing.T) {
	h := startupTestHarness(t)
	writeStampDirect(t, h.path(providerStoreFileName), `{"version":2,"min_reader":2}`)

	err := providerStartupStoreVerdict(context.Background())
	if err == nil {
		t.Fatal("a future-stamped store did not refuse the start")
	}
	want := "was written by a newer build (schema min_reader 2, this build reads up to 1); refusing to start slots or mint new identities"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal text = %q", err.Error())
	}
	code, ok := providerRunExitCode(err)
	if !ok || code != providerStartupRefusedExitCode || code == 0 {
		t.Fatalf("exit code = %d ok=%v, want %d", code, ok, providerStartupRefusedExitCode)
	}
	if providerStoreRunningDegraded() {
		t.Fatal("a refused store was reported as unavailable")
	}
	t.Logf("refusal: %s", err)
}

// A store that cannot be opened does not stop the start. It is reported once,
// counted, flagged on the scrape, and never described as schema-checked.
func TestStateStartupContinuesWhenTheStoreIsLocked(t *testing.T) {
	h := startupTestHarness(t)
	path := h.path(providerStoreFileName)
	writeStampDirect(t, path, `{"version":2,"min_reader":2}`) // would refuse if it could be read
	holder, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	before := persistentErrorsSeamSnapshot()[providerStoreUnavailableCategory]
	if err := providerStartupStoreVerdict(context.Background()); err != nil {
		t.Fatalf("an unopenable store refused the start: %v", err)
	}
	if !providerStoreRunningDegraded() {
		t.Fatal("the degraded flag is not set")
	}
	if got := persistentErrorsSeamSnapshot()[providerStoreUnavailableCategory]; got != before+1 {
		t.Fatalf("unavailable count %d -> %d, want +1", before, got)
	}
	var b strings.Builder
	writeStateStoreMetrics(&b)
	if !strings.Contains(b.String(), "urnet_state_store_degraded 1\n") {
		t.Fatalf("scrape does not show the degraded flag:\n%s", b.String())
	}
	events, _ := os.ReadFile(h.path("events.log"))
	line := string(events)
	if !strings.Contains(line, "state store unavailable, continuing without it") {
		t.Fatalf("events.log lacks the degraded line:\n%s", line)
	}
	if strings.Count(line, "continuing without it") != 1 {
		t.Fatalf("degraded line is not exactly one:\n%s", line)
	}
	if strings.Contains(line, "refusing to start") {
		t.Fatalf("an unread store was described as refused:\n%s", line)
	}
	// A second verdict does not log again.
	_ = providerStartupStoreVerdict(context.Background())
	events, _ = os.ReadFile(h.path("events.log"))
	if strings.Count(string(events), "continuing without it") != 1 {
		t.Fatalf("degraded line repeated:\n%s", events)
	}
}
