// Real custody and HTTP qualify first provisioning: one created client, crash
// resumption before any send, replay after it, and refusal of every remnant.
package clientauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A private directory holding only the operator's network login, the state a
// first provisioning starts from.
func newMeasurementProvisioningFixture(t *testing.T) (*registrationTestFixture, string) {
	t.Helper()
	fixture := newRegistrationTestFixture(t)
	dir := filepath.Dir(fixture.clientPath)
	if err := os.Remove(fixture.networkPath); err != nil {
		t.Fatal(err)
	}
	fixture.clientPath = filepath.Join(dir, ".validator.jwt")
	fixture.networkPath = filepath.Join(dir, "jwt")
	if err := WriteToken(fixture.networkPath, registrationTestToken(t, "", "original")); err != nil {
		t.Fatal(err)
	}
	return fixture, filepath.Join(dir, ".validator.key")
}

// Every entry and its bytes, except the lock that opening the store creates.
func measurementDirectoryState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, entry := range entries {
		if entry.Name() == ".validator.key.registration.lock" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		state[entry.Name()] = string(raw)
	}
	return state
}

func crashMeasurementProvisioningAfterKey(t *testing.T, keyPath string) []byte {
	t.Helper()
	stop := errors.New("synthetic crash after the measurement key fsync")
	owner, err := provisionValidatorMeasurementClientKey(t.Context(), keyPath, measurementProvisionHooks{afterKey: func() error { return stop }})
	if !errors.Is(err, stop) || owner != nil {
		t.Fatal("measurement key crash did not stop after durable publication", err)
	}
	seed, err := os.ReadFile(keyPath)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatal("measurement key crash escaped before the durable key", err)
	}
	return seed
}

