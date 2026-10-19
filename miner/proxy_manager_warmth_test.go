package miner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"

	"github.com/urfoundation/sn/clientauth"
)

// W-02 tests: the warmth evaluation (v33 provider/proxy_warmth.go adapted to
// the miner's per-proxy client JWT file), the warmth/earnings launch ordering
// and its per-tier stagger, and the pacer the launch loop drives.

// warmthTestClientID is a syntactically valid client id (connect.ParseId
// format) shared by the warmth fixtures.
const warmthTestClientID = "00000000-0000-0000-0000-0000000001ab"

// withHotRestart pins the hot-restart switch (the audit's package seam) so a
// warmth case does not depend on the host's env or control state.
func withHotRestart(t *testing.T, enabled bool) {
	t.Helper()
	prev := hotRestartEnabled
	hotRestartEnabled = func() bool { return enabled }
	t.Cleanup(func() { hotRestartEnabled = prev })
}

// warmthTestJWT signs an unsigned fixture token carrying the given claims.
func warmthTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims(claims)).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// warmthSeedClientJWT writes the token where the sign-in path keeps this
// identity's client JWT (providerClientJwtPath), which is exactly what
// evaluateProxyWarmth reads.
func warmthSeedClientJWT(t *testing.T, settings *connect.ProxySettings, token string) {
	t.Helper()
	path, err := providerClientJwtPath(providerSlotIdentities{}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := clientauth.WriteToken(path, token); err != nil {
		t.Fatal(err)
	}
}

func TestProxyWarmthTierString(t *testing.T) {
	tests := []struct {
		tier ProxyWarmthTier
		want string
	}{
		{WarmthValid, "valid"},
		{WarmthRenewable, "renewable"},
		{WarmthCold, "cold"},
		{ProxyWarmthTier(99), "cold"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("ProxyWarmthTier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestProxyLaunchStagger(t *testing.T) {
	tests := []struct {
		name         string
		tier         ProxyWarmthTier
		isURLSourced bool
		want         time.Duration
	}{
		{"WarmthValid file", WarmthValid, false, WarmValidStagger},
		{"WarmthValid url", WarmthValid, true, WarmValidStagger},
		{"WarmthRenewable file", WarmthRenewable, false, WarmRenewableStagger},
		{"WarmthRenewable url", WarmthRenewable, true, WarmRenewableStagger},
		{"WarmthCold file", WarmthCold, false, ColdFileStagger},
		{"WarmthCold url", WarmthCold, true, ColdURLStagger},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proxyLaunchStagger(tt.tier, tt.isURLSourced)
			if got != tt.want {
				t.Errorf("proxyLaunchStagger(%v, %v) = %v, want %v", tt.tier, tt.isURLSourced, got, tt.want)
			}
		})
	}
}

func TestCurrentProviderNetworkID(t *testing.T) {
	// The merged currentProviderNetworkID consults the process-wide client
	// JWT store before the state directory's credential files
	// (client_jwt_hotrestart.go, pinned by TestProvideClientJWTFallsBackToStoreAnyNetworkID).
	// The test binary shares one store across tests, so isolate it here: this
	// test may only observe the state directory it seeds.
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	t.Run("no jwt file and no stored tokens returns empty", func(t *testing.T) {
		withTempHome(t)
		if got := currentProviderNetworkID(); got != "" {
			t.Errorf("currentProviderNetworkID() = %q, want empty", got)
		}
	})

	t.Run("jwt file with network_id returns network_id", func(t *testing.T) {
		dir := withTempHome(t)
		token := warmthTestJWT(t, map[string]any{"network_id": "net-xyz-123"})
		if err := os.WriteFile(filepath.Join(dir, "jwt"), []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if got := currentProviderNetworkID(); got != "net-xyz-123" {
			t.Errorf("currentProviderNetworkID() = %q, want net-xyz-123", got)
		}
	})

	t.Run("falls back to a stored per-proxy client JWT", func(t *testing.T) {
		withTempHome(t)
		settings := &connect.ProxySettings{Network: "tcp", Address: "1.2.3.4:1080"}
		warmthSeedClientJWT(t, settings, warmthTestJWT(t, map[string]any{
			"client_id":  warmthTestClientID,
			"network_id": "net-stored",
		}))
		if got := currentProviderNetworkID(); got != "net-stored" {
			t.Errorf("currentProviderNetworkID() = %q, want the stored client JWT's network id", got)
		}
	})
}

func TestEvaluateProxyWarmth(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, true)
	// The merged currentProviderNetworkID consults the process-wide client JWT
	// store before the state directory's credential files; the test binary
	// shares one store across tests, so isolate it (see
	// TestCurrentProviderNetworkID).
	restoreStore := withGlobalStore(t, filepath.Join(t.TempDir(), "store.json"))
	defer restoreStore()

	validExp := time.Now().Add(time.Hour).Unix()
	expiredExp := time.Now().Add(-time.Hour).Unix()

	validSettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-valid:1080"}
	renewableSettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-renewable:1080"}
	mismatchedSettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-mismatched-network:1080"}
	emptySettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-empty-fields:1080"}
	invalidClientIdSettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-invalid-clientid:1080"}
	noClientIdSettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-no-clientid-in-jwt:1080"}
	expiredNoClientIdSettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-expired-no-clientid-in-jwt:1080"}
	legacyNetSettings := &connect.ProxySettings{Network: "tcp", Address: "proxy-legacy-empty-net:1080"}

	warmthSeedClientJWT(t, validSettings, warmthTestJWT(t, map[string]any{
		"client_id":  warmthTestClientID,
		"exp":        validExp,
		"network_id": "net-abc",
	}))
	warmthSeedClientJWT(t, renewableSettings, warmthTestJWT(t, map[string]any{
		"client_id":  warmthTestClientID,
		"exp":        expiredExp,
		"network_id": "net-abc",
	}))
	warmthSeedClientJWT(t, mismatchedSettings, warmthTestJWT(t, map[string]any{
		"client_id":  warmthTestClientID,
		"exp":        validExp,
		"network_id": "net-other",
	}))
	// The stored token file exists but is empty: the sign-in path would have
	// nothing to reuse.
	if err := os.WriteFile(mustProviderClientJwtPath(t, emptySettings), nil, 0600); err != nil {
		t.Fatal(err)
	}
	warmthSeedClientJWT(t, invalidClientIdSettings, warmthTestJWT(t, map[string]any{
		"client_id":  "not-a-valid-id",
		"exp":        expiredExp,
		"network_id": "net-abc",
	}))
	// The miner stores no client_id sidecar beside the token (v33's store
	// does), so renewability is decided from the payload alone: a token whose
	// payload has no parsable client_id claim is cold, never renewable.
	warmthSeedClientJWT(t, noClientIdSettings, warmthTestJWT(t, map[string]any{
		"exp":        validExp,
		"network_id": "net-abc",
	}))
	warmthSeedClientJWT(t, expiredNoClientIdSettings, warmthTestJWT(t, map[string]any{
		"exp":        expiredExp,
		"network_id": "net-abc",
	}))
	warmthSeedClientJWT(t, legacyNetSettings, warmthTestJWT(t, map[string]any{
		"client_id": warmthTestClientID,
		"exp":       validExp,
		// no network_id claim: v33's legacy entry shape
	}))

	t.Run("valid unexpired token returns WarmthValid", func(t *testing.T) {
		if got := evaluateProxyWarmth(validSettings, "net-abc", providerSlotIdentities{}); got != WarmthValid {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthValid", got)
		}
	})

	t.Run("expired token with valid client_id claim returns WarmthRenewable", func(t *testing.T) {
		if got := evaluateProxyWarmth(renewableSettings, "net-abc", providerSlotIdentities{}); got != WarmthRenewable {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthRenewable", got)
		}
	})

	t.Run("network mismatch returns WarmthCold", func(t *testing.T) {
		if got := evaluateProxyWarmth(mismatchedSettings, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("missing stored token returns WarmthCold", func(t *testing.T) {
		missing := &connect.ProxySettings{Network: "tcp", Address: "proxy-nonexistent:1080"}
		if got := evaluateProxyWarmth(missing, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("empty stored token returns WarmthCold", func(t *testing.T) {
		if got := evaluateProxyWarmth(emptySettings, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("nil settings return WarmthCold", func(t *testing.T) {
		if got := evaluateProxyWarmth(nil, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth(nil) = %v, want WarmthCold", got)
		}
	})

	t.Run("invalid client ID format returns WarmthCold", func(t *testing.T) {
		if got := evaluateProxyWarmth(invalidClientIdSettings, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("valid jwt lacking client_id claim in payload returns WarmthCold", func(t *testing.T) {
		if got := evaluateProxyWarmth(noClientIdSettings, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold (no sidecar to renew from)", got)
		}
	})

	t.Run("expired jwt lacking client_id claim returns WarmthCold", func(t *testing.T) {
		if got := evaluateProxyWarmth(expiredNoClientIdSettings, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold (no sidecar to renew from)", got)
		}
	})

	t.Run("hot restart disabled returns WarmthCold", func(t *testing.T) {
		withHotRestart(t, false)
		if got := evaluateProxyWarmth(validSettings, "net-abc", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() with hot restart off = %v, want WarmthCold", got)
		}
	})

	t.Run("empty currentNetworkID filled in from the token returns WarmthValid", func(t *testing.T) {
		// Isolated: the stored-token fallback scan would otherwise pick up a
		// sibling fixture's network id from the shared state directory.
		withTempHome(t)
		settings := &connect.ProxySettings{Network: "tcp", Address: "proxy-fill:1080"}
		warmthSeedClientJWT(t, settings, warmthTestJWT(t, map[string]any{
			"client_id":  warmthTestClientID,
			"exp":        validExp,
			"network_id": "net-abc",
		}))
		if got := evaluateProxyWarmth(settings, "", providerSlotIdentities{}); got != WarmthValid {
			t.Errorf("evaluateProxyWarmth() with empty currentNetworkID but known token network id = %v, want WarmthValid", got)
		}
	})

	t.Run("legacy token with no network_id and known currentNetworkID returns WarmthValid", func(t *testing.T) {
		if got := evaluateProxyWarmth(legacyNetSettings, "net-abc", providerSlotIdentities{}); got != WarmthValid {
			t.Errorf("evaluateProxyWarmth() legacy empty network id = %v, want WarmthValid", got)
		}
	})

	t.Run("no network_id anywhere returns WarmthCold", func(t *testing.T) {
		withTempHome(t)
		settings := &connect.ProxySettings{Network: "tcp", Address: "proxy-no-net:1080"}
		warmthSeedClientJWT(t, settings, warmthTestJWT(t, map[string]any{
			"client_id": warmthTestClientID,
			"exp":       validExp,
			// no network_id claim anywhere: identity reuse cannot be verified
		}))
		if got := evaluateProxyWarmth(settings, "", providerSlotIdentities{}); got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() with no network_id anywhere = %v, want WarmthCold", got)
		}
	})
}

func mustProviderClientJwtPath(t *testing.T, settings *connect.ProxySettings) string {
	t.Helper()
	path, err := providerClientJwtPath(providerSlotIdentities{}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrioritizeAndScheduleProxies(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, true)

	validExp := time.Now().Add(time.Hour).Unix()
	expiredExp := time.Now().Add(-time.Hour).Unix()
	validJWT := warmthTestJWT(t, map[string]any{
		"client_id":  warmthTestClientID,
		"exp":        validExp,
		"network_id": "net-main",
	})
	expiredJWT := warmthTestJWT(t, map[string]any{
		"client_id":  warmthTestClientID,
		"exp":        expiredExp,
		"network_id": "net-main",
	})

	fileWarm := &connect.ProxySettings{Address: "file-warm"}
	fileRenewable := &connect.ProxySettings{Address: "file-renewable"}
	urlWarm := &connect.ProxySettings{Address: "url-warm"}
	urlRenewable := &connect.ProxySettings{Address: "url-renewable"}
	warmthSeedClientJWT(t, fileWarm, validJWT)
	warmthSeedClientJWT(t, fileRenewable, expiredJWT)
	warmthSeedClientJWT(t, urlWarm, validJWT)
	warmthSeedClientJWT(t, urlRenewable, expiredJWT)

	proxies := []*connect.ProxySettings{
		{Address: "url-cold"},
		{Address: "file-cold"},
		urlRenewable,
		fileRenewable,
		urlWarm,
		fileWarm,
	}
	sourceOf := map[string]string{
		"file-warm":      "file",
		"file-renewable": "file",
		"file-cold":      "file",
		"url-warm":       "url",
		"url-renewable":  "url",
		"url-cold":       "url",
	}

	schedules, warmCount, renewableCount, coldCount := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if warmCount != 2 {
		t.Errorf("warmCount = %d, want 2", warmCount)
	}
	if renewableCount != 2 {
		t.Errorf("renewableCount = %d, want 2", renewableCount)
	}
	if coldCount != 2 {
		t.Errorf("coldCount = %d, want 2", coldCount)
	}

	expectedOrder := []struct {
		addr    string
		tier    ProxyWarmthTier
		delay   time.Duration
		stagger time.Duration
	}{
		{"file-warm", WarmthValid, 0, WarmValidStagger},                                  // 0
		{"url-warm", WarmthValid, 25 * time.Millisecond, WarmValidStagger},               // 25ms
		{"file-renewable", WarmthRenewable, 50 * time.Millisecond, WarmRenewableStagger}, // 50ms
		{"url-renewable", WarmthRenewable, 100 * time.Millisecond, WarmRenewableStagger}, // 100ms
		{"file-cold", WarmthCold, 150 * time.Millisecond, ColdFileStagger},               // 150ms
		{"url-cold", WarmthCold, 300 * time.Millisecond, ColdURLStagger},                 // 300ms
	}

	if len(schedules) != len(expectedOrder) {
		t.Fatalf("len(schedules) = %d, want %d", len(schedules), len(expectedOrder))
	}

	for i, exp := range expectedOrder {
		sched := schedules[i]
		if sched.Settings.Address != exp.addr {
			t.Errorf("schedule[%d].Settings.Address = %q, want %q", i, sched.Settings.Address, exp.addr)
		}
		if sched.Tier != exp.tier {
			t.Errorf("schedule[%d] (%s) Tier = %v, want %v", i, exp.addr, sched.Tier, exp.tier)
		}
		if sched.Delay != exp.delay {
			t.Errorf("schedule[%d] (%s) Delay = %v, want %v", i, exp.addr, sched.Delay, exp.delay)
		}
		if sched.Stagger != exp.stagger {
			t.Errorf("schedule[%d] (%s) Stagger = %v, want %v", i, exp.addr, sched.Stagger, exp.stagger)
		}
	}
}

func TestPrioritizeAndScheduleProxiesStableOrder(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, false)

	// All proxies in the same source and warmth tier must preserve input order.
	proxies := []*connect.ProxySettings{
		{Address: "file-cold-1"},
		{Address: "file-cold-2"},
		{Address: "file-cold-3"},
	}
	sourceOf := map[string]string{
		"file-cold-1": "file",
		"file-cold-2": "file",
		"file-cold-3": "file",
	}

	schedules, _, _, coldCount := prioritizeAndScheduleProxies(proxies, sourceOf, "net-123")
	if coldCount != 3 {
		t.Errorf("coldCount = %d, want 3", coldCount)
	}
	for i, expectedAddr := range []string{"file-cold-1", "file-cold-2", "file-cold-3"} {
		if schedules[i].Settings.Address != expectedAddr {
			t.Errorf("schedule[%d] = %s, want %s (stable sort preserved)", i, schedules[i].Settings.Address, expectedAddr)
		}
	}
}

// The launch list consumes the schedule: proxies are reordered by warmth and
// provenance, the direct (nil) slot stays in the list, and every slot keeps
// the index it was configured at.
func TestScheduleProxyLaunchOrdersProxiesAndKeepsDirectSlot(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, false)

	fileProxy := &connect.ProxySettings{Address: "file-1:1080"}
	urlProxy := &connect.ProxySettings{Address: "url-1:1080"}
	providers := []*connect.ProxySettings{nil, fileProxy, urlProxy}
	sourceOf := map[string]string{
		"file-1:1080": "file",
		"url-1:1080":  "url",
	}

	slots := scheduleProxyLaunch(providers, sourceOf, "net-main")

	if len(slots) != 3 {
		t.Fatalf("len(slots) = %d, want 3", len(slots))
	}
	if slots[0].Settings != fileProxy || slots[0].Index != 1 {
		t.Errorf("slot[0] = (index %d, settings %p), want the file proxy at its configured index 1", slots[0].Index, slots[0].Settings)
	}
	if slots[1].Settings != urlProxy || slots[1].Index != 2 {
		t.Errorf("slot[1] = (index %d, settings %p), want the URL proxy at its configured index 2", slots[1].Index, slots[1].Settings)
	}
	if slots[2].Settings != nil || slots[2].Index != 0 {
		t.Errorf("slot[2] = (index %d, settings %p), want the direct slot at index 0 last", slots[2].Index, slots[2].Settings)
	}
	if slots[0].Delay != 0 || slots[0].Stagger != ColdFileStagger {
		t.Errorf("file proxy pacing = (delay %v, stagger %v), want (0, %v)", slots[0].Delay, slots[0].Stagger, ColdFileStagger)
	}
	// The unproven URL proxy launches behind the curated one at the
	// conservative URL stagger.
	if slots[1].Delay != ColdFileStagger || slots[1].Stagger != ColdURLStagger {
		t.Errorf("URL proxy pacing = (delay %v, stagger %v), want (%v, %v)", slots[1].Delay, slots[1].Stagger, ColdFileStagger, ColdURLStagger)
	}
	if slots[2].Delay != 0 || slots[2].Stagger != 0 {
		t.Errorf("direct slot pacing = (delay %v, stagger %v), want (0, 0)", slots[2].Delay, slots[2].Stagger)
	}
}

func TestEarningsBreaksTiesWithinTheSameGroup(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, false)
	restore := withGlobalEarningsStore(t)
	defer restore()

	// All three are cold file proxies, so provenance and warmth are equal and
	// only earnings can separate them.
	creditEarningsAt(globalProxyEarningsStore, "middle", 5_000_000, time.Now())
	creditEarningsAt(globalProxyEarningsStore, "richest", 900_000_000, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "poorest"},
		{Address: "middle"},
		{Address: "richest"},
	}
	sourceOf := map[string]string{"poorest": "file", "middle": "file", "richest": "file"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	want := []string{"richest", "middle", "poorest"}
	for i := range want {
		if got := schedules[i].Settings.Address; got != want[i] {
			t.Fatalf("launch order = %v, want %v", launchOrderOf(schedules), want)
		}
	}
}

func launchOrderOf(schedules []ProxySchedule) []string {
	order := make([]string, len(schedules))
	for i, s := range schedules {
		order[i] = s.Settings.Address
	}
	return order
}

func TestProvenEarnerFromURLIsPromotedAheadOfColdFileProxies(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, false)
	restore := withGlobalEarningsStore(t)
	defer restore()

	// A URL-sourced proxy that has actually moved real billable traffic is no
	// longer an unproven address, so it stops waiting behind every file-sourced
	// proxy.
	creditEarningsAt(globalProxyEarningsStore, "url-earner", earningsPromotionBytes*2, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "file-plain"},
		{Address: "url-earner"},
	}
	sourceOf := map[string]string{"file-plain": "file", "url-earner": "url"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrderOf(schedules); got[0] != "url-earner" {
		t.Fatalf("launch order = %v, want proven URL earner first", got)
	}
	// A promoted URL proxy launches with the cold-file stagger: it has earned
	// trusted provenance and must not be penalized with the URL stagger.
	for _, sched := range schedules {
		if sched.Settings.Address == "url-earner" && sched.Stagger != ColdFileStagger {
			t.Fatalf("promoted URL proxy stagger = %v, want ColdFileStagger (%v)", sched.Stagger, ColdFileStagger)
		}
	}
}

func TestUnprovenURLProxyStaysBehindFileProxies(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, false)
	restore := withGlobalEarningsStore(t)
	defer restore()

	// Just under the bar. Promotion must require real earnings, not noise, or
	// every URL address eventually drifts past the file list.
	creditEarningsAt(globalProxyEarningsStore, "url-almost", earningsPromotionBytes-1, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "url-almost"},
		{Address: "file-plain"},
	}
	sourceOf := map[string]string{"file-plain": "file", "url-almost": "url"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrderOf(schedules); got[0] != "file-plain" {
		t.Fatalf("launch order = %v, want file proxy first", got)
	}
}

func TestWarmthStillOutranksEarnings(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, true)
	restore := withGlobalEarningsStore(t)
	defer restore()

	warm := &connect.ProxySettings{Address: "warm-broke"}
	warmthSeedClientJWT(t, warm, warmthTestJWT(t, map[string]any{
		"client_id":  warmthTestClientID,
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"network_id": "net-main",
	}))

	// A cold identity has to mint against the rate-limited auth API. Being a
	// proven earner must not buy it a scarce mint slot ahead of a warm identity
	// that can dial straight through.
	creditEarningsAt(globalProxyEarningsStore, "cold-rich", 5_000_000_000, time.Now())

	proxies := []*connect.ProxySettings{
		{Address: "cold-rich"},
		warm,
	}
	sourceOf := map[string]string{"cold-rich": "file", "warm-broke": "file"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if got := launchOrderOf(schedules); got[0] != "warm-broke" {
		t.Fatalf("launch order = %v, want warm proxy first", got)
	}
}

func TestEqualNonZeroEarningsPreservesOrder(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, false)
	restore := withGlobalEarningsStore(t)
	defer restore()

	now := time.Now()
	creditEarningsAt(globalProxyEarningsStore, "alpha", 5_000_000, now)
	creditEarningsAt(globalProxyEarningsStore, "bravo", 5_000_000, now)
	creditEarningsAt(globalProxyEarningsStore, "charlie", 5_000_000, now)

	proxies := []*connect.ProxySettings{
		{Address: "alpha"},
		{Address: "bravo"},
		{Address: "charlie"},
	}
	sourceOf := map[string]string{"alpha": "file", "bravo": "file", "charlie": "file"}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	got := launchOrderOf(schedules)
	want := []string{"alpha", "bravo", "charlie"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("launch order = %v, want %v (stable sort should preserve input order on tie)", got, want)
		}
	}
}

// TestExplorationQuotaInterleavesUnprovenProxy verifies the exploration quota:
// for every 5 trusted cold proxies, one unproven cold proxy is interleaved so a
// node that restarts frequently cannot permanently starve its unproven set.
func TestExplorationQuotaInterleavesUnprovenProxy(t *testing.T) {
	withTempHome(t)
	withHotRestart(t, false)
	restore := withGlobalEarningsStore(t)
	defer restore()

	now := time.Now()
	trustedAddrs := make([]string, 7)
	for i := range trustedAddrs {
		trustedAddrs[i] = "file-" + string(rune('a'+i))
	}
	unprovenAddrs := []string{"url-a", "url-b", "url-c"}

	proxies := make([]*connect.ProxySettings, 0, 10)
	for _, a := range trustedAddrs {
		proxies = append(proxies, &connect.ProxySettings{Address: a})
		creditEarningsAt(globalProxyEarningsStore, a, 100_000_000, now)
	}
	for _, a := range unprovenAddrs {
		proxies = append(proxies, &connect.ProxySettings{Address: a})
	}

	sourceOf := map[string]string{}
	for _, a := range trustedAddrs {
		sourceOf[a] = "file"
	}
	for _, a := range unprovenAddrs {
		sourceOf[a] = "url"
	}

	schedules, _, _, _ := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	order := launchOrderOf(schedules)
	firstUnprovenIdx := -1
	for i, addr := range order {
		if addr == unprovenAddrs[0] {
			firstUnprovenIdx = i
			break
		}
	}
	if firstUnprovenIdx < 0 {
		t.Fatalf("unproven proxy not found in launch order: %v", order)
	}
	// No warm/renewable proxies, so cold starts at index 0. With 7 trusted and
	// 3 unproven the interleave puts one unproven after 5 trusted cold proxies.
	if firstUnprovenIdx != 5 {
		t.Fatalf("first unproven proxy at index %d, want 5 (after 5 trusted cold proxies): %v", firstUnprovenIdx, order)
	}
}

// The proxy manager records the REAL provenance ("file", "internal", "url") of
// each desired identity, which is the map the launch ordering consumes.
func TestProxyManagerRecordsRealProvenance(t *testing.T) {
	dir := withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	proxyFile := filepath.Join(dir, "proxy-file.txt")
	if err := os.WriteFile(proxyFile, []byte("8.8.8.8:1080:user:pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: proxyFile, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"9.9.9.9:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	m := newProxyManager(defaultProxyManagerConfig())
	launch, _, _, err := m.desired(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(launch) != 2 {
		t.Fatalf("launch set = %v, want the file and URL proxies", launch)
	}

	sourceOf := m.proxySourceOf()
	if got := sourceOf[(&connect.ProxySettings{Address: "8.8.8.8:1080", Auth: &proxy.Auth{User: "user"}}).Key()]; got != "file" {
		t.Errorf("file proxy provenance = %q, want \"file\"", got)
	}
	if got := sourceOf["9.9.9.9:1080"]; got != "url" {
		t.Errorf("URL proxy provenance = %q, want \"url\"", got)
	}
}

func TestBackoffPacerWithDelay(t *testing.T) {
	ctx := context.Background()

	t.Run("zero delay returns immediately", func(t *testing.T) {
		start := time.Now()
		ok := backoffPacerWithDelay(0, 0, ctx)
		if !ok {
			t.Error("expected backoffPacerWithDelay to return true")
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Errorf("expected immediate return, elapsed %v", elapsed)
		}
	})

	t.Run("context cancellation terminates immediately", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel() // canceled upfront

		start := time.Now()
		ok := backoffPacerWithDelay(2*time.Second, 100*time.Millisecond, cancelCtx)
		if ok {
			t.Error("expected backoffPacerWithDelay to return false on canceled context")
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("expected fast exit on canceled context, elapsed %v", elapsed)
		}
	})

	t.Run("completes after delay", func(t *testing.T) {
		start := time.Now()
		// Base delay 15ms with a 2ms slot implies ±1ms jitter. The assertion
		// uses a safe lower bound so scheduler/timer granularity on virtualized
		// runners cannot flake it.
		ok := backoffPacerWithDelay(15*time.Millisecond, 2*time.Millisecond, ctx)
		if !ok {
			t.Error("expected backoffPacerWithDelay to return true")
		}
		if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
			t.Errorf("expected elapsed >= 10ms, got %v", elapsed)
		}
	})

	t.Run("legacy backoffPacer wrapper behaves identically", func(t *testing.T) {
		if !backoffPacer(0, 0, time.Now(), ctx) {
			t.Error("backoffPacer(0, 0) returned false, want true")
		}

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		if backoffPacer(5, 100, time.Now(), cancelCtx) {
			t.Error("backoffPacer with canceled context returned true, want false")
		}
	})
}
