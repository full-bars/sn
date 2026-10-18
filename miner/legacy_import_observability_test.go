package miner

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// --- narration capture -------------------------------------------------------

// legacyImportLogBuffer is a writer safe for the import's concurrent proof
// workers and its heartbeat goroutine.
type legacyImportLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (self *legacyImportLogBuffer) Write(raw []byte) (int, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.b.Write(raw)
}

func (self *legacyImportLogBuffer) String() string {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.b.String()
}

// captureLegacyImportLog points the package writer at a buffer for one test,
// so the narration is asserted exactly as the default (no narrator option) run
// emits it.
func captureLegacyImportLog(t *testing.T) *legacyImportLogBuffer {
	t.Helper()
	old := legacyImportLogWriter
	buffer := &legacyImportLogBuffer{}
	legacyImportLogWriter = buffer
	t.Cleanup(func() { legacyImportLogWriter = old })
	return buffer
}

// --- pure line helpers -------------------------------------------------------

func TestLegacyImportLineHelpers(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			"detection",
			legacyImportDetectionLine(".client_jwts.json", 11806, "zero", "absent", "0197d746"),
			"detection: store=.client_jwts.json entries=11806 key=zero marker=absent network=0197d746",
		},
		{
			"mapping",
			legacyImportMappingLine(1000, 3, 4, 993),
			"mapping: slots=1000 configured, 3 mapped, 4 already-custodied, 993 no-store-entry",
		},
		{
			"wait with detail",
			legacyImportWaitLine("api dial", "family=v4, attempt=1, timeout=4s"),
			"waiting for api dial (family=v4, attempt=1, timeout=4s)",
		},
		{
			"wait without detail",
			legacyImportWaitLine("api dial", ""),
			"waiting for api dial",
		},
		{
			"heartbeat",
			legacyImportHeartbeatLine(legacyImportProgress{proven: 1, fallback: 2, skipped: 3, pending: 4}, 90*time.Second),
			"heartbeat: proven=1 fallback=2 skipped=3 pending=4 elapsed=1m30s",
		},
		{
			"summary",
			legacyImportSummaryLine(1, 2, 3, 4, 2500*time.Millisecond, "budget"),
			"summary: imported=1 fallback=2 skipped=3 left=4 elapsed=2.5s result=budget",
		},
		{
			"slot outcome with detail",
			legacyImportSlotOutcomeLine("direct", "proven", "client_id=abcdef12 proof=server_key"),
			"direct: proven client_id=abcdef12 proof=server_key",
		},
		{
			"slot outcome without detail",
			legacyImportSlotOutcomeLine("proxy:127.0.0.1:1080", "refused", ""),
			"proxy:127.0.0.1:1080: refused",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.got != testCase.want {
				t.Fatalf("line = %q, want %q", testCase.got, testCase.want)
			}
		})
	}
}

func TestLegacyImportNarratorWaitThrottle(t *testing.T) {
	buffer := &legacyImportLogBuffer{}
	narrator := newLegacyImportNarrator(buffer)
	clock := time.Now()
	narrator.now = func() time.Time { return clock }

	narrator.wait("api dial", "family=v4, attempt=1")
	clock = clock.Add(time.Second)
	narrator.wait("api dial", "family=v4, attempt=2")
	narrator.wait("api dial", "family=v4, attempt=3")
	clock = clock.Add(5 * time.Second)
	narrator.wait("api dial", "family=v4, attempt=4")
	// A different kind has its own window and is not suppressed by the first.
	narrator.wait("proof slot", "direct")

	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	want := []string{
		legacyImportLinePrefix + "waiting for api dial (family=v4, attempt=1)",
		legacyImportLinePrefix + "waiting for api dial (family=v4, attempt=4) (2 more suppressed)",
		legacyImportLinePrefix + "waiting for proof slot (direct)",
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %#v, want %d lines", lines, len(want))
	}
	for index := range want {
		if lines[index] != want[index] {
			t.Fatalf("line %d = %q, want %q", index, lines[index], want[index])
		}
	}
}

