package urnettools

// Execution coverage for the OpenRC binary-swap rollback. The installer moves
// the live provider binary aside to dodge ETXTBSY, so a failure between that
// move and the rename-back used to leave the tree with NO binary while the
// service had already been stopped - and left the periodic auto-update entry
// paused with nothing saying so.
//
// These cases execute the REAL rollback function against a real directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rcServiceStub returns a stub dir holding an rc-service that records each
// call in marker and exits 0. A shell function cannot stand in for
// `rc-service`: the hyphen is legal in a bash function name but a syntax
// error in dash (Ubuntu's /bin/sh), so the stub is a PATH entry like the
// real command.
func rcServiceStub(t *testing.T) (stubDir, marker string) {
	t.Helper()
	stubDir = t.TempDir()
	marker = filepath.Join(t.TempDir(), "rc-service.called")
	body := "#!/bin/sh\nprintf 'RC_SERVICE_CALLED\\n' >> '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "rc-service"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return stubDir, marker
}

// TestOpenRCRollbackRestoresPreviousBinary pins the core behaviour: after a
// failed replacement, the previous binary is back in place.
func TestOpenRCRollbackRestoresPreviousBinary(t *testing.T) {
	skipWithoutPOSIXShell(t)
	fn := extractShellFunc(t, readInstaller(t), "openrc_rollback_binary_swap")
	if fn == "" {
		t.Fatal("could not extract openrc_rollback_binary_swap from the Linux installer")
	}

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The state a failed swap leaves behind: the new binary is absent and the
	// previous one is sitting in .old.
	old := filepath.Join(binDir, "urnetwork.old")
	if err := os.WriteFile(old, []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}

	stubDir, _ := rcServiceStub(t)
	out, err := runShellScriptWithPath(t, stubDir, `
install_path="`+dir+`"
openrc_swap_pending=1
openrc_units_stopped=0
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
openrc_restore_update_entry() { printf 'RESTORED_UPDATE_ENTRY\n'; }
`+fn+`
openrc_rollback_binary_swap
`)
	if err != nil {
		t.Fatalf("rollback failed: %v (%s)", err, out)
	}

	if _, err := os.Stat(old); err == nil {
		t.Error("the .old file must be consumed by the rollback")
	}
	restored, err := os.ReadFile(filepath.Join(binDir, "urnetwork"))
	if err != nil {
		t.Fatalf("the previous binary was not restored: %v", err)
	}
	if string(restored) != "PREVIOUS" {
		t.Errorf("restored binary contains %q, want the previous version", string(restored))
	}
	if !strings.Contains(out, "RESTORED_UPDATE_ENTRY") {
		t.Error("the rollback must un-pause the periodic update entry; leaving it paused " +
			"silently disables auto-update forever after a failed install")
	}
}

// TestOpenRCRollbackRestartsStoppedService pins the third thing restored: a
// service that stop_openrc_units stopped must come back, otherwise a failed
// update leaves the box with no provider running.
func TestOpenRCRollbackRestartsStoppedService(t *testing.T) {
	skipWithoutPOSIXShell(t)
	fn := extractShellFunc(t, readInstaller(t), "openrc_rollback_binary_swap")
	if fn == "" {
		t.Fatal("could not extract openrc_rollback_binary_swap from the Linux installer")
	}

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "urnetwork.old"), []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		stopped  string
		wantCall bool
	}{
		{"service was stopped", "1", true},
		{"service was not stopped", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The installer deliberately silences rc-service (`> /dev/null 2>&1`), so
			// the stub records the call to a FILE rather than to stdout.
			stubDir := t.TempDir()
			record := filepath.Join(stubDir, "called")
			stub := filepath.Join(stubDir, "rc-service")
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nexit 0\n"
			if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			out, err := runShellScriptWithPath(t, stubDir, `
install_path="`+dir+`"
openrc_swap_pending=1
openrc_units_stopped=`+tc.stopped+`
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
openrc_restore_update_entry() { :; }
`+fn+`
openrc_rollback_binary_swap
`)
			if err != nil {
				t.Fatalf("rollback failed: %v (%s)", err, out)
			}
			calledB, readErr := os.ReadFile(record)
			called := readErr == nil && strings.TrimSpace(string(calledB)) != ""
			if called != tc.wantCall {
				t.Errorf("rc-service called=%v, want %v (output: %s)", called, tc.wantCall, out)
			}
			if tc.wantCall && !strings.Contains(string(calledB), "urnetwork start") {
				t.Errorf("expected `rc-service urnetwork start`, got %q", string(calledB))
			}
		})
	}
}

