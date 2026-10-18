package miner

// client_jwt_hotrestart.go ports the v33 hot-restart half of provider auth
// onto the miner's per-slot path: a stored client JWT is reused under the same
// client_id when the process restarts and hot restart is enabled, renewed with
// the same client_id when it has expired, and only minted fresh when the
// identity cannot safely be reused (no stored entry, a network_id mismatch, a
// tombstoned identity, or a renewal the platform refused).
//
// The v33 references are provider/provider_auth.go provideAuth (the reuse
// decision tree), provider/renewal_watcher.go (renewClientJWT and the renewal
// error classification), provider/proxy_warmth.go currentProviderNetworkID and
// provider/client_jwt_store.go (the store itself, ported beside this file).
// The machine-local pieces the v33 provider reads from ~/.urnetwork/jwt are
// read from the miner's own network token path (networkJwtPath), so a
// URNETWORK_STATE_DIR install stays self-contained.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/clientauth"
)

// clientJWTRenewAttempts bounds the in-place renewal retries for one slot
// before the caller's backoff takes over. v33 relies on its auth ladder to
// retry a deferred renewal; the miner authenticates each slot once per run, so
// a couple of quick retries keep a blip from failing the slot.
const clientJWTRenewAttempts = 3

// clientJWTRenewRetryDelay is the pause between those retries. A var so tests
// can shorten it.
var clientJWTRenewRetryDelay = 5 * time.Second

// errClientJWTInvalid is returned when a local token is definitely expired.
// Same semantics as the v33 provider's ErrTokenInvalid.
var errClientJWTInvalid = errors.New("auth: token is invalid or expired")

// validateJWTExpiry parses the JWT locally to check the 'exp' claim without
// signature verification. This is intentional: the JWTs parsed here are read
// from local disk and are only used for identity-reuse decisions, not for
// authorization. DO NOT reuse this function on network-supplied JWTs where
// signature verification is required (M9 security note). It returns
// errClientJWTInvalid if the token is definitely expired (with 30s leeway).
func validateJWTExpiry(byJwt string) error {
	expParser := gojwt.NewParser()
	if tok, _, parseErr := expParser.ParseUnverified(byJwt, gojwt.MapClaims{}); parseErr == nil {
		if claims, ok := tok.Claims.(gojwt.MapClaims); ok {
			if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp)+30 {
				return errClientJWTInvalid
			}
		}
	}
	return nil
}

// jwtContainsClientId reports whether the token's payload carries a client_id
// claim. A stored token without one cannot be handed to a connect client
// verbatim, so the reuse path renews it instead.
func jwtContainsClientId(byJwt string) bool {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return false
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return false
	}
	_, hasClientId := claims["client_id"]
	return hasClientId
}

// jwtClientId extracts the client_id claim without signature verification.
func jwtClientId(byJwt string) string {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return ""
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return ""
	}
	clientId, _ := claims["client_id"].(string)
	return clientId
}

// jwtNetworkId extracts the network_id claim from a JWT without signature
// verification. Intentional: the JWTs parsed here are read from local disk
// (~/.urnetwork/ or URNETWORK_STATE_DIR) and are used only for
// diagnostics/routing decisions, not for authorization. DO NOT reuse this
// function on network-supplied JWTs where signature verification is required
// (M9 security note).
func jwtNetworkId(byJwt string) (string, bool) {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return "", false
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return "", false
	}
	networkId, ok := claims["network_id"].(string)
	return networkId, ok
}

// accountNetworkId is the network_id claim of the account (network) JWT, or ""
// when the token is malformed or lacks the claim.
func accountNetworkId(byJwt string) string {
	if nid, ok := jwtNetworkId(byJwt); ok {
		return nid
	}
	return ""
}

// readNetworkJWT reads the account (network) sign-in token the miner keeps at
// networkJwtPath. The network token renewer may have rotated it, so renewal
// reads it fresh on every attempt.
func readNetworkJWT(path string) (string, error) {
	if path == "" {
		return "", errors.New("network token path is unset")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// currentProviderNetworkID extracts the network_id claim from the active
// network token file, falling back to any unambiguous stored network_id in the
// client JWT store, and then to the stored per-proxy credential files (see
// anyStoredProviderNetworkID, proxy_manager_warmth.go) when the store holds
// no unambiguous id. Ported from the v33 provider's proxy_warmth.go; the two
// fallbacks cover the miner's two on-disk representations of the same
// identities (.client_jwts.json and the .provider-*.jwt files), so a node
// whose store has not been written yet still resolves its network.
func currentProviderNetworkID() string {
	if path, err := providerStatePath("jwt"); err == nil && path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if nid, ok := jwtNetworkId(strings.TrimSpace(string(data))); ok && nid != "" {
				return nid
			}
		}
	}
	if store := loadGlobalClientJWTStore(); store != nil {
		if nid := store.AnyNetworkID(); nid != "" {
			return nid
		}
	}
	return anyStoredProviderNetworkID()
}