func TestLegacyImportNarratorNilIsSilent(t *testing.T) {
	var narrator *legacyImportNarrator
	narrator.line("must not panic: %d", 1)
	narrator.wait("api dial", "family=v4")
	narrator.detection("store", 1, "normal", "absent", "net")
	narrator.mapping(1, 1, 0, 0)
	narrator.slotOutcome("direct", "proven", "")
	narrator.heartbeat(legacyImportProgress{}, time.Second)
	narrator.summary(0, 0, 0, 0, time.Second, "complete")
}

// --- full-run narration ------------------------------------------------------

func TestLegacyImportNarratesDetectionMappingOutcomesSummary(t *testing.T) {
	buffer := captureLegacyImportLog(t)
	fixture := newLegacyImportFixture(t, true, 2)
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatalf("proven slots must not mint, got %v", result.mintSlots)
	}
	text := buffer.String()

	first := "detection: store=.client_jwts.json entries=3 key=normal marker=absent network=" + legacyImportID8(fixture.networkID)
	mapping := "mapping: slots=2 configured, 2 mapped, 0 already-custodied, 0 no-store-entry"
	for _, want := range []string{first, mapping, "summary: imported=2 fallback=0 skipped=0 left=0 elapsed=", "result=complete"} {
		if !strings.Contains(text, want) {
			t.Fatalf("narration misses %q:\n%s", want, text)
		}
	}
	// Both configured slots resolved with a named result; nothing is silent.
	for _, settings := range fixture.proxySettings {
		if !strings.Contains(text, "proxy:"+settings.Address+" (user ") {
			t.Fatalf("narration misses a slot line for %s:\n%s", settings.Address, text)
		}
	}
	if !strings.Contains(text, "proven client_id=") || !strings.Contains(text, "proof=server_key") {
		t.Fatalf("narration misses the proof verdict:\n%s", text)
	}
	// The cadence: detection, then mapping, then the slot outcomes, then the
	// summary.
	if !(strings.Index(text, "detection:") < strings.Index(text, mapping)) {
		t.Fatalf("mapping precedes detection:\n%s", text)
	}
	if !(strings.Index(text, mapping) < strings.Index(text, "proven client_id=")) {
		t.Fatalf("a slot outcome precedes mapping:\n%s", text)
	}
	if !strings.HasSuffix(strings.TrimSpace(text), "result=complete") {
		t.Fatalf("summary is not the last line:\n%s", firstLines(text, 12))
	}
}

// The direct identity is a candidate like any other slot and is proved alone
// before the proxies, so its result is the first outcome line an operator sees.
func TestLegacyImportDirectIdentityIsNarratedFirst(t *testing.T) {
	buffer := captureLegacyImportLog(t)
	fixture := newLegacyImportFixture(t, true, 2)
	providers := append([]*connect.ProxySettings{nil}, fixture.proxySettings...)
	result := runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       fixture.dir,
		networkJwtPath: filepath.Join(fixture.dir, "jwt"),
		providers:      providers,
		recorder:       fixture.recorder,
		newProver: func(legacyImportProofDialWait) (legacyImportProver, func(), error) {
			return fixture.prover, func() {}, nil
		},
	})
	if len(result.mintSlots) != 0 {
		t.Fatalf("proven slots must not mint, got %v", result.mintSlots)
	}
	text := buffer.String()
	if !strings.Contains(text, "mapping: slots=3 configured, 3 mapped, 0 already-custodied, 0 no-store-entry") {
		t.Fatalf("mapping misses the direct slot:\n%s", text)
	}
	directAt := strings.Index(text, "direct: proven client_id=")
	proxyAt := strings.Index(text, "proxy:"+fixture.proxySettings[0].Address)
	if directAt < 0 || proxyAt < 0 {
		t.Fatalf("direct or proxy outcome missing (direct=%d proxy=%d):\n%s", directAt, proxyAt, text)
	}
	if proxyAt < directAt {
		t.Fatalf("the direct identity result is not first:\n%s", text)
	}
	if !strings.Contains(text, "summary: imported=3 fallback=0 skipped=0 left=0 elapsed=") {
		t.Fatalf("summary misses the direct slot:\n%s", text)
	}
	// The conveyed order is a property, not an accident: the direct write
	// happens before the proxies, exactly as its proof did.
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); err != nil {
		t.Fatalf("direct credential: %v", err)
	}
}

