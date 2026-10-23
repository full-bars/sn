//go:build linux

package miner

import (
	"os"
	"path/filepath"
	"testing"
)

// readPSI takes a RESOURCE under /proc/pressure ("memory", "cpu", "io"); "some"
// is a line inside that file, not a resource. Passing the line name opened a
// file that never exists, so the reading was silently omitted on every node.
// This pins the path end to end against a fixture rather than asserting that
// the reader merely did not error.
func TestReadPSIFillsTheSomeLineFromTheMemoryResource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory"), []byte(
		"some avg10=0.50 avg60=2.50 avg300=1.00 total=1234\nfull avg10=0.10 avg60=0.20 avg300=0.30 total=5678\n",
	), 0600); err != nil {
		t.Fatal(err)
	}
	prev := psiResourcePath
	psiResourcePath = func(resource string) string { return filepath.Join(dir, resource) }
	t.Cleanup(func() { psiResourcePath = prev })

	some, err := readPSI("memory")
	if err != nil {
		t.Fatalf("readPSI(memory): %v", err)
	}
	if some != 2.50 {
		t.Fatalf("some avg60 = %v, want 2.50", some)
	}
	// The host block must carry it: this is the path that was silently empty.
	host, _ := readBaselineHost()
	if host.PSISomeAvg60 == nil || *host.PSISomeAvg60 != 2.50 {
		t.Fatalf("host PSI some = %v, want 2.50 from the memory fixture", host.PSISomeAvg60)
	}
}
