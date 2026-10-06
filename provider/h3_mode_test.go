package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The accepted values are exactly off, direct, a positive count, and all, with
// on aliasing direct and 0 aliasing off. Removing the "on" case makes the alias
// assertion fail.
func TestParseH3ModeValues(t *testing.T) {
	cases := []struct {
		value string
		kind  h3ModeKind
		cap   int
	}{
		{"off", h3ModeOff, 0},
		{"0", h3ModeOff, 0},
		{"direct", h3ModeDirect, 0},
		{"on", h3ModeDirect, 0},
		{"all", h3ModeAll, 0},
		{"", h3ModeAll, 0},
		{"7", h3ModeCap, 7},
		{"128", h3ModeCap, 128},
		// A leading zero on a positive count still parses as that count.
		{"01", h3ModeCap, 1},
	}
	for _, c := range cases {
		kind, n, err := parseH3Mode(c.value)
		if err != nil {
			t.Fatalf("parseH3Mode(%q): %v", c.value, err)
		}
		if kind != c.kind || n != c.cap {
			t.Fatalf("parseH3Mode(%q) = (%d, %d), want (%d, %d)", c.value, kind, n, c.kind, c.cap)
		}
	}
	// "0" is off (handled above). Any OTHER zero spelling must be rejected, not
	// accepted as a cap of zero: a zero cap keeps the direct identity eligible
	// and would silently mean `direct`, the opposite of off.
	for _, bad := range []string{"sometimes", "-1", "on off", "1.5", "00", "+0", "-0"} {
		if _, _, err := parseH3Mode(bad); err == nil {
			t.Fatalf("parseH3Mode(%q) accepted a value it should reject", bad)
		}
	}
}

// On is an alias for direct, and the default is all. This is the user-facing
// contract: on must not mean a narrower set.
func TestH3ModeDefaultIsAllAndOnAliasesDirect(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)

	if _, err := SetH3Mode("all"); err != nil {
		t.Fatal(err)
	}
	if H3ModeName() != "all" {
		t.Fatalf("mode = %q, want all", H3ModeName())
	}
	if !snH3Eligible(0, nil, true) || !snH3Eligible(3, &connect.ProxySettings{}, false) {
		t.Fatal("all must include the direct identity and every proxy")
	}

	if _, err := SetH3Mode("on"); err != nil {
		t.Fatal(err)
	}
	if H3ModeName() != "direct" {
		t.Fatalf("set h3 on resolved to %q, want direct", H3ModeName())
	}
	if !snH3Eligible(0, nil, true) {
		t.Fatal("direct mode must include the direct identity")
	}
	if snH3Eligible(3, &connect.ProxySettings{}, false) {
		t.Fatal("direct mode must exclude a proxied identity")
	}
}

// The predicate reflects the mode: off excludes everyone, a cap includes only
// the resolved best-N proxy keys.
func TestSnH3EligibleFollowsTheMode(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)

	if _, err := SetH3Mode("off"); err != nil {
		t.Fatal(err)
	}
	if snH3Eligible(0, nil, true) || snH3Eligible(3, &connect.ProxySettings{}, false) {
		t.Fatal("off must exclude every identity")
	}

	// A cap names the best proxies; drive the resolved set directly so the test
	// does not depend on grades on disk.
	h3ModeValue.Store(&h3ResolvedMode{
		kind:         h3ModeCap,
		cap:          2,
		raw:          "2",
		eligibleKeys: map[string]bool{"keep": true},
	})
	if !h3EligibleForKey("keep", false) {
		t.Fatal("a capped-in proxy must be eligible")
	}
	if h3EligibleForKey("drop", false) {
		t.Fatal("a proxy outside the cap must not be eligible")
	}
	if !h3EligibleForKey("keep", true) {
		t.Fatal("a cap must leave the direct identity eligible")
	}
}

