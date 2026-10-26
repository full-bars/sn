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

// The terminal stop is driven by the persisted streak, not by one run: a
// single all-slots-failed run stays retryable, and only errProviderAuthStopped
// asks for the terminal code.
func TestProviderRunExitCodeAuthStoppedIsTerminal(t *testing.T) {
	stopped := errors.Join(errors.New("joined"), errProviderAuthStopped)
	code, ok := providerRunExitCode(stopped)
	if !ok || code != minerExitAuthStopped {
		t.Fatalf("stopped: code=%d ok=%v want %d", code, ok, minerExitAuthStopped)
	}
	rejected := &providerNoSlotAuthenticatedError{tried: 2, top: "the network sign-in was rejected", topNum: 2, credentialsRejected: true}
	code, ok = providerRunExitCode(rejected)
	if !ok || code != providerNoSlotAuthenticatedExitCode {
		t.Fatalf("single refusal: code=%d ok=%v want %d", code, ok, providerNoSlotAuthenticatedExitCode)
	}
}

// A server that rejects the sign-in (401) must be countable through the real
// registration path, not only through the constructor, and one such run must
// still exit with the retryable code.
func TestProviderRunCredentialsRejectedIsCounted(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	fixture.status = 401
	err := fixture.run(t.Context(), true, providerRegistrationHooks{})
	if err == nil {
		t.Fatal("run with rejected credentials returned nil")
	}
	var noSlot *providerNoSlotAuthenticatedError
	if !errors.As(err, &noSlot) || !noSlot.credentialsRejected {
		t.Fatalf("a 401 run must be countable: err=%v", err)
	}
	code, ok := providerRunExitCode(err)
	if !ok || code != providerNoSlotAuthenticatedExitCode {
		t.Fatalf("one 401 run must stay retryable: code=%d ok=%v", code, ok)
	}
}
