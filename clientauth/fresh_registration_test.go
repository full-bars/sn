//go:build linux || darwin

package clientauth

// URNETWORK_HOT_RESTART=0's fresh registration path never reads the retained
// credential, allocates a new client and leaves the slot custody consistent
// for the identity it registered. The fixture is the actual HTTP boundary with
// real private files: the versioned route allocates at most one client per
// scope (as the server does), the legacy route allocates a new client on
// every call, and the refresh route answers for the client it is presented.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// The bootstrap and client tokens name one principal and roles, exactly as
// the server's sign-in inheritance makes them.
func freshRegistrationToken(t *testing.T, clientId, marker string) string {
	t.Helper()
	claims := gojwt.MapClaims{
		"network_id": "00000000-0000-0000-0000-000000000301",
		"user_id":    "00000000-0000-0000-0000-000000000401",
		"roles":      []string{"provider"},
		"principal":  "synthetic-provider-owner",
		"marker":     marker,
		"exp":        time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
	if clientId != "" {
		claims["client_id"], claims["device_id"] = clientId, testDeviceId
	}
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type freshRegistrationFixture struct {
	test      *testing.T
	stateLock sync.Mutex
	dir       string
	api       *sdk.Api
	scope     RegistrationScope

	networkPath string
	clientPath  string

	versionedClient string
	versionedPosts  int
	legacyPosts     int
	allocations     int
	refreshed       []string
	legacyRequest   *sdk.AuthNetworkClientArgs
}

func newFreshRegistrationFixture(t *testing.T) *freshRegistrationFixture {
	t.Helper()
	dir := providerRegistrationPrivateDir(t)
	self := &freshRegistrationFixture{test: t, dir: dir, networkPath: filepath.Join(dir, "jwt"), clientPath: filepath.Join(dir, ".provider.jwt")}
	if err := WriteToken(self.networkPath, freshRegistrationToken(t, "", "bootstrap")); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(self.serveHttp))
	t.Cleanup(server.Close)
	api, closeApi := testApi(t, server.URL)
	t.Cleanup(closeApi)
	self.api = api
	endpoint, err := api.NetworkClientRegistrationEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	self.scope = RegistrationScope{Endpoint: endpoint, ClientKey: "0x" + strings.Repeat("34", 32), ClientRole: "provider-v1", ClientSlot: "direct"}
	return self
}

// allocateLocked is one server allocation, numbered so successive clients are
// distinguishable.
func (self *freshRegistrationFixture) allocateLocked() string {
	self.allocations++
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", 100+self.allocations)
}

func (self *freshRegistrationFixture) serveHttp(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024))
	closeErr := r.Body.Close()
	if err != nil || closeErr != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/hello" {
		w.WriteHeader(http.StatusOK)
		return
	}
	switch r.URL.Path {
	case "/network/register-client-v1":
		var request sdk.RegisterNetworkClientArgs
		if json.Unmarshal(body, &request) != nil {
			self.test.Error("fresh fixture could not decode the versioned request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		self.stateLock.Lock()
		self.versionedPosts++
		if self.versionedClient == "" {
			self.versionedClient = self.allocateLocked()
		}
		client := self.versionedClient
		self.stateLock.Unlock()
		digest := sha256.Sum256(body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema": sdk.NetworkClientRegistrationSchema, "registration_id": request.RegistrationId, "request_sha256": hex.EncodeToString(digest[:]), "client_id": client, "device_id": testDeviceId, "by_client_jwt": freshRegistrationToken(self.test, client, "registered")})
	case "/network/auth-client":
		var request sdk.AuthNetworkClientArgs
		if json.Unmarshal(body, &request) != nil {
			self.test.Error("fresh fixture could not decode the legacy request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		self.stateLock.Lock()
		self.legacyPosts++
		self.legacyRequest = &request
		client := self.allocateLocked()
		self.stateLock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"by_client_jwt": freshRegistrationToken(self.test, client, "legacy"), "client_id": client})
	case "/auth/refresh":
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		identity, err := registrationIdentity(token)
		if err != nil || !validRegistrationIdentityId(identity.clientId) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		self.stateLock.Lock()
		self.refreshed = append(self.refreshed, identity.clientId)
		self.stateLock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"by_jwt": freshRegistrationToken(self.test, identity.clientId, "refreshed")})
	default:
		self.test.Errorf("fresh fixture received an unrelated route %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (self *freshRegistrationFixture) counts() (versioned, legacy, allocations, refreshes int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.versionedPosts, self.legacyPosts, self.allocations, len(self.refreshed)
}

func (self *freshRegistrationFixture) refreshedClients() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.refreshed...)
}

