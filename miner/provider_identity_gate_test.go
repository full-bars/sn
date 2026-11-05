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

// ALLOW_CLIENT_REGISTRATION is an explicit operator setting and counts as
// intent. `auto` is the Docker image default and permits on purpose: tightening
// it would stop every fresh container from registering.
func TestProvideRegistrationEnvPermitsAllocation(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "auto", "Auto", "y", "yes", "on"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ALLOW_CLIENT_REGISTRATION", value)
			allow := providerClientRegistrationAllowed(docopt.Opts{})
			if !allow || !providerSlotCreatePermitted(allow, false, nil) {
				t.Fatalf("ALLOW_CLIENT_REGISTRATION=%q must permit allocation", value)
			}
			dir := t.TempDir()
			owner, err := clientauth.OpenProviderClientKey(context.Background(), filepath.Join(dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: allow})
			if err != nil {
				t.Fatalf("env %q on a fresh dir refused: %v", value, err)
			}
			_ = owner.Close()
		})
	}
}

func TestProvideAutoRegistrationEnvIsPinned(t *testing.T) {
	t.Setenv("ALLOW_CLIENT_REGISTRATION", "auto")
	if !providerClientRegistrationAllowed(docopt.Opts{}) {
		t.Fatal("auto is the shipped Docker default and must permit a fresh container to register")
	}
}

// The CI contract (identity-gate-fresh): no flag, no usable env, empty dir.
func TestProvideNoFlagNoEnvFreshDirRefuses(t *testing.T) {
	for _, value := range []string{"", "0", "false", "no", "off", "maybe"} {
		t.Run("env="+value, func(t *testing.T) {
			t.Setenv("ALLOW_CLIENT_REGISTRATION", value)
			allow := providerClientRegistrationAllowed(docopt.Opts{})
			if allow || providerSlotCreatePermitted(allow, false, nil) {
				t.Fatalf("ALLOW_CLIENT_REGISTRATION=%q must not permit allocation", value)
			}
			dir := t.TempDir()
			owner, err := clientauth.OpenProviderClientKey(context.Background(), filepath.Join(dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: allow})
			if owner != nil {
				_ = owner.Close()
				t.Fatal("opened a provider key on a fresh dir")
			}
			var refused *clientauth.RegistrationRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("want a registration refusal, got %v", err)
			}
		})
	}
}

// An existing identity starts with no flag and no env: nothing is created.
func TestProvideExistingIdentityStartsWithNoFlagNoEnv(t *testing.T) {
	t.Setenv("ALLOW_CLIENT_REGISTRATION", "")
	keyPath := filepath.Join(t.TempDir(), ".provider.key")
	first, err := clientauth.OpenProviderClientKey(context.Background(), keyPath, clientauth.ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	seed := first.Seed()
	_ = first.Close()
	allow := providerClientRegistrationAllowed(docopt.Opts{})
	if allow {
		t.Fatal("no flag and no env must not permit creation")
	}
	second, err := clientauth.OpenProviderClientKey(context.Background(), keyPath, clientauth.ProviderClientKeyOptions{AllowCreate: allow})
	if err != nil {
		t.Fatalf("existing identity refused without flag or env: %v", err)
	}
	defer second.Close()
	if !bytes.Equal(second.Seed(), seed) {
		t.Fatal("identity on disk was not reused")
	}
}

// identity-gate-adopt: adopting a legacy key is not permission to allocate.
func TestProvideAdoptLegacyKeyOnFreshDirStillRefuses(t *testing.T) {
	t.Setenv("ALLOW_CLIENT_REGISTRATION", "")
	opts := docopt.Opts{"--adopt-legacy-provider-key": true}
	allow := providerClientRegistrationAllowed(opts)
	if allow || providerSlotCreatePermitted(allow, false, nil) {
		t.Fatal("--adopt-legacy-provider-key must not permit allocation")
	}
	dir := t.TempDir()
	owner, err := clientauth.OpenProviderClientKey(context.Background(), filepath.Join(dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: allow, AdoptLegacyKey: true})
	if owner != nil {
		_ = owner.Close()
		t.Fatal("adopt on a fresh dir opened a key")
	}
	if err == nil {
		t.Fatal("adopt on a fresh dir must refuse")
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".provider.key")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("adopt wrote a key: %v", statErr)
	}
}
