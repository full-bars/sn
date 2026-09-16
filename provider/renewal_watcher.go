package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// renewalLog formats and prints log messages for renewal events with standard timestamps.
func renewalLog(format string, args ...any) {
	fmt.Printf("%s "+format, append([]any{time.Now().Format("0102 15:04:05")}, args...)...)
}

// renewalMutex serializes /network/auth-client renewal calls process-wide so
// boxes with many proxies (50-60+) don't stampede the API with simultaneous
// requests and get them all rejected/rate-limited.
//
// The mutex deliberately covers the whole renewal (network call + store write
// + OOB/transport hot-swap): releasing it around just the store write would
// let the HTTP calls overlap, which is the stampede the mutex exists to
// prevent. Renewals are naturally staggered (each proxy mints at a different
// time, and the 12h pre-expiry window absorbs scheduling jitter), so actual
// contention is rare. Worst case for a 60-proxy box renewing back-to-back at
// ~300ms per auth-client call is ~18s of serialized renewals — acceptable,
// and it happens only on the pathological "every token expires at once" case.
var renewalMutex sync.Mutex

// proxyJWTRenewBefore is how long before a client JWT's exp claim the watcher
// renews it. Tokens live 24h on the beta backend (and now mainnet's new
// format), so this fires ~12h into a token's life, leaving 12h of runway for
// retries. On backends still issuing long-lived tokens (~51 days), the
// threshold simply never fires until the token is 12h from its own expiry —
// the watcher is a no-op there, preserving the old behavior.
const proxyJWTRenewBefore = 12 * time.Hour

// proxyJWTRenewTimeout bounds a single /network/auth-client renewal call so a
// hung transport cannot block the watcher (and the process-wide mutex)
// indefinitely. Mirrors the account-JWT refresher's verification timeout.
const proxyJWTRenewTimeout = 30 * time.Second

// revokedIdentityAuthFailureThreshold is how many transport auth failures a
// reused identity is allowed before it's treated as revoked rather than
// merely unlucky (network blip, transient API error).
const revokedIdentityAuthFailureThreshold = 5

// RenewalOOBControl defines the out-of-band control methods required by runProxyJWTWatcher.
//
// Adaptation note: In the fork, Audit401Count, ResetAudit401Count, and SetOn401 were added
// directly to connect.ApiOutOfBandControl. In v2026 connect, connect.ApiOutOfBandControl
// does not have 401 audit methods. RenewalOOBControl defines this interface, and RenewalOOB
// wraps connect.ApiOutOfBandControl to intercept and record 401 Unauthorized responses.
type RenewalOOBControl interface {
	SetByJwt(byJwt string)
	SetOn401(fn func())
	Audit401Count() uint64
	ResetAudit401Count()
}

// RenewalOOB wraps a connect.ApiOutOfBandControl to intercept and record 401
// Unauthorized responses and provide the audit/callback hooks needed by
// runProxyJWTWatcher.
type RenewalOOB struct {
	oob           *connect.ApiOutOfBandControl
	audit401Count atomic.Uint64
	on401         atomic.Value // func()
}

// NewRenewalOOB creates a RenewalOOB wrapping a newly constructed connect.ApiOutOfBandControl.
func NewRenewalOOB(ctx context.Context, clientStrategy *connect.ClientStrategy, byJwt string, apiUrl string) *RenewalOOB {
	return &RenewalOOB{
		oob: connect.NewApiOutOfBandControl(ctx, clientStrategy, byJwt, apiUrl),
	}
}

// WrapRenewalOOB wraps an existing connect.ApiOutOfBandControl.
func WrapRenewalOOB(oob *connect.ApiOutOfBandControl) *RenewalOOB {
	if oob == nil {
		return nil
	}
	return &RenewalOOB{oob: oob}
}

func (r *RenewalOOB) SetByJwt(byJwt string) {
	if r != nil && r.oob != nil {
		r.oob.SetByJwt(byJwt)
	}
}

func (r *RenewalOOB) SetOn401(fn func()) {
	if r == nil {
		return
	}
	if fn == nil {
		r.on401.Store((func())(nil))
		return
	}
	r.on401.Store(fn)
}

func (r *RenewalOOB) fireOn401() {
	if r == nil {
		return
	}
	if fn, ok := r.on401.Load().(func()); ok && fn != nil {
		fn()
	}
}

