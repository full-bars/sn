package clientauth

// The registration path is the only place that knows whether a slot kept,
// created or replaced its identity. These tests pin that report: a caller
// counting a start's mints is told, not left to infer it from the credential.

import (
	"errors"
	"net/http"
	"os"
	"testing"
)

// identityOutcomeRecorder collects one attempt's reported identity outcomes.
type identityOutcomeRecorder struct {
	outcomes []IdentityOutcome
	reasons  []string
}

func (self *identityOutcomeRecorder) observe(outcome IdentityOutcome, reason string) {
	self.outcomes = append(self.outcomes, outcome)
	self.reasons = append(self.reasons, reason)
}

// only returns the single outcome reported, failing when the attempt reported
// none or more than one.
func (self *identityOutcomeRecorder) only(t *testing.T) IdentityOutcome {
	t.Helper()
	if len(self.outcomes) != 1 {
		t.Fatalf("reported %d outcomes %v, want exactly 1", len(self.outcomes), self.outcomes)
	}
	return self.outcomes[0]
}

// A slot with nothing saved mints its first identity, and the report says so:
// this is the ordinary case, which must not read as a heal.
func TestRegistrationReportsMintedNewForAFirstStart(t *testing.T) {
	fixture := newRegistrationTestFixture(t)
	recorder := &identityOutcomeRecorder{}
	if _, err := fixture.register(t.Context(), true, registrationHooks{identity: recorder.observe}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if outcome := recorder.only(t); outcome != IdentityOutcomeMintedNew {
		t.Fatalf("first start reported %v, want minted new", outcome)
	}
	if recorder.reasons[0] != "" {
		t.Fatalf("first start carried reason %q, want none", recorder.reasons[0])
	}
}

// The saved credential accepted unchanged is not a heal, so it is reported as
// a reuse with no reason and the caller records nothing.
func TestRegistrationReportsReusedForASavedCredential(t *testing.T) {
	fixture := newRegistrationTestFixture(t)
	if _, err := fixture.register(t.Context(), true, registrationHooks{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	recorder := &identityOutcomeRecorder{}
	if _, err := fixture.register(t.Context(), true, registrationHooks{identity: recorder.observe}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if outcome := recorder.only(t); outcome != IdentityOutcomeReused {
		t.Fatalf("restart reported %v, want reused", outcome)
	}
	if recorder.reasons[0] != "" {
		t.Fatalf("reuse carried reason %q, want none", recorder.reasons[0])
	}
}

// The platform refusing the saved credential is a revocation on the attempt
// that saw it, and the reason says which half of the custody was refused.
func TestRegistrationReportsRevokedWhenThePlatformRefuses(t *testing.T) {
	fixture := newRegistrationTestFixture(t)
	if _, err := fixture.register(t.Context(), true, registrationHooks{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fixture.stateLock.Lock()
	fixture.refreshStatus = http.StatusUnauthorized
	fixture.stateLock.Unlock()
	recorder := &identityOutcomeRecorder{}
	var refused *RegistrationRefusedError
	if _, err := fixture.register(t.Context(), true, registrationHooks{identity: recorder.observe}); !errors.As(err, &refused) || refused.Code != "client_revoked" {
		t.Fatalf("refused credential: %v", err)
	}
	if outcome := recorder.only(t); outcome != IdentityOutcomeRevoked {
		t.Fatalf("refused credential reported %v, want revoked", outcome)
	}
	if recorder.reasons[0] != "saved identity rejected" {
		t.Fatalf("refused credential reason = %q, want the saved-identity rejection", recorder.reasons[0])
	}
}

// Custody this slot can no longer present is replaced, and the report says
// replacement rather than a first start: that difference is the whole reason
// an operator reads the count.
func TestRegistrationReportsMintedReplacementForLostCustody(t *testing.T) {
	fixture := newRegistrationTestFixture(t)
	if err := WriteToken(fixture.clientPath, registrationTestToken(t, testClientId, "existing-client")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.register(t.Context(), false, registrationHooks{}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := os.Remove(fixture.clientPath); err != nil {
		t.Fatal(err)
	}
	recorder := &identityOutcomeRecorder{}
	if _, err := fixture.register(t.Context(), true, registrationHooks{identity: recorder.observe}); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	if outcome := recorder.only(t); outcome != IdentityOutcomeMintedReplacement {
		t.Fatalf("lost custody reported %v, want minted replacement", outcome)
	}
	if recorder.reasons[0] != "saved identity missing" {
		t.Fatalf("lost custody reason = %q, want saved identity missing", recorder.reasons[0])
	}
}

// A retained operation completed on a later start is the identity the server
// already bound to this key, so it is neither a new identity nor a replacement.
func TestRegistrationReportsRecoveredForARetainedOperation(t *testing.T) {
	fixture := newRegistrationTestFixture(t)
	stop := errors.New("synthetic credential handoff failure")
	if _, err := fixture.register(t.Context(), true, registrationHooks{afterBinding: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("credential handoff: %v", err)
	}
	recorder := &identityOutcomeRecorder{}
	if _, err := fixture.register(t.Context(), true, registrationHooks{identity: recorder.observe}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if outcome := recorder.only(t); outcome != IdentityOutcomeRecovered {
		t.Fatalf("retained operation reported %v, want recovered", outcome)
	}
}

// An attempt that decides nothing reports nothing: creation switched off is
// the operator's decision, not an identity outcome.
func TestRegistrationReportsNothingWhenCreationIsDisabled(t *testing.T) {
	fixture := newRegistrationTestFixture(t)
	if err := WriteToken(fixture.clientPath, registrationTestToken(t, testClientId, "existing-client")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.register(t.Context(), false, registrationHooks{}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := os.Remove(fixture.clientPath); err != nil {
		t.Fatal(err)
	}
	recorder := &identityOutcomeRecorder{}
	var refused *RegistrationRefusedError
	if _, err := fixture.register(t.Context(), false, registrationHooks{identity: recorder.observe}); !errors.As(err, &refused) || refused.Code != "client_registration_disabled" {
		t.Fatalf("creation switched off: %v", err)
	}
	if len(recorder.outcomes) != 0 {
		t.Fatalf("disabled creation reported %v, want nothing", recorder.outcomes)
	}
}