// A cap limits only the proxy set. The direct identity is the baseline path and
// keeps H3 under every mode except off, so a cap must never take H3 away from
// it. Making the cap exclude the direct identity again fails this test.
func TestH3CapKeepsDirectEligible(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)

	// A cap whose resolved proxy set does not contain the direct identity: the
	// direct identity must still be eligible.
	h3ModeValue.Store(&h3ResolvedMode{
		kind:         h3ModeCap,
		cap:          1,
		raw:          "1",
		eligibleKeys: map[string]bool{"capped-proxy": true},
	})
	if !h3EligibleForKey(directProxyKey, true) {
		t.Fatal("the direct identity must keep H3 under a cap")
	}
	if !snH3Eligible(0, nil, true) {
		t.Fatal("snH3Eligible must include the direct identity under a cap")
	}
	if !h3EligibleForKey("capped-proxy", false) {
		t.Fatal("a proxy inside the cap must be eligible")
	}
	if h3EligibleForKey("uncapped-proxy", false) {
		t.Fatal("a proxy outside the cap must not be eligible")
	}

	// off still disables the direct identity, so the rule is a cap carve-out.
	h3ModeValue.Store(&h3ResolvedMode{kind: h3ModeOff, raw: "off"})
	if snH3Eligible(0, nil, true) {
		t.Fatal("off must still exclude the direct identity")
	}
}

// A cap of N yields EXACTLY N proxies plus the direct identity: the direct
// identity is admitted on top of the cap and never consumes one of the N. With
// the cap at 2 and three proxies running, two proxies and the direct identity
// are eligible. Decrementing the proxy cap to make room for the direct identity
// (or counting the direct identity inside h3_proxies) fails this test.
func TestH3CapCountsDirectOnTopOfNProxies(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)

	proxies := []string{"proxy-a", "proxy-b", "proxy-c"}
	for _, k := range proxies {
		unregisterH3Running(k)
	}
	unregisterH3Running(directProxyKey)
	defer func() {
		for _, k := range proxies {
			unregisterH3Running(k)
		}
		unregisterH3Running(directProxyKey)
	}()

	h3ModeValue.Store(&h3ResolvedMode{
		kind:         h3ModeCap,
		cap:          2,
		raw:          "2",
		eligibleKeys: map[string]bool{"proxy-a": true, "proxy-b": true},
	})

	baseProxies := h3ProxySetSize()
	baseTotal := h3SetSize()

	// Register the running set exactly as provide.go builds it: the direct
	// identity plus every running proxy, each judged by the predicate.
	registerH3Running(directProxyKey, h3EligibleForKey(directProxyKey, true))
	for _, k := range proxies {
		registerH3Running(k, h3EligibleForKey(k, false))
	}

	if got := h3ProxySetSize() - baseProxies; got != 2 {
		t.Fatalf("H3 proxies = %d, want exactly the 2 the cap names", got)
	}
	if got := h3SetSize() - baseTotal; got != 3 {
		t.Fatalf("H3 grand total = %d, want 2 proxies plus the direct identity", got)
	}
	// The cap is not decremented to make room for the direct identity.
	if !h3EligibleForKey("proxy-a", false) || !h3EligibleForKey("proxy-b", false) {
		t.Fatal("both capped proxies must stay eligible; the direct identity must not displace one")
	}
	if h3EligibleForKey("proxy-c", false) {
		t.Fatal("the third proxy is outside the cap")
	}
	if !h3EligibleForKey(directProxyKey, true) {
		t.Fatal("the direct identity must be eligible on top of the cap")
	}
}

// The mode is applied per identity: an excluded identity is pinned to H1 only
// so the engine never dials H3.
func TestApplyH3ModeToSettingsPinsExcludedToH1(t *testing.T) {
	eligible := connect.DefaultPlatformTransportSettings()
	applyH3ModeToSettings(eligible, true)
	if _, ok := eligible.ModePreferences[connect.TransportModeH3]; !ok {
		t.Fatal("an eligible identity lost the H3 mode")
	}
	excluded := connect.DefaultPlatformTransportSettings()
	applyH3ModeToSettings(excluded, false)
	if len(excluded.ModePreferences) != 1 {
		t.Fatalf("excluded identity has %d mode preferences, want only H1", len(excluded.ModePreferences))
	}
	if _, ok := excluded.ModePreferences[connect.TransportModeH1]; !ok {
		t.Fatal("excluded identity is not pinned to H1")
	}
	if _, ok := excluded.ModePreferences[connect.TransportModeH3]; ok {
		t.Fatal("excluded identity kept an H3 mode preference")
	}
}

// The h3 key is live, and the apply path resolves the mode.
func TestH3ControlKeyAppliesLive(t *testing.T) {
	if needsRestart("h3") {
		t.Fatal("h3 must be live, not restart-only")
	}
	if !controlKeys["h3"] {
		t.Fatal("controlKeys is missing the h3 key; the socket would reject it")
	}
	previous := H3ModeName()
	defer SetH3Mode(previous)

	for _, value := range []string{"off", "direct", "on", "all", "5"} {
		if err := validateControlValue("h3", value); err != nil {
			t.Fatalf("validateControlValue h3 %q: %v", value, err)
		}
		if err := applyLiveSideEffect("h3", value); err != nil {
			t.Fatalf("applyLiveSideEffect h3 %q: %v", value, err)
		}
	}
	if err := validateControlValue("h3", "nonsense"); err == nil {
		t.Fatal("validateControlValue accepted a bad h3 value")
	}
}

