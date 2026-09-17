package provider

// provider_auth.go — provider-side auth: mint fresh client identities,
// reuse/renew persisted ones, and watch for revoked identities.
// Ported from main.go lines 4231-4810.
//
// Dependencies that already exist in this package:
//   - globalClientJWTStore, clientJWTEntry (client_jwt_store.go)
//   - resolveApiUrl (sn.go)
//   - shmLogFatal (shmlog_*.go)
//   - atomicWriteFile (stubs_batch_b.go)
//   - tlog (tlog.go)
//   - parseJWTExpiryTime (hotswap.go)
//   - jwtContainsClientId, jwtClientId, jwtNetworkId (renewal_watcher.go)
//   - formatDuration (renewal_watcher.go)
//   - formatBytes (proxy_health_log.go)
//   - hotRestartEnabled (control_socket.go)
//   - currentProviderNetworkID (proxy_warmth.go)
//   - validateJWTExpiry (stubs_batch_b.go)
//   - logDashboardLabel (control_socket.go)
//   - resolveNodeName (control_socket.go)
//   - RequireVersion (hotswap.go)
//   - isHotSwapDraining (hotswap.go)
//   - ProxyEverUp, ProxyAuthFailureCount (proxy_health.go)
//   - newProviderAuthClientArgsForRenewal, renewClientJWT (renewal_watcher.go)
//   - revokedIdentityAuthFailureThreshold (renewal_watcher.go)

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
)

// containerIDRe matches a default Docker container hostname (12-char hex),
// so we can omit it from the dashboard label when it carries no useful meaning.
var containerIDRe = regexp.MustCompile("^[0-9a-f]{12}$")

// ipDetectionDisabledPath returns ~/.urnetwork/disable_ip_autodetect, a file
// an operator can create to prevent the provider from fetching its public IP
// at startup/renewal. An empty file or missing file has no effect.
func ipDetectionDisabledPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "disable_ip_autodetect"), nil
}

// ipDetectionDisabled checks whether the operator has created the disable file.
func ipDetectionDisabled() bool {
	path, err := ipDetectionDisabledPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// resolvePublicIP returns the public IP advertised by the provider for
// dashboard identity. Priority:
//  1. URNETWORK_PUBLIC_IP env var
//  2. ~/.urnetwork/disable_ip_autodetect file exists → ""
//  3. cached fetch from ip.me (5s timeout) — cached for 60s
func resolvePublicIP() string {
	if v := strings.TrimSpace(os.Getenv("URNETWORK_PUBLIC_IP")); v != "" {
		return v
	}
	if ipDetectionDisabled() {
		return ""
	}
	if ip := getCachedPublicIP(); ip != "" {
		return ip
	}
	return ""
}

var (
	cachedIP        string
	cachedIPTime    time.Time
	cachedIPMu      sync.Mutex
	cachedIPRefresh bool
	ipCacheTTL      = 60 * time.Second
)

func getCachedPublicIP() string {
	cachedIPMu.Lock()
	now := time.Now()
	fresh := now.Sub(cachedIPTime) < ipCacheTTL
	ip := cachedIP
	shouldFetch := !fresh && !cachedIPRefresh
	var fetch func() string
	if shouldFetch {
		cachedIPRefresh = true
		fetch = fetchPublicIPFunc
	}
	cachedIPMu.Unlock()
	if fresh {
		return ip
	}
	if shouldFetch {
		go func() {
			newIP := fetch()
			cachedIPMu.Lock()
			if newIP != "" {
				cachedIP = newIP
				cachedIPTime = time.Now()
			}
			cachedIPRefresh = false
			cachedIPMu.Unlock()
		}()
	}
	return ip
}

var fetchPublicIPFunc = fetchPublicIP

func fetchPublicIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://ip.me", nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() == nil {
		return ""
	}
	return ip
}

