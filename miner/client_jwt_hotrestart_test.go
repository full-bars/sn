package miner

// client_jwt_hotrestart_test.go adapts the v33 hot-restart tests
// (provider/client_jwt_hotrestart_test.go at urnetwork-3.23-fix) to the
// miner's provideClientJWT. The v33 suite exercised provideAuth directly; the
// miner decides reuse before its durable-registration mint, so the fresh-mint
// path here is a fake injectable through the config's Mint seam.

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// createFakeJWTWithClaims builds an unsigned JWT with the given payload. The
// code under test parses these unverified on purpose (local expiry and
// identity checks only), so a fake signature is enough.
func createFakeJWTWithClaims(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return fmt.Sprintf("%s.%s.fakesig", header, payload)
}

// jwtRegressionSettings is an authenticated proxy identity (address + user).
// Its store key is settings.Key(), not the bare address.
func jwtRegressionSettings(addr, user string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: user, Password: "p"}}
}

// withGlobalStore swaps globalClientJWTStore to a fresh store backed by path
// for the duration of t, restoring the original on exit. Tests must isolate
// from the process-wide default (which points at the real ~/.urnetwork).
func withGlobalStore(t *testing.T, path string) func() {
	t.Helper()
	orig := loadGlobalClientJWTStore()
	storeGlobalClientJWTStore(newClientJWTStore(path))
	return func() { storeGlobalClientJWTStore(orig) }
}

// setHotRestart pins the URNETWORK_HOT_RESTART switch for one test.
func setHotRestart(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv("URNETWORK_HOT_RESTART", "1")
	} else {
		t.Setenv("URNETWORK_HOT_RESTART", "0")
	}
}

// clientJWTTestConfig builds an isolated config: a temp state dir (so
// currentProviderNetworkID reads no real state), a network token at
// networkJwt, and a slot credential file path.
func clientJWTTestConfig(t *testing.T, networkJwt string, proxySettings *connect.ProxySettings) clientJWTProvideConfig {
	t.Helper()
	dir := t.TempDir()
	networkPath := filepath.Join(dir, "jwt")
	if networkJwt != "" {
		if err := os.WriteFile(networkPath, []byte(networkJwt), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return clientJWTProvideConfig{
		NetworkJwtPath: networkPath,
		ClientJwtPath:  filepath.Join(dir, ".provider.jwt"),
		ProxySettings:  proxySettings,
		ApiUrl:         "https://api.invalid.example",
		Description:    "test",
	}
}

// fakeMint is the fresh-mint seam: it records calls and returns a canned
// identity.
type fakeMint struct {
	calls int
	jwt   string
	id    connect.Id
	err   error
}

func (m *fakeMint) mint(context.Context) (string, connect.Id, error) {
	m.calls++
	return m.jwt, m.id, m.err
}

func newFakeMint() *fakeMint {
	return &fakeMint{jwt: "fresh-jwt", id: connect.Id{9}}
}

// withTestRenewRetry zeroes the renewal retry pause so a transient-failure
// test does not sleep.
func withTestRenewRetry(t *testing.T) {
	t.Helper()
	orig := clientJWTRenewRetryDelay
	clientJWTRenewRetryDelay = time.Millisecond
	t.Cleanup(func() { clientJWTRenewRetryDelay = orig })
}

// --- store snapshot/prune tests (ported unchanged) --------------------------

func TestClientJWTStoreSnapshotWritesBak(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)
	store.entries["proxy-1"] = clientJWTEntry{
		ByClientJWT: "fake", ClientID: testClientId, MintedAt: time.Now(),
	}
	data := []byte(`{"proxy-1":{"by_client_jwt":"fake","client_id":"` + testClientId + `","network_id":"","minted_at":"2026-01-01T00:00:00Z"}}`)

	store.snapshotLocked(data, 1)

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, err := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(matches))
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("backup content mismatch:\nwant %q\ngot  %q", data, got)
	}
}

