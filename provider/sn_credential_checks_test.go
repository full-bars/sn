package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

const snCredentialTestColdkey = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"

func snCredentialTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("URNETWORK_STATE_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// An invalid coldkey is refused locally, before the wallet request is built or
// any token is read: a typo must never reach the platform.
func TestSnSetWalletRefusesAnInvalidColdkeyBeforeAnyRequest(t *testing.T) {
	snCredentialTestHome(t) // no network jwt exists: reaching readNetworkJwt would give a different error
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := snSetWallet(ctx, connect.NewClientStrategyWithDefaults(ctx), server.URL, "not-an-address")
	if err == nil || !strings.Contains(err.Error(), "invalid ss58 coldkey") {
		t.Fatalf("an invalid coldkey must be refused locally, got %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("an invalid coldkey must not reach the platform, saw %d requests", requests.Load())
	}
}

// The platform answers 200 with an error payload; that must come back as an error.
func TestSnSetWalletSurfacesThePlatformErrorPayload(t *testing.T) {
	home := snCredentialTestHome(t)
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", "jwt"), []byte("network-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"wallet refused by the platform"}}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := snSetWallet(ctx, connect.NewClientStrategyWithDefaults(ctx), server.URL, snCredentialTestColdkey)
	if err == nil || !strings.Contains(err.Error(), "wallet refused by the platform") {
		t.Fatalf("the platform's error payload must be returned, got %v", err)
	}
}

func TestRunClaimRefusesBadCredentialInputsBeforeAnyRequest(t *testing.T) {
	home := snCredentialTestHome(t)
	empty := filepath.Join(home, "empty.jwt")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		opts docopt.Opts
		want string
	}{
		{"an empty --provider-jwt file", docopt.Opts{"--provider-jwt": empty}, "is empty"},
		{"an invalid --legacy-coldkey", docopt.Opts{"--legacy-coldkey": "not-an-address"}, "invalid --legacy-coldkey"},
		{"no credential at all", docopt.Opts{}, "--store-client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := runClaim(ctx, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	t.Run("the no-credential message names all three options", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := runClaim(ctx, docopt.Opts{})
		for _, want := range []string{"--store-client", "--provider-jwt", "--legacy-coldkey"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("the guidance must mention %s, got %v", want, err)
			}
		}
	})
}

// Signing needs an endpoint to broadcast through; refuse before fetching a claim.
func TestRunClaimKeyFileWithoutRpcIsRefusedBeforeFetching(t *testing.T) {
	home := snCredentialTestHome(t)
	jwtFile := filepath.Join(home, "client.jwt")
	if err := os.WriteFile(jwtFile, []byte("a-client-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := runClaim(ctx, docopt.Opts{
		"--provider-jwt": jwtFile,
		"--api_url":      server.URL,
		"--key_file":     "/some/key",
	})
	if err == nil || !strings.Contains(err.Error(), "--key_file needs --rpc") {
		t.Fatalf("--key_file without --rpc must be refused, got %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("nothing may be requested before the refusal, saw %d requests", requests.Load())
	}
}
