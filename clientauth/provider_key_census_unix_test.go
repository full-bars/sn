//go:build linux || darwin

package clientauth

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// More matching .provider names than the bounded census retains. A state
// directory holds about three provider identity files per slot, so a census
// bound on matching names is a bound on slots.
const providerKeyCensusLargeCount = 4100

// Legacy adoption reads every retained credential, however many slots left
// one: about 4,100 valid .provider-<id>.jwt files must not trip a census bound.
func TestProviderKeyMarkerAdoptsBeyondTheCensusBound(t *testing.T) {
	dir := providerRegistrationPrivateDir(t)
	path := filepath.Join(dir, ".provider.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{29}, ed25519.SeedSize), 0600); err != nil {
		t.Fatal(err)
	}
	credential := []byte(registrationTestToken(t, testClientId, "provider"))
	for i := 0; i < providerKeyCensusLargeCount; i++ {
		name := fmt.Sprintf(".provider-%016x.jwt", i)
		if !providerClientCredentialName(name) {
			t.Fatalf("%s is not a provider credential name", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), credential, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := openRegistrationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if err := admitProviderKeyMarker(store, true); err != nil {
		t.Fatalf("legacy adoption across %d credentials was refused: %v", providerKeyCensusLargeCount, err)
	}
}

// A missing key beside retained history is refused as retained history, never
// as a census failure, however many .provider names the directory holds.
func TestProviderKeyMissingBeyondTheCensusBoundRefusesAsRetainedHistory(t *testing.T) {
	dir := providerRegistrationPrivateDir(t)
	for i := 0; i < providerKeyCensusLargeCount; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(".provider-%016x.jwt", i)), []byte("synthetic retained history"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := OpenProviderClientKey(t.Context(), filepath.Join(dir, ".provider.key"), ProviderClientKeyOptions{AllowCreate: true})
	if err == nil || owner != nil {
		t.Fatal("a missing key beside retained history created a new identity")
	}
	var refused *RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "provider_key_missing_with_retained_history" {
		t.Fatalf("refusal = %v, want provider_key_missing_with_retained_history", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".provider.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refused open wrote a provider key")
	}
}