func (r *RenewalOOB) Audit401Count() uint64 {
	if r == nil {
		return 0
	}
	return r.audit401Count.Load()
}

func (r *RenewalOOB) ResetAudit401Count() {
	if r == nil {
		return
	}
	r.audit401Count.Store(0)
}

func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *connect.HttpStatusError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusUnauthorized
	}
	return strings.Contains(err.Error(), "401 Unauthorized")
}

func (r *RenewalOOB) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	if r == nil || r.oob == nil {
		if callback != nil {
			callback(nil, errors.New("nil OOB"))
		}
		return
	}
	r.oob.SendControl(frames, func(resultFrames []*protocol.Frame, err error) {
		if isUnauthorizedError(err) {
			r.audit401Count.Add(1)
			r.fireOn401()
		}
		if callback != nil {
			callback(resultFrames, err)
		}
	})
}

// proxyJWTWatcherConfig carries everything the per-proxy renewal watcher
// needs. Every field except Tick is populated by the provideWithProxy wiring.
type proxyJWTWatcherConfig struct {
	IdentityKey string
	ClientID    connect.Id
	// CurrentJWT is the live client JWT the proxy started with. It seeds the
	// watcher's expiry decision so a store-write failure at startup cannot
	// leave currentJwt empty (which would disable the exp-driven renewal).
	// Falls back to the store entry when empty.
	CurrentJWT string
	// Description is the client description sent with a renewal. It is a
	// fixed fallback for callers (and tests) that don't need it to track
	// runtime changes; DescribeFn, when set, takes priority and is
	// re-evaluated on every renewal instead.
	Description string
	// DescribeFn, when non-nil, recomputes the description immediately
	// before each renewal (startup check, hourly tick, or 401 fast-path)
	// instead of reusing the value captured when the watcher was started.
	// Without this, a runtime node rename (`urnet-tools rename`) or a
	// public-IP change picked up by ip-autodetect between renewals would
	// never reach the server: the watcher would keep sending the identity
	// string it had at startup for the proxy's entire lifetime.
	DescribeFn     func() string
	ApiURL         string
	ClientStrategy *connect.ClientStrategy
	OOB            RenewalOOBControl
	Transport      *connect.PlatformTransport
	// RenewNow is signaled by the OOB's 401 callback (and available for
	// tests to drive the fast-path directly).
	RenewNow chan struct{}
	// Tick overrides the internal 1h ticker in tests; nil means 1h.
	Tick       <-chan time.Time
	ProxyIndex int
	// InstanceId is the proxy's transport instance id from startup; it is
	// reused on every renewal so the server sees a stable session identity
	// instead of a fresh one per token rotation.
	InstanceId connect.Id
	// RevocationDone, when set, is closed by a successful renewal so the
	// pre-existing revocation watcher (watchReusedIdentityForRevocation)
	// stops racing the renewal watcher on the same store entry: without it,
	// a renewed-but-not-yet-reconnected proxy could be evicted as
	// "never authenticated" and the entry deleted under the fresh token.
	RevocationDone chan struct{}
}

