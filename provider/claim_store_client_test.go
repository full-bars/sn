package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

func claimStoreTestStore(t *testing.T, entries map[string]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("URNETWORK_STATE_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := providerStatePath(".client_jwts.json")
	if err != nil {
		t.Fatal(err)
	}
	store := newClientJWTStore(path)
	for key, jwt := range entries {
		if err := store.Put(key, clientJWTEntry{ByClientJWT: jwt, ClientID: "c-" + key, MintedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaimStoreClientJwt(t *testing.T) {
	good := createFakeJWTWithClaims(map[string]interface{}{"client_id": "abc", "exp": float64(time.Now().Add(time.Hour).Unix())})
	network := createFakeJWTWithClaims(map[string]interface{}{"network_id": "n", "exp": float64(time.Now().Add(time.Hour).Unix())})
	expired := createFakeJWTWithClaims(map[string]interface{}{"client_id": "abc", "exp": float64(time.Now().Add(-time.Hour).Unix())})
	claimStoreTestStore(t, map[string]string{"direct": good, "net": network, "old": expired, "empty": " "})

	if got, err := claimStoreClientJwt("direct"); err != nil || got != good {
		t.Fatalf("a valid client token must be returned, got err=%v", err)
	}
	for key, want := range map[string]string{
		"missing:1080": "no client",
		"net":          "does not hold a client token",
		"old":          "expired",
		"empty":        "empty token",
	} {
		_, err := claimStoreClientJwt(key)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("key %q: err = %v, want it to mention %q", key, err, want)
		}
		if strings.Contains(err.Error(), good) || strings.Contains(err.Error(), network) {
			t.Fatalf("key %q: the error must never contain a token", key)
		}
	}
}

func TestRunClaimRefusesTwoCredentialSources(t *testing.T) {
	claimStoreTestStore(t, nil)
	err := runClaim(context.Background(), docopt.Opts{"--store-client": "direct", "--provider-jwt": "/x"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("two sources must be refused before any request, got %v", err)
	}
}
