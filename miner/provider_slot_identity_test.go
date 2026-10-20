package miner

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func slotIdentitySettings(address, user string) *connect.ProxySettings {
	settings := &connect.ProxySettings{Network: "tcp", Address: address}
	if user != "" {
		settings.Auth = &proxy.Auth{User: user, Password: "synthetic-pass"}
	}
	return settings
}

func historicalSlotHash(settings *connect.ProxySettings) [32]byte {
	return sha256.Sum256([]byte(settings.Network + "\x00" + settings.Address))
}

// mixedProviderSettings returns 500 configured proxies: 480 addresses seen
// once (every tenth with an auth user) and 10 addresses carrying two distinct
// accounts each (the shape a provider list with per-account endpoints has).
func mixedProviderSettings() (all, unique []*connect.ProxySettings) {
	for index := 0; index < 480; index++ {
		user := ""
		if index%10 == 0 {
			user = fmt.Sprintf("unique-user-%d", index)
		}
		settings := slotIdentitySettings(fmt.Sprintf("10.1.%d.%d:1080", index/256, index%256), user)
		all = append(all, settings)
		unique = append(unique, settings)
	}
	for index := 0; index < 10; index++ {
		address := fmt.Sprintf("10.9.0.%d:1080", index)
		all = append(all,
			slotIdentitySettings(address, fmt.Sprintf("acct-%d-short", index)),
			slotIdentitySettings(address, fmt.Sprintf("acct-%d-long", index)))
	}
	return all, unique
}

// 500 configured proxies including shared-address groups: every slot and
// credential path is distinct, single-use addresses keep the historical
// address-only name byte-identical, and startup validation accepts the list
// the address-only rule used to refuse.
func TestProviderSlotIdentitySharedAddressesStayDistinct(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	providers, unique := mixedProviderSettings()
	if len(providers) != 500 {
		t.Fatalf("built %d proxies, want 500", len(providers))
	}
	identities := newProviderSlotIdentities(providers)
	seenSlot, seenPath := map[string]bool{}, map[string]bool{}
	for _, settings := range providers {
		slot := providerRegistrationSlot(identities, settings)
		path, err := providerClientJwtPath(identities, settings)
		if err != nil {
			t.Fatal(err)
		}
		if seenSlot[slot] || seenPath[path] {
			t.Fatalf("two identities collide at %s", settings.Key())
		}
		seenSlot[slot], seenPath[path] = true, true
	}
	for _, settings := range unique {
		sum := historicalSlotHash(settings)
		path, err := providerClientJwtPath(identities, settings)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(".provider-%x.jwt", sum[:8]); filepath.Base(path) != want {
			t.Fatalf("single-use address %s lost its historical name: %s want %s", settings.Address, filepath.Base(path), want)
		}
		if want := fmt.Sprintf("proxy-sha256:%x", sum[:]); providerRegistrationSlot(identities, settings) != want {
			t.Fatalf("single-use address %s lost its historical slot", settings.Address)
		}
	}
	if err := validateProviderRegistrationSlots(providers); err != nil {
		t.Fatalf("startup refused a shared-address list: %v", err)
	}

	// The address-only rule this fix replaces does collapse the shared groups,
	// so the test still pins the bug it guards.
	sharedShort, sharedLong := providers[480], providers[481]
	legacy := providerSlotIdentities{}
	if legacy.hash(sharedShort) != legacy.hash(sharedLong) {
		t.Fatal("the historical address-only rule no longer collapses shared addresses")
	}
	if identities.hash(sharedShort) == identities.hash(sharedLong) {
		t.Fatal("distinct accounts at one address share a slot")
	}
}

func TestProviderSlotIdentitySharedAddressMixedForms(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	withoutUser := slotIdentitySettings("10.5.0.1:1080", "")
	withUser := slotIdentitySettings("10.5.0.1:1080", "acct-a")
	otherAddress := slotIdentitySettings("10.5.0.2:1080", "")
	providers := []*connect.ProxySettings{withoutUser, withUser, otherAddress}
	identities := newProviderSlotIdentities(providers)
	if identities.hash(withoutUser) == identities.hash(withUser) {
		t.Fatal("an authenticated and an unauthenticated entry at one address must stay distinct")
	}
	if err := validateProviderRegistrationSlots(providers); err != nil {
		t.Fatalf("startup refused the mixed forms: %v", err)
	}
	duplicated := []*connect.ProxySettings{withUser, slotIdentitySettings("10.5.0.1:1080", "acct-a")}
	if err := validateProviderRegistrationSlots(duplicated); err == nil {
		t.Fatal("an exact duplicate is still an operator error and must be refused")
	}
}

// The automatic import maps every 3.23-fix identity to its own credential
// file: 500 configured proxies including 20 on 10 shared addresses import
// once each, zero collisions, and the 3.23-fix files stay byte-identical so a
// rollback still reuses every client id.
func TestLegacyImportSharedAddressesKeepEveryIdentity(t *testing.T) {
	providers, _ := mixedProviderSettings()
	fixture := newLegacyImportFixtureWithSettings(t, providers)
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatalf("proven slots must not mint, got %d", len(result.mintSlots))
	}
	committed, fallbacks, skipped, _ := fixture.recorder.counts()
	if committed != len(providers) || fallbacks != 0 || skipped != 0 {
		t.Fatalf("committed=%d fallbacks=%d skipped=%d, want %d/0/0", committed, fallbacks, skipped, len(providers))
	}
	journal := fixture.journal()
	if len(journal.Slots) != len(providers) {
		t.Fatalf("journal holds %d slots, want %d", len(journal.Slots), len(providers))
	}
	targets := map[string]string{}
	for _, settings := range providers {
		key := settings.Key()
		recorded, present := journal.Slots[key]
		if !present || recorded.State != "imported" {
			t.Fatalf("slot %s journal = %+v present=%v", key, recorded, present)
		}
		if other, duplicate := targets[recorded.Target]; duplicate {
			t.Fatalf("%s and %s share %s", other, key, recorded.Target)
		}
		targets[recorded.Target] = key
		if _, err := os.Stat(fixture.target(settings)); err != nil {
			t.Fatalf("slot %s has no credential: %v", key, err)
		}
	}
	fixture.assertOriginalsUntouched()
	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(store) != len(providers)+1 {
		t.Fatalf("rollback store holds %d entries, want %d", len(store), len(providers)+1)
	}
	for _, settings := range providers {
		entry, present := store[settings.Key()]
		if !present || entry.ClientID == "" {
			t.Fatalf("rollback lost the identity for %s", settings.Address)
		}
	}
}
