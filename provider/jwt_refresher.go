package provider

// jwt_refresher.go — JWT refresh and periodic refresher.
// Ported from main.go lines 2304-2550.
//
// AuthCodeCreateArgs/Result are defined locally because connect v2026
// does not expose the auth-code-create endpoint (used only for
// self-renewal of the network JWT).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// AuthCodeCreateArgs is the request for AuthCodeCreate, which creates a
// single-use auth code that can be exchanged for a fresh network JWT.
type AuthCodeCreateArgs struct{}

// AuthCodeCreateError is the error payload from AuthCodeCreate.
type AuthCodeCreateError struct {
	Message               string `json:"message"`
	AuthCodeLimitExceeded bool   `json:"auth_code_limit_exceeded"`
}

// AuthCodeCreateResult is the response from AuthCodeCreate.
type AuthCodeCreateResult struct {
	Error    *AuthCodeCreateError `json:"error,omitempty"`
	AuthCode string               `json:"auth_code"`
}

func refreshJWT(ctx context.Context, apiUrl, byJwt string) (string, error) {
	tlog("🔑 [jwt] refresh → step 1/3: requesting auth code...\n")

	// AuthCodeCreate is not exposed by BringYourApi in connect v2026, so we
	// call the REST endpoint directly with the network JWT as Bearer.
	ccBody, err := json.Marshal(&AuthCodeCreateArgs{})
	if err != nil {
		return "", fmt.Errorf("code-create marshal: %w", err)
	}
	ccReq, err := http.NewRequestWithContext(ctx, "POST", apiUrl+"/auth/code-create", strings.NewReader(string(ccBody)))
	if err != nil {
		return "", fmt.Errorf("code-create request: %w", err)
	}
	ccReq.Header.Set("Authorization", "Bearer "+byJwt)
	ccReq.Header.Set("Content-Type", "application/json")
	ccClient := &http.Client{Timeout: 30 * time.Second}
	ccResp, err := ccClient.Do(ccReq)
	if err != nil {
		return "", fmt.Errorf("code-create api error: %w", err)
	}
	ccRespBody, _ := io.ReadAll(io.LimitReader(ccResp.Body, 64*1024))
	ccResp.Body.Close()
	if ccResp.StatusCode >= 400 {
		return "", fmt.Errorf("code-create rejected (HTTP %d): %s", ccResp.StatusCode, string(ccRespBody))
	}
	var ccResult AuthCodeCreateResult
	if err := json.Unmarshal(ccRespBody, &ccResult); err != nil {
		return "", fmt.Errorf("code-create unmarshal: %w", err)
	}
	if ccResult.Error != nil {
		if ccResult.Error.AuthCodeLimitExceeded {
			return "", fmt.Errorf("auth code limit exceeded: %s", ccResult.Error.Message)
		}
		return "", fmt.Errorf("code-create rejected: %s", ccResult.Error.Message)
	}
	if ccResult.AuthCode == "" {
		return "", fmt.Errorf("empty auth code in response")
	}

	tlog("🔑 [jwt] refresh → step 1/3 ok: auth code received (%d chars)\n", len(ccResult.AuthCode))

	tlog("🔑 [jwt] refresh → step 2/3: exchanging auth code for network JWT...\n")

	// Step 2 uses BringYourApi.AuthCodeLogin (available in connect v2026).
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	clCallback, clChannel := connect.NewBlockingApiCallback[*connect.AuthCodeLoginResult](ctx)
	api.AuthCodeLogin(&connect.AuthCodeLoginArgs{
		AuthCode: ccResult.AuthCode,
	}, clCallback)

	var clResult connect.ApiCallbackResult[*connect.AuthCodeLoginResult]
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case clResult = <-clChannel:
	}
	if clResult.Error != nil {
		return "", fmt.Errorf("code-login api error: %w", clResult.Error)
	}
	if clResult.Result == nil {
		return "", fmt.Errorf("empty result from code-login API")
	}
	if clResult.Result.Error != nil {
		return "", fmt.Errorf("code-login rejected: %s", clResult.Result.Error.Message)
	}
	if clResult.Result.ByJwt == "" {
		return "", fmt.Errorf("empty ByJwt in code-login response")
	}
	if jwtContainsClientId(clResult.Result.ByJwt) {
		return "", fmt.Errorf("regression guard: code-login returned a client JWT instead of a network JWT")
	}

	newJwt := clResult.Result.ByJwt

	tlog("🔑 [jwt] refresh → step 2/3 ok: network JWT received (%d chars)\n", len(newJwt))

	tlog("🔑 [jwt] refresh → step 3/3: verifying new token against %s/transfer/stats...\n", apiUrl)

	// Verify the fresh token works before returning it so the caller never
	// overwrites the on-disk JWT with a dead token. Uses a lightweight read-only
	// API endpoint to keep side effects zero (no auth codes, no client rows).
	req, err := http.NewRequestWithContext(ctx, "GET", apiUrl+"/transfer/stats", nil)
	if err != nil {
		return "", fmt.Errorf("could not build verification request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+newJwt)
	verifyClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := verifyClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fresh token failed verification: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("fresh token rejected by server (HTTP %d)", resp.StatusCode)
	}

	var stats struct {
		PaidBytes   uint64 `json:"paid_bytes_provided"`
		UnpaidBytes uint64 `json:"unpaid_bytes_provided"`
	}
	if err := json.Unmarshal(body, &stats); err == nil {
		tlog("🔑 [jwt] refresh → step 3/3 ok: verification passed (HTTP %d, unpaid: %s, paid: %s)\n", resp.StatusCode, formatBytes(stats.UnpaidBytes), formatBytes(stats.PaidBytes))
	} else {
		tlog("🔑 [jwt] refresh → step 3/3 ok: verification passed (HTTP %d)\n", resp.StatusCode)
	}

	return newJwt, nil
}