// TestOpenRCRollbackIsNoOpWithoutPendingSwap pins that the rollback does
// nothing on the normal path, where the swap completed and .old was already
// removed. Without the pending flag it must not touch the live binary.
func TestOpenRCRollbackIsNoOpWithoutPendingSwap(t *testing.T) {
	skipWithoutPOSIXShell(t)
	fn := extractShellFunc(t, readInstaller(t), "openrc_rollback_binary_swap")
	if fn == "" {
		t.Fatal("could not extract openrc_rollback_binary_swap from the Linux installer")
	}

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(binDir, "urnetwork")
	if err := os.WriteFile(live, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	stubDir, marker := rcServiceStub(t)
	out, err := runShellScriptWithPath(t, stubDir, `
install_path="`+dir+`"
openrc_swap_pending=0
openrc_units_stopped=0
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
openrc_restore_update_entry() { printf 'RESTORED_UPDATE_ENTRY\n'; }
`+fn+`
openrc_rollback_binary_swap
`)
	if err != nil {
		t.Fatalf("rollback failed: %v (%s)", err, out)
	}
	if out != "" {
		t.Errorf("with no pending swap the rollback must do nothing, got: %s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("with no pending swap the rollback must not touch the service")
	}
	got, err := os.ReadFile(live)
	if err != nil || string(got) != "NEW" {
		t.Errorf("the live binary must be untouched, got %q (%v)", string(got), err)
	}
}

// TestOpenRCBinarySwapRollsBackOnFailure pins the WIRING: every guarded copy
// and chmod in both install flows must invoke the rollback. Without this the
// rollback function exists but nothing calls it. Count the call sites rather
// than matching one exact string, so dropping the rollback from ONE flow is
// caught even when the other still has it.
func TestOpenRCBinarySwapRollsBackOnFailure(t *testing.T) {
	source := readInstaller(t)

	swaps := strings.Count(source, `mv -f "$install_path/bin/urnetwork" "$install_path/bin/urnetwork.old"`)
	if swaps < 2 {
		t.Fatalf("expected the binary swap in both install flows, found %d", swaps)
	}

	// Every swap must be preceded (in the same guarded block) by a rollback on
	// the failure path: one rollback per copy AND per chmod.
	guardRollbacks := strings.Count(source, "openrc_rollback_binary_swap\n            exit 1")
	wantRollbacks := 2 * swaps // one cp failure + one chmod failure per flow
	if guardRollbacks < wantRollbacks {
		t.Errorf("found %d rollback-on-failure call sites, want at least %d "+
			"(one per copy and one per chmod in each of %d install flows); a failed "+
			"replacement otherwise leaves the tree with no binary at all",
			guardRollbacks, wantRollbacks, swaps)
	}
	// The flag must be set when the swap starts and cleared only after it
	// completes, so the rollback knows there is something to restore.
	if strings.Count(source, "openrc_swap_pending=1") < swaps {
		t.Errorf("every swap must set openrc_swap_pending=1 (%d swaps, %d sets)",
			swaps, strings.Count(source, "openrc_swap_pending=1"))
	}
	if !strings.Contains(source, "openrc_swap_pending=0") {
		t.Error("openrc_swap_pending must be cleared once the swap completes")
	}
}