func (self *freshRegistrationFixture) clientKeyOwner(t *testing.T) *ProviderClientKeyOwner {
	t.Helper()
	owner, err := OpenProviderClientKey(t.Context(), filepath.Join(self.dir, ".provider.key"), ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	return owner
}

// A fresh registration replaces the retained versioned identity: a new client
// is allocated through the legacy route, the credential file is overwritten,
// the abandoned record and anchor are gone, the new identity's existing-client
// custody is published, and a later retained-custody start loads the new
// credential without refusal.
func TestProviderFreshRegistrationReplacesRetainedIdentity(t *testing.T) {
	fixture := newFreshRegistrationFixture(t)
	owner := fixture.clientKeyOwner(t)

	tokenA, idA, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", true)
	if err != nil || idA == (connect.Id{}) {
		t.Fatalf("retained identity setup: %v", err)
	}
	if _, err := os.Stat(fixture.clientPath + ".registration"); err != nil {
		t.Fatal(err)
	}
	if versioned, legacy, allocations, _ := fixture.counts(); versioned != 1 || legacy != 0 || allocations != 1 {
		t.Fatalf("retained setup used versioned=%d legacy=%d allocations=%d", versioned, legacy, allocations)
	}

	// The hot-restart-off registration: a new client, never the retained one.
	tokenB, idB, err := owner.RegisterFreshClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", true)
	if err != nil {
		t.Fatalf("RegisterFreshClientJwt: %v", err)
	}
	if idB == (connect.Id{}) || idB == idA || tokenB == tokenA {
		t.Fatalf("fresh registration reused the retained identity: a=%s b=%s", idA, idB)
	}
	versioned, legacy, allocations, _ := fixture.counts()
	if versioned != 1 || legacy != 1 || allocations != 2 {
		t.Fatalf("fresh registration used versioned=%d legacy=%d allocations=%d, want no versioned retry and one legacy allocation", versioned, legacy, allocations)
	}
	request := fixture.legacyRequest
	if request == nil || request.ClientId != nil || request.DeviceDescription != "provider" || !request.ProvideIntent {
		t.Fatalf("fresh registration request = %+v, want a no-client-id provider-intent request", request)
	}

	// The credential file is overwritten with the new identity.
	stored, err := ReadToken(fixture.clientPath)
	if err != nil || stored != tokenB {
		t.Fatalf("fresh credential = %q err=%v, want the new token", stored, err)
	}
	identityB, err := registrationIdentity(stored)
	if err != nil || identityB.clientId != idB.String() {
		t.Fatalf("fresh credential identity = %+v, want %s", identityB, idB)
	}

	// The abandoned versioned operation cannot coexist with the new custody.
	for _, name := range []string{".registration", ".registration.started"} {
		if _, err := os.Stat(fixture.clientPath + name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fresh registration retained %s", name)
		}
	}
	markerRaw, err := os.ReadFile(fixture.clientPath + ".registration.existing")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(markerRaw, []byte(idB.String())) || bytes.Contains(markerRaw, []byte(idA.String())) {
		t.Fatalf("existing-client custody does not name exactly the new identity: %s", markerRaw)
	}
	var marker struct {
		Schema    string                `json:"schema"`
		Scope     RegistrationScope     `json:"scope"`
		Principal registrationPrincipal `json:"principal"`
		ClientId  string                `json:"client_id"`
		DeviceId  string                `json:"device_id"`
	}
	public := ed25519.NewKeyFromSeed(owner.Seed()).Public().(ed25519.PublicKey)
	if json.Unmarshal(markerRaw, &marker) != nil ||
		marker.Schema != "urnetwork-existing-client-v1" ||
		marker.Scope != (RegistrationScope{Endpoint: fixture.scope.Endpoint, ClientKey: "0x" + hex.EncodeToString(public), ClientRole: "provider-v1", ClientSlot: "direct"}) ||
		marker.ClientId != idB.String() || marker.DeviceId != identityB.deviceId || !marker.Principal.equal(identityB.registrationPrincipal) {
		t.Fatalf("existing-client custody = %s", markerRaw)
	}

	// The next retained-custody start (hot restart on) loads the new credential
	// without a refusal: it refreshes the new client, never the replaced one.
	refreshesBefore := len(fixture.refreshedClients())
	tokenC, idC, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", false)
	if err != nil {
		t.Fatalf("retained start after fresh registration was refused: %v", err)
	}
	if idC != idB {
		t.Fatalf("retained start loaded %s, want the fresh identity %s", idC, idB)
	}
	if identityC, err := registrationIdentity(tokenC); err != nil || identityC.clientId != idB.String() {
		t.Fatalf("retained start credential = %q, want client %s", tokenC, idB)
	}
	refreshed := fixture.refreshedClients()
	if len(refreshed) == refreshesBefore {
		t.Fatalf("retained start did not refresh the fresh identity: %v", refreshed)
	}
	// The transport may issue several physical refreshes for one logical
	// refresh; none of them may name the replaced identity.
	for _, client := range refreshed[refreshesBefore:] {
		if client != idB.String() {
			t.Fatalf("retained start refreshed %s, want only the fresh identity %s", client, idB)
		}
	}
	if strings.Contains(strings.Join(refreshed, " "), idA.String()) {
		t.Fatalf("retained start refreshed the replaced identity %s: %v", idA, refreshed)
	}
	if _, err := os.Stat(fixture.clientPath + ".registration"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retained start created a versioned record for the adopted identity")
	}
}

// A fresh registration after adopted (marker-only) custody replaces the marker
// with the new identity, and the next retained-custody start adopts it.
func TestProviderFreshRegistrationAfterAdoptedCustody(t *testing.T) {
	fixture := newFreshRegistrationFixture(t)
	owner := fixture.clientKeyOwner(t)

	seeded := "00000000-0000-0000-0000-000000000501"
	if err := WriteToken(fixture.clientPath, freshRegistrationToken(t, seeded, "seed")); err != nil {
		t.Fatal(err)
	}
	if _, idSeed, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", false); err != nil || idSeed.String() != seeded {
		t.Fatalf("adopting the seeded identity: %v", err)
	}
	if _, err := os.Stat(fixture.clientPath + ".registration.existing"); err != nil {
		t.Fatal("seeded identity was not adopted as existing-client custody")
	}
	if _, err := fixture.ownerReadRegistration(owner); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("adopted custody unexpectedly has a versioned record")
	}

	tokenB, idB, err := owner.RegisterFreshClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", true)
	if err != nil {
		t.Fatalf("RegisterFreshClientJwt after adoption: %v", err)
	}
	markerRaw, err := os.ReadFile(fixture.clientPath + ".registration.existing")
	if err != nil || !bytes.Contains(markerRaw, []byte(idB.String())) || bytes.Contains(markerRaw, []byte(seeded)) {
		t.Fatalf("adopted custody was not replaced by the fresh identity: %s err=%v", markerRaw, err)
	}
	stored, err := ReadToken(fixture.clientPath)
	if err != nil || stored != tokenB {
		t.Fatalf("adopted credential = %q err=%v, want the fresh token", stored, err)
	}
	if _, idC, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", false); err != nil || idC != idB {
		t.Fatalf("retained start after adoption replacement: id=%s err=%v, want %s", idC, err, idB)
	}
}

