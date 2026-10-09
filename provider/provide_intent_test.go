package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestProvideIntentDefaultsOnAndCanBeDisabled(t *testing.T) {
	t.Setenv("URNETWORK_PROVIDE_INTENT", "")
	if !provideIntentEnabled() {
		t.Fatal("provide intent must default to on")
	}
	t.Setenv("URNETWORK_PROVIDE_INTENT", "1")
	if !provideIntentEnabled() {
		t.Fatal("URNETWORK_PROVIDE_INTENT=1 must keep it on")
	}
	t.Setenv("URNETWORK_PROVIDE_INTENT", "0")
	if provideIntentEnabled() {
		t.Fatal("URNETWORK_PROVIDE_INTENT=0 must turn it off")
	}
}

// Every ClientAuth the provider presents (first dial and each renewal) comes
// from one builder, so they declare the same intent.
func TestNewProviderClientAuthDeclaresIntent(t *testing.T) {
	t.Setenv("URNETWORK_PROVIDE_INTENT", "")
	id := connect.NewId()
	auth := newProviderClientAuth("jwt", id)
	if !auth.ProvideIntent || auth.ByJwt != "jwt" || auth.InstanceId != id {
		t.Fatalf("auth = %+v", auth)
	}
	t.Setenv("URNETWORK_PROVIDE_INTENT", "0")
	if newProviderClientAuth("jwt", id).ProvideIntent {
		t.Fatal("the opt-out must reach the auth the transport presents")
	}
}

// Both auth-client requests (mint and renewal) carry the declaration. The
// platform records provider status at creation, so the mint is the one that
// matters; renewal sends it for consistency.
func TestAuthClientRequestsCarryProvideIntent(t *testing.T) {
	t.Setenv("URNETWORK_PROVIDE_INTENT", "")
	renewal := newProviderAuthClientArgsForRenewal("d", connect.NewId())
	if !renewal.ProvideIntent {
		t.Fatal("renewal request must carry provide intent")
	}
	t.Setenv("URNETWORK_PROVIDE_INTENT", "0")
	if newProviderAuthClientArgsForRenewal("d", connect.NewId()).ProvideIntent {
		t.Fatal("the opt-out must reach the renewal request")
	}
}

func TestMintRequestCarriesProvideIntent(t *testing.T) {
	t.Setenv("URNETWORK_PROVIDE_INTENT", "")
	if !newProviderAuthClientArgsForMint("d").ProvideIntent {
		t.Fatal("the request that creates a client must carry provide intent")
	}
	if newProviderAuthClientArgsForMint("d").ClientId != nil {
		t.Fatal("a mint must not name a client id")
	}
	t.Setenv("URNETWORK_PROVIDE_INTENT", "0")
	if newProviderAuthClientArgsForMint("d").ProvideIntent {
		t.Fatal("the opt-out must reach the mint request")
	}
}

// A raw ClientAuth or auth-client request literal anywhere else would skip the
// declaration without any test noticing. Everything goes through the builders.
func TestNoRawClientAuthLiteralsOutsideTheBuilders(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "provide_intent.go" || name == "renewal_watcher.go" {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"connect.ClientAuth{", "connect.AuthNetworkClientArgs{"} {
			if strings.Contains(string(raw), banned) {
				t.Fatalf("%s builds %s directly; use newProviderClientAuth / newProviderAuthClientArgsForMint", name, banned)
			}
		}
	}
}
