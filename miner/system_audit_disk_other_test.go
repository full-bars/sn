//go:build !linux

package miner

import (
	"os"
	"path/filepath"
	"testing"
)

// Outside Linux the audit has no statfs/write pair to trust, so its contract is
// to report nothing and say so. A zero dressed as a reading (0 MB/s, slow) is
// worse than silence: it would page someone about a disk this platform cannot
// measure, and the seam would publish urnet_disk_* families that never came
// from a benchmark.
func TestDiskAuditStubReportsSkippedNotAValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)

	if free, low := CheckDiskSpace(dir); free != 0 || low {
		t.Fatalf("CheckDiskSpace = (%d, %v), want (0, false): unknown is not low", free, low)
	}
	if slow, low := RunDiskAudit(dir); slow || low {
		t.Fatalf("RunDiskAudit = (%v, %v), want (false, false)", slow, low)
	}
	if _, _, _, ok := DiskBenchStatus(dir); ok {
		t.Fatal("DiskBenchStatus reported ok on a platform with no benchmark")
	}
	if reason := DiskBenchSkipReason(); reason == "" {
		t.Fatal("DiskBenchSkipReason is empty: the stub must say why it reports nothing")
	}
	if d := diskBenchSeamReading(); d != nil {
		t.Fatalf("seam reported %+v with no benchmark recorded", d)
	}

	// A result recorded by another run must not turn into a reading here
	// either: the stub cannot judge it, so it stays silent.
	if err := os.WriteFile(filepath.Join(dir, "disk-bench.json"), []byte(`{"at":"2026-01-01T00:00:00Z","device":2049,"result":{"mbps":12.5,"bytes":4194304,"slow":false}}`), 0600); err != nil {
		t.Fatalf("write cache fixture: %v", err)
	}
	if d := diskBenchSeamReading(); d != nil {
		t.Fatalf("seam reported %+v from a recorded result the stub cannot read", d)
	}
}
