//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRecordHotswapDeclineRefusesSymlinkedTemp: the CLI writes the counters
// as root into a directory the provider user controls. A symlink planted at
// the temp path must not redirect that write.
func TestRecordHotswapDeclineRefusesSymlinkedTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, hotswapCountsFile+".tmp")); err != nil {
		t.Fatal(err)
	}

	recordHotswapDecline(dir, "version_old")

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "untouched" {
		t.Fatalf("counter write followed the symlink: target now %q", got)
	}
}
