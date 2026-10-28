//go:build linux

package urnettools

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// stubSystemctl puts a no-op systemctl first on PATH.
func stubSystemctl(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A fresh account has no ~/.config/systemd/user yet; the updater service and
// the timer must both land anyway.
func TestWriteTimerCalendarCreatesMissingUnitDirectory(t *testing.T) {
	unitDir := filepath.Join(t.TempDir(), ".config/systemd/user")
	stubSystemctl(t)

	const timer = "urnetwork-update.timer"
	p := Provider{Unit: "urnetwork.service"}
	if err := writeTimerCalendarAt(filepath.Join(unitDir, timer), timer, p, "daily"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{timer, "urnetwork-update.service"} {
		if _, err := os.Stat(filepath.Join(unitDir, name)); err != nil {
			t.Errorf("%s not created: %v", name, err)
		}
	}
}

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

// A timer written before the updater service existed names the provider unit,
// so the node restarts the provider on schedule instead of updating it.
func TestWriteTimerCalendarMigratesOldTimer(t *testing.T) {
	unitDir := t.TempDir()
	stubSystemctl(t)

	const timer = "urnetwork-update.timer"
	timerPath := filepath.Join(unitDir, timer)
	old := "[Unit]\nDescription=Run URnetwork Update\n\n[Timer]\nOnCalendar=daily\nUnit=urnetwork.service\n"
	if err := os.WriteFile(timerPath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Provider{Unit: "urnetwork.service"}
	if err := writeTimerCalendarAt(timerPath, timer, p, "weekly"); err != nil {
		t.Fatal(err)
	}

	timerText, _ := os.ReadFile(timerPath)
	if !strings.Contains(string(timerText), "Unit=urnetwork-update.service") || strings.Contains(string(timerText), "Unit=urnetwork.service") {
		t.Errorf("timer still targets the provider: %q", timerText)
	}
	if !strings.Contains(string(timerText), "OnCalendar=weekly") {
		t.Errorf("calendar not rewritten: %q", timerText)
	}
	serviceText, err := os.ReadFile(filepath.Join(unitDir, "urnetwork-update.service"))
	if err != nil {
		t.Fatalf("updater service not created: %v", err)
	}
	if !strings.Contains(string(serviceText), " update -y --unit ") {
		t.Errorf("updater service does not update: %q", serviceText)
	}
}

func TestEnsureUpdaterServiceRepairsWrongExecStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urnetwork-update.service")
	wrong := "[Service]\nExecStart=/bin/true\n"
	if err := os.WriteFile(path, []byte(wrong), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureUpdaterService(path, "urnetwork-update.service", Provider{Unit: "urnetwork.service"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), " update -y --unit ") {
		t.Errorf("service not rewritten: %q", got)
	}

	// The shell installer's own updater is left alone.
	shell := "[Service]\nExecStart=/home/u/.local/share/urnetwork-provider/bin/urnet-tools update -y\n"
	if err := os.WriteFile(path, []byte(shell), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureUpdaterService(path, "urnetwork-update.service", Provider{Unit: "urnetwork.service"}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != shell {
		t.Errorf("a working updater was rewritten: %q", got)
	}
}

func TestExecutableByChecksFileAndDirectories(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "urnet-tools")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !executableBy(account, tool) {
		t.Error("owner's executable file reported not runnable")
	}
	if err := os.Chmod(tool, 0o644); err != nil {
		t.Fatal(err)
	}
	if executableBy(account, tool) {
		t.Error("a file without an execute bit reported runnable")
	}
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if executableBy(account, tool) {
		t.Error("a file in a directory the user cannot search reported runnable")
	}
}
