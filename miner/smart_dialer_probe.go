package miner

// smart_dialer_probe.go ports the 3.23-fix v33 provider's
// provider/smart_dialer_probe.go: measure, in the background, the connect cost
// of the transports the dialer never gets to try.
//
// The smart dialer ranks the transports it has measured, and the first-choice
// transport wins wherever it works, so the others are never dialed and never
// measured (connect.ClientStrategy.ProbeDialersWith exists for exactly this).
// Each identity's strategy therefore probes its own dialer set: the cost of a
// transport is a property of the path that identity's traffic takes, not of the
// host, so a proxied slot's measurement is that proxy's measurement and the
// direct slot's is the host's own.
//
// Where this attaches in the miner, and what that costs:
//
// The provider builds the client strategy that serves its control plane, so the
// provider probes that one. The miner does not: the slot assembles its settings
// in slotClientStrategySettings and `sdk.NewNetworkSpaceWithUrls` builds the
// strategy from them; a non-hosted `sdk.DeviceLocal` (the one
// newProviderDeviceLocal creates) then SHARES that same strategy
// (`networkSpace.clientStrategy`), so one strategy per slot serves the API, the
// device's control dials and the platform OOB calls. The pin exposes no
// accessor for it, so the run cannot name it and cannot probe it — probing a
// second strategy built from the same settings would measure a dialer set
// nothing else dials with.
//
// The run therefore keeps one strategy of its own, built from those same
// settings (same proxy, same ConnectSettings, same logger and leg observer),
// for the dials the run itself drives: the slot's wallet proof and its
// client-identity renewal. That is the strategy probed here. It is per
// identity, per its settings, exactly like the provider's; the slot's API and
// device strategy is not covered until the pin gains an accessor.
//
// Cost: a probe is a real GET to the api url on a connection of its own, so it
// rides the slot's proxy and lands on the slot's control-plane bandwidth
// counter, the same one the slot's api calls already land on. The schedule
// (smart_dialer_schedule.go) is what bounds it.

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const (
	// smartDialerProbeCheckInterval is how often each strategy checks whether
	// any transport lacks a current connect-cost measurement. The library keeps
	// its own, much longer, refresh schedule (it backs off while the ranking is
	// stable), so a check with nothing due does no network work and this can be
	// short.
	smartDialerProbeCheckInterval = 5 * time.Minute
	// smartDialerProbeMinDelay and smartDialerProbeStartJitter spread the first
	// check of a large proxy set. The identity has already authenticated by the
	// time this starts, and the scheduler holds the actual connects behind the
	// startup grace and the auth-idle gate.
	smartDialerProbeMinDelay    = 5 * time.Second
	smartDialerProbeStartJitter = 55 * time.Second
)

// startSmartDialerProbes measures, in the background, the connect cost of
// every transport the strategy has not been able to measure on its own.
//
// The preference gates allocation, not just the connects: while the smart
// dialer is off (the default) this returns at once, so an opted-out identity
// carries no probe goroutine, no probe context, no timer and no summary
// ticker. Only an operator who turned the key on pays for any of it.
// Call it only after the identity authenticated: an identity that never
// authenticates, or sits in slow retry, never probes.
//
// index and addr identify the identity in the logs; addr is host:port (or
// "direct"), never the proxy key. owner is the identity the gateway hold is
// taken for (smartDialerGatewayOwner): the proxy's key, not the slot label, so
// a slot that a reload renumbers keeps the address it already owns. A proxied
// identity skips the transports that behave identically behind SOCKS.
//
// The returned stop ends the loop and waits for it. The provider leaves the
// loop to the strategy's context; the miner's slot closes its strategy at the
// end of the slot, and a probe must not run against a closed strategy, so the
// caller stops the probes first (the defer registered after the strategy's
// close runs before it).
func startSmartDialerProbes(ctx context.Context, strategy *connect.ClientStrategy, apiUrl string, owner string, index int, addr string, viaSocks bool) (stop func()) {
	if strategy == nil || !connect.SmartDialerEnabled() {
		// Off is the default. Nothing is allocated on this path: the caller
		// gets a stop that is safe to call (and to call twice), and the
		// process gets no goroutine, no timer and no summary ticker per
		// identity. Turning the key on is restart-required, so a slot that is
		// already running keeps what it started with; see below for the belt
		// to that brace.
		return func() {}
	}
	delay := smartDialerProbeMinDelay + time.Duration(rand.Int63n(int64(smartDialerProbeStartJitter)))
	options := connect.SmartDialerProbeOptions{
		CollapseSocksTwins: viaSocks,
		Gate: func(ctx context.Context) (func(), error) {
			return globalSmartDialerScheduler.Gate(ctx, addr, owner)
		},
		OnRound: func(round connect.ProbeRound) {
			logProbeRound(index, addr, round)
		},
	}
	startSmartDialerSummary()
	probeCtx, probeCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go connect.HandleError(func() {
		defer close(done)
		runSmartDialerProbes(probeCtx, delay, smartDialerProbeCheckInterval, func() bool {
			// The key is restart-required, so in practice nothing flips this
			// under a running loop. If anything ever does turn the preference
			// off, the loop must stop rather than keep waking every interval
			// to run a probe that measures nothing: off has to mean off.
			return !connect.SmartDialerEnabled()
		}, func(ctx context.Context) int {
			return strategy.ProbeDialersWith(ctx, apiUrl, options)
		})
	})
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			probeCancel()
			<-done
			stopSmartDialerSummary()
		})
	}
}

// runSmartDialerProbes waits out the initial delay, then runs probe on every
// interval until ctx ends or withdrawn reports that the preference is gone.
// What a round found is reported by the probe's own OnRound hook, not here.
func runSmartDialerProbes(ctx context.Context, initialDelay time.Duration, interval time.Duration, withdrawn func() bool, probe func(context.Context) int) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if withdrawn != nil && withdrawn() {
			return
		}
		probe(ctx)
		timer.Reset(interval)
	}
}
