package urnettools

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// nativeExecutableMagic returns a header that isRecognizedExecutable accepts
// on the running OS: ELF on linux and the BSDs, Mach-O on darwin, PE on
// windows.
func nativeExecutableMagic() []byte {
	switch runtime.GOOS {
	case "darwin":
		return []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}
	case "windows":
		return []byte("MZ\x90\x00\x03\x00\x00\x00")
	default:
		return []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	}
}

// TestUpdateVerification_NoMatchLogsStateDirs pins the diagnostic for the
// silent wait: when discovery has no entry for the provider's state dir at
// all, the loop prints the discovered state dirs once and then a bounded
// heartbeat instead of staying quiet for the whole window.
func TestUpdateVerification_NoMatchLogsStateDirs(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{testProvider("/home/other/.urnetwork", 200)}
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when no discovered provider matches")
	}
	if !strings.Contains(out, `no provider entry matches state dir "/home/user/.urnetwork"`) {
		t.Errorf("expected the missing-state-dir diagnostic, got:\n%s", out)
	}
	if !strings.Contains(out, `"/home/other/.urnetwork"`) {
		t.Errorf("expected the discovered state dir in the diagnostic, got:\n%s", out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 1 {
		t.Errorf("expected the state-dir listing exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "present but not ready"); got != 0 {
		t.Errorf("an absent state-dir entry must not print the not-ready line, got %d:\n%s", got, out)
	}
	// maxIterations is 30; the heartbeat prints at iterations 6/11/16/21/26.
	if got := strings.Count(out, "no matching provider yet"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_NoMatchNoProvidersLogsNone pins the zero-provider
// rendering of the missing-state-dir diagnostic: discovery sees nothing at
// all, a distinct failure shape from "sees other state dirs".
func TestUpdateVerification_NoMatchNoProvidersLogsNone(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider { return nil }

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when discovery sees no providers")
	}
	if !strings.Contains(out, "discovery sees: (none)") {
		t.Errorf("expected the (none) rendering, got:\n%s", out)
	}
	if got := strings.Count(out, "present but not ready"); got != 0 {
		t.Errorf("a zero-provider discovery must not print the not-ready line, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet (iteration"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_NotReadyEntryLogsReason pins the diagnostic for the
// other silent case: discovery has an entry for the provider's state dir, but
// it cannot satisfy the readiness predicate — typically the stale
// pre-restart process, whose swapped-out binary reads as binaryDeleted.
func TestUpdateVerification_NotReadyEntryLogsReason(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{{StateDir: p.StateDir, PID: 100, BinaryDeleted: true}}
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when the only discovered entry is stale")
	}
	if !strings.Contains(out, "present but not ready") {
		t.Errorf("expected the not-ready diagnostic, got:\n%s", out)
	}
	if !strings.Contains(out, "pid 100, binaryDeleted=true") {
		t.Errorf("expected the entry's readiness fields, got:\n%s", out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 0 {
		t.Errorf("a present state-dir entry must not print the absent-dir listing, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "present but not ready"); got != 1 {
		t.Errorf("expected the not-ready detail exactly once, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_TransientNotReadyThenSuccess pins the transition
// shape: the stale pre-restart process is seen once (not-ready reported
// exactly once, no heartbeat), then the restarted process appears and the
// loop returns success without further diagnostics.
func TestUpdateVerification_TransientNotReadyThenSuccess(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	callCount := 0

	verifyDiscoverFn = func() []Provider {
		callCount++
		if callCount == 1 {
			return []Provider{{StateDir: p.StateDir, PID: 100, BinaryDeleted: true}}
		}
		return []Provider{{StateDir: p.StateDir, PID: 200}}
	}
	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "/proc/200/exe", nil
	}
	verifyProviderVersionFn = func(binary string) string {
		return cfg.Tag
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err != nil {
		t.Fatalf("expected success once the restarted process appears, got: %v", err)
	}
	if got := strings.Count(out, "present but not ready"); got != 1 {
		t.Errorf("expected the not-ready transient reported exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 0 {
		t.Errorf("expected no heartbeats once the entry is present, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 0 {
		t.Errorf("expected no absent-dir listing, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_TransientMissThenSuccess pins the cold-restart
// shape: the old process is already gone when the loop starts (discovery
// sees nothing), then the new process appears and the loop succeeds — the
// absent-dir listing prints exactly once and no heartbeat follows.
func TestUpdateVerification_TransientMissThenSuccess(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	callCount := 0

	verifyDiscoverFn = func() []Provider {
		callCount++
		if callCount == 1 {
			return nil
		}
		return []Provider{{StateDir: p.StateDir, PID: 200}}
	}
	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "/proc/200/exe", nil
	}
	verifyProviderVersionFn = func(binary string) string {
		return cfg.Tag
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err != nil {
		t.Fatalf("expected success once the restarted process appears, got: %v", err)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 1 {
		t.Errorf("expected the absent-dir listing exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 0 {
		t.Errorf("expected no heartbeats once the entry appears, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "present but not ready"); got != 0 {
		t.Errorf("expected no not-ready diagnostics, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_NotReadyTakesPrecedenceOverListing pins the
// branch-selection rule: when discovery has a stale entry for the
// provider's own state dir plus entries for other dirs, the not-ready line
// fires and the absent-dir listing stays silent.
func TestUpdateVerification_NotReadyTakesPrecedenceOverListing(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{
			{StateDir: p.StateDir, PID: 100, BinaryDeleted: true},
			testProvider("/home/other/.urnetwork", 200),
		}
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when the same-dir entry never becomes ready")
	}
	if got := strings.Count(out, "present but not ready"); got != 1 {
		t.Errorf("expected the not-ready line exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 0 {
		t.Errorf("a present same-dir entry must suppress the absent-dir listing, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
}

// Under OpenRC the running image can be a file the service user influenced, so
// the loop must read its version without running it. The exec-capable resolver
// must never be called there; an image with no readable stamp fails closed.
func TestUpdateVerification_OpenRCReadsVersionWithoutExec(t *testing.T) {
	const tag = "v3.23.0-fix.35.7"
	// The read-only resolver only scans files that start with the running
	// OS's native executable magic, so the fixtures do too. A fixed ELF header
	// would be refused on Windows and macOS before the stamp is ever read.
	native := nativeExecutableMagic()
	stamped := append(append([]byte{}, native...), []byte("\n"+versionStampPrefix+tag+"\n")...)
	unstamped := append(append([]byte{}, native...), []byte("\nno stamp here\n")...)
	script := []byte("#!/bin/sh\necho " + tag + "\n")

	for _, tc := range []struct {
		name       string
		image      []byte
		supervisor string
		viaFn      bool
		wantOK     bool
		wantExec   bool
	}{
		{name: "openrc field, stamped image verifies", image: stamped, supervisor: "openrc", wantOK: true},
		{name: "openrc detected from the process, stamped image verifies", image: stamped, viaFn: true, wantOK: true},
		{name: "openrc, unstamped image fails closed", image: unstamped, supervisor: "openrc"},
		{name: "openrc detected from the process, unstamped image fails closed", image: unstamped, viaFn: true},
		{name: "other supervisor keeps the exec-capable resolver", image: script, wantOK: true, wantExec: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubVerifyLoop(t)
			dir := t.TempDir()
			image := dir + "/urnetwork"
			if err := os.WriteFile(image, tc.image, 0o755); err != nil {
				t.Fatal(err)
			}
			oldSup := providerSupervisedByOpenRCFn
			t.Cleanup(func() { providerSupervisedByOpenRCFn = oldSup })
			providerSupervisedByOpenRCFn = func(Provider) bool { return tc.viaFn }

			p := testProvider("/home/user/.urnetwork", 100)
			cfg := testConfig(tag)
			verifyDiscoverFn = func() []Provider {
				return []Provider{{StateDir: p.StateDir, PID: 200, Supervisor: tc.supervisor}}
			}
			verifyRunningImageHandleFn = func(int) (string, error) { return image, nil }
			execCalled := false
			verifyProviderVersionFn = func(string) string {
				execCalled = true
				return tag
			}

			var err error
			captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
			if (err == nil) != tc.wantOK {
				t.Fatalf("verify err=%v, want ok=%v", err, tc.wantOK)
			}
			if execCalled != tc.wantExec {
				t.Fatalf("exec-capable resolver called=%v, want %v", execCalled, tc.wantExec)
			}
		})
	}
}
