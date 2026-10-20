// The identity-store read is the only credential source of `provider claim`
// that is neither a file nor the network token. These tests pin the v33
// resolution rules (exact key, bare proxy address, refusal to guess), the
// local token checks, and that the read never writes the store.
package miner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"
)

// storeTestJwt builds an unsigned local token. The store read and the claim's
// credential selection never verify a signature: the platform does that.
func storeTestJwt(t testing.TB, claims gojwt.MapClaims) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func storeTestClientJwt(t testing.TB, clientId, marker string, exp time.Time) string {
	t.Helper()
	return storeTestJwt(t, gojwt.MapClaims{
		"network_id": "00000000-0000-0000-0000-000000000301",
		"client_id":  clientId,
		"device_id":  "00000000-0000-0000-0000-000000000501",
		"exp":        exp.Unix(),
		"marker":     marker,
	})
}

// storeTestNetworkJwt is the account token: it names no client, so claim must
// refuse it (v33 provider/claim_credential.go clientClaimCredential).
func storeTestNetworkJwt(t testing.TB) string {
	t.Helper()
	return storeTestJwt(t, gojwt.MapClaims{
		"network_id": "00000000-0000-0000-0000-000000000301",
		"exp":        time.Now().Add(24 * time.Hour).Unix(),
	})
}

// writeStoreTestStore writes the on-disk store and returns its path.
func writeStoreTestStore(t *testing.T, entries map[string]clientJWTStoreEntry) string {
	t.Helper()
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)
	path := filepath.Join(stateDir, clientJWTStoreFile)
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The operator types the address, the address plus the user, or "direct"; the
// resolution must pick exactly the stored identity each time.
func TestClaimStoreClientResolvesExactAddressAndDirectKeys(t *testing.T) {
	directToken := storeTestClientJwt(t, "00000000-0000-0000-0000-000000000201", "direct", time.Now().Add(time.Hour))
	aliceToken := storeTestClientJwt(t, "00000000-0000-0000-0000-000000000202", "alice", time.Now().Add(time.Hour))
	writeStoreTestStore(t, map[string]clientJWTStoreEntry{
		"direct":                    {ByClientJWT: directToken, ClientID: "00000000-0000-0000-0000-000000000201"},
		"gw.example:1080\x1falice":  {ByClientJWT: aliceToken, ClientID: "00000000-0000-0000-0000-000000000202"},
		"other.example:1080\x1fbob": {ByClientJWT: storeTestClientJwt(t, "00000000-0000-0000-0000-000000000203", "bob", time.Now().Add(time.Hour)), ClientID: "00000000-0000-0000-0000-000000000203"},
	})
	for _, testCase := range []struct{ key, token string }{
		{"direct", directToken},
		{"gw.example:1080", aliceToken},
		{"gw.example:1080\x1falice", aliceToken},
	} {
		selection, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": testCase.key})
		if err != nil {
			t.Fatalf("%q: %v", testCase.key, err)
		}
		if selection.storeJwt != testCase.token {
			t.Fatalf("%q selected another identity", testCase.key)
		}
		if selection.storeClient != testCase.key {
			t.Fatalf("%q lost the operator's key: %q", testCase.key, selection.storeClient)
		}
		if selection.jwtFile != "" || selection.legacyNetwork {
			t.Fatalf("%q leaked file or legacy wiring: %#v", testCase.key, selection)
		}
		token, clientId, err := readSnCredentials(selection)
		if err != nil {
			t.Fatalf("%q claim credential: %v", testCase.key, err)
		}
		if token != testCase.token || clientId == nil {
			t.Fatalf("%q selected identity %v", testCase.key, clientId)
		}
	}
}

// Two identities behind one address cannot be guessed. The refusal counts them
// and echoes only what the operator typed, never the usernames on disk.
func TestClaimStoreClientRefusesAmbiguousAddressWithoutEchoingUsers(t *testing.T) {
	writeStoreTestStore(t, map[string]clientJWTStoreEntry{
		"gw.example:1080\x1falice": {ByClientJWT: storeTestClientJwt(t, "00000000-0000-0000-0000-000000000201", "alice", time.Now().Add(time.Hour))},
		"gw.example:1080\x1fbob":   {ByClientJWT: storeTestClientJwt(t, "00000000-0000-0000-0000-000000000202", "bob", time.Now().Add(time.Hour))},
	})
	_, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": "gw.example:1080"})
	if err == nil {
		t.Fatal("an ambiguous address selected an identity")
	}
	message := err.Error()
	if !strings.Contains(message, "2 identities") || !strings.Contains(message, "gw.example:1080") {
		t.Fatalf("refusal lost the count or the typed address: %q", message)
	}
	if strings.Contains(message, "alice") || strings.Contains(message, "bob") {
		t.Fatalf("refusal echoed a stored username: %q", message)
	}
}