func TestClientJWTStoreSnapshotIsAtomic(t *testing.T) {
	// A successful snapshot must leave NO .tmp half-file behind — otherwise a
	// crash mid-write would leave an unparseable artifact that the next
	// dedup pass would read instead of the real backup.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	store.snapshotLocked([]byte(`{"k":1}`), 1)

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := filepath.Glob(filepath.Join(dir, base+"-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Fatalf("expected no .tmp left after atomic snapshot, got %v", tmp)
	}
}

func TestClientJWTStoreSnapshotDedupWithinInterval(t *testing.T) {
	// Identical content + recent previous snapshot → skip. Prevents a
	// crash-loop from burning all backup slots.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)
	store.entries["k"] = clientJWTEntry{ClientID: testClientId, MintedAt: time.Now()}

	data := []byte(`{"k":{"client_id":"` + testClientId + `"}}`)
	store.snapshotLocked(data, 1)
	store.snapshotLocked(data, 1) // identical content, recent → must skip

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != 1 {
		t.Fatalf("expected dedup to keep exactly 1 backup, got %d", len(matches))
	}
}

func TestClientJWTStoreSnapshotFiresWhenContentChanges(t *testing.T) {
	// Different content within the dedup window MUST create a new backup —
	// dedup is about identical-content crash loops, not about suppressing all
	// change-tracking.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	store.snapshotLocked([]byte(`{"v":1}`), 1)
	store.snapshotLocked([]byte(`{"v":2}`), 1)

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != 2 {
		t.Fatalf("expected 2 backups for changed content, got %d", len(matches))
	}
}

func TestClientJWTStoreSnapshotDistinctContentSameSecond(t *testing.T) {
	// Regression: two distinct-content snapshots in the same wall-clock
	// second used to collide on the filename (20060102T150405Z is second
	// resolution) and the second write's rename silently overwrote the
	// first. The timestamp format now carries microseconds
	// (20060102T150405.000000Z) so distinct content always lands in a
	// distinct file, even if both fire within the same second.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	store.snapshotLocked([]byte(`{"a":1}`), 1)
	store.snapshotLocked([]byte(`{"b":2}`), 1) // same second, different content

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != 2 {
		t.Fatalf("expected 2 distinct backups for distinct content in the same second, got %d (collision bug?)", len(matches))
	}
}

func TestClientJWTStorePruneKeepsMaxBackups(t *testing.T) {
	// Write clientJWTMaxBackups+3 backups, assert only MaxBackups remain,
	// and the OLDEST 3 were pruned.
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	for i := 0; i < clientJWTMaxBackups+3; i++ {
		// Content differs from the previous snapshot, so dedup never skips.
		store.snapshotLocked([]byte(`{"i":`+string(rune('0'+i%10))+`}`), 1)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	matches, _ := filepath.Glob(filepath.Join(dir, base+"-*.bak"))
	if len(matches) != clientJWTMaxBackups {
		t.Fatalf("expected exactly %d backups after prune, got %d", clientJWTMaxBackups, len(matches))
	}
}

func TestClientJWTStoreCorruptFileSnapshotsRawBytes(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	corrupt := []byte("not json at all")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	store := newClientJWTStore(path)

	if _, ok := store.Get("anything"); ok {
		t.Fatal("a corrupt store must read as empty")
	}

	matches, _ := filepath.Glob(filepath.Join(home, ".client_jwts.json-*.bak"))
	if len(matches) != 1 {
		t.Fatalf("expected the corrupt bytes to be snapshotted, got %d backups", len(matches))
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Errorf("snapshot = %q, want the raw corrupt bytes %q", got, corrupt)
	}
}

func TestClientJWTStoreSnapshotConcurrent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".client_jwts.json")
	store := newClientJWTStore(path)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			store.snapshotLocked([]byte(fmt.Sprintf(`{"n":%d}`, n)), 1)
		}(i)
	}
	wg.Wait()

	// The exact survivor set is racy by nature; the invariant is that every
	// snapshot artifact left behind is a complete file and the count is
	// bounded by the retention window.
	matches, _ := filepath.Glob(filepath.Join(home, ".client_jwts.json-*.bak"))
	if len(matches) == 0 || len(matches) > clientJWTMaxBackups {
		t.Fatalf("concurrent snapshots left %d backups, want 1..%d", len(matches), clientJWTMaxBackups)
	}
}

