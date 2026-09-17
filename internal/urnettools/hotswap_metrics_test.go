package urnettools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHotswapDeclineReasonMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect string
	}{
		{"nil error", nil, ""},
		{"version_old", ErrHotSwapNotSupported, "version_old"},
		{"unit_not_notify", ErrHotSwapUnitNotNotify, "unit_not_notify"},
		{"unit_query_error", errors.New("query systemd unit type for foo.service: exit status 1"), "unit_query_error"},
		{"windows_stub", errors.New("hotswap not yet supported on Windows"), "windows"},
		{"trigger_pid", errors.New("provider has no valid PID"), "trigger_pid"},
		{"trigger_find", errors.New("find process 1234: no such process"), "trigger_find"},
		{"trigger_signal", errors.New("send SIGUSR2 to PID 1234: operation not permitted"), "trigger_signal"},
		{"unknown", errors.New("something completely unexpected"), "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hotswapDeclineReason(tt.err)
			if got != tt.expect {
				t.Errorf("hotswapDeclineReason(%v) = %q, want %q", tt.err, got, tt.expect)
			}
		})
	}
}

func TestRecordHotswapDecline(t *testing.T) {
	dir := t.TempDir()

	// Record several declines.
	recordHotswapDecline(dir, "version_old")
	recordHotswapDecline(dir, "version_old")
	recordHotswapDecline(dir, "unit_not_notify")

	counts := readHotswapDeclines(dir)
	if counts == nil {
		t.Fatal("readHotswapDeclines returned nil")
	}
	if counts["version_old"] != 2 {
		t.Errorf("version_old = %d, want 2", counts["version_old"])
	}
	if counts["unit_not_notify"] != 1 {
		t.Errorf("unit_not_notify = %d, want 1", counts["unit_not_notify"])
	}
}

func TestRecordHotswapSuccess(t *testing.T) {
	dir := t.TempDir()

	recordHotswapSuccess(dir)
	recordHotswapSuccess(dir)
	recordHotswapDecline(dir, "version_old")

	counts := readHotswapDeclines(dir)
	if counts == nil {
		t.Fatal("readHotswapDeclines returned nil")
	}
	if counts["success"] != 2 {
		t.Errorf("success = %d, want 2", counts["success"])
	}
	if counts["version_old"] != 1 {
		t.Errorf("version_old = %d, want 1", counts["version_old"])
	}
}

func TestReadHotswapDeclinesEmpty(t *testing.T) {
	// Non-existent file should return nil.
	counts := readHotswapDeclines(t.TempDir())
	if counts != nil {
		t.Errorf("expected nil, got %v", counts)
	}
}

func TestReadHotswapDeclinesCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".hotswap_declines.json")
	os.WriteFile(path, []byte("not json"), 0600)

	counts := readHotswapDeclines(dir)
	if counts != nil {
		t.Errorf("expected nil for corrupt file, got %v", counts)
	}
}

func TestReadHotswapDeclinesEmptyStateDir(t *testing.T) {
	counts := readHotswapDeclines("")
	if counts != nil {
		t.Errorf("expected nil for empty state dir, got %v", counts)
	}
}

func TestHotswapDeclineCountsJSONRoundTrip(t *testing.T) {
	// Verify the on-disk format is stable JSON.
	dir := t.TempDir()
	recordHotswapDecline(dir, "version_old")
	recordHotswapDecline(dir, "unit_not_notify")

	data, err := os.ReadFile(filepath.Join(dir, ".hotswap_declines.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dc hotswapDeclineCounts
	if err := json.Unmarshal(data, &dc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dc.Counts["version_old"] != 1 || dc.Counts["unit_not_notify"] != 1 {
		t.Errorf("unexpected counts: %v", dc.Counts)
	}
}