// providerDescription builds the display-name string sent as the client
// description at mint AND renewal time.
func providerDescription(nodeName string) string {
	displayName := resolveNodeName(nodeName)
	if displayName == "" {
		hostname, _ := os.Hostname()
		if hostHostname := strings.TrimSpace(os.Getenv("HOST_HOSTNAME")); hostHostname != "" {
			displayName = hostHostname
		} else {
			displayName = hostname
		}
	}
	isContainerID := containerIDRe.MatchString(displayName)
	publicIP := resolvePublicIP()
	var dashboardLabel string
	if ip4 := net.ParseIP(publicIP).To4(); ip4 != nil {
		parts := strings.Split(ip4.String(), ".")
		redactedIP := fmt.Sprintf("%s.x.x.%s", parts[0], parts[3])
		if displayName == "" || isContainerID {
			dashboardLabel = redactedIP
		} else {
			dashboardLabel = fmt.Sprintf("%s @ %s", displayName, redactedIP)
		}
	} else {
		if displayName == "" || isContainerID {
			dashboardLabel = "provider"
		} else {
			dashboardLabel = displayName
		}
	}
	description := fmt.Sprintf("%s [%s]", dashboardLabel, RequireVersion())
	logDashboardLabel(description)
	return description
}

// renewClientJWTFn is the injectable renewal entry point. It defaults to the
// real network-backed renewClientJWT but can be overridden in tests to exercise
// the renew-on-expiry branch of provideAuth deterministically.
var renewClientJWTFn = renewClientJWT

