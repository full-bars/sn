package miner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/clientauth"
)

// The miner's run state must stay out of the provider key's namespace.
// clientauth treats every retained `.provider*` file beside a missing
// .provider.key as custody history and refuses a first registration
// (provider_key_missing_with_retained_history). beginControlRun runs before
// the key is opened on every start, so a `.provider*` breadcrumb (v33's
// .provider_version) would make a genuinely fresh install unable to register
// — the bug this pins down. The breadcrumb writes under the miner's name
// (.miner_version), and the run marker is .miner-run-state.json.
func TestBeginControlRunLeavesFreshProviderRegistrationOpen(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	resetControlRuntimeForTest()
	defer resetControlRuntimeForTest()

	beginControlRun(time.Now())

	// The breadcrumb exists under the miner's own name...
	version, err := os.ReadFile(filepath.Join(dir, minerVersionBreadcrumb))
	if err != nil {
		t.Fatalf("beginControlRun did not write %s: %v", minerVersionBreadcrumb, err)
	}
	if got := strings.TrimSpace(string(version)); got != minerControlVersion() {
		t.Fatalf("%s = %q, want %q", minerVersionBreadcrumb, got, minerControlVersion())
	}
	if minerVersionBreadcrumb == ".provider_version" {
		t.Fatal("the breadcrumb name is back in the provider key namespace")
	}

	finishControlRun("clean")

	// ...and no startup file sounds like provider custody.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".provider") {
			t.Fatalf("a fresh start wrote %s into the provider key namespace", entry.Name())
		}
	}

	// A fresh install can still register: AllowCreate is what the provider
	// startup passes, and it must create the key rather than refuse it.
	owner, err := clientauth.OpenProviderClientKey(t.Context(), filepath.Join(dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatalf("a fresh registration was refused after the miner wrote its startup state: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

// Guard on the collision itself: a `.provider*` file beside a missing key is
// what clientauth reads as retained history, so if the breadcrumb ever moves
// back into that namespace the test above must fail rather than silently
// refuse registrations. This encodes the clientauth behavior the fix depends
// on, not the miner's.
func TestProviderVersionBreadcrumbWouldRefuseFreshRegistration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".provider_version"), []byte("v-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := clientauth.OpenProviderClientKey(t.Context(), filepath.Join(dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: true})
	var refused *clientauth.RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "provider_key_missing_with_retained_history" || owner != nil {
		t.Fatalf("a `.provider*` breadcrumb no longer refuses a fresh registration: owner=%v err=%v", owner, err)
	}
}