// ownerReadRegistration reads the versioned record directly through the key
// owner's directory, so an absent record is an os.ErrNotExist, not a guess.
func (self *freshRegistrationFixture) ownerReadRegistration(owner *ProviderClientKeyOwner) ([]byte, error) {
	store, err := openRegistrationStoreForOwner(self.clientPath, owner.store)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.close() }()
	return store.read(filepath.Base(self.clientPath) + ".registration")
}

// A fresh identity is new client creation: without explicit creation authority
// nothing is read, allocated or overwritten.
func TestProviderFreshRegistrationRequiresExplicitCreation(t *testing.T) {
	fixture := newFreshRegistrationFixture(t)
	owner := fixture.clientKeyOwner(t)

	tokenA, _, err := owner.LoadOrRegisterClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", true)
	if err != nil {
		t.Fatal(err)
	}
	recordBefore, err := os.ReadFile(fixture.clientPath + ".registration")
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = owner.RegisterFreshClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", false)
	var refused *RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "fresh_identity_requires_explicit_creation" {
		t.Fatalf("fresh registration without creation authority = %v", err)
	}
	if _, legacy, allocations, _ := fixture.counts(); legacy != 0 || allocations != 1 {
		t.Fatalf("unauthorized fresh registration reached the API: legacy=%d allocations=%d", legacy, allocations)
	}
	stored, err := ReadToken(fixture.clientPath)
	if err != nil || stored != tokenA {
		t.Fatalf("unauthorized fresh registration changed the credential: %q err=%v", stored, err)
	}
	recordAfter, err := os.ReadFile(fixture.clientPath + ".registration")
	if err != nil || !bytes.Equal(recordBefore, recordAfter) {
		t.Fatalf("unauthorized fresh registration changed the record: %v", err)
	}
}

