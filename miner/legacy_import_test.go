package miner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/clientauth"
)

// All credentials here are synthetic. The fixture writes exactly the files a
// 3.23-fix provider leaves (minus .client_jwts.json backups) and nothing is
// copied from a real node.

func legacyTestToken(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString([]byte("synthetic-signature"))
}

func legacyTestClientToken(networkID, clientID, deviceID string) string {
	return legacyTestToken(map[string]any{
		"network_id": networkID,
		"user_id":    connect.NewId().String(),
		"principal":  "synthetic-principal",
		"roles":      []string{"provider"},
		"client_id":  clientID,
		"device_id":  deviceID,
		"iat":        time.Now().Unix(),
		"exp":        time.Now().Add(24 * time.Hour).Unix(),
	})
}

func legacyTestNetworkToken(networkID string) string {
	return legacyTestToken(map[string]any{
		"network_id": networkID,
		"user_id":    connect.NewId().String(),
		"principal":  "synthetic-principal",
		"roles":      []string{"provider"},
		"iat":        time.Now().Unix(),
		"exp":        time.Now().Add(24 * time.Hour).Unix(),
	})
}

// fakeLegacyImportProver answers the two proof questions with fixed values.
type fakeLegacyImportProver struct {
	serviceKey  map[string][]byte // client id -> server-held key; nil means null
	publicErr   error
	refreshErr  map[string]error
	publicCalls int
	refreshCall int
	// transientRemaining > 0 makes the next calls return the retryable
	// unavailable read, then fall through to refreshErr.
	transientRemaining int
}

func (self *fakeLegacyImportProver) ServerPublicKey(ctx context.Context, clientID connect.Id) ([]byte, error) {
	self.publicCalls++
	if self.publicErr != nil {
		return nil, self.publicErr
	}
	key, ok := self.serviceKey[clientID.String()]
	if !ok {
		return nil, fmt.Errorf("synthetic: no server key for %s", clientID.String()[:8])
	}
	return key, nil
}

func (self *fakeLegacyImportProver) RefreshAccepted(ctx context.Context, token string) error {
	self.refreshCall++
	if self.transientRemaining > 0 {
		self.transientRemaining--
		return &clientauth.RegistrationRefreshUnavailableError{}
	}
	if self.refreshErr == nil {
		return nil
	}
	return self.refreshErr[token]
}

type recordingLegacyImportRecorder struct {
	mu        sync.Mutex
	committed []string
	fallbacks []string
	skipped   []string
	summaries []string
}

func (self *recordingLegacyImportRecorder) legacyImportCommitted(clientID8, slot, target string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.committed = append(self.committed, clientID8+"|"+slot+"|"+target)
}

func (self *recordingLegacyImportRecorder) legacyImportFallback(clientID8, slot, reason string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.fallbacks = append(self.fallbacks, clientID8+"|"+slot+"|"+reason)
}

func (self *recordingLegacyImportRecorder) legacyImportSkipped(reason, detail string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.skipped = append(self.skipped, reason+"|"+detail)
}

func (self *recordingLegacyImportRecorder) legacyImportSummary(imported, fallback, skipped int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.summaries = append(self.summaries, fmt.Sprintf("%d/%d/%d", imported, fallback, skipped))
}

func (self *recordingLegacyImportRecorder) text() string {
	self.mu.Lock()
	defer self.mu.Unlock()
	return strings.Join(append(append(append(append([]string{}, self.committed...), self.fallbacks...), self.skipped...), self.summaries...), "\n")
}

func (self *recordingLegacyImportRecorder) counts() (int, int, int, int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.committed), len(self.fallbacks), len(self.skipped), len(self.summaries)
}

// legacyImportFixture is a 3.23-fix state dir with generated credentials.
type legacyImportFixture struct {
	t              *testing.T
	dir            string
	seed           []byte
	public         []byte
	networkID      string
	directClientID string
	directDevID    string
	proxySettings  []*connect.ProxySettings
	proxyClientIDs map[string]string // store key -> client id
	proxyDevIDs    map[string]string
	prover         *fakeLegacyImportProver
	recorder       *recordingLegacyImportRecorder
	originals      map[string][]byte
}