// runProxyJWTWatcher renews the proxy's client JWT before it expires and
// immediately when the backend rejects the current token. It blocks until ctx
// is done.
//
// Renewal conditions (OR):
//  1. Immediately at startup if the cached client JWT is already within
//     proxyJWTRenewBefore of expiry (covers hot-restart with an expired or
//     near-expired token — no waiting for the first hourly tick).
//  2. The cached client JWT's exp is within proxyJWTRenewBefore (12h).
//  3. The OOB control observed a 401 since the last reset (renew-now signal
//     from SetOn401, or the counter polled on tick).
//  4. The platform transport accumulated auth failures (ProxyAuthFailureCount
//     >= threshold) — the transport auth path is separate from the OOB, and
//     a 401 there would otherwise go undetected until the hourly tick.
//
// On success the fresh JWT (SAME client_id — the server UPDATEs the existing
// network_client row) is hot-swapped into the OOB control and the platform
// transport, then persisted to the client-JWT store so a restart reuses it.
func runProxyJWTWatcher(ctx context.Context, cfg proxyJWTWatcherConfig) {
	tick := cfg.Tick
	if tick == nil {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		tick = ticker.C
	}

	// Wire the immediate 401 trigger: when the OOB control sees a 401 it
	// pings renewNow (non-blocking), so renewal fires at once instead of
	// waiting up to an hour for the next tick.
	if cfg.OOB != nil {
		cfg.OOB.SetOn401(func() {
			select {
			case cfg.RenewNow <- struct{}{}:
			default:
			}
		})
	}

	renew := func() bool {
		accountJWT, err := readAccountJWT()
		if err != nil {
			renewalLog("⚠️ [jwt-renew] proxy[%d] %s: cannot read account JWT: %v\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		renewalMutex.Lock()
		defer renewalMutex.Unlock()

		renewCtx, cancel := context.WithTimeout(ctx, proxyJWTRenewTimeout)
		defer cancel()

		// H-2: route through the shared adaptive rate limiter (same AIMD
		// throttle as the mint path). Without this, a 401 storm drives one
		// renewal per backend-401 — the fast-path fires as fast as the
		// backend rejects, and the mutex serializes but does not rate-limit.
		if err := globalAuthRateLimiter.Wait(renewCtx); err != nil {
			renewalLog("⚠️ [jwt-renew] proxy[%d] %s renewal skipped: rate limiter: %v\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		description := cfg.Description
		if cfg.DescribeFn != nil {
			description = cfg.DescribeFn()
		}
		newJwt, err := renewClientJWT(renewCtx, cfg.ApiURL, accountJWT, cfg.ClientID, description, cfg.ClientStrategy)
		// Feed the outcome back so the limiter's AIMD adjusts (429s halve
		// the rate, sustained success creeps it back up).
		globalAuthRateLimiter.ReportResult(err)
		if err != nil {
			renewalLog("⚠️ [jwt-renew] proxy[%d] %s renewal failed: %v (will retry)\n", cfg.ProxyIndex, cfg.IdentityKey, err)
			return false
		}

		if cfg.OOB != nil {
			cfg.OOB.SetByJwt(newJwt)
		}
		if cfg.Transport != nil {
			cfg.Transport.SetAuth(&connect.ClientAuth{
				ByJwt:      newJwt,
				InstanceId: cfg.InstanceId,
				AppVersion: RequireVersion(),
			})
		}
		// Keep the previous NetworkID when the account JWT parse fails: a
		// mismatch would make the store treat the entry as mint-fresh on
		// restart, silently losing the identity we just renewed.
		networkID := accountNetworkId(accountJWT)
		if networkID == "" {
			if prev, ok := globalClientJWTStore.Get(cfg.IdentityKey); ok {
				networkID = prev.NetworkID
			}
		}
		if err := globalClientJWTStore.Put(cfg.IdentityKey, clientJWTEntry{
			ByClientJWT: newJwt,
			ClientID:    cfg.ClientID.String(),
			NetworkID:   networkID,
			MintedAt:    time.Now(),
		}); err != nil {
			renewalLog("⚠️ [jwt-renew] proxy[%d] %s renewal OK in memory but store write failed: %v — keeping old token armed for retry\n",
				cfg.ProxyIndex, cfg.IdentityKey, err)
			// Do NOT ResetAudit401Count and do NOT stand down the revocation
			// watcher: the in-memory swap is live but the persistence failed, so
			// a restart would load the old (expiring) token. Keep the 401 counter
			// armed and currentJwt on the old token so the next trigger renews
			// again; the disk failure is logged loudly above.
			return false
		}
		if cfg.OOB != nil {
			cfg.OOB.ResetAudit401Count()
		}
		// The identity is demonstrably alive (server just re-signed it), so
		// the revocation watcher must stand down — otherwise it can evict
		// this entry while the transport is still reconnecting.
		if cfg.RevocationDone != nil {
			select {
			case <-cfg.RevocationDone:
			default:
				close(cfg.RevocationDone)
			}
		}
		if exp := parseJWTExpiryTime(newJwt); exp != nil {
			renewalLog("🔁 [jwt-renew] proxy[%d] %s renewed client JWT (client_id %s, exp %s)\n",
				cfg.ProxyIndex, cfg.IdentityKey, cfg.ClientID, formatDuration(time.Until(*exp)))
		} else {
			renewalLog("🔁 [jwt-renew] proxy[%d] %s renewed client JWT (client_id %s)\n",
				cfg.ProxyIndex, cfg.IdentityKey, cfg.ClientID)
		}
		return true
	}

	// H2: track the live token in the watcher, not just the store. A store
	// entry can vanish (mint-time Put failure, revocation-watcher eviction,
	// disk error), which would silently disable the exp-driven check. The
	// store remains the persistence sink; the live copy is the source of
	// truth for the expiry decision. Prefer the JWT the proxy actually
	// started with (CurrentJWT) — it is in scope at the call site and can't
	// be missing due to a store write failure.
	currentJwt := cfg.CurrentJWT
	if currentJwt == "" {
		if entry, ok := globalClientJWTStore.Get(cfg.IdentityKey); ok {
			currentJwt = entry.ByClientJWT
		}
	}

	// H-4: snapshot the transport auth-failure count at startup and refresh
	// it after every successful renewal. ProxyAuthFailureCount is a CUMULATIVE
	// lifetime counter — comparing it raw against the threshold would renew
	// every hour forever once a flaky spell crossed 5 at any point. Comparing
	// against a baseline means only NEW failures since the last renewal
	// trigger the transport-auth fast path.
	authFailureBaseline := int64(0)
	if cfg.ProxyIndex >= 0 {
		authFailureBaseline = ProxyAuthFailureCount(cfg.ProxyIndex)
	}

	renewIfNeeded := func(reason string) {
		need := false
		if cfg.OOB != nil {
			need = cfg.OOB.Audit401Count() > 0
		}
		if !need && cfg.ProxyIndex >= 0 && ProxyAuthFailureCount(cfg.ProxyIndex)-authFailureBaseline >= revokedIdentityAuthFailureThreshold {
			// The transport auth path is separate from the OOB; repeated
			// transport auth failures mean the bearer is being rejected at
			// the data-plane level even if no OOB call has fired.
			need = true
		}
		if !need && currentJwt != "" {
			if exp := parseJWTExpiryTime(currentJwt); exp != nil && time.Until(*exp) < proxyJWTRenewBefore {
				need = true
			}
		}
		if need {
			renewalLog("🔁 [jwt-renew] proxy[%d] %s renewal trigger: %s\n", cfg.ProxyIndex, cfg.IdentityKey, reason)
			if renew() {
				if entry, ok := globalClientJWTStore.Get(cfg.IdentityKey); ok {
					currentJwt = entry.ByClientJWT
				}
				if cfg.ProxyIndex >= 0 {
					authFailureBaseline = ProxyAuthFailureCount(cfg.ProxyIndex)
				}
			}
		}
	}

	// Startup check: a hot-restart that reused an already-expired client JWT
	// must not sit as a black hole until the first hourly tick.
	renewIfNeeded("startup check")

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			renewIfNeeded("hourly check")
		case <-cfg.RenewNow:
			renewIfNeeded("401 fast-path")
		}
	}
}



// readAccountJWT reads the account (network) JWT from disk. The account-JWT
// refresher may have rotated it, so renewal reads it fresh on every attempt.
func readAccountJWT() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, ".urnetwork", "jwt"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func newProviderAuthClientArgsForRenewal(description string, clientId connect.Id) *connect.AuthNetworkClientArgs {
	return &connect.AuthNetworkClientArgs{
		ClientId:    &clientId,
		Description: description,
		DeviceSpec:  "",
	}
}

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
		return "", ctx.Err()
	case result = <-channel:
	}
	if result.Error != nil {
		return "", fmt.Errorf("auth-client renewal api error: %w", result.Error)
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


func jwtContainsClientId(byJwt string) bool {
	parser := jwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, jwt.MapClaims{})
	if err != nil {
		return false
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	_, hasClientId := claims["client_id"]
	return hasClientId
}

func jwtClientId(byJwt string) string {
	parser := jwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, jwt.MapClaims{})
	if err != nil {
		return ""
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return ""
	}
	clientId, _ := claims["client_id"].(string)
	return clientId
}

func jwtNetworkId(byJwt string) (string, bool) {
	parser := jwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, jwt.MapClaims{})
	if err != nil {
		return "", false
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", false
	}
	networkId, ok := claims["network_id"].(string)
	return networkId, ok
}

func accountNetworkId(byJwt string) string {
	if nid, ok := jwtNetworkId(byJwt); ok {
		return nid
	}
	return ""
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}