func TestMeasurementProvisioningCreatesExactlyOneClient(t *testing.T) {
	fixture, keyPath := newMeasurementProvisioningFixture(t)
	// A refused plain run leaves only the store's own lock behind.
	plain, err := OpenValidatorMeasurementClientKey(t.Context(), keyPath, false)
	var refused *RegistrationRefusedError
	if plain != nil || !errors.As(err, &refused) || refused.Code != "measurement_key_requires_explicit_recovery" {
		t.Fatal("pristine measurement directory opened without provisioning", err)
	}
	owner, err := ProvisionValidatorMeasurementClientKey(t.Context(), keyPath)
	if err != nil {
		t.Fatal(err)
	}
	seed := owner.Seed()
	marker := measurementKeyMarker(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	for _, name := range []string{".validator.key.identity", ".validator.key.provisioning"} {
		if raw, err := os.ReadFile(filepath.Join(filepath.Dir(keyPath), name)); err != nil || !bytes.Equal(raw, marker) {
			t.Fatal("measurement provisioning did not publish its marker", name, err)
		}
	}
	if raw, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(raw, seed) {
		t.Fatal("measurement provisioning did not publish its key", err)
	}
	token, id, err := owner.ProvisionClientJwt(t.Context(), fixture.api, fixture.networkPath)
	if err != nil || token == "" || id.String() != testClientId || fixture.postCount() != 1 {
		t.Fatal("measurement provisioning did not create its one direct client", err)
	}
	assertMeasurementProvisioningSpent(t, keyPath, marker)
	if _, _, err := owner.ProvisionClientJwt(t.Context(), fixture.api, fixture.networkPath); err == nil || fixture.postCount() != 1 {
		t.Fatal("spent measurement provisioning authority created again", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := ProvisionValidatorMeasurementClientKey(t.Context(), keyPath)
	if again != nil || !errors.As(err, &refused) || refused.Code != "measurement_provisioning_requires_pristine_directory" {
		t.Fatal("provisioned measurement directory was provisioned again", err)
	}
	// The plain runner takes over the created client unchanged.
	plain = openMeasurementFixtureOwner(t, keyPath, false)
	refreshed, id, err := plain.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath)
	if err != nil || refreshed == "" || id.String() != testClientId || fixture.postCount() != 1 {
		t.Fatal("plain measurement run did not take over the provisioned client", err)
	}
	assertMeasurementProvisioningSpent(t, keyPath, marker)
}

// The authority is spent on disk: the same bytes now provisioned, nothing
// left to provision with.
func assertMeasurementProvisioningSpent(t *testing.T, keyPath string, marker []byte) {
	t.Helper()
	dir := filepath.Dir(keyPath)
	if raw, err := os.ReadFile(filepath.Join(dir, ".validator.key.provisioned")); err != nil || !bytes.Equal(raw, marker) {
		t.Fatal("measurement provisioning was not kept as provisioned provenance", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".validator.key.provisioning")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed measurement provisioning kept its creation authority", err)
	}
}

// After the one creation, losing the registration, its anchor and the client
// credential leaves a provisioned identity, never a resumable provisioning.
func TestMeasurementProvisionedIdentityLossCreatesNoSecondClient(t *testing.T) {
	fixture, keyPath := newMeasurementProvisioningFixture(t)
	owner, err := ProvisionValidatorMeasurementClientKey(t.Context(), keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := owner.ProvisionClientJwt(t.Context(), fixture.api, fixture.networkPath); err != nil || fixture.postCount() != 1 {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".validator.jwt.registration", ".validator.jwt.registration.started", ".validator.jwt"} {
		if err := os.Remove(filepath.Join(filepath.Dir(keyPath), name)); err != nil {
			t.Fatal(err)
		}
	}
	again, err := ProvisionValidatorMeasurementClientKey(t.Context(), keyPath)
	var refused *RegistrationRefusedError
	if again != nil || !errors.As(err, &refused) || refused.Code != "measurement_provisioning_requires_pristine_directory" {
		t.Fatal("a provisioned identity resumed provisioning after losing its registration", err)
	}
	plain := openMeasurementFixtureOwner(t, keyPath, false)
	if _, _, err := plain.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath); !errors.As(err, &refused) || refused.Code != "client_registration_disabled" {
		t.Fatal("a provisioned identity's lost registration became an allocation", err)
	}
	if _, _, err := plain.loadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, true); err == nil {
		t.Fatal("a provisioned marker authorized a second creation")
	}
	if fixture.postCount() != 1 {
		t.Fatalf("a provisioned identity made %d registrations", fixture.postCount())
	}
}

// A crash after the registration completes but before the marker rename is
// finished by the plain run path, which spends the authority itself.
func TestMeasurementProvisioningCrashBeforeSpendIsCompletedByThePlainPath(t *testing.T) {
	fixture, keyPath := newMeasurementProvisioningFixture(t)
	stop := errors.New("synthetic crash after the provisioned registration")
	owner, err := provisionValidatorMeasurementClientKey(t.Context(), keyPath, measurementProvisionHooks{afterRegistration: func() error { return stop }})
	if err != nil {
		t.Fatal(err)
	}
	marker := measurementKeyMarker(ed25519.NewKeyFromSeed(owner.Seed()).Public().(ed25519.PublicKey))
	if _, _, err := owner.ProvisionClientJwt(t.Context(), fixture.api, fixture.networkPath); !errors.Is(err, stop) || fixture.postCount() != 1 {
		t.Fatal("provisioned registration did not stop after completing", err)
	}
	dir := filepath.Dir(keyPath)
	for _, name := range []string{".validator.jwt.registration", ".validator.jwt", ".validator.key.provisioning"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("the crash point is not after a retained registration and persisted credential", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".validator.key.provisioned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the authority was spent before the crash point", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	plain := openMeasurementFixtureOwner(t, keyPath, false)
	if _, id, err := plain.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath); err != nil || id.String() != testClientId || fixture.postCount() != 1 {
		t.Fatal("plain run did not complete the provisioned registration", err)
	}
	assertMeasurementProvisioningSpent(t, keyPath, marker)
	// A leftover marker beside its provisioned copy is spent the same way; a
	// marker naming another key is a custody fault.
	if err := os.WriteFile(filepath.Join(dir, ".validator.key.provisioning"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := plain.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath); err != nil {
		t.Fatal(err)
	}
	assertMeasurementProvisioningSpent(t, keyPath, marker)
	other := measurementKeyMarker(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{32}, ed25519.SeedSize)).Public().(ed25519.PublicKey))
	if err := os.WriteFile(filepath.Join(dir, ".validator.key.provisioning"), other, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := plain.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath); err == nil {
		t.Fatal("a provisioning marker for another key was spent")
	}
	if raw, err := os.ReadFile(filepath.Join(dir, ".validator.key.provisioned")); err != nil || !bytes.Equal(raw, marker) || fixture.postCount() != 1 {
		t.Fatal("a foreign provisioning marker replaced the provisioned provenance", err)
	}
}

func TestMeasurementProvisioningResumesAfterKeyCrash(t *testing.T) {
	fixture, keyPath := newMeasurementProvisioningFixture(t)
	seed := crashMeasurementProvisioningAfterKey(t, keyPath)
	// The plain run path still cannot create, and its refused attempt leaves
	// the client credential lock behind.
	plain, err := OpenValidatorMeasurementClientKey(t.Context(), keyPath, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = plain.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath)
	var refused *RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "client_registration_disabled" || fixture.postCount() != 0 {
		t.Fatal("plain measurement run created a client", err)
	}
	if _, _, err := plain.ProvisionClientJwt(t.Context(), fixture.api, fixture.networkPath); err == nil || fixture.postCount() != 0 {
		t.Fatal("opened measurement owner borrowed provisioning authority", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := ProvisionValidatorMeasurementClientKey(t.Context(), keyPath)
	if err != nil {
		t.Fatal("interrupted measurement provisioning did not resume", err)
	}
	defer resumed.Close()
	if !bytes.Equal(resumed.Seed(), seed) {
		t.Fatal("resumed measurement provisioning replaced its key")
	}
	if _, id, err := resumed.ProvisionClientJwt(t.Context(), fixture.api, fixture.networkPath); err != nil || id.String() != testClientId || fixture.postCount() != 1 {
		t.Fatal("resumed measurement provisioning did not create its one client", err)
	}
}

func TestMeasurementProvisioningReplaysAfterFirstSend(t *testing.T) {
	fixture, keyPath := newMeasurementProvisioningFixture(t)
	owner, err := ProvisionValidatorMeasurementClientKey(t.Context(), keyPath)
	if err != nil {
		t.Fatal(err)
	}
	marker := measurementKeyMarker(ed25519.NewKeyFromSeed(owner.Seed()).Public().(ed25519.PublicKey))
	ctx, cancel := context.WithCancel(t.Context())
	fixture.onCommit = cancel
	if _, _, err := owner.ProvisionClientJwt(ctx, fixture.api, fixture.networkPath); err == nil || fixture.postCount() != 1 {
		t.Fatal("measurement provisioning lost reply did not cross the HTTP commit", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(keyPath), ".validator.key.provisioned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a lost reply spent the provisioning authority before the registration completed", err)
	}
	original, err := os.ReadFile(fixture.clientPath + ".registration")
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := ProvisionValidatorMeasurementClientKey(t.Context(), keyPath)
	var refused *RegistrationRefusedError
	if again != nil || !errors.As(err, &refused) || refused.Code != "measurement_provisioning_requires_pristine_directory" {
		t.Fatal("sent measurement provisioning was provisioned again", err)
	}
	plain := openMeasurementFixtureOwner(t, keyPath, false)
	token, id, err := plain.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath)
	if err != nil || token == "" || id.String() != testClientId || fixture.postCount() != 2 {
		t.Fatal("measurement provisioning lost reply did not replay its original operation", err)
	}
	var before, completed registrationRecord
	after, err := os.ReadFile(fixture.clientPath + ".registration")
	if err != nil || json.Unmarshal(original, &before) != nil || json.Unmarshal(after, &completed) != nil || before.Request != completed.Request || before.Scope != completed.Scope || completed.ClientId != testClientId {
		t.Fatal("measurement provisioning replay changed its retained request")
	}
	// The replay that completed the registration also spent the authority.
	assertMeasurementProvisioningSpent(t, keyPath, marker)
}

func TestMeasurementProvisioningRefusesEveryRemnant(t *testing.T) {
	seed := bytes.Repeat([]byte{29}, ed25519.SeedSize)
	marker := measurementKeyMarker(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	remnants := []struct {
		name string
		raw  []byte
	}{
		{name: ".validator.key", raw: seed},
		{name: ".validator.key.identity", raw: marker},
		{name: ".validator.key.provisioning", raw: marker},
		{name: ".validator.jwt.registration", raw: []byte("synthetic retained request")},
		{name: ".validator.jwt.registration.started", raw: []byte("synthetic first-send anchor")},
		{name: ".validator.jwt.registration.existing", raw: []byte("synthetic existing client")},
		{name: ".validator.jwt", raw: []byte("synthetic client credential")},
		{name: ".validator.jwt.rejected", raw: []byte("blocked")},
		{name: ".validator.retained", raw: []byte("synthetic unknown history")},
		{name: ".validator.key.provisioned", raw: marker},
	}
	root := t.TempDir()
	prepare := func(label string, files map[string][]byte) string {
		dir := filepath.Join(root, label)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, raw := range files {
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	refuses := func(label string, dir string) {
		before := measurementDirectoryState(t, dir)
		owner, err := ProvisionValidatorMeasurementClientKey(t.Context(), filepath.Join(dir, ".validator.key"))
		var refused *RegistrationRefusedError
		if owner != nil || !errors.As(err, &refused) || refused.Code != "measurement_provisioning_requires_pristine_directory" {
			t.Fatalf("measurement remnant %s was provisioned: %v", label, err)
		}
		if after := measurementDirectoryState(t, dir); !maps.Equal(before, after) {
			t.Fatalf("refused measurement remnant %s changed its directory", label)
		}
	}
	// Only the empty set and the key with both markers are provisioning states.
	resumable := 1<<0 | 1<<1 | 1<<2
	for set := 1; set < 1<<len(remnants); set++ {
		if set == resumable {
			continue
		}
		files := map[string][]byte{}
		for index, remnant := range remnants {
			if set&(1<<index) != 0 {
				files[remnant.name] = remnant.raw
			}
		}
		label := fmt.Sprintf("set-%03x", set)
		refuses(label, prepare(label, files))
	}
	other := measurementKeyMarker(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{30}, ed25519.SeedSize)).Public().(ed25519.PublicKey))
	for index, fault := range []struct {
		name string
		raw  []byte
	}{
		{name: ".validator.key", raw: make([]byte, ed25519.SeedSize)},
		{name: ".validator.key", raw: seed[:ed25519.SeedSize-1]},
		{name: ".validator.key.identity", raw: other},
		{name: ".validator.key.provisioning", raw: other},
	} {
		files := map[string][]byte{".validator.key": seed, ".validator.key.identity": marker, ".validator.key.provisioning": marker}
		files[fault.name] = fault.raw
		label := fmt.Sprintf("mismatch-%d", index)
		refuses(label, prepare(label, files))
	}
	// A crash before the key leaves a keyless remnant; the plain path keeps
	// today's refusal for it.
	for _, files := range []map[string][]byte{
		{".validator.key.provisioning": marker},
		{".validator.key.provisioning": marker, ".validator.key.identity": marker},
	} {
		dir := prepare(fmt.Sprintf("keyless-%d", len(files)), files)
		owner, err := OpenValidatorMeasurementClientKey(t.Context(), filepath.Join(dir, ".validator.key"), false)
		var refused *RegistrationRefusedError
		if owner != nil || !errors.As(err, &refused) || refused.Code != "measurement_key_requires_explicit_recovery" {
			t.Fatal("keyless measurement provisioning remnant opened", err)
		}
	}
}

// The registration guard itself requires this owner's directory to hold a
// provisioning marker naming the key, whatever the caller asks for.
func TestMeasurementCreationRequiresProvisioningMarker(t *testing.T) {
	fixture, keyPath := newMeasurementProvisioningFixture(t)
	crashMeasurementProvisioningAfterKey(t, keyPath)
	markerPath := filepath.Join(filepath.Dir(keyPath), ".validator.key.provisioning")
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	owner := openMeasurementFixtureOwner(t, keyPath, false)
	if _, _, err := owner.loadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, true); err == nil || fixture.postCount() != 0 {
		t.Fatal("measurement creation passed the guard without a provisioning marker", err)
	}
	other := measurementKeyMarker(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{31}, ed25519.SeedSize)).Public().(ed25519.PublicKey))
	if err := os.WriteFile(markerPath, other, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := owner.loadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, true); err == nil || fixture.postCount() != 0 {
		t.Fatal("measurement creation passed the guard with another key's marker", err)
	}
	// A provisioned marker is spent authority, alone or beside a provisioning
	// marker for the same key.
	marker := measurementKeyMarker(ed25519.NewKeyFromSeed(owner.Seed()).Public().(ed25519.PublicKey))
	provisionedPath := filepath.Join(filepath.Dir(keyPath), ".validator.key.provisioned")
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provisionedPath, marker, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := owner.loadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, true); err == nil || fixture.postCount() != 0 {
		t.Fatal("measurement creation passed the guard with a provisioned marker", err)
	}
	if err := os.WriteFile(markerPath, marker, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := owner.loadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, true); err == nil || fixture.postCount() != 0 {
		t.Fatal("measurement creation passed the guard beside a provisioned marker", err)
	}
	if _, err := os.Stat(fixture.clientPath + ".registration"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused measurement creation retained a request", err)
	}
}