// The resolved mode and the set size are exposed where the operator looks.
func TestH3SetSizeAndModeAreReported(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)
	defer func() {
		unregisterH3Running("health-probe-a")
		unregisterH3Running("health-probe-b")
	}()

	registerH3Running("health-probe-a", true)
	registerH3Running("health-probe-b", false)
	if got := h3SetSize(); got < 1 {
		t.Fatalf("h3SetSize = %d, want at least the one eligible probe", got)
	}
	if _, err := SetH3Mode("direct"); err != nil {
		t.Fatal(err)
	}
	if H3ModeName() != "direct" {
		t.Fatalf("mode = %q, want direct", H3ModeName())
	}
	if !strings.Contains(providerExtraMetrics(), `urnet_h3_mode_info{mode="direct"}`) {
		t.Fatal("/metrics does not carry the resolved h3 mode")
	}
	if !strings.Contains(providerExtraMetrics(), "urnet_h3_set_size ") {
		t.Fatal("/metrics does not carry the h3 set size")
	}
	if !strings.Contains(providerExtraMetrics(), "urnet_h3_proxy_set_size ") {
		t.Fatal("/metrics does not carry the h3 proxy set size")
	}
}

// A live mode change reconnects only the identities whose membership changed:
// the one that leaves the set is cancelled and dropped from the cancel map, and
// the one that stays is left alone. Removing the cancel/delete from
// reapplyH3ModeLive makes the "left" assertion fail.
func TestReapplyH3ModeLiveReconnectsOnlyChangedIdentities(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)
	if _, err := SetH3Mode("all"); err != nil {
		t.Fatal(err)
	}

	st := &provideState{proxyCancelMap: map[string]context.CancelFunc{}}
	var cancelledKeep, cancelledDrop bool
	st.proxyCancelMap["keep"] = func() { cancelledKeep = true }
	st.proxyCancelMap["drop"] = func() { cancelledDrop = true }
	registerH3Running("keep", true)
	registerH3Running("drop", true)
	defer unregisterH3Running("keep")
	defer unregisterH3Running("drop")

	// Keep exactly one proxy, then re-apply: drop leaves the set, keep does not.
	h3ModeValue.Store(&h3ResolvedMode{
		kind:         h3ModeCap,
		cap:          1,
		raw:          "1",
		eligibleKeys: map[string]bool{"keep": true},
	})
	reapplyH3ModeLive(st)

	if cancelledKeep {
		t.Fatal("an identity that stayed in the H3 set was reconnected")
	}
	if !cancelledDrop {
		t.Fatal("an identity that left the H3 set was not reconnected")
	}
	if _, ok := st.proxyCancelMap["drop"]; ok {
		t.Fatal("a left identity stayed in the cancel map; a reload would not respawn it")
	}
	if _, ok := st.proxyCancelMap["keep"]; !ok {
		t.Fatal("a staying identity was removed from the cancel map")
	}
}

// A live mode change reconnects a changed identity by cancelling the old launch
// and respawning a new one. The two overlap: the replacement registers its
// h3RunningEligible entry before the cancelled launch finishes unwinding and
// runs its cleanup. The cleanup must release the entry only while its own
// launch is still the current one, or it erases the live identity from health
// and metrics. Removing the launch check from unregisterH3RunningIfCurrent
// makes the middle assertion fail.
func TestReconnectKeepsRunningEntryWhenOldLaunchCleansUp(t *testing.T) {
	const key = "h3-launch-race"
	unregisterH3Running(key)
	defer unregisterH3Running(key)

	// The launch that is being replaced.
	oldLaunch := registerH3Running(key, true)

	// The replacement launch registers before the old one unwinds.
	replacementLaunch := registerH3Running(key, true)
	if oldLaunch == replacementLaunch {
		t.Fatal("registerH3Running reused a launch id; the ownership check cannot work")
	}

	// The cancelled launch's defer runs here.
	unregisterH3RunningIfCurrent(key, oldLaunch)
	if eligible, tracked := h3RunningEligibleOf(key); !tracked {
		t.Fatal("the cancelled launch's cleanup erased the replacement launch's entry; the live identity dropped out of health and metrics")
	} else if !eligible {
		t.Fatal("the replacement launch's entry was left with the wrong eligibility")
	}

	// The replacement's own cleanup does release it.
	unregisterH3RunningIfCurrent(key, replacementLaunch)
	if _, tracked := h3RunningEligibleOf(key); tracked {
		t.Fatal("the owning launch's cleanup did not remove the entry")
	}
}