// --- provideClientJWT reuse/renew decision tree -----------------------------

func TestProvideClientJWTReusesValidUnexpiredEntry(t *testing.T) {
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	if err := loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1", // matches current → reuse
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, _, _ string, _ connect.Id, _ string, _ *connect.ClientStrategy) (string, error) {
		t.Fatal("renewal must NOT be called for a valid, unexpired entry")
		return "", nil
	}
	mint := newFakeMint()
	cfg.Mint = mint.mint

	byJwt, id, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if !reused {
		t.Error("expected reused=true for valid unexpired entry")
	}
	if id.String() != testClientId {
		t.Errorf("client_id = %q, want %q", id.String(), testClientId)
	}
	if byJwt != goodJwt {
		t.Errorf("returned jwt = %q, want %q", byJwt, goodJwt)
	}
	if mint.calls != 0 {
		t.Errorf("mint called %d times on the reuse path, want 0", mint.calls)
	}
}

func TestProvideClientJWTRenewsOnExpiredEntry(t *testing.T) {
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	expiredJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(-time.Hour).Unix()),
	})
	if err := loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: expiredJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now().Add(-25 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	renewedJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId, // same id — renewal keeps identity
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	calls := 0
	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, _, _ string, clientId connect.Id, _ string, _ *connect.ClientStrategy) (string, error) {
		calls++
		if clientId.String() != testClientId {
			t.Errorf("renewal called with clientId=%q, want %q", clientId.String(), testClientId)
		}
		return renewedJwt, nil
	}
	mint := newFakeMint()
	cfg.Mint = mint.mint

	byJwt, id, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if calls != 1 {
		t.Errorf("renewal called %d times, want 1", calls)
	}
	if !reused {
		t.Error("expected reused=true after successful renewal")
	}
	if id.String() != testClientId {
		t.Errorf("client_id = %q, want %q (must be preserved across renewal)", id.String(), testClientId)
	}
	if byJwt != renewedJwt {
		t.Errorf("returned jwt = %q, want renewed %q", byJwt, renewedJwt)
	}
	if mint.calls != 0 {
		t.Errorf("mint called %d times, want 0", mint.calls)
	}

	// The renewal must persist the fresh JWT so the next restart reuses it
	// without going to the network.
	stored, ok := loadGlobalClientJWTStore().Get("direct")
	if !ok {
		t.Fatal("renewed entry should be in store after Put")
	}
	if stored.ByClientJWT != renewedJwt {
		t.Errorf("store by_jwt = %q, want renewed %q", stored.ByClientJWT, renewedJwt)
	}
	if stored.ClientID != testClientId {
		t.Errorf("store client_id = %q, want %q", stored.ClientID, testClientId)
	}
}

func TestProvideClientJWTFallsThroughOnRenewalVerdict(t *testing.T) {
	// A renewal the platform answered with a verdict (e.g. the client no
	// longer exists) must fall through to a fresh mint; the expired entry is
	// left in place until the mint replaces it.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	expiredJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(-time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: expiredJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now().Add(-25 * time.Hour),
	})

	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, _, _ string, _ connect.Id, _ string, _ *connect.ClientStrategy) (string, error) {
		return "", errFakeRenew
	}
	freshJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": "00000000-0000-0000-0000-000000000002",
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	mint := &fakeMint{jwt: freshJwt, id: connect.Id{2}}
	cfg.Mint = mint.mint

	byJwt, _, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if reused {
		t.Fatal("a refused renewal must not report the identity as reused")
	}
	if mint.calls != 1 {
		t.Fatalf("mint called %d times, want 1", mint.calls)
	}
	if byJwt != freshJwt {
		t.Errorf("returned jwt = %q, want the fresh mint %q", byJwt, freshJwt)
	}
	stored, ok := loadGlobalClientJWTStore().Get("direct")
	if !ok || stored.ByClientJWT != freshJwt {
		t.Fatalf("mint must persist its fresh identity, got %+v ok=%v", stored, ok)
	}
}