func newLegacyImportFixture(t *testing.T, withAuth bool, proxies int) *legacyImportFixture {
	t.Helper()
	fixture := &legacyImportFixture{
		t:              t,
		dir:            t.TempDir(),
		networkID:      connect.NewId().String(),
		proxyClientIDs: map[string]string{},
		proxyDevIDs:    map[string]string{},
		prover:         &fakeLegacyImportProver{serviceKey: map[string][]byte{}, refreshErr: map[string]error{}},
		recorder:       &recordingLegacyImportRecorder{},
		originals:      map[string][]byte{},
	}
	fixture.seed = make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(fixture.seed); err != nil {
		t.Fatal(err)
	}
	fixture.public = ed25519.NewKeyFromSeed(fixture.seed).Public().(ed25519.PublicKey)
	fixture.write(".provider.key", fixture.seed)
	fixture.write("jwt", []byte(legacyTestNetworkToken(fixture.networkID)))
	// Legacy side files that must survive untouched and must not read as sn
	// custody.
	fixture.write(".provider.cert", []byte("synthetic-cert"))
	fixture.write(".provider_version", []byte("synthetic-version"))

	store := map[string]legacyClientJwtEntry{}
	fixture.directClientID = connect.NewId().String()
	fixture.directDevID = connect.NewId().String()
	directToken := legacyTestClientToken(fixture.networkID, fixture.directClientID, fixture.directDevID)
	store[legacyImportDirectKey] = legacyClientJwtEntry{
		ByClientJWT: directToken,
		ClientID:    fixture.directClientID,
		NetworkID:   fixture.networkID,
		MintedAt:    time.Now(),
	}
	fixture.prover.serviceKey[fixture.directClientID] = fixture.public
	for index := 0; index < proxies; index++ {
		address := fmt.Sprintf("127.0.0.1:%d", 18471+index)
		settings := &connect.ProxySettings{Network: "tcp", Address: address}
		if withAuth {
			settings.Auth = &proxy.Auth{User: fmt.Sprintf("fakeuser%d", index+1), Password: "fakepass"}
		}
		key := settings.Key()
		clientID := connect.NewId().String()
		deviceID := connect.NewId().String()
		store[key] = legacyClientJwtEntry{
			ByClientJWT: legacyTestClientToken(fixture.networkID, clientID, deviceID),
			ClientID:    clientID,
			NetworkID:   fixture.networkID,
			MintedAt:    time.Now(),
		}
		fixture.proxySettings = append(fixture.proxySettings, settings)
		fixture.proxyClientIDs[key] = clientID
		fixture.proxyDevIDs[key] = deviceID
		fixture.prover.serviceKey[clientID] = fixture.public
	}
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(legacyClientJwtStoreName, raw)
	return fixture
}

func (self *legacyImportFixture) write(name string, data []byte) {
	self.t.Helper()
	if err := os.WriteFile(filepath.Join(self.dir, name), data, 0o600); err != nil {
		self.t.Fatal(err)
	}
}

func (self *legacyImportFixture) read(name string) []byte {
	self.t.Helper()
	raw, err := os.ReadFile(filepath.Join(self.dir, name))
	if err != nil {
		self.t.Fatal(err)
	}
	return raw
}

func (self *legacyImportFixture) captureOriginals() {
	self.t.Helper()
	for _, name := range []string{legacyClientJwtStoreName, ".provider.key", ".provider.cert", ".provider_version"} {
		self.originals[name] = self.read(name)
	}
}

func (self *legacyImportFixture) assertOriginalsUntouched() {
	self.t.Helper()
	for name, want := range self.originals {
		if got := self.read(name); !bytes.Equal(got, want) {
			self.t.Fatalf("original %s changed", name)
		}
	}
}

func (self *legacyImportFixture) target(settings *connect.ProxySettings) string {
	return legacyImportTargetPath(self.dir, settings)
}

