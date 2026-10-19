// Read-only view of the provider's client JWT store (.client_jwts.json).
//
// `provider claim --store-client=<key>` needs exactly what v33's
// provider/claim_credential.go read: the identity store, keyed by proxy
// address (or "direct") with an address+user form for credentialed proxies.
// The store WRITER (merge, inter-process lock, snapshots, prune) belongs to the
// in-flight client-JWT store port (feat/p2-hotswap, AU-07); this file is the
// CLI-side read the claim needs, against the same on-disk format, and must be
// replaced by the shared store API when that port lands.
package miner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urfoundation/sn/clientauth"
)

// clientJWTStoreFile is the store filename in the provider state directory.
const clientJWTStoreFile = ".client_jwts.json"

// clientJWTStoreEntry mirrors the v33 store entry. The read only consumes
// ByClientJWT, but the whole shape is kept so a corrupt entry is detected the
// same way.
type clientJWTStoreEntry struct {
	ByClientJWT string    `json:"by_client_jwt"`
	ClientID    string    `json:"client_id"`
	NetworkID   string    `json:"network_id"`
	MintedAt    time.Time `json:"minted_at"`
}

// clientJWTStoreRead is the loaded store: a missing or unreadable file is an
// empty store (v33 loadLocked), so every identity simply fails to resolve.
type clientJWTStoreRead struct {
	entries map[string]clientJWTStoreEntry
}

// readClientJWTStore loads the store without creating or rewriting it.
func readClientJWTStore(path string) *clientJWTStoreRead {
	store := &clientJWTStoreRead{entries: map[string]clientJWTStoreEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return store
	}
	var entries map[string]clientJWTStoreEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		// v33 snapshots the raw bytes and starts empty; the read-only view
		// cannot write a snapshot, and an empty store is the same outcome.
		return store
	}
	store.entries = entries
	return store
}

// Find resolves a user-typed key to a stored entry, exactly like the v33
// store's Find: an exact key wins; a bare proxy address also matches the
// credentialed identities at that address (their keys are address + "\x1f" +
// user), and resolves only when exactly one identity lives there. With several
// matches it returns them all (sorted) and no entry, so the caller refuses
// rather than guesses.
func (self *clientJWTStoreRead) Find(key string) (resolved string, entry clientJWTStoreEntry, candidates []string) {
	if e, ok := self.entries[key]; ok {
		return key, e, nil
	}
	prefix := key + "\x1f"
	for k := range self.entries {
		if strings.HasPrefix(k, prefix) {
			candidates = append(candidates, k)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 1 {
		return candidates[0], self.entries[candidates[0]], nil
	}
	return "", clientJWTStoreEntry{}, candidates
}

// Count returns how many identities the store holds.
func (self *clientJWTStoreRead) Count() int {
	return len(self.entries)
}

// claimClientToken accepts a stored token only if it names a client and has
// not expired, so a network token or a stale one fails here and not as an
// opaque server refusal (v33 provider/claim_credential.go
// clientClaimCredential). The expiry is read locally without checking the
// signature: the platform verifies the token authoritatively on the claim
// read, and this only spares the operator the round trip. 30s skew, spelled
// as v33 provider/main.go validateJWTExpiry. The token is never printed.
func claimClientToken(byJwt, source string) (string, error) {
	token := strings.TrimSpace(byJwt)
	if token == "" {
		return "", fmt.Errorf("%s is empty", source)
	}
	if _, err := clientauth.ClientIdFromJwt(token); err != nil {
		return "", fmt.Errorf("%s does not name a client (is it the network token?); claim needs a client token", source)
	}
	expParser := gojwt.NewParser()
	if tok, _, parseErr := expParser.ParseUnverified(token, gojwt.MapClaims{}); parseErr == nil {
		if claims, ok := tok.Claims.(gojwt.MapClaims); ok {
			if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp)+30 {
				return "", fmt.Errorf("%s has expired; let the provider renew it or pass a fresher --provider-jwt", source)
			}
		}
	}
	return token, nil
}

// snClaimCredentialFromOpts resolves `provider claim`'s credential source,
// porting v33 provider/claim_credential.go resolveClaimCredential: exactly one
// of --store-client, --provider-jwt and --legacy-coldkey. --store-client reads
// the identity store; without it the miner's existing selection is unchanged.
func snClaimCredentialFromOpts(opts docopt.Opts) (snCredentialSelection, error) {
	storeKey, _ := opts.String("--store-client")
	jwtPath, _ := opts.String("--provider-jwt")
	legacyColdkey, _ := opts.String("--legacy-coldkey")

	given := 0
	for _, v := range []string{storeKey, jwtPath, legacyColdkey} {
		if v != "" {
			given++
		}
	}
	if 1 < given {
		return snCredentialSelection{}, errors.New("pass only one of --store-client, --provider-jwt and --legacy-coldkey")
	}
	key := strings.TrimSpace(storeKey)
	if key == "" {
		return snCredentialsFromOpts(opts), nil
	}

	path, err := providerStatePath(clientJWTStoreFile)
	if err != nil {
		return snCredentialSelection{}, err
	}
	store := readClientJWTStore(path)
	resolved, entry, candidates := store.Find(key)
	if 1 < len(candidates) {
		return snCredentialSelection{}, fmt.Errorf("%d identities in %s share the address %q, one per proxy account. Pass the full key, the address then \\x1f then the user, e.g. --store-client=$'%s\\x1f<user>'", len(candidates), path, key, key)
	}
	if resolved == "" && len(candidates) == 0 {
		return snCredentialSelection{}, fmt.Errorf("no client %q in %s (%d identities). The key is the proxy address the identity was minted for, or \"direct\"", key, path, store.Count())
	}
	token, err := claimClientToken(entry.ByClientJWT, fmt.Sprintf("the client token stored for %q in %s", key, path))
	if err != nil {
		return snCredentialSelection{}, err
	}
	return snCredentialSelection{storeClient: key, storeJwt: token}, nil
}
