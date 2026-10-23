package provider

import (
	"os"
	"path/filepath"
	"testing"
)

// withTrimHome isolates the operator trim file and the OOM cap state so a
// developer's own box cannot influence the result.
func withTrimHome(t *testing.T, trimFile string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if trimFile != "" {
		if err := os.WriteFile(filepath.Join(dir, "proxy_trim"), []byte(trimFile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoteTrimSelectionRecordsDesiredAndCap(t *testing.T) {
	withTrimHome(t, "500\n")
	noteTrimSelection(1200)

	desired, cap, source, ok := currentTrimSelection()
	if !ok {
		t.Fatal("selection not recorded")
	}
	if desired != 1200 || cap != 500 || source != trimCapOperator {
		t.Fatalf("got desired=%d cap=%d source=%q, want 1200/500/%q", desired, cap, source, trimCapOperator)
	}
	// The systemd status line must keep reporting the trimmed count, not the
	// full desired pool.
	if got := configuredProxyCount(); got != 500 {
		t.Fatalf("configured proxy count = %d, want the trimmed 500", got)
	}
}

func TestNoteTrimSelectionClampsCapToDesired(t *testing.T) {
	withTrimHome(t, "5000\n")
	noteTrimSelection(300)

	if _, cap, _, _ := currentTrimSelection(); cap != 300 {
		t.Fatalf("cap = %d, want it clamped to the 300 configured", cap)
	}
	if got := configuredProxyCount(); got != 300 {
		t.Fatalf("configured proxy count = %d, want 300", got)
	}
}

func TestNoteTrimSelectionNoCap(t *testing.T) {
	withTrimHome(t, "")
	noteTrimSelection(700)

	desired, cap, source, ok := currentTrimSelection()
	if !ok {
		t.Fatal("selection not recorded")
	}
	if desired != 700 || cap != 0 || source != "" {
		t.Fatalf("got desired=%d cap=%d source=%q, want 700/0/empty", desired, cap, source)
	}
}