func (self *legacyImportFixture) run(hooks legacyImportHooks) legacyImportResult {
	self.t.Helper()
	return runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       self.dir,
		networkJwtPath: filepath.Join(self.dir, "jwt"),
		providers:      self.proxySettings,
		recorder:       self.recorder,
		newProver: func() (legacyImportProver, func(), error) {
			return self.prover, func() {}, nil
		},
		hooks: hooks,
	})
}

func (self *legacyImportFixture) journal() legacyImportJournal {
	self.t.Helper()
	journal, err := readLegacyImportJournal(self.dir)
	if err != nil {
		self.t.Fatal(err)
	}
	return journal
}

func TestLegacyImportTier1CommitsProvenSlots(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatalf("proven slots must not mint, got %v", result.mintSlots)
	}
	for _, settings := range fixture.proxySettings {
		key := settings.Key()
		raw, err := os.ReadFile(fixture.target(settings))
		if err != nil {
			t.Fatalf("credential for %s: %v", key, err)
		}
		store, err := readLegacyStoreRaw(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != store[key].ByClientJWT {
			t.Fatalf("credential bytes for %s are not the stored token", key)
		}
		info, err := os.Stat(fixture.target(settings))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("credential mode for %s = %v, %v", key, info.Mode().Perm(), err)
		}
	}
	// The direct entry's slot is not configured in this run and stays absent.
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfigured direct slot must not be imported")
	}
	wantMarker, err := clientauth.ProviderKeyIdentityMarker(fixture.seed)
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.read(legacyProviderMarkerName); !bytes.Equal(got, wantMarker) {
		t.Fatal("marker differs from the derived public identity")
	}
	journal := fixture.journal()
	if len(journal.Slots) != 2 {
		t.Fatalf("journal slots = %d, want 2", len(journal.Slots))
	}
	for key, slot := range journal.Slots {
		if slot.State != "imported" || slot.Proof != "server_key" {
			t.Fatalf("journal slot %s = %#v", key, slot)
		}
	}
	if committed, fallback, skipped, summaries := fixture.recorder.counts(); committed != 2 || fallback != 0 || skipped != 0 || summaries != 1 {
		t.Fatalf("records committed=%d fallback=%d skipped=%d summaries=%d", committed, fallback, skipped, summaries)
	}
	if fixture.prover.refreshCall != 0 {
		t.Fatalf("tier 1 pass must not need the refresh probe, got %d calls", fixture.prover.refreshCall)
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportSecondStartIsANoOp(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	first := fixture.run(legacyImportHooks{})
	if len(first.mintSlots) != 0 {
		t.Fatal("first start minted")
	}
	second := &recordingLegacyImportRecorder{}
	secondResult := runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       fixture.dir,
		networkJwtPath: filepath.Join(fixture.dir, "jwt"),
		providers:      fixture.proxySettings,
		recorder:       second,
		newProver: func() (legacyImportProver, func(), error) {
			return fixture.prover, func() {}, nil
		},
	})
	if len(secondResult.mintSlots) != 0 {
		t.Fatal("second start minted")
	}
	if committed, fallback, skipped, summaries := second.counts(); committed != 0 || fallback != 0 || skipped != 0 || summaries != 0 {
		t.Fatalf("second start must be silent, got committed=%d fallback=%d skipped=%d summaries=%d", committed, fallback, skipped, summaries)
	}
	if fixture.prover.publicCalls != 2 {
		t.Fatalf("second start probed the server again: %d calls", fixture.prover.publicCalls)
	}
}