// --- bounded proof phase -----------------------------------------------------

// hangingLegacyImportProver ignores its context on purpose: the proof phase
// must stay bounded by its own deadlines and budget, never by the prover.
type hangingLegacyImportProver struct {
	release chan struct{}
	once    sync.Once
}

func newHangingLegacyImportProver() *hangingLegacyImportProver {
	return &hangingLegacyImportProver{release: make(chan struct{})}
}

func (self *hangingLegacyImportProver) unblock() {
	self.once.Do(func() { close(self.release) })
}

func (self *hangingLegacyImportProver) ServerPublicKey(context.Context, connect.Id) ([]byte, error) {
	<-self.release
	return nil, errors.New("synthetic: hanging server key read")
}

func (self *hangingLegacyImportProver) RefreshAccepted(context.Context, string) error {
	<-self.release
	return errors.New("synthetic: hanging refresh")
}

// A prover that never answers must not keep the import silent: the per-proof
// deadline bounds each call, the budget ends the phase loudly, and the summary
// names the bound and what was left.
func TestLegacyImportHangingProverIsBoundedAndLoud(t *testing.T) {
	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()

	buffer := captureLegacyImportLog(t)
	fixture := newLegacyImportFixture(t, true, 4)
	hang := newHangingLegacyImportProver()
	defer hang.unblock()
	start := time.Now()
	result := runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       fixture.dir,
		networkJwtPath: filepath.Join(fixture.dir, "jwt"),
		providers:      fixture.proxySettings,
		recorder:       fixture.recorder,
		newProver: func(legacyImportProofDialWait) (legacyImportProver, func(), error) {
			return hang, func() {}, nil
		},
		proverBuildTimeout:      2 * time.Second,
		proofTimeout:            150 * time.Millisecond,
		proofConcurrency:        2,
		heartbeatInterval:       50 * time.Millisecond,
		unreachableShortCircuit: 1000,
		budget:                  700 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Fatalf("a hanging prover held the import for %s", elapsed)
	}
	if len(result.mintSlots) != 0 {
		t.Fatalf("a hanging prover must not mint, got %v", result.mintSlots)
	}
	text := buffer.String()
	for _, want := range []string{
		"detection:",
		"mapping:",
		"heartbeat:",
		"budget: reached after",
		"left for the next start",
		"summary:",
		"result=budget",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("narration misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "proven client_id=") {
		t.Fatalf("a hanging prover proved a slot:\n%s", text)
	}
	for _, settings := range fixture.proxySettings {
		if _, err := os.Stat(fixture.target(settings)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a hanging prover wrote a credential for %s", settings.Key())
		}
	}
	// Every candidate is deferred bookkeeping for the next start, none is a
	// permanent decision.
	journal := fixture.journal()
	for _, slot := range journal.Slots {
		if slot.State != "defer" {
			t.Fatalf("a hanging prover journaled %+v, want defer", slot)
		}
	}
}

// An API that cannot be reached stops the proof phase after a bounded run of
// consecutive unreachable reads instead of hammering every slot.
func TestLegacyImportUnreachableApiStopsHammering(t *testing.T) {
	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()

	buffer := captureLegacyImportLog(t)
	fixture := newLegacyImportFixture(t, true, 40)
	unreachable := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	fixture.prover.publicErr = unreachable
	fixture.prover.transientRemaining = 1 << 20
	fixture.prover.transientCause = unreachable
	start := time.Now()
	result := runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       fixture.dir,
		networkJwtPath: filepath.Join(fixture.dir, "jwt"),
		providers:      fixture.proxySettings,
		recorder:       fixture.recorder,
		newProver: func(legacyImportProofDialWait) (legacyImportProver, func(), error) {
			return fixture.prover, func() {}, nil
		},
		proofTimeout:            time.Second,
		proofConcurrency:        16,
		heartbeatInterval:       50 * time.Millisecond,
		unreachableShortCircuit: 16,
		budget:                  30 * time.Second,
	})
	elapsed := time.Since(start)
	if elapsed > 30*time.Second {
		t.Fatalf("an unreachable API held the import for %s", elapsed)
	}
	if len(result.mintSlots) != 0 {
		t.Fatalf("an unreachable API must not mint, got %v", result.mintSlots)
	}
	text := buffer.String()
	for _, want := range []string{
		"api unreachable: 16 consecutive proof reads could not reach the API; stopping the proof phase",
		"left for the next start (api_unreachable)",
		"summary:",
		"result=api_unreachable",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("narration misses %q:\n%s", want, text)
		}
	}
	// The overwhelming majority of the 40 slots was never attempted: the
	// short circuit stops the hammering after one concurrency window.
	if strings.Count(text, "deferred reason=refresh_unavailable") > 32 {
		t.Fatalf("the short circuit attempted too many slots:\n%s", text)
	}
}