func TestProvideClientJWTMismatchMintsFresh(t *testing.T) {
	// Stored NetworkID="net-X", current network token network_id="net-Y" →
	// mismatch, must NOT reuse, must NOT renew.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-Y",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	validJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: validJwt,
		ClientID:    testClientId,
		NetworkID:   "net-X", // mismatch
		MintedAt:    time.Now(),
	})

	calls := 0
	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, _, _ string, _ connect.Id, _ string, _ *connect.ClientStrategy) (string, error) {
		calls++
		return "", nil
	}
	freshJwt := "fresh-mismatch-jwt"
	mint := &fakeMint{jwt: freshJwt, id: connect.Id{3}}
	cfg.Mint = mint.mint

	byJwt, _, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if calls != 0 {
		t.Errorf("renewal called %d times on mismatch, want 0", calls)
	}
	if reused {
		t.Fatal("a network_id mismatch must mint fresh, not reuse")
	}
	if byJwt != freshJwt || mint.calls != 1 {
		t.Fatalf("mint result = %q calls=%d, want %q once", byJwt, mint.calls, freshJwt)
	}
}

func TestProvideClientJWTRejectsCurrentJWTWithoutNetworkID(t *testing.T) {
	// If the current network token has no network_id claim and the store has
	// none either, we can't verify the stored identity belongs to the same
	// account → fresh mint.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	validJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: validJwt,
		ClientID:    testClientId,
		NetworkID:   "", // legacy entry, current has none either
		MintedAt:    time.Now(),
	})

	mint := newFakeMint()
	cfg.Mint = mint.mint

	_, _, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if reused {
		t.Fatal("without a network_id on either side the identity must not be reused")
	}
	if mint.calls != 1 {
		t.Fatalf("mint called %d times, want 1", mint.calls)
	}
}

func TestProvideClientJWTFillsMissingNetworkIDFromStore(t *testing.T) {
	// The current network token lacks network_id, but the stored entry names
	// the network: that entry may be reused.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	})

	mint := newFakeMint()
	cfg.Mint = mint.mint

	byJwt, id, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if !reused || byJwt != goodJwt || id.String() != testClientId {
		t.Fatalf("expected the stored identity to be reused, reused=%v jwt=%q id=%q", reused, byJwt, id.String())
	}
}

func TestProvideClientJWTFallsBackToStoreAnyNetworkID(t *testing.T) {
	// Neither the network token nor this slot's entry names a network, but
	// another entry in the same store does: that unambiguous network_id lets
	// the slot reuse its stored identity.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	})
	_ = loadGlobalClientJWTStore().Put("other-proxy", clientJWTEntry{
		ByClientJWT: "other",
		ClientID:    "00000000-0000-0000-0000-000000000009",
		NetworkID:   "net-any",
		MintedAt:    time.Now(),
	})

	mint := newFakeMint()
	cfg.Mint = mint.mint

	_, _, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if !reused {
		t.Fatal("the store's unambiguous network_id must allow reuse")
	}
}

func TestProvideClientJWTSelfHealStampsNetworkID(t *testing.T) {
	// A legacy entry (NetworkID="") reused under a current token with a
	// network_id gets that network_id stamped exactly once, without touching
	// the stored token or client id.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	mintedAt := time.Now().Add(-time.Hour)
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "", // legacy
		MintedAt:    mintedAt,
	})

	mint := newFakeMint()
	cfg.Mint = mint.mint

	byJwt, id, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if !reused || byJwt != goodJwt || id.String() != testClientId {
		t.Fatalf("legacy entry not reused: reused=%v jwt=%q id=%q", reused, byJwt, id.String())
	}
	stored, ok := loadGlobalClientJWTStore().Get("direct")
	if !ok {
		t.Fatal("entry vanished")
	}
	if stored.NetworkID != "net-1" {
		t.Errorf("self-heal: store NetworkID = %q, want net-1", stored.NetworkID)
	}
	if stored.ByClientJWT != goodJwt {
		t.Errorf("self-heal: store by_jwt changed to %q, must preserve original", stored.ByClientJWT)
	}
	if stored.ClientID != testClientId {
		t.Errorf("self-heal: store client_id changed to %q, must preserve %q", stored.ClientID, testClientId)
	}
}

