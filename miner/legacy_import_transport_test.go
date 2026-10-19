package miner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingProofDialer answers family dials with scripted failures and records
// every attempt.
type recordingProofDialer struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error
}

func (self *recordingProofDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	self.mu.Lock()
	self.calls = append(self.calls, network+" "+addr)
	err := self.fail[network]
	self.mu.Unlock()
	if err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	// The proof reads no bytes from this synthetic connection; close the
	// server end so the client end is a clean EOF rather than a leak.
	_ = server.Close()
	return client, nil
}

func (self *recordingProofDialer) callList() []string {
	self.mu.Lock()
	defer self.mu.Unlock()
	return append([]string(nil), self.calls...)
}

// dialerWaitRecorder collects the family wait lines the dialer emits.
func dialerWaitRecorder(lines *[]string) legacyImportProofDialWait {
	return func(family string, attempt int, timeout time.Duration) {
		*lines = append(*lines, fmt.Sprintf("%s#%d", family, attempt))
	}
}

// The proof transport prefers IPv4 and falls back to the other family when the
// first dial fails: the reference host shape (dual stack, broken IPv6 route)
// must never sit in a multi-minute IPv6 demotion.
func TestLegacyImportProofDialerPrefersWorkingFamily(t *testing.T) {
	dial := &recordingProofDialer{fail: map[string]error{"tcp4": errors.New("synthetic: no route")}}
	var waits []string
	dialer := &legacyImportProofDialer{
		dial:         dial.DialContext,
		family4:      true,
		family6:      true,
		firstTimeout: time.Second,
		wait:         dialerWaitRecorder(&waits),
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "api.example:443")
	if err != nil {
		t.Fatalf("fallback dial: %v", err)
	}
	_ = conn.Close()
	calls := dial.callList()
	if len(calls) != 2 || calls[0] != "tcp4 api.example:443" || calls[1] != "tcp6 api.example:443" {
		t.Fatalf("family order = %v, want tcp4 then tcp6", calls)
	}
	want := []string{"v4#1", "v6#2"}
	if len(waits) != len(want) || waits[0] != want[0] || waits[1] != want[1] {
		t.Fatalf("wait lines = %v, want %v", waits, want)
	}
}

// A narrowed network (connect's force or a learned demotion) is honored.
func TestLegacyImportProofDialerHonorsNarrowedNetwork(t *testing.T) {
	dial := &recordingProofDialer{fail: map[string]error{}}
	var waits []string
	dialer := &legacyImportProofDialer{
		dial:         dial.DialContext,
		family4:      true,
		family6:      true,
		firstTimeout: time.Second,
		wait:         dialerWaitRecorder(&waits),
	}
	conn, err := dialer.DialContext(context.Background(), "tcp6", "api.example:443")
	if err != nil {
		t.Fatalf("narrowed dial: %v", err)
	}
	_ = conn.Close()
	if calls := dial.callList(); len(calls) != 1 || calls[0] != "tcp6 api.example:443" {
		t.Fatalf("narrowed dial calls = %v, want one tcp6 attempt", calls)
	}
	if len(waits) != 1 || waits[0] != "v6#1" {
		t.Fatalf("wait lines = %v, want [v6#1]", waits)
	}
}

// With no family evidence the dialer makes one family-agnostic attempt: no
// preference may be invented.
func TestLegacyImportProofDialerWithoutFamilyEvidence(t *testing.T) {
	dial := &recordingProofDialer{fail: map[string]error{}}
	dialer := &legacyImportProofDialer{
		dial:         dial.DialContext,
		family4:      false,
		family6:      false,
		firstTimeout: time.Second,
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "api.example:443")
	if err != nil {
		t.Fatalf("agnostic dial: %v", err)
	}
	_ = conn.Close()
	if calls := dial.callList(); len(calls) != 1 || calls[0] != "tcp api.example:443" {
		t.Fatalf("agnostic dial calls = %v, want one plain attempt", calls)
	}
}

// The per-attempt timeout never outlives the caller's deadline.
func TestLegacyImportProofDialerAttemptTimeoutIsCapped(t *testing.T) {
	dialer := &legacyImportProofDialer{firstTimeout: 4 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if got := dialer.attemptTimeout(ctx, 0); got > 250*time.Millisecond {
		t.Fatalf("second attempt timeout = %s, want <= the 250ms deadline", got)
	}
	if got := dialer.attemptTimeout(context.Background(), 4*time.Second); got != 4*time.Second {
		t.Fatalf("uncapped timeout = %s, want 4s", got)
	}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if got := dialer.attemptTimeout(expired, 4*time.Second); got != time.Millisecond {
		t.Fatalf("expired deadline timeout = %s, want 1ms", got)
	}
}

// A constructor that hangs must not hide the import: the build is bounded and
// the narration names both the wait and the timeout.
func TestLegacyImportProverConstructionTimeoutIsBounded(t *testing.T) {
	buffer := captureLegacyImportLog(t)
	fixture := newLegacyImportFixture(t, true, 1)
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	result := runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       fixture.dir,
		networkJwtPath: filepath.Join(fixture.dir, "jwt"),
		providers:      fixture.proxySettings,
		recorder:       fixture.recorder,
		newProver: func(legacyImportProofDialWait) (legacyImportProver, func(), error) {
			<-block
			return fixture.prover, func() {}, nil
		},
		proverBuildTimeout: 150 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("a hanging constructor held the import for %s", elapsed)
	}
	if len(result.mintSlots) != 0 {
		t.Fatalf("a failed construction must not mint, got %v", result.mintSlots)
	}
	text := buffer.String()
	for _, want := range []string{
		"waiting for prover construction (timeout=150ms)",
		"prover construction timed out after 150ms; the proof cannot run this start",
		"skipped: proof_unavailable:",
		"summary: imported=0 fallback=0 skipped=1 left=0 elapsed=",
		"result=proof_unavailable",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("narration misses %q:\n%s", want, text)
		}
	}
	if _, err := os.Stat(fixture.target(fixture.proxySettings[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed construction wrote a credential")
	}
}