// A 1000-slot stub import completes in bounded time, with heartbeats and a
// final summary; the direct identity's result is narrated before any proxy's.
func TestLegacyImportThousandSlotsBounded(t *testing.T) {
	buffer := captureLegacyImportLog(t)
	fixture := newLegacyImportFixture(t, false, 1000)
	providers := append([]*connect.ProxySettings{nil}, fixture.proxySettings...)
	start := time.Now()
	result := runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       fixture.dir,
		networkJwtPath: filepath.Join(fixture.dir, "jwt"),
		providers:      providers,
		recorder:       fixture.recorder,
		newProver: func(legacyImportProofDialWait) (legacyImportProver, func(), error) {
			return fixture.prover, func() {}, nil
		},
		proverBuildTimeout: 5 * time.Second,
		proofTimeout:       time.Second,
		proofConcurrency:   16,
		heartbeatInterval:  10 * time.Millisecond,
		budget:             60 * time.Second,
	})
	elapsed := time.Since(start)
	if len(result.mintSlots) != 0 {
		t.Fatalf("the stub import must not mint, got %d slots", len(result.mintSlots))
	}
	if elapsed > 300*time.Second {
		t.Fatalf("1000-slot stub import took %s", elapsed)
	}
	text := buffer.String()
	if !strings.Contains(text, "mapping: slots=1001 configured, 1001 mapped, 0 already-custodied, 0 no-store-entry") {
		t.Fatalf("mapping misses the 1001 slots:\n%s", firstLines(text, 12))
	}
	if !strings.Contains(text, "heartbeat:") {
		t.Fatalf("no heartbeat in the 1001-slot import:\n%s", firstLines(text, 12))
	}
	if !strings.Contains(text, "summary: imported=1001 fallback=0 skipped=0 left=0 elapsed=") ||
		!strings.Contains(text, "result=complete") {
		tail := text
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		t.Fatalf("summary misses the 1001 slots:\n%s", tail)
	}
	directAt := strings.Index(text, "direct: proven client_id=")
	proxyAt := strings.Index(text, "proxy:127.0.0.1:18471")
	if directAt < 0 || proxyAt < 0 || proxyAt < directAt {
		t.Fatalf("the direct identity is not narrated first (direct=%d proxy=%d)", directAt, proxyAt)
	}
	// Every slot has its credential file: the direct one and one per proxy.
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); err != nil {
		t.Fatalf("direct credential: %v", err)
	}
	credentialCount := 0
	entries, err := os.ReadDir(fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".provider-") && strings.HasSuffix(entry.Name(), ".jwt") {
			credentialCount++
		}
	}
	if credentialCount != len(fixture.proxySettings) {
		t.Fatalf("proxy credentials = %d, want %d", credentialCount, len(fixture.proxySettings))
	}
	// The heartbeat cadence was live during the run: at least the opening
	// heartbeat was emitted before the first slot resolved.
	if !strings.Contains(text, "heartbeat: proven=0") {
		t.Fatalf("the opening heartbeat is missing:\n%s", firstLines(text, 12))
	}
}

// firstLines trims a narration dump for failure messages.
func firstLines(text string, count int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}