func provideAuth(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, opts docopt.Opts, nodeName string, identityKey string) (byClientJwt string, clientId connect.Id, reused bool, returnErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// No HOME: no persisted JWT possible. Surface as a normal error, not
		// a panic (the old panic killed --version and one-shot commands in
		// bare environments; shakedown finding 2026-08-15).
		returnErr = fmt.Errorf("could not determine home directory: %w", err)
		return
	}
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")

	if _, err := os.Stat(jwtPath); errors.Is(err, os.ErrNotExist) {
		// jwt does not exist
		returnErr = fmt.Errorf("Jwt does not exist at %s", jwtPath)
		return
	}

	byJwtBytes, err := os.ReadFile(jwtPath)
	if err != nil {
		returnErr = err
		return
	}
	byJwt := strings.TrimSpace(string(byJwtBytes))

	// Layer 1: local pre-validation — avoids a network round-trip for an already-expired token.
	if err := validateJWTExpiry(byJwt); err != nil {
		returnErr = err
		return
	}

	// A stored client JWT is only safe to reuse under the network identity
	// that minted it — otherwise switching accounts (USER_AUTH change, new
	// ~/.urnetwork/jwt) would silently keep providing under the previous
	// account. An unscoped network_id claim on either side is treated as
	// a mismatch (mint fresh) rather than a match, since that's the safer
	// default for credential reuse.
	//
	// Hot restart (reuse of persisted client JWTs across process restarts)
	// has been ON by default since v3.23.0-fix.26 (URNETWORK_HOT_RESTART
	// != "0"). Explicitly set URNETWORK_HOT_RESTART=0 to disable.
	//
	// Legacy entries (stored with NetworkID="") may lack the network_id
	// field because they predate the field being added, or because the
	// account JWT at the time of storage had no network_id claim. These
	// are treated as compatible when the current account JWT DOES carry
	// a network_id (assumed same network, minted under the same account).
	// If the current JWT has no network_id claim, we always mint fresh
	// since we can't verify safe reuse. The first successful reuse or
	// renewal stamps the current NetworkID into the store, so subsequent
	// restarts get a clean match and any later account/network swap is
	// detected by the mismatch guard above rather than silently reusing
	// a stale identity.
	//
	// Compute description early — needed for both the renewal fallback path
	// (reuse an expired stored identity) and the fresh-mint path below.
	description := providerDescription(nodeName)

	currentNetworkId, haveCurrentNetworkId := jwtNetworkId(byJwt)
	if hotRestartEnabled() {
		if entry, ok := loadGlobalClientJWTStore().Get(identityKey); ok {
			// If the account JWT has no network_id claim, fill it in from the stored entry
			// to reuse/renew the existing client ID instead of minting a fresh one.
			if !haveCurrentNetworkId && entry.NetworkID != "" {
				currentNetworkId = entry.NetworkID
				haveCurrentNetworkId = true
			}
			// Fall back: if still missing, check if any entry in the store has a known network_id
			if !haveCurrentNetworkId {
				if knownNet := currentProviderNetworkID(); knownNet != "" {
					currentNetworkId = knownNet
					haveCurrentNetworkId = true
				}
			}
			if !haveCurrentNetworkId || (entry.NetworkID != "" && entry.NetworkID != currentNetworkId) {
				tlog("🔥 [hot-restart] %s: network_id mismatch (stored=%q current=%q have_current=%v), minting fresh\n", identityKey, entry.NetworkID, currentNetworkId, haveCurrentNetworkId)
			} else if reuseErr := validateJWTExpiry(entry.ByClientJWT); reuseErr != nil {
				// Stored client JWT expired: try renewal-with-same-client_id
				// before falling through to fresh mint, so the operator's
				// identities (and their server-side reliability reputation)
				// survive even when the JWT has aged out of its 24h window.
				if parsedId, parseErr := connect.ParseId(entry.ClientID); parseErr == nil {
					renewedJwt, renewErr := renewClientJWTFn(ctx, apiUrl, byJwt, parsedId, description, clientStrategy)
					if renewErr == nil {
						tlog("🔥 [hot-restart] %s: stored client JWT expired, renewed identity %s\n", identityKey, parsedId)
						if putErr := loadGlobalClientJWTStore().Put(identityKey, clientJWTEntry{
							ByClientJWT: renewedJwt,
							ClientID:    entry.ClientID,
							NetworkID:   currentNetworkId,
							MintedAt:    time.Now(),
						}); putErr != nil {
							tlog("⚠️ [jwt-store] failed to persist renewed client JWT for %s: %v\n", identityKey, putErr)
						}
						return renewedJwt, parsedId, true, nil
					}
					tlog("🔥 [hot-restart] %s: stored client JWT expired, renewal attempt failed (%v), minting fresh\n", identityKey, renewErr)
				} else {
					tlog("🔥 [hot-restart] %s: stored client JWT expired, invalid client_id %q (%v), minting fresh\n", identityKey, entry.ClientID, parseErr)
				}
			} else if !jwtContainsClientId(entry.ByClientJWT) {
				// Stored JWT missing client_id claim in payload: prioritize salvaging the
				// identity via renewal instead of minting fresh if client_id is valid.
				if parsedId, parseErr := connect.ParseId(entry.ClientID); parseErr == nil {
					renewedJwt, renewErr := renewClientJWTFn(ctx, apiUrl, byJwt, parsedId, description, clientStrategy)
					if renewErr == nil {
						tlog("🔥 [hot-restart] %s: stored client JWT missing client_id claim, salvaged identity via renewal: %s\n", identityKey, parsedId)
						if putErr := loadGlobalClientJWTStore().Put(identityKey, clientJWTEntry{
							ByClientJWT: renewedJwt,
							ClientID:    entry.ClientID,
							NetworkID:   currentNetworkId,
							MintedAt:    time.Now(),
						}); putErr != nil {
							tlog("⚠️ [jwt-store] failed to persist renewed client JWT for %s: %v\n", identityKey, putErr)
						}
						return renewedJwt, parsedId, true, nil
					}
					tlog("🔥 [hot-restart] %s: stored client JWT missing client_id claim, renewal salvage failed (%v), minting fresh\n", identityKey, renewErr)
				} else {
					tlog("🔥 [hot-restart] %s: stored client JWT missing client_id claim, minting fresh\n", identityKey)
				}
			} else if parsedId, parseErr := connect.ParseId(entry.ClientID); parseErr != nil {
				tlog("🔥 [hot-restart] %s: stored client_id %q failed to parse (%v), minting fresh\n", identityKey, entry.ClientID, parseErr)
			} else {
				reused = true
				// Self-heal: stamp the current network_id onto legacy
				// entries (stored with NetworkID="") so a future
				// account/network swap is detected by the mismatch guard
				// above instead of silently reusing a stale identity. Only
				// fires for entries whose NetworkID differs from the
				// current one — after the first reuse they match, so this
				// is a one-time write per proxy at startup, not per-auth.
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
				return entry.ByClientJWT, parsedId, true, nil
			}
		}
	}

	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	api.SetByJwt(byJwt)

	authClientCallback, authClientChannel := connect.NewBlockingApiCallback[*connect.AuthNetworkClientResult](ctx)

	// Final Description: "Identity [Version]" — computed by the shared helper
	// so mint (here) and in-process renewal (runProxyJWTWatcher) always send
	// the same string; the server UPDATEs the row's description on renewal.

	authClientArgs := &connect.AuthNetworkClientArgs{
		Description: description,
		DeviceSpec:  "",
	}

	api.AuthNetworkClient(authClientArgs, authClientCallback)

	var authClientResult connect.ApiCallbackResult[*connect.AuthNetworkClientResult]
	select {
	case <-ctx.Done():
		tlog("[auth] exiting: signal received during auth-client request\n")
		returnErr = ctx.Err()
		return
	case authClientResult = <-authClientChannel:
	}

	if authClientResult.Error != nil {
		returnErr = authClientResult.Error
		return
	}
	if authClientResult.Result == nil {
		returnErr = fmt.Errorf("auth response missing result")
		return
	}
	if authClientResult.Result.Error != nil {
		if authClientResult.Result.Error.ClientLimitExceeded {
			returnErr = fmt.Errorf("client limit exceeded: %s", authClientResult.Result.Error.Message)
			return
		}
		// Do NOT wrap in ErrTokenInvalid: that sentinel triggers shmLogFatal(78)
		// which kills the entire provider process. Auth-client rejections (bad
		// device spec, unrecognized description, temporary server error) should
		// retry via the normal backoff path, not crash all 120 nodes.
		returnErr = fmt.Errorf("auth-client rejected: %s", authClientResult.Result.Error.Message)
		return
	}

	byClientJwt = authClientResult.Result.ByClientJwt

	// parse the clientId
	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byClientJwt, gojwt.MapClaims{})
	if err != nil {
		returnErr = fmt.Errorf("failed to parse client JWT from API response: %w", err)
		return
	}

	claims, ok := token.Claims.(gojwt.MapClaims)
	if !ok {
		returnErr = fmt.Errorf("unexpected claims type in client JWT")
		return
	}

	clientIdStr, ok := claims["client_id"].(string)
	if !ok {
		returnErr = fmt.Errorf("client_id claim missing or not a string in client JWT")
		return
	}

	clientId, err = connect.ParseId(clientIdStr)
	if err != nil {
		returnErr = fmt.Errorf("invalid client_id in JWT claims: %w", err)
		return
	}

	// Always persist client JWTs so the store is ready the moment hot-restart
	// is enabled — no warmup or re-auth cycle needed. The read/reuse path
	// remains gated on URNETWORK_HOT_RESTART=1.
	if putErr := loadGlobalClientJWTStore().Put(identityKey, clientJWTEntry{
		ByClientJWT: byClientJwt,
		ClientID:    clientIdStr,
		NetworkID:   currentNetworkId,
		MintedAt:    time.Now(),
	}); putErr != nil {
		tlog("⚠️ [jwt-store] failed to persist client JWT for %s: %v\n", identityKey, putErr)
	}

	return
}