func TestLegacyImportDirectSlotWithoutProxies(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 0)
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("direct slot must import")
	}
	raw, err := os.ReadFile(filepath.Join(fixture.dir, ".provider.jwt"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := clientauth.ReadToken(filepath.Join(fixture.dir, ".provider.jwt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != token {
		t.Fatal("direct credential is not the stored token")
	}
	claim, err := clientauth.ClientIdFromJwt(token)
	if err != nil || claim.String() != fixture.directClientID {
		t.Fatalf("direct credential changed identity: %v %v", claim, err)
	}
}

func TestLegacyImportNeverOverwritesExistingCredential(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	// A matching marker means sn already owns the retained key; the import
	// then works per slot and must leave the existing credential alone.
	marker, err := clientauth.ProviderKeyIdentityMarker(fixture.seed)
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(legacyProviderMarkerName, marker)
	existing := []byte("synthetic-existing-credential")
	first := fixture.target(fixture.proxySettings[0])
	if err := os.WriteFile(first, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("the other slot must import")
	}
	if got := fixture.read(filepath.Base(first)); !bytes.Equal(got, existing) {
		t.Fatal("existing sn credential was overwritten")
	}
	second := fixture.target(fixture.proxySettings[1])
	if _, err := os.Stat(second); err != nil {
		t.Fatal("second slot was not imported:", err)
	}
	if committed, _, _, _ := fixture.recorder.counts(); committed != 1 {
		t.Fatalf("committed records = %d, want 1", committed)
	}
}

func TestLegacyImportUnconfiguredSlotsStayUntouched(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	unconfiguredSettings := fixture.proxySettings[1]
	fixture.proxySettings = fixture.proxySettings[:1]
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("configured slot must import")
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("direct entry of an unconfigured slot was imported")
	}
	unconfigured := legacyImportTargetPath(fixture.dir, unconfiguredSettings)
	if _, err := os.Stat(unconfigured); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfigured proxy slot was imported")
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportNetworkMismatchFallsBackWithoutTouchingStore(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 1)
	storeKey := fixture.proxySettings[0].Key()
	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}
	entry := store[storeKey]
	entry.NetworkID = connect.NewId().String()
	store[storeKey] = entry
	raw, _ := json.MarshalIndent(store, "", "  ")
	fixture.write(legacyClientJwtStoreName, raw)
	fixture.captureOriginals()

	result := fixture.run(legacyImportHooks{})
	if !result.mintSlots[storeKey] {
		t.Fatal("mismatched slot must fall back")
	}
	if _, err := os.Stat(fixture.target(fixture.proxySettings[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mismatched entry was imported")
	}
	if _, fallback, _, _ := fixture.recorder.counts(); fallback != 1 {
		t.Fatalf("fallback records = %d, want 1", fallback)
	}
	if !strings.Contains(fixture.recorder.text(), "network_id_mismatch") {
		t.Fatalf("reason missing from record: %s", fixture.recorder.text())
	}
	if !strings.Contains(fixture.recorder.text(), entry.ClientID[:8]) {
		t.Fatalf("fallback record must name the saved client_id: %s", fixture.recorder.text())
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportMalformedStoreWritesNothing(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	fixture.write(legacyClientJwtStoreName, []byte("{not json"))
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 || len(fixture.recorder.skipped) != 1 {
		t.Fatalf("malformed store must skip once, got %#v", fixture.recorder.skipped)
	}
	if !strings.Contains(fixture.recorder.skipped[0], "store_malformed") {
		t.Fatalf("reason = %s", fixture.recorder.skipped[0])
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, legacyProviderMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("malformed store wrote a marker")
	}
	if fixture.prover.publicCalls != 0 {
		t.Fatal("malformed store probed the server")
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportServerKeyMismatchFallsBack(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 1)
	clientID := fixture.proxyClientIDs[fixture.proxySettings[0].Key()]
	wrong := make([]byte, ed25519.PublicKeySize)
	wrong[0] = 1
	fixture.prover.serviceKey[clientID] = wrong
	result := fixture.run(legacyImportHooks{})
	if !result.mintSlots[fixture.proxySettings[0].Key()] {
		t.Fatal("mismatched key must fall back")
	}
	if _, err := os.Stat(fixture.target(fixture.proxySettings[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refuted slot was imported")
	}
	if !strings.Contains(fixture.recorder.text(), "server_key_differs") {
		t.Fatalf("reason missing: %s", fixture.recorder.text())
	}
	// The fallback still commits the marker: the retained seed is used for the
	// new registration, and nothing in the 3.23-fix files changes.
	if _, err := os.Stat(filepath.Join(fixture.dir, legacyProviderMarkerName)); err != nil {
		t.Fatal("fallback did not commit the marker:", err)
	}
	journal := fixture.journal()
	if slot := journal.Slots[fixture.proxySettings[0].Key()]; slot.State != "fallback" || slot.Reason != "server_key_differs" {
		t.Fatalf("journal slot = %#v", slot)
	}
}

func TestLegacyImportNullKeyUsesRefreshProbe(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 1)
	key := fixture.proxySettings[0].Key()
	fixture.prover.serviceKey[fixture.proxyClientIDs[key]] = nil
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("refresh-proven slot must commit")
	}
	if fixture.prover.refreshCall != 1 {
		t.Fatalf("refresh calls = %d, want 1", fixture.prover.refreshCall)
	}
	journal := fixture.journal()
	if slot := journal.Slots[key]; slot.State != "imported" || slot.Proof != "refresh" {
		t.Fatalf("journal slot = %#v", slot)
	}
}

func TestLegacyImportRefreshRejectedFallsBack(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 1)
	key := fixture.proxySettings[0].Key()
	clientID := fixture.proxyClientIDs[key]
	fixture.prover.serviceKey[clientID] = nil
	// The fake matches by token; refuse this token's refresh.
	fixture.overrideRefreshError(fixture.proxySettings[0], errors.New("synthetic refresh refusal"))
	result := fixture.run(legacyImportHooks{})
	if !result.mintSlots[key] {
		t.Fatal("rejected refresh must fall back")
	}
	if !strings.Contains(fixture.recorder.text(), "refresh_rejected") {
		t.Fatalf("reason missing: %s", fixture.recorder.text())
	}
}

func TestLegacyImportNetworkReadFailureMakesNoDecision(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 1)
	if err := os.Remove(filepath.Join(fixture.dir, "jwt")); err != nil {
		t.Fatal(err)
	}
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("an unreadable network sign-in must not mint")
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, legacyProviderMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unreadable network sign-in wrote a marker")
	}
	if !strings.Contains(fixture.recorder.text(), "network_sign_in_unreadable") {
		t.Fatalf("reason missing: %s", fixture.recorder.text())
	}
}

func TestLegacyImportKillAfterFirstCredentialRecovers(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	fixture.captureOriginals()
	interrupted := fixture.run(legacyImportHooks{afterCredentialWrite: func(storeKey string) error {
		return errors.New("synthetic kill after credential")
	}})
	if len(interrupted.mintSlots) != 0 {
		t.Fatal("interrupted run must not mint")
	}
	firstPath := fixture.target(fixture.proxySettings[0])
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatal("first credential missing after interruption:", err)
	}
	firstBytes := fixture.read(filepath.Base(firstPath))

	second := &recordingLegacyImportRecorder{}
	result := runLegacyProviderImport(context.Background(), legacyImportOptions{
		stateDir:       fixture.dir,
		networkJwtPath: filepath.Join(fixture.dir, "jwt"),
		providers:      fixture.proxySettings,
		recorder:       second,
		newProver: func() (legacyImportProver, func(), error) {
			return fixture.prover, func() {}, nil
		},
	})
	if len(result.mintSlots) != 0 {
		t.Fatal("recovery run minted")
	}
	if committed, _, _, _ := second.counts(); committed != 1 {
		t.Fatalf("recovery committed %d slots, want the remaining 1", committed)
	}
	if got := fixture.read(filepath.Base(firstPath)); !bytes.Equal(got, firstBytes) {
		t.Fatal("recovery overwrote the credential written before the interruption")
	}
	journal := fixture.journal()
	if slot, ok := journal.Slots[fixture.proxySettings[1].Key()]; !ok || slot.State != "imported" {
		t.Fatalf("recovery journal slot = %#v, ok=%v", slot, ok)
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportSnCustodyWithoutMarkerIsLeftAlone(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	fixture.write(".provider.jwt", []byte("synthetic-sn-credential"))
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("sn custody must not mint")
	}
	if !strings.Contains(fixture.recorder.text(), "sn_custody_without_marker") {
		t.Fatalf("reason missing: %s", fixture.recorder.text())
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, legacyProviderMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a marker was written over sn custody")
	}
	for _, settings := range fixture.proxySettings {
		if _, err := os.Stat(fixture.target(settings)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a credential was imported over sn custody")
		}
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportMarkerMismatchIsLeftAlone(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 1)
	fixture.write(legacyProviderMarkerName, []byte("urnetwork-provider-client-key-v1\n0xsynthetic\n"))
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 || fixture.prover.publicCalls != 0 {
		t.Fatal("a mismatched marker must stop the import")
	}
	if !strings.Contains(fixture.recorder.text(), "provider_marker_mismatch") {
		t.Fatalf("reason missing: %s", fixture.recorder.text())
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportFallbackCustodyBlocksMinting(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 1)
	// A matching marker means sn already owns this key; the registration
	// custody file then forbids the automatic fallback for this slot.
	marker, err := clientauth.ProviderKeyIdentityMarker(fixture.seed)
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(legacyProviderMarkerName, marker)
	key := fixture.proxySettings[0].Key()
	base := filepath.Base(fixture.target(fixture.proxySettings[0]))
	fixture.write(base+".registration", []byte("{}"))
	fixture.prover.serviceKey[fixture.proxyClientIDs[key]] = []byte{1}
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("registration custody must block the fallback")
	}
	if !strings.Contains(fixture.recorder.text(), "sn_custody_present") {
		t.Fatalf("reason missing: %s", fixture.recorder.text())
	}
	fixture.assertOriginalsUntouched()
}

func TestLegacyImportJournalFreezesDecisions(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	importedMarker, err := clientauth.ProviderKeyIdentityMarker(fixture.seed)
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(legacyProviderMarkerName, importedMarker)
	journal := newLegacyImportJournal()
	for _, settings := range fixture.proxySettings {
		key := settings.Key()
		journal.Slots[key] = legacyImportJournalSlot{State: "imported", ClientID: fixture.proxyClientIDs[key], Proof: "server_key"}
	}
	fixture.write(legacyImportJournalName, mustJSON(t, journal))

	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatal("an imported journal entry must not mint")
	}
	for _, settings := range fixture.proxySettings {
		if _, err := os.Stat(fixture.target(settings)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("an imported journal entry must not be re-imported")
		}
	}
	if fixture.prover.publicCalls != 0 {
		t.Fatal("an imported journal entry must not be probed again")
	}

	// A fallback journal entry stays sticky and enables the mint without any
	// proof attempt.
	fixture2 := newLegacyImportFixture(t, true, 2)
	journal2 := newLegacyImportJournal()
	for _, settings := range fixture2.proxySettings {
		key2 := settings.Key()
		journal2.Slots[key2] = legacyImportJournalSlot{State: "fallback", ClientID: fixture2.proxyClientIDs[key2], Reason: "server_key_differs"}
	}
	fixture2.write(legacyImportJournalName, mustJSON(t, journal2))
	result2 := fixture2.run(legacyImportHooks{})
	for _, settings := range fixture2.proxySettings {
		if !result2.mintSlots[settings.Key()] {
			t.Fatal("a fallback journal entry must keep minting enabled")
		}
	}
	if fixture2.prover.publicCalls != 0 {
		t.Fatalf("a fallback journal entry must not re-prove, public calls = %d", fixture2.prover.publicCalls)
	}
}

func TestLegacyImportDisabledAndExplicitFlagsDoNothing(t *testing.T) {
	for name, mutate := range map[string]func(*legacyImportOptions){
		"disabled":       func(opts *legacyImportOptions) { opts.disabled = true },
		"explicitCreate": func(opts *legacyImportOptions) { opts.explicitCreate = true },
		"explicitAdopt":  func(opts *legacyImportOptions) { opts.explicitAdopt = true },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newLegacyImportFixture(t, true, 2)
			opts := legacyImportOptions{
				stateDir:       fixture.dir,
				networkJwtPath: filepath.Join(fixture.dir, "jwt"),
				providers:      fixture.proxySettings,
				recorder:       fixture.recorder,
				newProver: func() (legacyImportProver, func(), error) {
					return fixture.prover, func() {}, nil
				},
			}
			mutate(&opts)
			result := runLegacyProviderImport(context.Background(), opts)
			if len(result.mintSlots) != 0 {
				t.Fatal("flag path must not mint")
			}
			if _, err := os.Stat(filepath.Join(fixture.dir, legacyProviderMarkerName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("flag path wrote a marker")
			}
			for _, settings := range fixture.proxySettings {
				if _, err := os.Stat(fixture.target(settings)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("flag path wrote a credential")
				}
			}
			if committed, fallback, skipped, summaries := fixture.recorder.counts(); committed != 0 || fallback != 0 || skipped != 0 || summaries != 0 {
				t.Fatalf("flag path emitted records: %d/%d/%d/%d", committed, fallback, skipped, summaries)
			}
		})
	}
}

func TestLegacyImportMappingMatchesRunPaths(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)
	direct := legacyImportTargetPath(stateDir, nil)
	wantDirect, err := providerClientJwtPath(nil)
	if err != nil || direct != wantDirect {
		t.Fatalf("direct path %q != run path %q (%v)", direct, wantDirect, err)
	}
	for _, settings := range []*connect.ProxySettings{
		{Network: "tcp", Address: "127.0.0.1:18471"},
		{Network: "tcp", Address: "127.0.0.1:18472", Auth: &proxy.Auth{User: "fakeuser1"}},
	} {
		got := legacyImportTargetPath(stateDir, settings)
		want, err := providerClientJwtPath(settings)
		if err != nil || got != want {
			t.Fatalf("path %q != run path %q (%v)", got, want, err)
		}
		if legacyImportStoreKey(settings) != settings.Key() {
			t.Fatal("store key must be the connect proxy key")
		}
	}
}