func TestProvideClientJWTReusesCredentialedProxyIdentity(t *testing.T) {
	// A credentialed proxy's saved login lives under its identity key
	// (address plus user), not its bare address. Looking it up under the
	// identity key is what makes a restart reuse instead of minting fresh.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	proxySettings := jwtRegressionSettings("gw.example:1080", "u1")
	cfg := clientJWTTestConfig(t, networkJwt, proxySettings)

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put(jwtStoreKey(proxySettings), clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	})

	mint := newFakeMint()
	cfg.Mint = mint.mint

	byJwt, id, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if !reused || id.String() != testClientId || byJwt != goodJwt {
		t.Fatalf("reused=%v id=%q, want the stored identity %q", reused, id.String(), testClientId)
	}

	// The bare address is NOT the key: looking up by it finds nothing, which
	// is exactly the failure the call site must avoid.
	if _, ok := loadGlobalClientJWTStore().Get(proxySettings.Address); ok {
		t.Fatalf("an entry exists under the bare address %q; the store key is the identity", proxySettings.Address)
	}
}

func TestProvideClientJWTKeepsIdentityOnTransientRenewalFailure(t *testing.T) {
	// A renewal that fails without a verdict (timeout, dropped connection,
	// HTTP failure) says nothing about the identity, so it must be kept and
	// reported for the caller's backoff, not thrown away by minting fresh.
	for _, tc := range []struct {
		name   string
		jwtExp time.Duration
		claims bool
	}{
		{"expired stored token", -time.Hour, true},
		{"stored token without client_id", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTestRenewRetry(t)
			setHotRestart(t, true)
			restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
			defer restoreStore()
			t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

			networkJwt := createFakeJWTWithClaims(map[string]interface{}{
				"client_id":  testClientId,
				"network_id": "net-1",
			})
			cfg := clientJWTTestConfig(t, networkJwt, nil)

			claims := map[string]interface{}{"exp": float64(time.Now().Add(tc.jwtExp).Unix())}
			if tc.claims {
				claims["client_id"] = testClientId
			}
			entry := clientJWTEntry{
				ByClientJWT: createFakeJWTWithClaims(claims),
				ClientID:    testClientId,
				NetworkID:   "net-1",
				MintedAt:    time.Now().Add(-25 * time.Hour),
			}
			_ = loadGlobalClientJWTStore().Put("direct", entry)

			origFn := renewClientJWTFn
			defer func() { renewClientJWTFn = origFn }()
			renewClientJWTFn = func(_ context.Context, _, _ string, _ connect.Id, _ string, _ *connect.ClientStrategy) (string, error) {
				return "", &renewalTransientError{errFakeRenew}
			}
			mint := newFakeMint()
			cfg.Mint = mint.mint

			_, _, reused, err := provideClientJWT(context.Background(), cfg)
			if err == nil || !isRenewalTransient(err) {
				t.Fatalf("provideClientJWT err = %v, want a deferred transient renewal error", err)
			}
			if reused {
				t.Fatal("a failed renewal must not report the identity as reused")
			}
			if mint.calls != 0 {
				t.Fatalf("mint called %d times on a deferred renewal, want 0 (the identity must be kept)", mint.calls)
			}
			stored, ok := loadGlobalClientJWTStore().Get("direct")
			if !ok || stored.ByClientJWT != entry.ByClientJWT || stored.ClientID != entry.ClientID {
				t.Fatalf("the stored identity must be left untouched, got %+v ok=%v", stored, ok)
			}
		})
	}
}

func TestProvideClientJWTMintsAndPuts(t *testing.T) {
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	freshJwt := "fresh-jwt-token"
	freshId := connect.Id{7}
	mint := &fakeMint{jwt: freshJwt, id: freshId}
	cfg.Mint = mint.mint

	byJwt, id, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if reused || byJwt != freshJwt || id != freshId {
		t.Fatalf("reused=%v jwt=%q id=%v, want the fresh mint", reused, byJwt, id)
	}
	stored, ok := loadGlobalClientJWTStore().Get("direct")
	if !ok {
		t.Fatal("the minted identity must be persisted for the next restart")
	}
	if stored.ByClientJWT != freshJwt || stored.ClientID != freshId.String() || stored.NetworkID != "net-1" {
		t.Fatalf("stored = %+v, want the fresh identity stamped with net-1", stored)
	}
}