// A provider slot with its own key directory, the custody shape a miner
// migrated from the 3.23-fix provider brings up. The caller closes the owner
// to model a process restart.
func newProviderSlotRecoveryFixture(t *testing.T) (*registrationTestFixture, *ProviderClientKeyOwner, string, string) {
	t.Helper()
	fixture := newRegistrationTestFixture(t)
	dir := filepath.Dir(fixture.clientPath)
	// The fixture verifies the durable record and anchor through this path, so
	// it must name the slot credential the provider owner writes.
	fixture.clientPath = filepath.Join(dir, ".provider.jwt")
	keyPath := filepath.Join(dir, ".provider.key")
	owner, err := OpenProviderClientKey(t.Context(), keyPath, ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	return fixture, owner, fixture.clientPath, keyPath
}

// The provider wrapper's own scope, so these tests exercise the directory
// custody a slot actually uses rather than a synthetic validator path.
func providerSlotRecoveryScope(t *testing.T, fixture *registrationTestFixture, owner *ProviderClientKeyOwner) RegistrationScope {
	t.Helper()
	endpoint, err := fixture.api.NetworkClientRegistrationEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	public := ed25519.NewKeyFromSeed(owner.Seed()).Public().(ed25519.PublicKey)
	return RegistrationScope{Endpoint: endpoint, ClientKey: "0x" + hex.EncodeToString(public), ClientRole: "provider-v1", ClientSlot: "direct"}
}

// A slot that minted once presents the persisted identity on its next start:
// the same client, no second registration request.
func TestProviderSlotRecoveryReusesMintedIdentityOnRestart(t *testing.T) {
	fixture, owner, clientPath, keyPath := newProviderSlotRecoveryFixture(t)
	first, firstId, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", true)
	if err != nil || first == "" || fixture.postCount() != 1 {
		t.Fatalf("provider slot did not mint: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenProviderClientKey(t.Context(), keyPath, ProviderClientKeyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, id, err := restarted.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", false); err != nil || id != firstId || fixture.postCount() != 1 {
		t.Fatalf("restart registered again instead of reusing the saved identity: %v", err)
	}
}

// An adopted identity the server later rejects is replaced: the slot mints a
// new client, clears the tombstone and persists it, instead of parking on
// custody the server has already refused.
func TestProviderSlotAdoptedIdentityRejectionMints(t *testing.T) {
	fixture, owner, clientPath, _ := newProviderSlotRecoveryFixture(t)
	if err := WriteToken(clientPath, registrationTestToken(t, testClientId, "adopted")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", false); err != nil {
		t.Fatal("adopting a saved credential failed", err)
	}
	if _, err := os.Stat(clientPath + ".registration.existing"); err != nil {
		t.Fatal("adoption published no custody marker", err)
	}
	fixture.stateLock.Lock()
	fixture.refreshStatus = http.StatusUnauthorized
	fixture.stateLock.Unlock()
	var refused *RegistrationRefusedError
	if _, _, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", true); !errors.As(err, &refused) || refused.Code != "client_revoked" {
		t.Fatalf("a rejected saved credential did not record its tombstone: %v", err)
	}
	if _, err := os.Stat(clientPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the rejected credential was kept", err)
	}
	token, id, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", true)
	if err != nil || token == "" || id.String() != testClientId || fixture.postCount() != 1 {
		t.Fatalf("a rejected adopted identity did not mint: %v", err)
	}
	if _, err := os.Stat(clientPath + ".rejected"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the tombstone survived the new identity", err)
	}
}

// A credential that cannot be installed leaves the retained operation: every
// later attempt replays that same request, so a read-only state directory can
// never turn one server binding into a series of new clients.
func TestProviderSlotFailedCredentialWriteDoesNotMintAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The failure this test needs is a directory that refuses new files.
		// chmod does not deny writes on Windows and the POSIX permission model
		// has no portable equivalent there, so the case is covered on the other
		// two platforms rather than asserted with a permission that does nothing.
		t.Skip("a read-only directory does not deny writes on windows")
	}
	if os.Geteuid() == 0 {
		// Root ignores the directory permission bits this test relies on, so
		// the write it wants refused would succeed and the failure would land
		// on the assertion instead of the permission check.
		t.Skip("a read-only directory does not deny writes to root")
	}
	fixture, owner, clientPath, keyPath := newProviderSlotRecoveryFixture(t)
	stop := errors.New("synthetic credential handoff failure")
	// The binding hook runs where the credential is installed: the operation
	// and the server's answer are durable, the credential is not.
	if _, _, err := loadOrRegisterClientJwtInDirectory(t.Context(), fixture.api, fixture.networkPath, clientPath, "provider", providerSlotRecoveryScope(t, fixture, owner), true, nil, registrationHooks{afterBinding: func() error { return stop }}, owner.store); !errors.Is(err, stop) {
		t.Fatalf("the slot did not stop at the credential handoff: %v", err)
	}
	request, err := os.ReadFile(clientPath + ".registration")
	if err != nil {
		t.Fatal(err)
	}
	posts := fixture.postCount()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	// A state directory that refuses new files: the write the credential needs
	// fails, while the retained operation stays readable.
	if err := os.Chmod(filepath.Dir(clientPath), 0500); err != nil {
		t.Fatal(err)
	}
	// Restore the mode even when a later assertion fails: t.TempDir's cleanup
	// cannot remove a 0500 directory, so a fatal would turn into a second
	// failure and hide the first.
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(clientPath), 0700) })
	readOnly, err := OpenProviderClientKey(t.Context(), keyPath, ProviderClientKeyOptions{})
	if err != nil {
		t.Fatalf("the read-only directory did not open its retained key: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := readOnly.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", true); err == nil {
			t.Fatalf("attempt %d reported a credential it could not write", i)
		}
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clientPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed write left a credential behind")
	}
	if after, readErr := os.ReadFile(clientPath + ".registration"); readErr != nil || !bytes.Equal(request, after) {
		t.Fatal("a failed credential write replaced the retained operation", readErr)
	}
	if err := os.Chmod(filepath.Dir(clientPath), 0700); err != nil {
		t.Fatal(err)
	}
	// The recovered start completes the retained operation: the identity stays
	// the one the server already bound instead of a new allocation.
	recovered, err := OpenProviderClientKey(t.Context(), keyPath, ProviderClientKeyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	token, id, err := recovered.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", true)
	if err != nil || token == "" || id.String() != testClientId {
		t.Fatalf("the retained operation did not complete after the write recovered: %v", err)
	}
	if after, readErr := os.ReadFile(clientPath + ".registration"); readErr != nil || !bytes.Equal(request, after) {
		t.Fatal("recovery replaced the retained operation", readErr)
	}
	// One request per attempt, all of them the same operation: never a second
	// registration because a write failed.
	if fixture.postCount() != posts+3 {
		t.Fatalf("registration requests = %d, want %d replays of one operation", fixture.postCount(), posts+3)
	}
	if _, _, err := recovered.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, clientPath, "direct", true); err != nil || fixture.postCount() != posts+3 {
		t.Fatalf("a stored credential still registered: %v", err)
	}
}