// A confirmed rejection is sticky in this original custody: an automatic
// restart may not clear it by allocating a replacement.
func TestProviderFreshRegistrationRefusesStickyRejection(t *testing.T) {
	fixture := newFreshRegistrationFixture(t)
	owner := fixture.clientKeyOwner(t)

	if err := WriteToken(fixture.clientPath+".rejected", "blocked"); err != nil {
		t.Fatal(err)
	}
	_, _, err := owner.RegisterFreshClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", true)
	var refused *RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "client_revoked" {
		t.Fatalf("fresh registration past a tombstone = %v", err)
	}
	if _, legacy, allocations, _ := fixture.counts(); legacy != 0 || allocations != 0 {
		t.Fatalf("tombstoned fresh registration reached the API: legacy=%d allocations=%d", legacy, allocations)
	}
	if _, err := os.Stat(fixture.clientPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tombstoned fresh registration wrote a credential")
	}
}

// The fresh registration is bootstrapped by the network sign-in; without it
// nothing is allocated.
func TestProviderFreshRegistrationRequiresNetworkSignIn(t *testing.T) {
	fixture := newFreshRegistrationFixture(t)
	owner := fixture.clientKeyOwner(t)

	if err := os.Remove(fixture.networkPath); err != nil {
		t.Fatal(err)
	}
	_, _, err := owner.RegisterFreshClientJwt(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "direct", true)
	var refused *RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "network_authentication_missing" {
		t.Fatalf("fresh registration without a network sign-in = %v", err)
	}
	if _, legacy, allocations, _ := fixture.counts(); legacy != 0 || allocations != 0 {
		t.Fatalf("sign-in-less fresh registration reached the API: legacy=%d allocations=%d", legacy, allocations)
	}
}

// Only the exact provider scope selects the fresh operation: chain, validator,
// deployment or unsupported roles are refused before any request.
func TestProviderFreshRegistrationRefusesNonProviderScope(t *testing.T) {
	fixture := newFreshRegistrationFixture(t)
	owner := fixture.clientKeyOwner(t)

	faults := map[string]func(*RegistrationScope){
		"role":      func(scope *RegistrationScope) { scope.ClientRole = "synthetic-other-role" },
		"chain":     func(scope *RegistrationScope) { scope.ChainId = 964 },
		"validator": func(scope *RegistrationScope) { scope.ValidatorId = 1 },
		"deployment": func(scope *RegistrationScope) {
			scope.DeploymentId = "synthetic-deployment"
		},
		"slot":     func(scope *RegistrationScope) { scope.ClientSlot = "proxy-sha256:12" },
		"key":      func(scope *RegistrationScope) { scope.ClientKey = "0x12" },
		"endpoint": func(scope *RegistrationScope) { scope.Endpoint += "/changed" },
	}
	for name, fault := range faults {
		scope := fixture.scope
		fault(&scope)
		if _, _, err := registerFreshClientJwtWithCustody(t.Context(), fixture.api, fixture.networkPath, fixture.clientPath, "provider", scope, true, owner.store); err == nil {
			t.Fatalf("fresh registration accepted the %s scope", name)
		}
	}
	if _, legacy, allocations, _ := fixture.counts(); legacy != 0 || allocations != 0 {
		t.Fatalf("refused fresh scopes reached the API: legacy=%d allocations=%d", legacy, allocations)
	}
}