func TestProvideClientJWTHotRestartDisabled(t *testing.T) {
	// URNETWORK_HOT_RESTART=0 turns the reuse path off entirely: the stored
	// entry is not read, not renewed, and not returned. The off path must also
	// not take the durable-registration mint, which reads and refreshes the
	// retained per-slot credential (reuse under another name): it must use the
	// explicit fresh-registration mint so a new client_id is registered.
	setHotRestart(t, false)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	})

	origFn := renewClientJWTFn
	defer func() { renewClientJWTFn = origFn }()
	renewClientJWTFn = func(_ context.Context, _, _ string, _ connect.Id, _ string, _ *connect.ClientStrategy) (string, error) {
		t.Fatal("renewal must not be called when hot restart is off")
		return "", nil
	}
	// The durable-registration mint stands for the credential-reading path:
	// hot restart off must leave it untouched.
	mint := newFakeMint()
	cfg.Mint = mint.mint
	freshJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": "00000000-0000-0000-0000-000000000003",
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	fresh := &fakeMint{jwt: freshJwt, id: connect.Id{3}}
	cfg.FreshMint = fresh.mint

	byJwt, _, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if reused {
		t.Fatal("hot restart off must mint fresh")
	}
	if byJwt != fresh.jwt || fresh.calls != 1 {
		t.Fatalf("fresh mint = %q calls=%d, want the fresh mint once", byJwt, fresh.calls)
	}
	if mint.calls != 0 {
		t.Fatalf("the credential-reading durable-registration mint ran %d times with hot restart off, want 0", mint.calls)
	}

	// The fresh identity is persisted for the next restart, so turning hot
	// restart back on loads the new credential instead of the replaced one.
	stored, ok := loadGlobalClientJWTStore().Get("direct")
	if !ok {
		t.Fatal("the fresh identity must be persisted for the next restart")
	}
	if stored.ByClientJWT != freshJwt || stored.ClientID != fresh.id.String() || stored.NetworkID != "net-1" {
		t.Fatalf("stored = %+v, want the fresh identity %q stamped with net-1", stored, fresh.id.String())
	}
}

func TestProvideClientJWTHotRestartOffRequiresFreshMint(t *testing.T) {
	// A caller that turns hot restart off cannot fall back to the retained
	// credential mint: the off contract is a new identity, so a missing fresh
	// mint is an error, never a silent reuse or refresh.
	setHotRestart(t, false)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)
	mint := newFakeMint()
	cfg.Mint = mint.mint

	if _, _, reused, err := provideClientJWT(context.Background(), cfg); err == nil || reused || mint.calls != 0 {
		t.Fatalf("hot restart off without a fresh mint = %v (reused=%v mint calls=%d), want a refusal", err, reused, mint.calls)
	}
}

func TestProvideClientJWTRejectedMarkerBypassesReuse(t *testing.T) {
	// A tombstoned credential (clientauth.MarkRejected) must never be
	// reused, even when a store entry still exists: the tombstone exists to
	// force explicit recovery.
	setHotRestart(t, true)
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	networkJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"network_id": "net-1",
	})
	cfg := clientJWTTestConfig(t, networkJwt, nil)

	goodJwt := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": testClientId,
		"exp":       float64(time.Now().Add(time.Hour).Unix()),
	})
	_ = loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: goodJwt,
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	})
	if err := os.WriteFile(cfg.ClientJwtPath+".rejected", []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}

	mint := newFakeMint()
	cfg.Mint = mint.mint

	_, _, reused, err := provideClientJWT(context.Background(), cfg)
	if err != nil {
		t.Fatalf("provideClientJWT err = %v", err)
	}
	if reused || mint.calls != 1 {
		t.Fatalf("reused=%v mint=%d, want a fresh mint past the tombstone", reused, mint.calls)
	}
}