// watchReusedIdentityForRevocation polls proxyIndex's transport health and
// evicts identityKey from the client JWT store if the reused identity keeps
// failing to authenticate and never once comes up. See the call site for
// why this can't be detected any other way: the reuse path is intentionally
// server-round-trip-free, so nothing else observes a server-side rejection.
//
// revocationDone, when non-nil, is closed by the renewal watcher on a
// successful renewal: the identity is then demonstrably alive (the server
// just re-signed it), so this watcher stops before it evicts the fresh entry
// while the transport is still reconnecting.
func watchReusedIdentityForRevocation(ctx context.Context, identityKey string, proxyIndex int, revocationDone <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-revocationDone:
			tlog("🛑 [jwt-store] identity for %s renewed successfully — revocation watcher standing down\n", identityKey)
			return
		case <-ticker.C:
		}
		if ProxyEverUp(proxyIndex) {
			// Authenticated successfully at least once — the identity is good.
			return
		}
		if ProxyAuthFailureCount(proxyIndex) >= revokedIdentityAuthFailureThreshold {
			// Re-check the renewal signal AFTER the failure-count check: the
			// renewal watcher may have closed revocationDone while this
			// goroutine was between its select and this eviction decision
			// (renewal writes the store then closes the channel synchronously,
			// but we could have already passed the select). Without this
			// double-check, a successfully renewed identity could be evicted
			// out from under the reconnecting transport.
			select {
			case <-revocationDone:
				tlog("🛑 [jwt-store] identity for %s renewed successfully — revocation watcher standing down\n", identityKey)
				return
			default:
			}
			if delErr := loadGlobalClientJWTStore().Delete(identityKey); delErr != nil {
				tlog("⚠️ [jwt-store] failed to evict possibly-revoked identity for %s: %v\n", identityKey, delErr)
			} else {
				tlog("⚠️ [jwt-store] reused client identity for %s never authenticated after %d transport auth failures — evicted, will mint fresh on next retry/restart\n",
					identityKey, revokedIdentityAuthFailureThreshold)
			}
			return
		}
	}
}
