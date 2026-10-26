//go:build linux || darwin

// A run whose every slot failed authentication must ask for a failing exit
// status. The supervisor policy (Restart=on-failure) only restarts on nonzero.
package miner

import (
	"errors"
	"strings"
	"testing"
)

// The fixture serves a malformed registration reply, so the only slot fails
// authentication without retrying and the run joins and returns. provide
// exits with providerRunExitCode when it reports one, and otherwise falls
// through to a normal return, which is process exit 0.
func TestProviderRunAllSlotsFailAuthenticationExitsNonzero(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	fixture.malformed = true
	err := fixture.run(t.Context(), true, providerRegistrationHooks{})
	if err == nil {
		t.Fatal("run with no authenticated slot returned nil")
	}
	code, ok := providerRunExitCode(err)
	if !ok || code == 0 {
		t.Fatalf("no slot authenticated but the run asks for no failing exit: code=%d ok=%v err=%v", code, ok, err)
	}
}

// Partial failure and clean ends keep their behaviour: only a run with no
// authenticated slot and an error from every launched slot asks for a failure.
func TestNoSlotAuthenticatedErrorOnlyWhenNothingAuthenticated(t *testing.T) {
	boom := errors.New("boom")
	if noSlotAuthenticatedError(1, 2, []error{boom, boom}) != nil {
		t.Fatal("a run with an authenticated slot must not be reported")
	}
	if noSlotAuthenticatedError(0, 2, []error{boom, nil}) != nil {
		t.Fatal("a slot that ended cleanly must not be reported")
	}
	if noSlotAuthenticatedError(0, 0, nil) != nil {
		t.Fatal("a run with no slots must not be reported")
	}
	err := noSlotAuthenticatedError(0, 3, []error{boom, errors.New("other"), boom})
	if err == nil || !errors.Is(err, errProviderNoSlotAuthenticated) || !strings.Contains(err.Error(), "tried 3 slots") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A run whose every slot was rejected for its credentials must ask for the
// terminal code, so a supervisor does not retry a password that cannot fix
// itself. A transient failure keeps the retrying code.
func TestProviderRunExitCodeCredentialsRejectedIsNotRetried(t *testing.T) {
	rejected := &providerNoSlotAuthenticatedError{tried: 2, top: "the network sign-in was rejected", topNum: 2, credentialsRejected: true}
	code, ok := providerRunExitCode(rejected)
	if !ok || code != providerCredentialsRejectedExitCode {
		t.Fatalf("credentials rejected: code=%d ok=%v want %d", code, ok, providerCredentialsRejectedExitCode)
	}
	// The run joins this error with whatever the slots returned, so the
	// concrete type can sit behind a join.
	code, ok = providerRunExitCode(errors.Join(errors.New("joined"), rejected))
	if !ok || code != providerCredentialsRejectedExitCode {
		t.Fatalf("joined: code=%d ok=%v want %d", code, ok, providerCredentialsRejectedExitCode)
	}
	transient := &providerNoSlotAuthenticatedError{tried: 2, top: "timeout", topNum: 1}
	code, ok = providerRunExitCode(transient)
	if !ok || code != providerNoSlotAuthenticatedExitCode {
		t.Fatalf("transient: code=%d ok=%v want %d", code, ok, providerNoSlotAuthenticatedExitCode)
	}
}

// A server that rejects the sign-in (401) must produce the terminal code
// through the real registration path, not only through the constructor.
func TestProviderRunCredentialsRejectedUsesConfigExit(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	fixture.status = 401
	err := fixture.run(t.Context(), true, providerRegistrationHooks{})
	if err == nil {
		t.Fatal("run with rejected credentials returned nil")
	}
	code, ok := providerRunExitCode(err)
	if !ok || code != providerCredentialsRejectedExitCode {
		t.Fatalf("rejected credentials: code=%d ok=%v err=%v", code, ok, err)
	}
}
