package miner

// Tests for the ported systemd STATUS= composer and its publisher.
//
// The expected strings are the relay's own expectations, taken from the
// relay's composer test (provider/systemd_status_test.go in this tree), so the
// miner cannot drift from the sentence a relay box reports for the same pool
// state: same words, same severity bands, same park/pause notes.

import (
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
)

// resetSystemdStatusState clears every input the STATUS line reads and puts it
// back on cleanup, so one case cannot leak a park, a pause or a resolution
// result into the next.
func resetSystemdStatusState(t *testing.T) {
	t.Helper()
	reset := func() {
		proxiesConfigured.Store(0)
		proxiesAuthenticated.Store(0)
		proxiesParked.Store(0)
		proxyAuditPaused.Store(false)
		proxyResolutionStatus.Store(proxyResolutionPending)
		proxyResolutionReason.Store("")
		proxyStatusState.Lock()
		proxyStatusState.pool = map[string]struct{}{}
		proxyStatusState.live = map[string]struct{}{}
		proxyStatusState.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// fabricatedPool builds the launch set the proxy manager publishes: identities
// by address, no credentials (Key() is the bare address).
func fabricatedPool(addrs ...string) []*connect.ProxySettings {
	out := make([]*connect.ProxySettings, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, &connect.ProxySettings{Address: addr})
	}
	return out
}

// TestSystemdStatusLineMatchesTheRelay is the composer's band table, copied
// from the relay so both roles word the same ratio the same way.
func TestSystemdStatusLineMatchesTheRelay(t *testing.T) {
	resetSystemdStatusState(t)
	cases := []struct {
		name        string
		total, live int64
		want        string
	}{
		{"before proxies resolve", 0, 0, "starting: resolving proxies"},
		{"none authenticated", 3, 0, "critical: 0/3 proxies authenticated (0%), retrying"},
		{"two of three live", 3, 2, "degraded: 2/3 proxies authenticated (67%), retrying"},
		{"all authenticated", 3, 3, "active: 3/3 proxies authenticated (100%)"},
		{"90% live is active", 10, 9, "active: 9/10 proxies authenticated (90%)"},
		{"89% live is partial", 100, 89, "partial: 89/100 proxies authenticated (89%)"},
		{"89.5% live is partial at the true ratio", 200, 179, "partial: 179/200 proxies authenticated (90%)"},
		{"70% live is partial", 10, 7, "partial: 7/10 proxies authenticated (70%)"},
		{"69% live is degraded", 100, 69, "degraded: 69/100 proxies authenticated (69%), retrying"},
		{"50% live is degraded", 10, 5, "degraded: 5/10 proxies authenticated (50%), retrying"},
		{"49% live is critical", 100, 49, "critical: 49/100 proxies authenticated (49%), retrying"},
		{"25% live is critical", 100, 25, "critical: 25/100 proxies authenticated (25%), retrying"},
		{"live exceeds configured clamps at 100", 2, 3, "active: 3/2 proxies authenticated (100%)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxiesConfigured.Store(tc.total)
			proxiesAuthenticated.Store(tc.live)
			if got := systemdStatusLine(); got != tc.want {
				t.Errorf("systemdStatusLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSystemdStatusLineResolutionStates covers the zero-configured branches,
// which read the resolution-status enum in proxy_manager_hooks.go.
func TestSystemdStatusLineResolutionStates(t *testing.T) {
	resetSystemdStatusState(t)
	cases := []struct {
		name   string
		status int32
		reason string
		want   string
	}{
		{
			"pending resolution",
			proxyResolutionPending, "",
			"starting: resolving proxies",
		},
		{
			"source unreachable",
			proxyResolutionFailed, "connection refused",
			"degraded: proxy source unreachable (connection refused), retrying",
		},
		{
			"failed with long reason is truncated",
			proxyResolutionFailed, "a very long error message that exceeds the maximum length limit for status line display",
			"degraded: proxy source unreachable (a very long error message that exceeds the maximum length li...), retrying",
		},
		{
			"failed with no reason says unknown",
			proxyResolutionFailed, "",
			"degraded: proxy source unreachable (unknown error), retrying",
		},
		{
			"empty source",
			proxyResolutionEmpty, "",
			"degraded: proxy source returned no usable proxies, retrying",
		},
		{
			"nothing to serve",
			proxyResolutionNoSource, "",
			"degraded: no proxy source configured and the direct transport is off",
		},
		{
			"direct only is settled",
			proxyResolutionZeroValid, "",
			"active: providing on direct/local IP (no proxy source configured)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxiesConfigured.Store(0)
			proxiesAuthenticated.Store(0)
			proxyResolutionStatus.Store(tc.status)
			proxyResolutionReason.Store(tc.reason)
			if got := systemdStatusLine(); got != tc.want {
				t.Errorf("systemdStatusLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSystemdStatusLineProxyAuditParks is the relay's park/pause table: parks
// are deliberate, so they must not read as an outage, and the note must still
// explain a count that is short at zero live.
func TestSystemdStatusLineProxyAuditParks(t *testing.T) {
	resetSystemdStatusState(t)
	cases := []struct {
		name                string
		total, live, parked int64
		paused              bool
		want                string
	}{
		{"parked and everything else up", 50, 47, 3, false, "active: 47/50 proxies authenticated (94%), 3 parked by proxy audit"},
		{"a real outage on top of parks", 50, 44, 3, false, "active: 44/50 proxies authenticated (88%), 3 parked by proxy audit"},
		{"none parked is unchanged", 50, 50, 0, false, "active: 50/50 proxies authenticated (100%)"},
		{"paused says so", 50, 50, 0, true, "active: 50/50 proxies authenticated (100%); proxy audit paused (paid proxy list unreadable)"},
		{"parks and paused", 50, 47, 3, true, "active: 47/50 proxies authenticated (94%), 3 parked by proxy audit; proxy audit paused (paid proxy list unreadable)"},
		{"parks above ten percent do not drag the word", 100, 85, 15, false, "active: 85/100 proxies authenticated (85%), 15 parked by proxy audit"},
		{"parks never hide a full outage", 50, 0, 3, false, "critical: 0/50 proxies authenticated (0%), 3 parked by proxy audit, retrying"},
		{"all parked still says why", 10, 0, 10, false, "critical: 0/10 proxies authenticated (0%), 10 parked by proxy audit, retrying"},
		{"all parked and paused still says why", 10, 0, 10, true, "critical: 0/10 proxies authenticated (0%), 10 parked by proxy audit; proxy audit paused (paid proxy list unreadable), retrying"},
		{"parked is clamped to the total", 3, 3, 9, false, "active: 3/3 proxies authenticated (100%), 3 parked by proxy audit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxiesConfigured.Store(tc.total)
			proxiesAuthenticated.Store(tc.live)
			setProxyAuditSystemdState(int(tc.parked), tc.paused)
			if got := systemdStatusLine(); got != tc.want {
				t.Errorf("systemdStatusLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSystemdStatusLineFromFabricatedPool is the end-to-end of the composer
// with a fabricated pool state: the two counters are not stored by hand, they
// are derived from the launch set and the auth transitions, and the sentence is
// the relay's.
func TestSystemdStatusLineFromFabricatedPool(t *testing.T) {
	resetSystemdStatusState(t)
	setProxyStatusPool(fabricatedPool("a:1", "b:2", "c:3"))

	if got, want := systemdStatusLine(), "critical: 0/3 proxies authenticated (0%), retrying"; got != want {
		t.Fatalf("after the launch set, systemdStatusLine() = %q, want %q", got, want)
	}
	if got := proxiesConfigured.Load(); got != 3 {
		t.Fatalf("configured = %d, want 3", got)
	}

	markProxyStatusLive("a:1")
	markProxyStatusLive("b:2")
	if got, want := systemdStatusLine(), "degraded: 2/3 proxies authenticated (67%), retrying"; got != want {
		t.Fatalf("two authenticated, systemdStatusLine() = %q, want %q", got, want)
	}

	// The same identity authenticating again (a reconnect) is not a second
	// proxy: the count is a set, not an event tally.
	markProxyStatusLive("a:1")
	if got := proxiesAuthenticated.Load(); got != 2 {
		t.Fatalf("a repeated success count = %d, want 2", got)
	}

	markProxyStatusDown("b:2")
	if got, want := systemdStatusLine(), "critical: 1/3 proxies authenticated (33%), retrying"; got != want {
		t.Fatalf("one down, systemdStatusLine() = %q, want %q", got, want)
	}

	// An unbalanced down cannot drive the count negative, and a repeated down
	// for an identity that is already down is not a state change.
	markProxyStatusDown("b:2")
	markProxyStatusDown("c:3")
	markProxyStatusDown("c:3")
	if got := proxiesAuthenticated.Load(); got != 1 {
		t.Fatalf("authenticated = %d after unbalanced downs, want 1 (only a:1 is live)", got)
	}
	markProxyStatusDown("a:1")
	markProxyStatusDown("a:1")
	if got, want := systemdStatusLine(), "critical: 0/3 proxies authenticated (0%), retrying"; got != want {
		t.Fatalf("every identity down, systemdStatusLine() = %q, want %q", got, want)
	}
}

// TestSystemdStatusLineReportsTheParkedProxySentence pins the sentence form an
// operator reads on a busy node, including the parked note, and shows that the
// severity word is banded on the effective ratio: 42 of 50 with 3 parked is
// 42/47 = 89.4%, which is partial, while the same count with 4 parked is
// 42/46 = 91.3% and reads active. The rendered percentage is over the
// configured set in both cases, exactly as the relay renders it.
func TestSystemdStatusLineReportsTheParkedProxySentence(t *testing.T) {
	resetSystemdStatusState(t)
	addrs := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		addrs = append(addrs, proxyTestAddr(i))
	}
	setProxyStatusPool(fabricatedPool(addrs...))
	for i := 0; i < 42; i++ {
		markProxyStatusLive(proxyTestAddr(i))
	}

	setProxyAuditSystemdState(3, false)
	if got, want := systemdStatusLine(), "partial: 42/50 proxies authenticated (84%), 3 parked by proxy audit"; got != want {
		t.Errorf("3 parked: systemdStatusLine() = %q, want %q", got, want)
	}

	setProxyAuditSystemdState(4, false)
	if got, want := systemdStatusLine(), "active: 42/50 proxies authenticated (84%), 4 parked by proxy audit"; got != want {
		t.Errorf("4 parked: systemdStatusLine() = %q, want %q", got, want)
	}

	setProxyAuditSystemdState(3, true)
	if got, want := systemdStatusLine(), "partial: 42/50 proxies authenticated (84%), 3 parked by proxy audit; proxy audit paused (paid proxy list unreadable)"; got != want {
		t.Errorf("paused: systemdStatusLine() = %q, want %q", got, want)
	}
}

func proxyTestAddr(i int) string {
	return fmt.Sprintf("proxy-%d:8080", i)
}

// TestSetProxyStatusPoolDropsIdentitiesNoLongerLaunched covers a reload that
// shrinks the pool: an identity that is no longer launched cannot stay
// authenticated, or the line would report more live proxies than it runs.
func TestSetProxyStatusPoolDropsIdentitiesNoLongerLaunched(t *testing.T) {
	resetSystemdStatusState(t)
	setProxyStatusPool(fabricatedPool("a:1", "b:2"))
	markProxyStatusLive("a:1")
	markProxyStatusLive("b:2")

	setProxyStatusPool(fabricatedPool("a:1"))
	if got := proxiesConfigured.Load(); got != 1 {
		t.Fatalf("configured after the shrink = %d, want 1", got)
	}
	if got := proxiesAuthenticated.Load(); got != 1 {
		t.Fatalf("authenticated after the shrink = %d, want 1 (b:2 is no longer launched)", got)
	}
	if got, want := systemdStatusLine(), "active: 1/1 proxies authenticated (100%)"; got != want {
		t.Fatalf("systemdStatusLine() = %q, want %q", got, want)
	}

	// A late transition for the dropped identity must not resurrect it.
	markProxyStatusLive("b:2")
	if got := proxiesAuthenticated.Load(); got != 1 {
		t.Fatalf("authenticated = %d, want 1: an identity this process does not launch is not reported", got)
	}
}

// TestMarkProxyStatusLiveIgnoresAnUnlaunchedIdentity keeps the counter honest
// when the auth path reports an identity the manager never published (a
// command-path read, or a transient settings object).
func TestMarkProxyStatusLiveIgnoresAnUnlaunchedIdentity(t *testing.T) {
	resetSystemdStatusState(t)
	setProxyStatusPool(fabricatedPool("a:1", "b:2"))
	markProxyStatusLive("zz:9")
	if got := proxiesAuthenticated.Load(); got != 0 {
		t.Fatalf("authenticated = %d, want 0", got)
	}
	if got, want := systemdStatusLine(), "critical: 0/2 proxies authenticated (0%), retrying"; got != want {
		t.Fatalf("systemdStatusLine() = %q, want %q", got, want)
	}
}

// TestSystemdStatusLineNeverEmpty is a cheap invariant: an empty STATUS= line
// would leave `systemctl status` showing the unit description instead of the
// pool state.
func TestSystemdStatusLineNeverEmpty(t *testing.T) {
	resetSystemdStatusState(t)
	for status := int32(0); status <= 5; status++ {
		for _, total := range []int64{0, 1, 7} {
			for _, live := range []int64{0, 1, 3, 9} {
				proxyResolutionStatus.Store(status)
				proxiesConfigured.Store(total)
				proxiesAuthenticated.Store(live)
				if got := systemdStatusLine(); got == "" {
					t.Fatalf("systemdStatusLine() empty for status=%d total=%d live=%d", status, total, live)
				}
			}
		}
	}
}
