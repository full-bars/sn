package miner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provide lifecycle helpers feed events.log the same three records the
// v33 provider writes from provide.go (STARTUP, SIGNAL, PROVIDER EXIT). The
// run() wiring calls them; these pin the record contents so the wiring cannot
// drift silently.

func TestRunCancelSourceKeepsTheFirstCanceller(t *testing.T) {
	src := &runCancelSource{}
	src.record()
	first := src.text()
	if first == "" || strings.Contains(first, "context cancelled by parent") {
		t.Fatalf("the first record must keep its stack, got %.60q", first)
	}

	// A second canceller leaves its own stack, but must not replace the
	// first one: the SIGNAL line reports who cancelled first (v33 keeps the
	// same first-writer-wins record).
	done := make(chan struct{})
	go func() {
		defer close(done)
		src.record()
	}()
	<-done

	if got := src.text(); got != first {
		t.Fatalf("a later canceller replaced the first stack:\nfirst %.60q\nafter %.60q", first, got)
	}
}

// A run whose parent context went down first (the startup signal handler
// cancels the parent, not this run's wrapper) has no stack to report, so the
// SIGNAL line carries the v33 parent-cancellation text instead.
func TestRunCancelSourceFallsBackToParentText(t *testing.T) {
	src := &runCancelSource{}
	if got := src.text(); !strings.Contains(got, "cancelled by parent") {
		t.Fatalf("an uncancelled source must report the parent fallback, got %q", got)
	}
}

func TestProviderStartupCritLogWritesTheStartupLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	providerStartupCritLog()

	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	line := string(b)
	for _, want := range []string{
		"STARTUP: version=",
		fmt.Sprintf("pid=%d", os.Getpid()),
		"host=",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("events.log line %.120q is missing %q", line, want)
		}
	}
}
