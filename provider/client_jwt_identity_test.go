package provider

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func newJWTStoreForTest(t *testing.T) *clientJWTStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return newClientJWTStore(filepath.Join(t.TempDir(), "jwts.json"))
}

func jwtEntryWithClient(clientID string) clientJWTEntry {
	return clientJWTEntry{ByClientJWT: "jwt-" + clientID, ClientID: clientID, NetworkID: "net-main", MintedAt: time.Now()}
}

func TestJWTStoreAdoptLegacy_SharedGatewayOneWinnerAndLegacyRemoved(t *testing.T) {
	s := newJWTStoreForTest(t)
	a, b := jwtRegressionSettings("gw.example:1080", "u1"), jwtRegressionSettings("gw.example:1080", "u2")
	winner, loser := jwtStoreKey(a), jwtStoreKey(b)
	if loser < winner {
		winner, loser = loser, winner
	}
	if err := s.Put("gw.example:1080", jwtEntryWithClient("legacy-client")); err != nil {
		t.Fatal(err)
	}

	adopted, split := s.AdoptLegacy([]*connect.ProxySettings{b, a})
	if adopted != 1 || split != 1 {
		t.Fatalf("adopted=%d split=%d, want 1 and 1", adopted, split)
	}
	if e, ok := s.Get(winner); !ok || e.ClientID != "legacy-client" {
		t.Fatalf("smallest key must inherit the legacy login, got ok=%v %+v", ok, e)
	}
	if _, ok := s.Get(loser); ok {
		t.Fatal("the other account must NOT inherit a shared identity: it mints fresh")
	}
	if _, ok := s.Get("gw.example:1080"); ok {
		t.Fatal("the legacy address-keyed slot must be removed after adoption")
	}
}

func TestJWTStoreAdoptLegacy_UnauthenticatedProxyKeepsTheBareSlot(t *testing.T) {
	s := newJWTStoreForTest(t)
	bare := &connect.ProxySettings{Network: "tcp", Address: "gw.example:1080"}
	a := jwtRegressionSettings("gw.example:1080", "u1")
	if err := s.Put("gw.example:1080", jwtEntryWithClient("bare-client")); err != nil {
		t.Fatal(err)
	}

	s.AdoptLegacy([]*connect.ProxySettings{bare, a})
	if e, ok := s.Get("gw.example:1080"); !ok || e.ClientID != "bare-client" {
		t.Fatalf("the unauthenticated proxy's slot must survive, got ok=%v %+v", ok, e)
	}
	if _, ok := s.Get(jwtStoreKey(a)); ok {
		t.Fatal("a credentialed account must not steal the unauthenticated proxy's login")
	}
}

func TestJWTStoreAdoptLegacy_IdempotentAndNeverOverwritesNewerEntry(t *testing.T) {
	s := newJWTStoreForTest(t)
	a := jwtRegressionSettings("gw.example:1080", "u1")
	if err := s.Put("gw.example:1080", jwtEntryWithClient("stale-legacy")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(jwtStoreKey(a), jwtEntryWithClient("current")); err != nil {
		t.Fatal(err)
	}

	s.AdoptLegacy([]*connect.ProxySettings{a})
	s.AdoptLegacy([]*connect.ProxySettings{a})
	if e, _ := s.Get(jwtStoreKey(a)); e.ClientID != "current" {
		t.Fatalf("a newer identity-keyed entry must never be overwritten, got %+v", e)
	}
	if _, ok := s.Get("gw.example:1080"); ok {
		t.Fatal("the stale legacy slot must be dropped")
	}
}

// Adopting saved logins must cost ONE rewrite of the store file however many
// move: every flush re-encodes and fsyncs the whole file, so per-login flushes
// stalled startup on nodes with thousands of saved logins.
func TestJWTStoreAdoptLegacy_OneFlushForManyLogins(t *testing.T) {
	s := newJWTStoreForTest(t)
	const n = 500

	var desired []*connect.ProxySettings
	seed := map[string]clientJWTEntry{}
	for i := 0; i < n; i++ {
		addr := "10.1." + strconv.Itoa(i/250) + "." + strconv.Itoa(i%250) + ":1080"
		seed[addr] = jwtEntryWithClient("c" + strconv.Itoa(i))
		desired = append(desired, jwtRegressionSettings(addr, "user"))
	}
	s.mu.Lock()
	s.loadLocked()
	if err := s.flushBatchLocked(seed, nil); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.flushes = 0
	s.mu.Unlock()

	adopted, split := s.AdoptLegacy(desired)
	if adopted != n || split != 0 {
		t.Fatalf("adopted=%d split=%d, want %d and 0", adopted, split, n)
	}
	if s.flushes != 1 {
		t.Fatalf("adoption flushed the store %d times, want exactly 1", s.flushes)
	}

	reloaded := newClientJWTStore(s.path)
	for i, d := range desired {
		got, ok := reloaded.Get(d.Key())
		if !ok || got.ClientID != "c"+strconv.Itoa(i) {
			t.Fatalf("identity key %q not persisted with its login: ok=%v got=%q", d.Key(), ok, got.ClientID)
		}
		if _, stale := reloaded.Get(d.Address); stale {
			t.Fatalf("legacy bare-address slot %q survived adoption", d.Address)
		}
	}

	if a, _ := s.AdoptLegacy(desired); a != 0 || s.flushes != 1 {
		t.Fatalf("second adoption moved %d logins and flushed %d times total, want 0 and still 1", a, s.flushes)
	}
}
