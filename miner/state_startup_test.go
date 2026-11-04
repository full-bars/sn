package miner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
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

func candidateHandshake(t *testing.T, stampEnv *string) (error, string) {
	t.Helper()
	if stampEnv == nil {
		os.Unsetenv(EnvHotSwapStoreStamp)
	} else {
		t.Setenv(EnvHotSwapStoreStamp, *stampEnv)
	}
	var out bytes.Buffer
	conn := struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(nil), &out}
	err := runHotSwapChildHandshake(conn, docopt.Opts{}, "http://127.0.0.1:1")
	return err, out.String()
}

// A candidate told of a future stamp refuses before it announces READY, so
// the parent keeps serving and no slot starts. Preflight (JWT, API dial) is
// never reached: the state dir has no jwt, so any other failure would show.
func TestStateHotswapCandidateRefusesAFutureStampBeforeReady(t *testing.T) {
	h := startupTestHarness(t)
	stamp := hotSwapStampPrefix + `{"version":2,"min_reader":2}`
	err, wire := candidateHandshake(t, &stamp)
	if !errors.Is(err, errProviderStoreNewer) {
		t.Fatalf("candidate err = %v, want a newer-store refusal", err)
	}
	if !strings.Contains(err.Error(), "was written by a newer build (schema min_reader 2, this build reads up to 1); refusing to start slots or mint new identities") {
		t.Fatalf("refusal text = %q", err)
	}
	if strings.Contains(wire, `"READY"`) || !strings.Contains(wire, `"ERROR"`) {
		t.Fatalf("candidate wrote %q, want ERROR and no READY", wire)
	}
	if code, ok := providerRunExitCode(startupRefusal(context.Background(), err)); !ok || code != providerStartupRefusedExitCode {
		t.Fatalf("exit code = %d ok=%v", code, ok)
	}
	events, _ := os.ReadFile(h.path("events.log"))
	if !strings.Contains(string(events), "PROVIDER REFUSED") {
		t.Fatalf("refusal not in events.log: %s", events)
	}
}

// A parent that could not read its store, or sent nothing, leaves the schema
// unchecked: the candidate runs degraded and does not fail on that account.
func TestStateHotswapCandidateDegradesWhenTheParentCannotReport(t *testing.T) {
	startupTestHarness(t)
	for name, env := range map[string]*string{"unset": nil, "unavailable": ptr(hotSwapStampUnavailable)} {
		resetProviderStoreDegradedForTest()
		err, wire := candidateHandshake(t, env)
		if errors.Is(err, errProviderStoreNewer) {
			t.Fatalf("%s: refused: %v", name, err)
		}
		if !providerStoreRunningDegraded() {
			t.Fatalf("%s: degraded flag not set (wire %q)", name, wire)
		}
	}
	resetProviderStoreDegradedForTest()
	if err := hotSwapCandidateStoreVerdict(); err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }

// The parent hands its stamp to the candidate through the spawn environment
// and takes it back afterwards; a readable stamp does not stop the candidate.
func TestStateHotswapParentSendsItsStampAtSpawn(t *testing.T) {
	h := startupTestHarness(t)
	if _, err := openProviderStoreForProcess(); err != nil {
		t.Fatal(err)
	}
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)
	origSpawn := spawnCandidateFunc
	t.Cleanup(func() { spawnCandidateFunc = origSpawn })
	var seen string
	spawnCandidateFunc = func(string, []string) (*HotswapParentSession, error) {
		seen = os.Getenv(EnvHotSwapStoreStamp)
		var ready bytes.Buffer
		_ = writeHotswapMessage(&ready, HotswapMessage{Type: HotswapMsgError, PID: 9, Error: "stop here"})
		return &HotswapParentSession{Reader: bufio.NewReader(&ready), Writer: io.Discard}, nil
	}
	_ = h
	_ = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if !strings.HasPrefix(seen, hotSwapStampPrefix+`{"version":1,"min_reader":1`) {
		t.Fatalf("candidate env = %q", seen)
	}
	if _, set := os.LookupEnv(EnvHotSwapStoreStamp); set {
		t.Fatal("the parent kept the stamp in its own environment")
	}
	if err := providerStoreStampVerdict([]byte(strings.TrimPrefix(seen, hotSwapStampPrefix)), "x"); err != nil {
		t.Fatalf("the parent's own stamp is refused by this build: %v", err)
	}
}