func TestLegacyImportRecordsNeverCarryTokens(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	fixture.run(legacyImportHooks{})
	text := fixture.recorder.text()
	for _, secret := range []string{"synthetic-signature", "eyJ"} {
		if strings.Contains(text, secret) {
			t.Fatalf("import records leak %q", secret)
		}
	}
	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range store {
		if strings.Contains(text, entry.ClientID) {
			t.Fatal("import records carry a full client_id")
		}
	}
}

// The production record emitter is exercised with a pipe so the exporter
// accepts the sink, and its output is checked for credentials and identity.
func TestProviderDiagnosticsLegacyImportRecords(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := newProviderDiagnostics(context.Background(), writeEnd)
	if err != nil {
		t.Fatal(err)
	}
	output.legacyImportCommitted("abcd1234", "proxy:127.0.0.1:18471", "wrote .provider-0011223344556677.jwt")
	output.legacyImportFallback("abcd1234", "proxy:127.0.0.1:18471", "server_key_differs")
	output.legacyImportSummary(1, 1, 0)
	snapshot := output.snapshot()
	if snapshot == nil || snapshot.LegacyImport == nil || snapshot.LegacyImport.Imported != 1 || snapshot.LegacyImport.Fallback != 1 {
		t.Fatalf("snapshot counters = %#v", snapshot)
	}
	if err := output.close(); err != nil {
		t.Fatal(err)
	}
	writeEnd.Close()
	raw, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("records = %d, want 3: %s", len(lines), raw)
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record is not json: %s", line)
		}
		if record["schema"] != providerDiagnosticSchema {
			t.Fatalf("record schema = %v", record["schema"])
		}
		if strings.Contains(line, "eyJ") {
			t.Fatalf("record carries a token: %s", line)
		}
	}
}

