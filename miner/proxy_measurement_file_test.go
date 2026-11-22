package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMeasurementFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := measurementFileEntry{Key: strings.Repeat("a", 64), Status: proxyUDPOK, At: at, TCPExit: "192.0.2.1", UDPExit: "192.0.2.1"}
	if err := saveMeasurementFile(path, []measurementFileEntry{entry}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"entries":[{"key":"` + strings.Repeat("a", 64) + `","status":1,"at":"2026-01-02T03:04:05Z","tcp_exit":"192.0.2.1","udp_exit":"192.0.2.1"}]}`
	if string(b) != want {
		t.Fatalf("bytes = %s", b)
	}
	got, err := loadMeasurementFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != entry {
		t.Fatalf("restored = %#v", got)
	}
}

func TestMeasurementFileRejectsWholeInvalidSnapshot(t *testing.T) {
	cases := []string{
		`{"version":2,"entries":[]}`,
		`{"version":1,"entries":[`,
		`{"version":1,"entries":[{"key":"invalid","status":1,"at":"2026-01-02T03:04:05Z"}]}`,
		strings.Repeat(" ", measurementFileMaxBytes+1),
	}
	for _, contents := range cases {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := loadMeasurementFile(path)
		if err == nil || got != nil {
			t.Fatalf("invalid snapshot accepted: %d bytes", len(contents))
		}
	}
}