// runJWTRefresher polls once per hour and refreshes the JWT under two
// independent conditions (OR logic — either one triggers a refresh):
//
//  1. Periodic refresh: it has been >= 7 days since the last successful
//     refresh, regardless of the token's actual expiry. This is the primary
//     mechanism — it guarantees the token is rotated on a fixed cadence so
//     it never gets anywhere near expiry under normal operation.
//  2. Expiry fallback: the token's exp claim is within 48 hours of expiring.
//     This is a safety net in case the periodic refresh above failed
//     repeatedly (e.g. multi-day API outage) — it gives a last-resort window
//     to recover before the provider would otherwise hit the exit-78 cycle.
//
// The last successful refresh time is persisted to disk (next to the JWT
// file) so the 7-day cadence survives provider restarts.
func runJWTRefresher(ctx context.Context, apiUrl string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")
	lastRefreshPath := filepath.Join(home, ".urnetwork", "jwt_last_refresh")

	const periodicInterval = 7 * 24 * time.Hour
	const expiryFallbackWindow = 48 * time.Hour

	// Startup jitter (0-9 minutes) to desynchronize refresh checks across the fleet.
	jitterMs := time.Duration(mathrand.Intn(10)) * time.Minute
	select {
	case <-time.After(jitterMs):
	case <-ctx.Done():
		return
	}

	readLastRefreshTime := func() time.Time {
		data, err := os.ReadFile(lastRefreshPath)
		if err != nil {
			// No record on disk — treat as never refreshed so the periodic
			// condition fires on the first check and establishes a baseline.
			return time.Time{}
		}
		unixSec, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(unixSec, 0)
	}

	writeLastRefreshTime := func(t time.Time) error {
		return atomicWriteFile(lastRefreshPath, []byte(strconv.FormatInt(t.Unix(), 10)), 0700)
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		if isHotSwapDraining.Load() {
			return
		}
		byJwtBytes, err := os.ReadFile(jwtPath)
		if err != nil {
			if !os.IsNotExist(err) {
				// Log non-NotExist errors (permission denied, truncated file, etc.)
				// so operators can diagnose why the refresher is stuck.
				tlog("[auth] warn: could not read jwt file for refresh: %v\n", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				continue
			}
		}
		if err == nil {
			byJwt := strings.TrimSpace(string(byJwtBytes))

			lastRefreshTime := readLastRefreshTime()
			sinceLastRefresh := time.Since(lastRefreshTime)
			periodicDue := sinceLastRefresh >= periodicInterval

			// A zero-value lastRefreshTime (no on-disk record yet, e.g. a
			// fresh node) makes sinceLastRefresh ~2026 years, which
			// time.Duration silently saturates to its ~292-year int64 max
			// (2562047h47m16s) rather than overflowing. Report "never"
			// instead of that nonsensical ceiling value.
			sinceLastRefreshDesc := formatDuration(sinceLastRefresh)
			if lastRefreshTime.IsZero() {
				sinceLastRefreshDesc = "never"
			}

			exp := parseJWTExpiryTime(byJwt)
			expiryDue := exp != nil && time.Until(*exp) <= expiryFallbackWindow

			// Emit warning when expiry is within 48h
			if expiryDue && exp != nil {
				remaining := time.Until(*exp)
				tlog("🔑 [jwt] ⚠ expires in %s — refresh triggered\n", formatDuration(remaining))
			}

			if periodicDue || expiryDue {
				var reason string
				switch {
				case periodicDue && expiryDue:
					reason = fmt.Sprintf("7-day periodic refresh due (last refresh %s ago) and within %s of expiry",
						sinceLastRefreshDesc, formatDuration(expiryFallbackWindow))
				case periodicDue:
					reason = fmt.Sprintf("7-day periodic refresh due (last refresh %s ago)", sinceLastRefreshDesc)
				default:
					reason = fmt.Sprintf("expiry fallback triggered (expires in %s, within %s threshold)",
						formatDuration(time.Until(*exp)), formatDuration(expiryFallbackWindow))
				}
				tlog("🔑 [jwt] refreshing token — %s\n", reason)

				newJwt, err := refreshJWT(ctx, apiUrl, byJwt)
				if err != nil {
					tlog("🔑 [jwt] refresh FAILED: %v — keeping existing JWT (will retry in 1h)\n", err)
				} else if err := atomicWriteFile(jwtPath, []byte(newJwt), 0600); err != nil {
					tlog("🔑 [jwt] refresh FAILED on disk write: %v — keeping existing JWT in memory (will retry in 1h)\n", err)
				} else {
					now := time.Now()
					if err := writeLastRefreshTime(now); err != nil {
						tlog("🔑 [jwt] refresh OK but failed to persist last-refresh timestamp: %v\n", err)
					}
					tlog("🔑 [jwt] refresh OK — network JWT written to %s (%d bytes, next refresh in %s)\n",
						jwtPath, len(newJwt), formatDuration(periodicInterval))
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// classifyAuthFailureCause derives a human-readable root cause for a proxy's
// final auth failure, so operators get an actionable message instead of a
// generic "JWT may be expired" that is wrong for the common cases below.
//
// "proxy unreachable" gets its own bucket, separate from real network errors
// reaching the API: it's synthesized by the local SOCKS5 reachability probe
// (probeProxySocks5) before any auth attempt is made, so it says nothing
// about api.bringyour.com's health — it just means this proxy's port is
// dead or not actually speaking SOCKS5. Bundling it with genuine API-side
// timeouts made dead entries in a public proxy list look identical to real
// outages, which is operationally misleading. Keeping them separate means
// operators can triage proxy health vs. API health independently.