func readLegacyStoreRaw(fixture *legacyImportFixture) (map[string]legacyClientJwtEntry, error) {
	var store map[string]legacyClientJwtEntry
	raw, err := os.ReadFile(filepath.Join(fixture.dir, legacyClientJwtStoreName))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, err
	}
	return store, nil
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// overrideRefreshError points every token's refresh answer at err.
func (self *legacyImportFixture) overrideRefreshError(settings *connect.ProxySettings, err error) {
	store, readErr := readLegacyStoreRaw(self)
	if readErr != nil {
		self.t.Fatal(readErr)
	}
	self.prover.refreshErr = map[string]error{store[settings.Key()].ByClientJWT: err}
}

// A transient proof read (the API was never reached) must not become a
// fallback: it is retried within the start.
func TestLegacyImportTransientProofRetriesWithinStart(t *testing.T) {
	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()

	fixture := newLegacyImportFixture(t, true, 2)
	fixture.captureOriginals()
	// Tier 1 cannot answer and the first refresh is a transient unavailable
	// read. The retry inside the same start proves the saved identities.
	fixture.prover.publicErr = errors.New("synthetic: api unavailable")
	fixture.prover.transientRemaining = 1
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatalf("a transient proof must not mint, got %v", result.mintSlots)
	}
	for _, settings := range fixture.proxySettings {
		if _, err := os.Stat(fixture.target(settings)); err != nil {
			t.Fatalf("credential for %s not imported after the retry: %v", settings.Key(), err)
		}
	}
	committed, fallbacks, _, _ := fixture.recorder.counts()
	if committed != len(fixture.proxySettings) || fallbacks != 0 {
		t.Fatalf("committed=%d fallbacks=%d, want %d and 0", committed, fallbacks, len(fixture.proxySettings))
	}
	fixture.assertOriginalsUntouched()
}