// clientJWTRejected reports whether a previous run recorded the platform's
// rejection of this slot's client credential. clientauth.MarkRejected writes
// the `<clientPath>.rejected` tombstone; a tombstoned identity is never
// reused, because the tombstone exists to force explicit recovery instead of
// a silent re-auth (clientauth/session.go).
func clientJWTRejected(clientJwtPath string) bool {
	if clientJwtPath == "" {
		return false
	}
	_, err := clientauth.ReadToken(clientJwtPath + ".rejected")
	return err == nil
}

// renewalTransientError marks a renewal that failed before the platform gave a
// verdict on the identity: a dropped connection, a timeout, a 5xx/408/429
// answer or a cancelled context. It says nothing about whether the client
// still exists, so the caller must retry rather than throw the identity away
// by minting a new one. A verdict ("Client does not exist.", a refusal, a
// mismatched client id) is a plain error and still falls through to a fresh
// mint.
type renewalTransientError struct{ err error }

func (e *renewalTransientError) Error() string { return e.err.Error() }
func (e *renewalTransientError) Unwrap() error { return e.err }

func isRenewalTransient(err error) bool {
	var transient *renewalTransientError
	return errors.As(err, &transient)
}

// renewalHTTPRefusal reports whether err is a non-200 HTTP answer that is a
// permanent refusal: a 4xx other than 408 (request timeout) and 429 (rate
// limited), which say "try again" rather than "no". Transport errors and 5xx
// carry no verdict. The status is read from the "<code> <text>: <body>" prefix
// that httpErrorFromResponse puts on every non-200 answer.
func renewalHTTPRefusal(err error) bool {
	msg := err.Error()
	if len(msg) < 4 || msg[3] != ' ' {
		return false
	}
	code, convErr := strconv.Atoi(msg[:3])
	if convErr != nil {
		return false
	}
	return 400 <= code && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// newProviderAuthClientArgsForRenewal builds the auth-client renewal request:
// naming the existing client_id is what makes the platform UPDATE that row
// instead of minting a new identity.
func newProviderAuthClientArgsForRenewal(description string, clientId connect.Id) *connect.AuthNetworkClientArgs {
	return &connect.AuthNetworkClientArgs{
		ClientId:    &clientId,
		Description: description,
		DeviceSpec:  "",
	}
}

// renewClientJWTFn is the injectable renewal entry point. It defaults to the
// real network-backed renewClientJWT but can be overridden in tests to exercise
// the renew-on-expiry branch of provideClientJWT deterministically.
var renewClientJWTFn = renewClientJWT

// renewClientJWT renews the client identity named by clientId with the network
// sign-in token byJwt as bearer, returning the platform's re-signed JWT (the
// same client_id, a fresh exp). Ported verbatim from the v33 provider's
// renewal_watcher.go so the failure classification and the identity guards are
// identical.
func renewClientJWT(ctx context.Context, apiUrl, byJwt string, clientId connect.Id, description string, clientStrategy *connect.ClientStrategy) (string, error) {
	if clientStrategy == nil {
		clientStrategy = connect.NewClientStrategyWithDefaults(ctx)
	}
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	callback, channel := connect.NewBlockingApiCallback[*connect.AuthNetworkClientResult](ctx)
	api.AuthNetworkClient(newProviderAuthClientArgsForRenewal(description, clientId), callback)

	var result connect.ApiCallbackResult[*connect.AuthNetworkClientResult]
	select {
	case <-ctx.Done():
		return "", &renewalTransientError{ctx.Err()}
	case result = <-channel:
	}
	if result.Error != nil {
		apiErr := fmt.Errorf("auth-client renewal api error: %w", result.Error)
		if renewalHTTPRefusal(result.Error) {
			// the platform answered with a permanent refusal (4xx other than
			// 408/429): that is a verdict, not an outage
			return "", apiErr
		}
		return "", &renewalTransientError{apiErr}
	}
	if result.Result == nil {
		return "", fmt.Errorf("empty result from auth-client renewal API")
	}
	if result.Result.Error != nil {
		return "", fmt.Errorf("auth-client renewal rejected: %s", result.Result.Error.Message)
	}
	if result.Result.ByClientJwt == "" {
		return "", fmt.Errorf("empty by_client_jwt in renewal response")
	}
	if !jwtContainsClientId(result.Result.ByClientJwt) {
		return "", fmt.Errorf("regression guard: renewal returned a JWT without client_id claim")
	}
	if got := jwtClientId(result.Result.ByClientJwt); got != clientId.String() {
		return "", fmt.Errorf("renewal returned client_id %q, want %q — refusing to swap", got, clientId.String())
	}
	return result.Result.ByClientJwt, nil
}

// clientJWTProvideConfig carries one slot's hot-restart inputs. Mint performs
// the fresh-identity path (the miner's durable registration custody,
// authenticateProvider); the caller closes over everything else it needs.
type clientJWTProvideConfig struct {
	// Api is the slot's API session. The reuse path binds it to the reused
	// client JWT so the rest of the slot runs under the stored identity.
	Api *sdk.Api
	// NetworkJwtPath is the account sign-in token path (the bearer a renewal
	// needs). ClientJwtPath is the slot's durable credential file.
	NetworkJwtPath string
	ClientJwtPath  string
	// KeyOwner, when set, persists a reused/renewed token through the same
	// descriptor-relative custody the refresh callbacks use.
	KeyOwner *clientauth.ProviderClientKeyOwner
	// ProxySettings identifies the slot; IdentityKey overrides the store key
	// (tests). Both default to the v33 identity key jwtStoreKey(settings).
	ProxySettings *connect.ProxySettings
	IdentityKey   string
	// ApiUrl, Description and ClientStrategySettings parameterize a renewal.
	// The strategy settings keep the renewal dial on the slot's proxy.
	ApiUrl                 string
	Description            string
	ClientStrategySettings *connect.ClientStrategySettings
	// Mint mints a fresh client identity. Required.
	Mint func(context.Context) (string, connect.Id, error)
}

// minerProviderDescription is the client description the miner sends at mint
// time (run.go's device description). The renewal sends the same string so the
// platform's row description stays stable across a renewal.
func minerProviderDescription() string {
	return fmt.Sprintf("provider %s %s", runtime.GOOS, RequireVersion())
}

// provideClientJWT returns the client JWT and client id for one proxy slot.
//
// With hot restart enabled (the v33 default; URNETWORK_HOT_RESTART=0 disables
// it) and a stored entry for this identity, the entry is reused without a
// server round trip when it is still valid and belongs to the current network.
// An expired entry is renewed with the same client_id so the identity's
// server-side reliability history survives. Anything that cannot be verified
// (no network_id on either side, a mismatch, an unparseable client_id, a
// refused renewal) falls through to a fresh mint. Every mint and renewal is
// persisted, so the next restart can reuse it.
func provideClientJWT(ctx context.Context, cfg clientJWTProvideConfig) (byClientJwt string, clientId connect.Id, reused bool, returnErr error) {
	identityKey := cfg.IdentityKey
	if identityKey == "" {
		identityKey = jwtStoreKey(cfg.ProxySettings)
	}

	networkJwt := ""
	if cfg.NetworkJwtPath != "" {
		if data, err := os.ReadFile(cfg.NetworkJwtPath); err == nil {
			networkJwt = strings.TrimSpace(string(data))
		}
	}

	// A stored client JWT is only safe to reuse under the network identity
	// that minted it — otherwise switching accounts (USER_AUTH change, new
	// sign-in token) would silently keep providing under the previous account.
	// An unscoped network_id on either side is treated as a mismatch (mint
	// fresh), since that's the safer default for credential reuse. Legacy
	// entries (stored with NetworkID="") are compatible when the current
	// account token carries a network_id, and the first reuse or renewal
	// stamps it.
	currentNetworkId, haveCurrentNetworkId := jwtNetworkId(networkJwt)

	if hotRestartEnabled() {
		if entry, ok := loadGlobalClientJWTStore().Get(identityKey); ok && !clientJWTRejected(cfg.ClientJwtPath) {
			if !haveCurrentNetworkId && entry.NetworkID != "" {
				currentNetworkId = entry.NetworkID
				haveCurrentNetworkId = true
			}
			// Fall back: if still missing, check if any entry in the store
			// has a known network_id.
			if !haveCurrentNetworkId {
				if knownNet := currentProviderNetworkID(); knownNet != "" {
					currentNetworkId = knownNet
					haveCurrentNetworkId = true
				}
			}

			description := cfg.Description
			if description == "" {
				description = minerProviderDescription()
			}

			switch {
			case !haveCurrentNetworkId || (entry.NetworkID != "" && entry.NetworkID != currentNetworkId):
				tlog("🔥 [hot-restart] %s: network_id mismatch (stored=%q current=%q have_current=%v), minting fresh\n", identityKey, entry.NetworkID, currentNetworkId, haveCurrentNetworkId)

			case validateJWTExpiry(entry.ByClientJWT) != nil:
				// Stored client JWT expired: renew-with-same-client_id before
				// falling through to fresh mint, so the operator's identities
				// (and their server-side reliability reputation) survive even
				// when the JWT has aged out of its 24h window.
				renewedJwt, renewedId, ok, err := renewStoredClientJWT(ctx, cfg, identityKey, entry, networkJwt, description, currentNetworkId)
				if err != nil {
					return "", connect.Id{}, false, err
				}
				if ok {
					bindReusedClientJWT(cfg, renewedJwt)
					return renewedJwt, renewedId, true, nil
				}
				// A verdict (the platform refused the identity) falls through
				// to the fresh mint.

			case !jwtContainsClientId(entry.ByClientJWT):
				// Stored JWT missing the client_id claim: prioritize salvaging
				// the identity via renewal instead of minting fresh if the
				// stored client_id is valid.
				renewedJwt, renewedId, ok, err := renewStoredClientJWT(ctx, cfg, identityKey, entry, networkJwt, description, currentNetworkId)
				if err != nil {
					return "", connect.Id{}, false, err
				}
				if ok {
					bindReusedClientJWT(cfg, renewedJwt)
					return renewedJwt, renewedId, true, nil
				}
				// Salvage refused: mint fresh.

			default:
				parsedId, parseErr := connect.ParseId(entry.ClientID)
				if parseErr != nil {
					tlog("🔥 [hot-restart] %s: stored client_id %q failed to parse (%v), minting fresh\n", identityKey, entry.ClientID, parseErr)
					break
				}
				// Self-heal: stamp the current network_id onto legacy entries
				// (stored with NetworkID="") so a future account/network swap
				// is detected by the mismatch guard above instead of silently
				// reusing a stale identity. Only fires for entries whose
				// NetworkID differs from the current one — after the first
				// reuse they match, so this is a one-time write per proxy at
				// startup, not per-auth.
				if entry.NetworkID != currentNetworkId {
					if putErr := loadGlobalClientJWTStore().Put(identityKey, clientJWTEntry{
						ByClientJWT: entry.ByClientJWT,
						ClientID:    entry.ClientID,
						NetworkID:   currentNetworkId,
						MintedAt:    entry.MintedAt,
					}); putErr != nil {
						tlog("⚠️ [jwt-store] failed to self-heal network_id for %s: %v\n", identityKey, putErr)
					}
				}
				bindReusedClientJWT(cfg, entry.ByClientJWT)
				return entry.ByClientJWT, parsedId, true, nil
			}
		}
	}

	byClientJwt, clientId, err := cfg.Mint(ctx)
	if err != nil {
		return "", connect.Id{}, false, err
	}
	// Always persist client JWTs so the store is ready the moment hot-restart
	// is enabled — no warmup or re-auth cycle needed. The read/reuse path
	// remains gated on hotRestartEnabled().
	if putErr := loadGlobalClientJWTStore().Put(identityKey, clientJWTEntry{
		ByClientJWT: byClientJwt,
		ClientID:    clientId.String(),
		NetworkID:   currentNetworkId,
		MintedAt:    time.Now(),
	}); putErr != nil {
		tlog("⚠️ [jwt-store] failed to persist client JWT for %s: %v\n", identityKey, putErr)
	}
	return byClientJwt, clientId, false, nil
}

// renewStoredClientJWT renews entry's identity with the same client_id.
//
// ok=false with no error means the platform gave a verdict (the identity is
// gone or unparseable) and the caller should mint fresh. A transient failure
// is retried a bounded number of times; if it still has no verdict the
// identity is kept, the entry is left untouched, and the returned error wraps
// a renewalTransientError so the caller backs off instead of minting a new
// identity.
func renewStoredClientJWT(
	ctx context.Context,
	cfg clientJWTProvideConfig,
	identityKey string,
	entry clientJWTEntry,
	networkJwt string,
	description string,
	currentNetworkId string,
) (renewedJwt string, renewedId connect.Id, ok bool, returnErr error) {
	parsedId, parseErr := connect.ParseId(entry.ClientID)
	if parseErr != nil {
		// An entry whose client_id cannot be parsed is not evidence about a
		// real identity; a renewal would have to guess. Mint fresh.
		tlog("🔥 [hot-restart] %s: stored client_id %q failed to parse (%v), minting fresh\n", identityKey, entry.ClientID, parseErr)
		return "", connect.Id{}, false, nil
	}
	if networkJwt == "" {
		// Without the account bearer a renewal cannot be attempted at all;
		// leave the entry in place and mint through the custody path, which
		// reports the real missing-sign-in cause.
		tlog("🔥 [hot-restart] %s: no network sign-in token available to renew identity %s; minting through custody\n", identityKey, parsedId)
		return "", connect.Id{}, false, nil
	}

	var lastErr error
	for attempt := 1; attempt <= clientJWTRenewAttempts; attempt++ {
		renewCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		renewedJwt, renewErr := renewClientJWTWithSlotStrategy(renewCtx, cfg, networkJwt, parsedId, description)
		cancel()
		if renewErr == nil {
			if putErr := loadGlobalClientJWTStore().Put(identityKey, clientJWTEntry{
				ByClientJWT: renewedJwt,
				ClientID:    parsedId.String(),
				NetworkID:   currentNetworkId,
				MintedAt:    time.Now(),
			}); putErr != nil {
				tlog("⚠️ [jwt-store] failed to persist renewed client JWT for %s: %v\n", identityKey, putErr)
			}
			tlog("🔥 [hot-restart] %s: renewed identity %s\n", identityKey, parsedId)
			return renewedJwt, parsedId, true, nil
		}
		lastErr = renewErr
		if !isRenewalTransient(renewErr) {
			// A verdict from the platform: the stored identity cannot be
			// renewed. Mint fresh (the custody path owns any tombstone).
			tlog("🔥 [hot-restart] %s: renewal of identity %s failed (%v), minting fresh\n", identityKey, parsedId, renewErr)
			return "", connect.Id{}, false, nil
		}
		if attempt < clientJWTRenewAttempts {
			tlog("🔥 [hot-restart] %s: renewal of identity %s failed without a verdict (%v); retrying in %s\n", identityKey, parsedId, renewErr, clientJWTRenewRetryDelay)
			select {
			case <-ctx.Done():
				return "", connect.Id{}, false, errors.Join(renewErr, ctx.Err())
			case <-time.After(clientJWTRenewRetryDelay):
			}
		}
	}
	tlog("🔥 [hot-restart] %s: stored client JWT expired, renewal of identity %s failed without a verdict (%v); keeping the identity and retrying instead of minting a new one\n", identityKey, parsedId, lastErr)
	return "", connect.Id{}, false, fmt.Errorf("renewal of %s deferred: %w", parsedId, lastErr)
}

// renewClientJWTWithSlotStrategy runs one renewal call on a client strategy
// built from the slot's settings, so the dial takes the same proxy route the
// slot itself uses. The temporary strategy is closed before returning.
func renewClientJWTWithSlotStrategy(ctx context.Context, cfg clientJWTProvideConfig, networkJwt string, clientId connect.Id, description string) (string, error) {
	var strategy *connect.ClientStrategy
	if cfg.ClientStrategySettings != nil {
		strategy = connect.NewClientStrategy(ctx, cfg.ClientStrategySettings)
		defer strategy.Close()
	}
	return renewClientJWTFn(ctx, cfg.ApiUrl, networkJwt, clientId, description, strategy)
}

// bindReusedClientJWT makes a reused or renewed token the slot's live
// credential: the API session signs calls with it, and the durable credential
// file beside the provider key is brought in line so the miner's other
// consumers (wallet proof, tooling) see the same identity.
func bindReusedClientJWT(cfg clientJWTProvideConfig, token string) {
	if token == "" {
		return
	}
	if cfg.Api != nil {
		cfg.Api.SetByJwt(token)
	}
	if cfg.ClientJwtPath == "" {
		return
	}
	// Keep the file write cheap and quiet when it is already current.
	if existing, err := clientauth.ReadToken(cfg.ClientJwtPath); err == nil && existing == token {
		return
	}
	var err error
	if cfg.KeyOwner != nil {
		err = cfg.KeyOwner.PersistClientJwt(cfg.ClientJwtPath, token)
	} else {
		err = clientauth.WriteToken(cfg.ClientJwtPath, token)
	}
	if err != nil {
		tlog("⚠️ [jwt-store] failed to persist reused client JWT to %s: %v\n", cfg.ClientJwtPath, err)
	}
}
