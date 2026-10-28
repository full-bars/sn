//go:build linux

package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTimerCalendarCreatesUpdaterService(t *testing.T) {
	unitDir := t.TempDir()
	binDir := t.TempDir()
	systemctl := filepath.Join(binDir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const timer = "urnetwork-native-update.timer"
	p := Provider{Unit: "urnetwork-native.service"}
	if err := writeTimerCalendarAt(filepath.Join(unitDir, timer), timer, p, "daily"); err != nil {
		t.Fatal(err)
	}

	timerText, err := os.ReadFile(filepath.Join(unitDir, timer))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(timerText), "Unit=urnetwork-native-update.service") {
		t.Errorf("timer target = %q, want urnetwork-native-update.service", timerText)
	}

	servicePath := filepath.Join(unitDir, "urnetwork-native-update.service")
	serviceText, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("read updater service %s: %v", servicePath, err)
	}
	if !strings.Contains(string(serviceText), " update -y --unit ") || !strings.Contains(string(serviceText), "urnetwork-native.service") {
		t.Errorf("updater service does not update the provider unit: %q", serviceText)
	}
}
