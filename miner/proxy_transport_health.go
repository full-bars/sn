package miner

// proxy_transport_health.go: a slot's health follows its platform transport's
// real connection state, not just the outcome of its authentication.
//
// Port of provider/proxy_transport_health.go. The provider builds its own
// connect.PlatformTransport and so can watch IsConnected/ConnectedNotify
// directly: "up" means the transport authenticated and registered routes, a
// transition back to disconnected marks the proxy down and counts one transport
// drop. The miner does not own the transport — sdk.DeviceLocal builds it inside
// the provider from this slot's clientStrategySettings, which carry the proxy —
// and exposes exactly that fact as GetProviderConnected: "the provider's current
// platform carrier has a registered route. This is the headless readiness
// signal; object construction alone does not mean the provider can be
// discovered or carry traffic." That is the same claim the provider's
// IsConnected makes, one layer up.
//
// The pin exports no connected-notify for the provider transport (the SDK's own
// watchProviderConnected is internal), so this port samples the read instead of
// waiting on an edge. A sampled disconnect is held for
// proxyTransportDropConfirm before it counts, because one sample is not
// evidence: a registration can churn for a moment without the carrier being
// gone.
//
// A transport migration is not what that hold is for, and it is not a case this
// watch can hit. Every migration the provider receives is platform-directed: a
// control-frame ResidentMigrate becomes requestPlatformTransportMove with
// platformDirected set, and the provider's madeBeforeBreak refuses to release
// the live carrier for those -- a replacement the memory budget cannot hold
// beside it is closed instead, and that is the H3-to-H3 memory-capped case. Only
// a carrier policy change (SetTransportPolicy, which nothing in this repo calls)
// still breaks first. The pin asserts exactly this in
// TestDeviceLocalProviderPlatformMigrationNeverBreaksBeforeMake, over a drain, a
// compulsory move and an advisory move under a full H3 budget. So a migration
// leaves the previous generation installed and connected for its whole
// duration, and GetProviderConnected reads false here only when a carrier that
// was carrying traffic is actually gone.
//
// That is a property of the pin, not a guarantee of the SDK's ABI, so it is
// stated where a change would break it: DeviceLocal.SetProviderTransportSettings
// is what reaches SetTransportPolicy, nothing here calls it -- the H3 setting
// reaches an identity as h3ModePreferences when it builds its transport, and a
// change relaunches the identity (applyH3Mode) -- and wiring a live carrier
// policy change in would make a break-before-make migration reachable, reading
// as a drop for up to the SDK's MigrateConnectTimeout (60s). Widening this
// window is not the fix for that: it would blind real drops for the same 60s,
// while a false drop costs one counter increment and a transient
// recently_offline that cannot reach the reaper's floor
// (degradedReaperMinDownTime, 30m). The fix would be a carrier generation the
// SDK exports, so a swap can be told from a death.
//
// What this is NOT: noteLiveProxyLegFailure (proxy_leg_observer.go) counts a
// platform dial that failed inside the proxy's connect step. That is a failed
// attempt to establish, it can happen to a proxy that never carried anything,
// and it deliberately excludes unreachable-proxy and rejected-login errors. A
// transport drop is the opposite order: the connection existed and then went
// away, whatever the cause. Same family of complaint, different evidence, and
// the counters stay separate so neither is read as the other.
//
// One boundary is kept from the existing miner design: the auth path still marks
// the identity up on a successful authentication (noteProviderAuthSuccess),
// because that authentication is an API round trip through this slot's proxy and
// proves the relay carried it. What it cannot prove is that the connection stays
// up, which is what this watch adds. An identity that authenticates and then
// never establishes a transport stays "up" until the auth path observes a
// failure; only a connection that existed and was lost is a drop.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// proxyTransportState is the slice of *sdk.DeviceLocal that transport health
// needs. An interface so tests can drive it.
type proxyTransportState interface {
	GetProviderConnected() bool
}

const (
	// proxyTransportWatchInterval is how often a live slot samples its provider
	// transport. The read is a mutex and a bool; the cadence is well under the
	// health cadence (proxyHealthLogInterval, 60s) so a drop reaches health
	// before the next snapshot is written.
	proxyTransportWatchInterval = 5 * time.Second
)

// proxyTransportDropConfirm is how long a disconnected reading must persist
// before it counts as a drop. A variable so tests can shorten it; zero confirms
// on the first disconnected sample.
var proxyTransportDropConfirm = 10 * time.Second

// watchProxyTransportHealth follows transport's connection state for the
// identity key and returns a stop that cancels the watch and waits for it to
// exit. A caller that defers stop before it defers the device close cannot have
// the close itself read as a drop.
func watchProxyTransportHealth(ctx context.Context, key string, transport proxyTransportState, interval time.Duration) (stop func()) {
	if key == "" || transport == nil {
		return func() {}
	}
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchProxyTransportConnection(watchCtx, key, transport, interval)
	}()
	return func() {
		cancel()
		<-done
	}
}

// watchProxyTransportConnection marks the identity up on each transition to
// connected and down (counting a transport drop) on each confirmed transition
// back to disconnected. It never marks the identity up before the transport has
// reported a live connection: an identity that never connects is an auth or
// reachability problem, which the auth path already records.
func watchProxyTransportConnection(ctx context.Context, key string, transport proxyTransportState, interval time.Duration) {
	if interval <= 0 {
		interval = proxyTransportWatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	connected := false
	var disconnectedSince time.Time
	for {
		// Teardown is not a drop: the slot leaving (reload, reaper, shutdown)
		// closes the device, which reads as disconnected.
		if ctx.Err() != nil {
			return
		}
		now := transport.GetProviderConnected()
		switch {
		case now:
			disconnectedSince = time.Time{}
			if !connected {
				connected = true
				noteLiveProxyTransportUp(key)
			}
		case connected:
			if disconnectedSince.IsZero() {
				disconnectedSince = time.Now()
			}
			// Hold the disconnected read: one sample is not a drop. A
			// migration cannot appear here (see the file comment: the
			// platform-directed paths never release the live carrier), so
			// this window is sized for sampling noise, not for a swap.
			if time.Since(disconnectedSince) >= proxyTransportDropConfirm {
				if ctx.Err() != nil {
					return
				}
				connected = false
				disconnectedSince = time.Time{}
				noteLiveProxyHealthDrop(key)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// writeProxyTransportDropMetric emits the transport-drop gauge, the counter the
// provider reports as "drops:" in its health line. Its own series, beside the
// auth-failure and proxy-leg gauges, because the three are different failures.
func writeProxyTransportDropMetric(b *strings.Builder, drops int64) {
	fmt.Fprintf(b, "# HELP urnet_proxy_transport_drops Platform transport connections lost after being established, summed over the proxies currently in state.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_transport_drops gauge\n")
	fmt.Fprintf(b, "urnet_proxy_transport_drops %d\n", drops)
}