// A cap must rank over every proxy the box runs, not just the internal config.
// The launcher publishes the full desired set (internal or file source, plus
// URL-sourced), so a cap can include a file- or URL-sourced proxy. Resolve from
// readProxySettings() again and the published set is ignored, so this fails.
func TestH3CapRanksOverPublishedCandidates(t *testing.T) {
	// Save and restore the published candidate set so other tests are not
	// affected by this one.
	h3ProxyCandidatesMu.Lock()
	prevSet, prevKnown := h3ProxyCandidatesSet, h3ProxyCandidatesKnown
	h3ProxyCandidatesMu.Unlock()
	defer func() {
		h3ProxyCandidatesMu.Lock()
		h3ProxyCandidatesSet, h3ProxyCandidatesKnown = prevSet, prevKnown
		h3ProxyCandidatesMu.Unlock()
	}()

	// These two stand in for proxies a file source or URL feed would supply.
	// They are not in the internal config, so readProxySettings() would not
	// return them.
	fileProxy := &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:19999"}
	otherProxy := &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:19998"}
	publishH3ProxyCandidates([]*connect.ProxySettings{fileProxy, otherProxy})

	resolved, err := buildH3ResolvedMode("1")
	if err != nil {
		t.Fatalf("buildH3ResolvedMode(1): %v", err)
	}
	if resolved.kind != h3ModeCap {
		t.Fatalf("kind = %v, want cap", resolved.kind)
	}
	if len(resolved.eligibleKeys) != 1 {
		t.Fatalf("a cap of 1 resolved %d proxy keys from the published set, want 1: %v", len(resolved.eligibleKeys), resolved.eligibleKeys)
	}
	if !resolved.eligibleKeys[fileProxy.Key()] && !resolved.eligibleKeys[otherProxy.Key()] {
		t.Fatalf("a cap of 1 resolved no published proxy key: %v", resolved.eligibleKeys)
	}
}

// An empty-but-KNOWN published set must cap to zero proxies, not fall back to
// readProxySettings(). The launcher publishes an empty set when the box runs no
// file- or URL-sourced proxies; resolving the internal config then would cap
// over stale internal keys that match no running identity, breaking the
// known/unknown contract buildH3ResolvedMode relies on. The regression this
// pins: cloning the published set with append(nil, set...) returns nil for an
// EMPTY set, so the empty-but-known case was mistaken for "unknown" and the
// fallback ran.
func TestH3CapEmptyPublishedCandidatesDoNotFallBack(t *testing.T) {
	// A stale internal config that readProxySettings() WOULD rank. If the
	// empty published set falls back to it, the cap resolves a proxy here.
	withTempHome(t)
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"10.7.7.7:8080": ""}})
	if internal := readProxySettings(); len(internal) == 0 {
		t.Fatal("test premise: the stale internal config must be readable from the temp home")
	}

	h3ProxyCandidatesMu.Lock()
	prevSet, prevKnown := h3ProxyCandidatesSet, h3ProxyCandidatesKnown
	h3ProxyCandidatesMu.Unlock()
	defer func() {
		h3ProxyCandidatesMu.Lock()
		h3ProxyCandidatesSet, h3ProxyCandidatesKnown = prevSet, prevKnown
		h3ProxyCandidatesMu.Unlock()
	}()

	// Known but empty: the box runs zero proxies from any launcher source.
	publishH3ProxyCandidates(nil)
	if got := h3ProxyCandidateSettings(); got == nil {
		t.Fatal("a known-but-empty published set returned a nil slice; a nil return means " +
			"\"no launcher published\", so buildH3ResolvedMode falls back to readProxySettings()")
	}

	resolved, err := buildH3ResolvedMode("1")
	if err != nil {
		t.Fatalf("buildH3ResolvedMode(1): %v", err)
	}
	if len(resolved.eligibleKeys) != 0 {
		t.Fatalf("an empty-but-known published set capped to %d proxy keys (%v); it fell back "+
			"to readProxySettings() and ranked stale internal keys the box does not run",
			len(resolved.eligibleKeys), resolved.eligibleKeys)
	}
}
