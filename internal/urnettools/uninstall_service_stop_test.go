package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The Windows provider is a per-user supervisor ("service run
// urnetwork-provider") that restarts its miner when it dies. Uninstall has to
// stop the supervisor BEFORE touching files, or the miner keeps the binary and
// state dir open ("Access is denied" / "used by another process").
// These tests pin the ordering and the abort-on-failure contract with a seam,
// so they run on every platform.

func stubStopService(t *testing.T, fn func(Provider) error) {
	t.Helper()
	prev := stopProviderServiceFn
	stopProviderServiceFn = func(p Provider, _ bool) error { return fn(p) }
	t.Cleanup(func() { stopProviderServiceFn = prev })
}

// stubCleanup keeps the tests off the machine's real lifecycle artifacts; on
// Windows the real cleanup deletes the user's Startup shortcut and service
// config.
func stubCleanup(t *testing.T) {
	t.Helper()
	prev, prevRunning := cleanupLifecycleFn, runningProviderProcessesFn
	cleanupLifecycleFn = func(Provider) {}
	// nor read the machine's real process list
	runningProviderProcessesFn = func() []Provider { return nil }
	t.Cleanup(func() { cleanupLifecycleFn, runningProviderProcessesFn = prev, prevRunning })
}

func uninstallFixture(t *testing.T) Provider {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "install", "urnetwork")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	return Provider{Binary: bin, StateDir: state}
}

// stubServiceOwned makes the fixture a provider the supervisor owns, the case
// where uninstall stops the service. Without it the real Windows ownership
// check sees no saved service and takes the bare-provider path. The service
// files removal is stubbed too, so a test never deletes the real Startup
// shortcut or service config.
func stubServiceOwned(t *testing.T) {
	t.Helper()
	stubOwnership(t, serviceOwned, func(Provider) error {
		t.Error("an owned provider is stopped through its service, not by pid")
		return nil
	}, func() {})
}

func TestUninstallStopsServiceBeforeRemovingFiles(t *testing.T) {
	p := uninstallFixture(t)
	stubCleanup(t)
	stubServiceOwned(t)
	called := 0
	stubStopService(t, func(got Provider) error {
		called++
		// Files must still exist when the supervisor is stopped; they can
		// only be released by the stop, so removal has to come after.
		if _, err := os.Stat(p.Binary); err != nil {
			t.Errorf("binary already removed when service stop ran: %v", err)
		}
		if _, err := os.Stat(p.StateDir); err != nil {
			t.Errorf("state dir already removed when service stop ran: %v", err)
		}
		return nil
	})
	if err := uninstallProvider(p); err != nil {
		t.Fatalf("uninstallProvider: %v", err)
	}
	if called != 1 {
		t.Fatalf("stopProviderService called %d times, want 1", called)
	}
	if _, err := os.Stat(p.Binary); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("binary should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(p.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state dir should be removed, stat err = %v", err)
	}
}

// A supervisor that could not be stopped is still holding the files. Pressing
// on would delete state under a live provider, so uninstall must stop with an
// error and leave everything in place.
func TestUninstallAbortsWhenServiceWillNotStop(t *testing.T) {
	p := uninstallFixture(t)
	stubCleanup(t)
	stubServiceOwned(t)
	stubStopService(t, func(Provider) error { return errors.New("the service didn't stop") })
	err := uninstallProvider(p)
	if err == nil {
		t.Fatal("uninstallProvider must fail when the service cannot be stopped")
	}
	if _, err := os.Stat(p.Binary); err != nil {
		t.Errorf("binary must be untouched after a failed stop: %v", err)
	}
	if _, err := os.Stat(p.StateDir); err != nil {
		t.Errorf("state dir must be untouched after a failed stop: %v", err)
	}
}