// A missing, unreadable or corrupt store is an empty store, like v33's
// loadLocked, and reading never creates or rewrites the file.
func TestClaimStoreClientRefusesUnknownKeyAndAbsentOrCorruptStore(t *testing.T) {
	writeStoreTestStore(t, map[string]clientJWTStoreEntry{
		"direct": {ByClientJWT: storeTestClientJwt(t, "00000000-0000-0000-0000-000000000201", "direct", time.Now().Add(time.Hour))},
	})
	if _, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": "gone.example:1080"}); err == nil {
		t.Fatal("an unknown key selected an identity")
	} else if !strings.Contains(err.Error(), "(1 identities)") {
		t.Fatalf("refusal lost the store size: %q", err)
	}

	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)
	path := filepath.Join(stateDir, clientJWTStoreFile)
	if _, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": "direct"}); err == nil {
		t.Fatal("a missing store selected an identity")
	} else if !strings.Contains(err.Error(), "(0 identities)") {
		t.Fatalf("missing store refusal: %q", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the read created the store: %v", err)
	}

	corrupt := []byte("{not json")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": "direct"}); err == nil {
		t.Fatal("a corrupt store selected an identity")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(corrupt) {
		t.Fatalf("the read rewrote a corrupt store: %q", after)
	}
}

// The store key resolves to a token, but the token itself still has to name a
// client and be current; the refusals never print the token.
func TestClaimStoreClientRefusesEmptyExpiredAndNetworkTokens(t *testing.T) {
	expired := storeTestClientJwt(t, "00000000-0000-0000-0000-000000000201", "expired", time.Now().Add(-time.Minute))
	network := storeTestNetworkJwt(t)
	writeStoreTestStore(t, map[string]clientJWTStoreEntry{
		"empty.example:1080":   {ByClientJWT: "   "},
		"expired.example:1080": {ByClientJWT: expired},
		"network.example:1080": {ByClientJWT: network},
	})
	for _, testCase := range []struct{ key, want string }{
		{"empty.example:1080", "is empty"},
		{"expired.example:1080", "has expired"},
		{"network.example:1080", "does not name a client"},
	} {
		_, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": testCase.key})
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%q: want refusal %q, got %v", testCase.key, testCase.want, err)
		}
		for _, secret := range []string{expired, network} {
			if secret != "" && strings.Contains(err.Error(), secret) {
				t.Fatalf("%q refusal printed a token", testCase.key)
			}
		}
	}
}

// Exactly one credential source, as v33 required: the grammar refuses two and
// the resolver refuses two even when it is called directly. With no
// --store-client the miner's existing selection is untouched.
func TestClaimStoreClientIsOneSourceAndLeavesOtherSelectionsAlone(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	for _, arguments := range [][]string{
		{"claim", "--store-client=direct", "--provider-jwt=/synthetic/provider.jwt"},
		{"claim", "--store-client=direct", "--legacy-coldkey=synthetic-coldkey"},
	} {
		if _, err := parser.ParseArgs(mainUsage(), arguments, "synthetic"); err == nil {
			t.Fatalf("grammar accepted two credential sources: %v", arguments)
		}
	}
	opts := parseArgsForTest(t, []string{"claim", "--store-client=direct"})
	if key, err := opts.String("--store-client"); err != nil || key != "direct" {
		t.Fatalf("claim lost the store key: %q, %v", key, err)
	}

	if _, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": "direct", "--provider-jwt": "/synthetic/provider.jwt"}); err == nil {
		t.Fatal("the resolver accepted two credential sources")
	}
	selection, err := snClaimCredentialFromOpts(docopt.Opts{"--legacy-coldkey": "synthetic-coldkey"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.jwtFile != "" || !selection.legacyNetwork || selection.storeJwt != "" {
		t.Fatalf("--legacy-coldkey selection changed: %#v", selection)
	}
	if _, err := snClaimCredentialFromOpts(docopt.Opts{}); err != nil {
		t.Fatal(err)
	}
}

// A store credential has no file, so the path resolution that names files for
// the wallet and daemon paths must refuse it rather than fall back to
// .provider.jwt and authenticate as a different identity.
func TestStoreCredentialHasNoFilePath(t *testing.T) {
	selection := snCredentialSelection{storeClient: "direct", storeJwt: storeTestClientJwt(t, "00000000-0000-0000-0000-000000000201", "direct", time.Now().Add(time.Hour))}
	if _, err := snCredentialPath(selection); err == nil {
		t.Fatal("a store credential resolved a file path")
	}
	if _, _, err := readSnCredentials(snCredentialSelection{storeJwt: storeTestNetworkJwt(t)}); err == nil {
		t.Fatal("a store token without a client identity was accepted")
	}
}
