package urnettools

// Behavioural coverage for the OpenRC restart-failure rollback (M6). The
// installer swaps the provider binary, then starts the service. If that start
// fails, install_openrc_units must roll the swap back AND start the restored
// binary, otherwise the box is left with the new binary that did not start.
//
// The real install_openrc_units and openrc_rollback_binary_swap run here. Only
// the system edges are stubbed: the user and home lookups, the root-path
// finalise step, and rc-service / rc-update on PATH.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRCRestartFailureRollsBackAndRestartsRestored(t *testing.T) {
	skipWithoutPOSIXShell(t)
	source := readInstaller(t)
	fnInstall := extractShellFunc(t, source, "install_openrc_units")
	fnRollback := extractShellFunc(t, source, "openrc_rollback_binary_swap")
	if fnInstall == "" || fnRollback == "" {
		t.Fatal("could not extract install_openrc_units or openrc_rollback_binary_swap from the Linux installer")
	}

	root := t.TempDir()
	installRoot := filepath.Join(root, "lib")
	installPath := filepath.Join(installRoot, "urnetwork-provider")
	binDir := filepath.Join(installPath, "bin")
	home := filepath.Join(root, "home")
	initd := filepath.Join(root, "initd", "urnetwork")
	live := filepath.Join(binDir, "urnetwork")
	for _, dir := range []string{binDir, home, filepath.Dir(initd)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The state a swap leaves behind: the new binary is live and the previous
	// one is parked in .old, ready for the rollback.
	if err := os.WriteFile(live, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live+".old", []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}

	// rc-service fails only on the first start, the one on the new binary.
	// Every call records the binary that was live at that moment.
	stubDir := t.TempDir()
	record := filepath.Join(stubDir, "rc-service.calls")
	flag := filepath.Join(stubDir, "first-start-done")
	rcService := "#!/bin/sh\n" +
		"printf '%s live=%s\\n' \"$*\" \"$(cat '" + live + "' 2>/dev/null)\" >> '" + record + "'\n" +
		"if [ \"$1\" = urnetwork ] && [ \"$2\" = start ] && [ ! -e '" + flag + "' ]; then\n" +
		"    : > '" + flag + "'\n" +
		"    exit 1\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "rc-service"), []byte(rcService), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "rc-update"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runShellScriptWithPath(t, stubDir, `
install_path="`+installPath+`"
openrc_install_root="`+installRoot+`"
openrc_user="urnet"
openrc_initd_file="`+initd+`"
openrc_swap_pending=1
openrc_units_stopped=1
id() { if [ "$1" = -u ]; then echo 0; fi; return 0; }
openrc_user_home() { echo "`+home+`"; }
openrc_resolve_parent() { if [ "$1" = --full ]; then shift; fi; echo "$1"; }
openrc_finalize_root_paths() { return 0; }
openrc_restore_update_entry() { :; }
openrc_service_running() { return 1; }
pr_info() { printf 'INFO: %s\n' "$*"; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
`+fnRollback+`
`+fnInstall+`
install_openrc_units
`)
	if err == nil {
		t.Fatalf("a failed service start must fail the install (exit 0 hides it); out:\n%s", out)
	}
	if got, rerr := os.ReadFile(live); rerr != nil || string(got) != "PREVIOUS" {
		t.Errorf("rollback did not restore the previous binary: got %q (%v); out:\n%s", string(got), rerr, out)
	}
	if _, serr := os.Stat(live + ".old"); serr == nil {
		t.Error("the .old file must be consumed by the rollback")
	}
	if !strings.Contains(out, "Restored the previous provider binary") {
		t.Errorf("expected the operator to be told the previous binary was restored:\n%s", out)
	}

	calls, rerr := os.ReadFile(record)
	if rerr != nil {
		t.Fatalf("rc-service was never called: %v", rerr)
	}
	want := "urnetwork start live=NEW\nurnetwork start live=PREVIOUS\n"
	if string(calls) != want {
		t.Errorf("rc-service calls:\n%s\nwant the restored binary started after the failed start:\n%s\n(out:\n%s)", calls, want, out)
	}
	if !strings.Contains(out, "Restarted the service on the previous binary") {
		t.Errorf("expected the operator to be told the service was restarted on the previous binary:\n%s", out)
	}
}