// --- rejection eviction ------------------------------------------------------

func TestAuthLogoutEvictsHotRestartEntry(t *testing.T) {
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	dir := t.TempDir()
	clientJwtPath := filepath.Join(dir, ".provider.jwt")
	networkJwtPath := filepath.Join(dir, "jwt")
	if err := os.WriteFile(networkJwtPath, []byte("network-jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientJwtPath, []byte("client-jwt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadGlobalClientJWTStore().Put("direct", clientJWTEntry{
		ByClientJWT: "client-jwt",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	output, err := newProviderDiagnostics(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer output.close()
	callbacks := &providerAuthenticationCallbacks{
		diagnostics:    output,
		clientJwtPath:  clientJwtPath,
		networkJwtPath: networkJwtPath,
		identityKey:    "direct",
		cancel:         func() {},
	}
	callbacks.AuthLogout()

	if _, ok := loadGlobalClientJWTStore().Get("direct"); ok {
		t.Fatal("a rejected credential must be evicted from the hot-restart store")
	}
	// The custody tombstone is still what the durable-registration path reads.
	if _, err := os.Stat(clientJwtPath + ".rejected"); err != nil {
		t.Fatalf("expected the rejection tombstone beside the credential: %v", err)
	}
}

// --- renewal classification (ported) ----------------------------------------

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

var errFakeRenew = &fakeError{msg: "fake renewal failure"}

func TestIsRenewalTransientOnlyForUnverdictedFailures(t *testing.T) {
	if !isRenewalTransient(&renewalTransientError{errFakeRenew}) {
		t.Fatal("a marked error must classify as transient")
	}
	if !isRenewalTransient(fmt.Errorf("wrapped: %w", &renewalTransientError{errFakeRenew})) {
		t.Fatal("a wrapped marked error must classify as transient")
	}
	if isRenewalTransient(errFakeRenew) {
		t.Fatal("an unmarked error is a verdict and must not classify as transient")
	}
}

func renewTestApi(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func TestRenewClientJWTClassifiesFailures(t *testing.T) {
	clientId := connect.NewId()

	t.Run("an HTTP failure carries no verdict", func(t *testing.T) {
		url := renewTestApi(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := renewClientJWT(ctx, url, "account-jwt", clientId, "d", nil)
		if err == nil || !isRenewalTransient(err) {
			t.Fatalf("an HTTP 500 must be transient, got %v", err)
		}
	})

	t.Run("a permanent refusal is a verdict", func(t *testing.T) {
		url := renewTestApi(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Client does not exist."}`, http.StatusUnauthorized)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := renewClientJWT(ctx, url, "account-jwt", clientId, "d", nil)
		if err == nil {
			t.Fatal("a 401 must be an error")
		}
		if isRenewalTransient(err) {
			t.Fatalf("a 401 is a verdict and must not be transient, got %v", err)
		}
	})
}

func TestAccountNetworkIdParsing(t *testing.T) {
	if got := accountNetworkId(createFakeJWTWithClaims(map[string]interface{}{"network_id": "net-1"})); got != "net-1" {
		t.Fatalf("accountNetworkId = %q, want net-1", got)
	}
	if got := accountNetworkId("garbage"); got != "" {
		t.Fatalf("accountNetworkId(garbage) = %q, want empty", got)
	}
	if got := accountNetworkId(createFakeJWTWithClaims(map[string]interface{}{})); got != "" {
		t.Fatalf("accountNetworkId(no claim) = %q, want empty", got)
	}
}

func TestJwtClientIdHelpers(t *testing.T) {
	token := createFakeJWTWithClaims(map[string]interface{}{"client_id": testClientId})
	if !jwtContainsClientId(token) {
		t.Fatal("expected client_id claim to be found")
	}
	if got := jwtClientId(token); got != testClientId {
		t.Fatalf("jwtClientId = %q, want %q", got, testClientId)
	}
	if jwtContainsClientId("nonsense") {
		t.Fatal("a malformed token must not report a client_id claim")
	}
	if strings.TrimSpace(jwtClientId("")) != "" {
		t.Fatal("an empty token must yield an empty client id")
	}
}