// A proof that stays unavailable is deferred: nothing is journaled, nothing is
// minted, the saved credential is untouched, and the next start proves it.
func TestLegacyImportTransientProofIsNotJournaledAsFallback(t *testing.T) {
	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()

	fixture := newLegacyImportFixture(t, true, 2)
	fixture.captureOriginals()
	fixture.prover.publicErr = errors.New("synthetic: api unavailable")
	fixture.prover.transientRemaining = legacyImportTransientAttempts * len(fixture.proxySettings)
	first := fixture.run(legacyImportHooks{})
	if len(first.mintSlots) != 0 {
		t.Fatalf("a transient proof must not mint, got %v", first.mintSlots)
	}
	journal := fixture.journal()
	for _, settings := range fixture.proxySettings {
		key := settings.Key()
		if recorded, ok := journal.Slots[key]; ok {
			t.Fatalf("slot %s journaled from a transient proof: %+v", key, recorded)
		}
		if _, err := os.Stat(fixture.target(settings)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("slot %s wrote a credential from a transient proof", key)
		}
	}
	_, fallbacks, _, _ := fixture.recorder.counts()
	if fallbacks != 0 {
		t.Fatalf("a transient proof recorded %d fallbacks", fallbacks)
	}
	if !strings.Contains(fixture.recorder.text(), "refresh_unavailable") {
		t.Fatalf("the unavailable read is not reported: %s", fixture.recorder.text())
	}
	fixture.assertOriginalsUntouched()

	// The next start proves the saved identities again and imports them.
	fixture.prover.publicErr = nil
	fixture.prover.transientRemaining = 0
	second := fixture.run(legacyImportHooks{})
	if len(second.mintSlots) != 0 {
		t.Fatalf("recovery must not mint, got %v", second.mintSlots)
	}
	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}
	journal = fixture.journal()
	for _, settings := range fixture.proxySettings {
		key := settings.Key()
		raw, err := os.ReadFile(fixture.target(settings))
		if err != nil {
			t.Fatalf("slot %s not imported on the next start: %v", key, err)
		}
		if string(raw) != store[key].ByClientJWT {
			t.Fatalf("slot %s credential is not the saved token", key)
		}
		if recorded := journal.Slots[key]; recorded.State != "imported" {
			t.Fatalf("slot %s journal state = %q, want imported", key, recorded.State)
		}
	}
	fixture.assertOriginalsUntouched()
}
