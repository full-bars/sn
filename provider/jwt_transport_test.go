package provider

import (
	"context"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urfoundation/sn/ss58"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A JWT goes out only over https or to a literal loopback host. Both requests
// that carry one must refuse before anything is dialed.
func TestJwtRequestsRefuseCleartextToARemoteHost(t *testing.T) {
	for _, apiUrl := range []string{
		"http://203.0.113.7:8080",
		"http://api.example.com",
		"http://user:pass@127.0.0.1:1",
		"ftp://127.0.0.1",
	} {
		target := hotkeyWalletSingleTarget(apiUrl, "network-jwt")
		var result map[string]any
		err := hotkeyWalletCall(context.Background(), target, http.MethodGet, "/sn/epoch", nil, &result)
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("hotkeyWalletCall(%q) must refuse with an https message, got %v", apiUrl, err)
		}
		if _, err := fetchPoolClaim(context.Background(), apiUrl, "client-jwt", 1, ""); err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("fetchPoolClaim(%q) must refuse with an https message, got %v", apiUrl, err)
		}
	}
}

// Loopback http stays usable (local test servers and operators on the box).
func TestJwtRequestsAllowLoopbackHttp(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"epoch": 5}`))
	}))
	defer server.Close()

	target := hotkeyWalletSingleTarget(server.URL, "network-jwt")
	var result map[string]any
	if err := hotkeyWalletCall(context.Background(), target, http.MethodGet, "/sn/epoch", nil, &result); err != nil {
		t.Fatalf("a loopback http request must work: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected one request, saw %d", hits.Load())
	}
}

// testClientJwt is an unsigned token that names a client and has not expired,
// the shape claim accepts.
func testClientJwt(t *testing.T, expiresIn time.Duration) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"client_id": "019e2d83-3118-5186-995f-aabe3b2dcf0b",
		"exp":       time.Now().Add(expiresIn).Unix(),
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// The delegation is network-scoped, so every box of one network shares one slot.
// A box whose hotkey differs from the one listed must not take the slot over on
// its own: two boxes with different hotkeys would otherwise take the network
// back from each other every hour, moving the paid coldkey each time.
func TestRefuseHotkeyTakeover(t *testing.T) {
	mine := [32]byte{1}
	other := [32]byte{2}
	entryFor := func(key [32]byte) *hotkeyWalletEntry {
		address, err := ss58.Encode(key, ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		return &hotkeyWalletEntry{ConsentScope: "hotkey", HotkeySs58: address}
	}

	if err := refuseHotkeyTakeover(nil, mine, false); err != nil {
		t.Fatalf("no listed delegation: nothing to take over, got %v", err)
	}
	if err := refuseHotkeyTakeover(entryFor(mine), mine, false); err != nil {
		t.Fatalf("the listed delegation is this hotkey's own: got %v", err)
	}
	err := refuseHotkeyTakeover(entryFor(other), mine, false)
	if err == nil || !strings.Contains(err.Error(), "--replace-other-hotkey") {
		t.Fatalf("another hotkey's delegation must be refused and name the opt-in, got %v", err)
	}
	if err := refuseHotkeyTakeover(entryFor(other), mine, true); err != nil {
		t.Fatalf("the explicit opt-in allows the takeover, got %v", err)
	}
	// an entry that cannot be read is not a reason to overwrite it silently
	if err := refuseHotkeyTakeover(&hotkeyWalletEntry{ConsentScope: "hotkey", HotkeySs58: "garbage"}, mine, false); err == nil {
		t.Fatal("an unreadable listed hotkey must be refused, not overwritten")
	}
}

// `hotkey status` must not say the hotkey's coldkey is being paid when a
// higher-precedence consent exists or the delegation window has passed.
func TestHotkeyWalletDelegationNotes(t *testing.T) {
	delegation := &hotkeyWalletEntry{ConsentScope: "hotkey", FromEpoch: 10, ThroughEpoch: 100}

	if notes := hotkeyWalletDelegationNotes(delegation, nil, 50, true); len(notes) != 1 || !strings.Contains(notes[0], "per-provider") {
		t.Fatalf("a live delegation still carries the per-provider precedence note, got %v", notes)
	}

	network := &hotkeyWalletEntry{ConsentScope: "network", ColdkeySs58: "5NetworkColdkey"}
	notes := strings.Join(hotkeyWalletDelegationNotes(delegation, network, 50, true), "\n")
	if !strings.Contains(notes, "outranks") || !strings.Contains(notes, "5NetworkColdkey") {
		t.Fatalf("a signed network consent must be named as outranking the delegation, got %q", notes)
	}

	notes = strings.Join(hotkeyWalletDelegationNotes(delegation, nil, 99, true), "\n")
	if !strings.Contains(notes, "not effective") {
		t.Fatalf("a delegation about to end must say it is not effective, got %q", notes)
	}

	notes = strings.Join(hotkeyWalletDelegationNotes(delegation, nil, 0, false), "\n")
	if strings.Contains(notes, "not effective") {
		t.Fatalf("an unknown operator epoch must not claim the window passed, got %q", notes)
	}
}
