//go:build linux

package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openrcInstallService is the Go side of install_openrc_units: write
// /etc/init.d/<service> and add it to the default runlevel. The installer does
// not START the service (authentication has to happen first), so neither does
// this, and the caller is told the next command.
func TestOpenRCInstallServiceWritesScriptAndEnables(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "urnetwork")
	var calls [][]string
	oldPath, oldRun := openrcInitScriptPath, openrcRunFn
	t.Cleanup(func() {
		openrcInitScriptPath = oldPath
		openrcRunFn = oldRun
	})
	openrcInitScriptPath = scriptPath
	openrcRunFn = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}

	binary := "/usr/local/lib/urnetwork-provider/bin/urnetwork"
	if err := openrcInstallService(binary, "urnet", "/home/urnet", false); err != nil {
		t.Fatalf("openrcInstallService: %v", err)
	}

	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("init script was not written: %v", err)
	}
	script := string(data)
	for _, want := range []string{
		"#!/sbin/openrc-run",
		`command="` + binary + `"`,
		`command_user="urnet"`,
		`supervisor="supervise-daemon"`,
		"export URNETWORK_INIT=openrc",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("installed init script lacks %q", want)
		}
	}
	fi, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	// OpenRC only runs an executable init script; 0644 is ignored with no
	// diagnostic beyond "service not found".
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed init script mode %v is not executable", fi.Mode().Perm())
	}
	if len(calls) != 1 {
		t.Fatalf("ran %d commands, want 1: %v", len(calls), calls)
	}
	if got := strings.Join(calls[0], " "); !strings.HasSuffix(got, "rc-update add urnetwork default") {
		t.Errorf("enable command = %q, want rc-update add urnetwork default", got)
	}
}

func TestOpenRCInstallServiceRefusesRootUser(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "urnetwork")
	oldPath, oldRun := openrcInitScriptPath, openrcRunFn
	t.Cleanup(func() {
		openrcInitScriptPath = oldPath
		openrcRunFn = oldRun
	})
	openrcInitScriptPath = scriptPath
	openrcRunFn = func(args ...string) error {
		t.Fatalf("a refused install ran %v", args)
		return nil
	}
	for _, user := range []string{"root", ""} {
		err := openrcInstallService("/usr/bin/urnetwork", user, "/root", false)
		if err == nil {
			t.Fatalf("openrcInstallService accepted service user %q", user)
		}
		if !strings.Contains(err.Error(), "root") {
			t.Errorf("error for user %q does not explain the refusal: %v", user, err)
		}
		if _, statErr := os.Stat(scriptPath); !os.IsNotExist(statErr) {
			t.Errorf("a refused install still wrote the init script (user %q)", user)
		}
	}
}

func TestOpenRCInstallServiceDryRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "urnetwork")
	oldPath, oldRun := openrcInitScriptPath, openrcRunFn
	t.Cleanup(func() {
		openrcInitScriptPath = oldPath
		openrcRunFn = oldRun
	})
	openrcInitScriptPath = scriptPath
	openrcRunFn = func(args ...string) error {
		t.Fatalf("dry-run ran %v", args)
		return nil
	}
	if err := openrcInstallService("/usr/bin/urnetwork", "urnet", "/home/urnet", true); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, statErr := os.Stat(scriptPath); !os.IsNotExist(statErr) {
		t.Error("dry-run wrote the init script")
	}
}
