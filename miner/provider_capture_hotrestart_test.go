package miner

// provider_capture_hotrestart_test.go covers the capture launch's retained
// client identity against the ported v33 hot-restart reuse
// (client_jwt_hotrestart.go provideClientJWT). The store lookup runs before
// the durable-registration mint, so a launch that records one client must
// never reuse, renew or write back an entry for another: every write on that
// path (the renewal Put, the network_id self-heal and the slot credential
// rewrite in bindReusedClientJWT) would otherwise present an identity the
// profile never recorded.
//
// The mint here is the slot's real custody path, not a fake, so the
// assertions about the credential and the registration endpoint are about
// code that actually runs in a launch.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/clientauth"
)

// captureHotRestartStoreEntry is the client identity a store entry records,
// with the network the fixture's sign-in token belongs to left unstamped
// (""), so the reuse path's network_id self-heal is in scope as well.
func captureHotRestartSeedStore(t *testing.T, path string, entry clientJWTEntry) {
	t.Helper()
	data, err := json.Marshal(map[string]clientJWTEntry{jwtStoreKey(nil): entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// captureHotRestartProvide runs one slot's real client-identity attempt
// through provideClientJWT: the process-wide store is the one at storePath,
// and the mint is the slot's durable registration custody with the client the
// launch must present.
func captureHotRestartProvide(t *testing.T, fixture *providerRegistrationFixture, retained *clientauth.RetainedClientIdentity, allowCreate bool) (string, connect.Id, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
	defer strategy.Close()
	api := sdk.NewApi(ctx, strategy, fixture.server.URL)
	defer func() {
		if err := api.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	networkPath := filepath.Join(fixture.dir, "jwt")
	credentialPath := filepath.Join(fixture.dir, ".provider.jwt")
	keyOwner, err := clientauth.OpenProviderClientKey(ctx, filepath.Join(fixture.dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := keyOwner.Close(); err != nil {
			t.Error(err)
		}
	}()
	return provideClientJWT(ctx, clientJWTProvideConfig{
		Api:                    api,
		NetworkJwtPath:         networkPath,
		ClientJwtPath:          credentialPath,
		KeyOwner:               keyOwner,
		Retained:               retained,
		ApiUrl:                 fixture.server.URL,
		Description:            "direct",
		ClientStrategySettings: connect.DefaultClientStrategySettings(),
		ClientStrategy:         strategy,
		Mint: func(mintCtx context.Context) (string, connect.Id, error) {
			return keyOwner.LoadOrRegisterClientJwtObserved(mintCtx, api, networkPath, credentialPath, "direct", allowCreate, retained, nil)
		},
		FreshMint: func(context.Context) (string, connect.Id, error) {
			t.Error("hot restart on ran the fresh-registration mint")
			return "", connect.Id{}, nil
		},
	})
}

// A capture launch whose slot holds no credential at all must not adopt the
// store's client: the store is a cache of what custody settled, not an
// authority over what this launch may present. Before the reuse path compared
// the entry's client, this launch rewrote the slot's credential with the
// stored client and returned it as a reuse.
func TestCaptureLaunchRefusesStoreEntryForAnotherClientWithoutWriting(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}
	held := connect.Id{0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x60}

	fixture := newProviderRegistrationFixture(t)
	storePath := filepath.Join(fixture.dir, ".client_jwts.json")
	heldToken := providerRegistrationTestToken(t, held.String(), "stored-client")
	captureHotRestartSeedStore(t, storePath, clientJWTEntry{
		ByClientJWT: heldToken,
		ClientID:    held.String(),
		MintedAt:    time.Now(),
	})
	defer withGlobalStore(t, storePath)()
	setHotRestart(t, true)

	storeBefore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	posts, legacy, allocations, refreshes := fixture.counts()

	// The profile records `recorded` and creation is withheld, exactly as the
	// launch computes it for a capture profile.
	_, _, reused, err := captureHotRestartProvide(t, fixture, &clientauth.RetainedClientIdentity{ClientId: recorded}, false)
	var refused *clientauth.RegistrationRefusedError
	if err == nil || !errors.As(err, &refused) || refused.Code != "client_registration_disabled" {
		credential, credentialErr := clientauth.ReadToken(filepath.Join(fixture.dir, ".provider.jwt"))
		storeAfter, _ := os.ReadFile(storePath)
		t.Fatalf("capture launch over a store entry for %v: err = %v reused = %v, slot credential = %q (%v) for client %q, store rewritten %v; want the creation refusal", held, err, reused, credential, credentialErr, jwtClientId(credential), !bytes.Equal(storeAfter, storeBefore))
	}
	if reused {
		t.Fatal("capture launch reported the store's other client as a reuse")
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture launch wrote the store's client to the slot credential: %v", err)
	}
	storeAfter, err := os.ReadFile(storePath)
	if err != nil || !bytes.Equal(storeAfter, storeBefore) {
		t.Fatalf("capture launch rewrote the store entry it must not present: %s (%v)", storeAfter, err)
	}
	if entry, ok := loadGlobalClientJWTStore().Get(jwtStoreKey(nil)); !ok || entry.ClientID != held.String() || entry.ByClientJWT != heldToken || entry.NetworkID != "" {
		t.Fatalf("store entry = %+v, want the untouched %v entry", entry, held)
	}
	if afterPosts, afterLegacy, afterAllocations, afterRefreshes := fixture.counts(); afterPosts != posts || afterLegacy != legacy || afterAllocations != allocations || afterRefreshes != refreshes {
		t.Fatalf("capture launch reached the registration endpoint (%d->%d posts, %d->%d legacy, %d->%d allocations, %d->%d refreshes)", posts, afterPosts, legacy, afterLegacy, allocations, afterAllocations, refreshes, afterRefreshes)
	}
}

// The same store entry beside a slot that holds the recorded client's
// credential must not replace it either: the launch presents what custody
// owns, and the mismatching entry is replaced by that client rather than
// written over the slot.
func TestCaptureLaunchNeverPresentsTheStoresOtherClient(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}
	held := connect.Id{0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x60}

	fixture := newProviderRegistrationFixture(t)
	storePath := filepath.Join(fixture.dir, ".client_jwts.json")
	heldToken := providerRegistrationTestToken(t, held.String(), "stored-client")
	captureHotRestartSeedStore(t, storePath, clientJWTEntry{
		ByClientJWT: heldToken,
		ClientID:    held.String(),
		MintedAt:    time.Now(),
	})
	defer withGlobalStore(t, storePath)()
	setHotRestart(t, true)
	providerCaptureCredentialOnlyCustody(t, fixture, recorded)
	credentialPath := filepath.Join(fixture.dir, ".provider.jwt")
	if retained, err := clientauth.ReadToken(credentialPath); err != nil || jwtClientId(retained) != recorded.String() {
		t.Fatalf("fixture credential = %q (%v), want the recorded client %v", retained, err, recorded)
	}

	_, clientId, reused, err := captureHotRestartProvide(t, fixture, &clientauth.RetainedClientIdentity{ClientId: recorded}, false)
	credential, credentialErr := clientauth.ReadToken(credentialPath)
	if credentialErr != nil || credential == heldToken || jwtClientId(credential) != recorded.String() {
		t.Fatalf("slot credential = %q (%v) for client %q, want the recorded %v and never the store's %q (err = %v, reused = %v)", credential, credentialErr, jwtClientId(credential), recorded, heldToken, err, reused)
	}
	if err != nil {
		t.Fatalf("capture launch holding the recorded client: %v", err)
	}
	if reused {
		t.Fatal("capture launch reported the store's other client as a reuse")
	}
	if clientId != recorded {
		t.Fatalf("capture launch presented %v, want the recorded %v", clientId, recorded)
	}
	// The store may only ever name the client this launch presented.
	if entry, ok := loadGlobalClientJWTStore().Get(jwtStoreKey(nil)); !ok || entry.ClientID != recorded.String() {
		t.Fatalf("store entry = %+v, want the recorded client %v never the stored %v", entry, recorded, held)
	}
}

// A store entry for the client the profile recorded is the ordinary reuse: the
// launch takes it, and the slot credential is written from it as before.
func TestCaptureLaunchReusesStoreEntryForTheRecordedClient(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}

	fixture := newProviderRegistrationFixture(t)
	storePath := filepath.Join(fixture.dir, ".client_jwts.json")
	recordedToken := providerRegistrationTestToken(t, recorded.String(), "recorded-client")
	captureHotRestartSeedStore(t, storePath, clientJWTEntry{
		ByClientJWT: recordedToken,
		ClientID:    recorded.String(),
		// stamped, so the reuse path's self-heal has nothing to write
		NetworkID: accountNetworkId(providerRegistrationTestToken(t, "", "bootstrap")),
		MintedAt:  time.Now(),
	})
	defer withGlobalStore(t, storePath)()
	setHotRestart(t, true)
	storeBefore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	posts, _, _, _ := fixture.counts()

	byClientJwt, clientId, reused, err := captureHotRestartProvide(t, fixture, &clientauth.RetainedClientIdentity{ClientId: recorded}, false)
	if err != nil {
		t.Fatalf("capture launch over the recorded client's store entry: %v", err)
	}
	if !reused || clientId != recorded || byClientJwt != recordedToken {
		t.Fatalf("capture launch = (%v, %v, reused %v), want the stored (%v, %v) reused", clientId, byClientJwt, reused, recorded, recordedToken)
	}
	credential, err := clientauth.ReadToken(filepath.Join(fixture.dir, ".provider.jwt"))
	if err != nil || credential != recordedToken {
		t.Fatalf("slot credential = %q (%v), want the reused %q", credential, err, recordedToken)
	}
	if afterPosts, _, _, _ := fixture.counts(); afterPosts != posts {
		t.Fatalf("reuse reached the registration endpoint (%d -> %d posts)", posts, afterPosts)
	}
	if storeAfter, err := os.ReadFile(storePath); err != nil || !bytes.Equal(storeAfter, storeBefore) {
		t.Fatalf("reuse rewrote its own store entry: %s (%v)", storeAfter, err)
	}
}

// A caller that records no client asserts no expectation, so the store keeps
// deciding on its own exactly as it did before capture launches carried an
// identity: the entry is reused and the slot credential is written from it.
func TestStoreReuseUnchangedWithoutARecordedClient(t *testing.T) {
	held := connect.Id{0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x60}

	for _, test := range []struct {
		name     string
		retained *clientauth.RetainedClientIdentity
	}{
		{name: "ordinary launch"},
		{name: "capture launch with no recorded client", retained: &clientauth.RetainedClientIdentity{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProviderRegistrationFixture(t)
			storePath := filepath.Join(fixture.dir, ".client_jwts.json")
			heldToken := providerRegistrationTestToken(t, held.String(), "stored-client")
			captureHotRestartSeedStore(t, storePath, clientJWTEntry{
				ByClientJWT: heldToken,
				ClientID:    held.String(),
				NetworkID:   accountNetworkId(providerRegistrationTestToken(t, "", "bootstrap")),
				MintedAt:    time.Now(),
			})
			defer withGlobalStore(t, storePath)()
			setHotRestart(t, true)
			posts, _, _, _ := fixture.counts()

			byClientJwt, clientId, reused, err := captureHotRestartProvide(t, fixture, test.retained, true)
			if err != nil {
				t.Fatalf("%s over a store entry: %v", test.name, err)
			}
			if !reused || clientId != held || byClientJwt != heldToken {
				t.Fatalf("%s = (%v, %v, reused %v), want the stored (%v, %v) reused", test.name, clientId, byClientJwt, reused, held, heldToken)
			}
			credential, err := clientauth.ReadToken(filepath.Join(fixture.dir, ".provider.jwt"))
			if err != nil || credential != heldToken {
				t.Fatalf("%s slot credential = %q (%v), want the reused %q", test.name, credential, err, heldToken)
			}
			if afterPosts, _, _, _ := fixture.counts(); afterPosts != posts {
				t.Fatalf("%s reached the registration endpoint (%d -> %d posts)", test.name, posts, afterPosts)
			}
		})
	}
}
