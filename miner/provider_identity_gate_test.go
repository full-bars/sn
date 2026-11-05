package miner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urfoundation/sn/clientauth"
)

// The provide path's gate: a bare provide on an empty state dir must not
// create a provider identity. The key open is the real clientauth path; the
// client mint is the second gate (providerSlotCreatePermitted) and is pure.
// No network is involved.

func TestProvideBareFreshStateDirDoesNotAllocateIdentity(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".provider.key")
	allow := providerClientRegistrationAllowed(docopt.Opts{"--allow-client-registration": false})
	if allow {
		t.Fatal("a bare provide must not permit identity creation")
	}
	if providerSlotCreatePermitted(allow, false, nil) {
		t.Fatal("a bare provide on a fresh dir must not permit a client mint")
	}

	owner, err := clientauth.OpenProviderClientKey(context.Background(), keyPath, clientauth.ProviderClientKeyOptions{AllowCreate: allow})
	if owner != nil {
		_ = owner.Close()
		t.Fatal("bare provide opened a provider key on a fresh dir")
	}
	var refused *clientauth.RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "provider_key_requires_explicit_recovery" {
		t.Fatalf("want provider_key_requires_explicit_recovery refusal, got %v", err)
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused provide wrote a key file: stat err %v", statErr)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() == ".provider.key" || entry.Name() == ".provider.jwt" {
			t.Fatalf("refused provide left identity file %s", entry.Name())
		}
	}
}

func TestProvideExistingIdentityStartsWithoutFlag(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".provider.key")

	// First run: the operator explicitly creates the identity.
	first, err := clientauth.OpenProviderClientKey(context.Background(), keyPath, clientauth.ProviderClientKeyOptions{AllowCreate: providerClientRegistrationAllowed(docopt.Opts{"--allow-client-registration": true})})
	if err != nil {
		t.Fatalf("explicit first creation refused: %v", err)
	}
	seed := first.Seed()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Later bare provide: the identity on disk is reused, nothing is created.
	allow := providerClientRegistrationAllowed(docopt.Opts{})
	second, err := clientauth.OpenProviderClientKey(context.Background(), keyPath, clientauth.ProviderClientKeyOptions{AllowCreate: allow})
	if err != nil {
		t.Fatalf("bare provide with an identity on disk refused: %v", err)
	}
	defer second.Close()
	if !bytes.Equal(second.Seed(), seed) {
		t.Fatal("bare provide did not reuse the identity already on disk")
	}
}

func TestProvideClientRegistrationNeedsExplicitFlag(t *testing.T) {
	if providerClientRegistrationAllowed(docopt.Opts{}) {
		t.Fatal("no flag must not permit creation")
	}
	if !providerClientRegistrationAllowed(docopt.Opts{"--allow-client-registration": true}) {
		t.Fatal("explicit flag must permit creation")
	}
}
