package provider

import (
	"context"
	"math/rand"
	"strconv"
	"time"

	"github.com/urnetwork/connect"
)

const (
	// smartDialerProbeCheckInterval is how often each proxy's strategy checks
	// whether any transport lacks a current connect-cost measurement. The
	// library keeps its own, much longer, refresh schedule (it backs off while
	// the ranking is stable), so a check with nothing due does no network work
	// and this can be short.
	smartDialerProbeCheckInterval = 5 * time.Minute
	// smartDialerProbeMinDelay and smartDialerProbeStartJitter spread the first
	// check of a large proxy set. The proxy has already authenticated by the
	// time this starts, and the scheduler holds the actual connects behind the
	// startup grace and the auth-idle gate.
	smartDialerProbeMinDelay    = 5 * time.Second
	smartDialerProbeStartJitter = 55 * time.Second
)

// startSmartDialerProbes measures, in the background, the connect cost of
// every transport the strategy has not been able to measure on its own (the
// first-choice transport always wins where it works, so the others are never
// dialed; see connect.ClientStrategy.ProbeDialersWith). Everything is a no-op
// while the smart dialer is off. Call it only after the proxy authenticated: a
// proxy that never authenticates, or sits in slow retry, never probes.
//
// index and addr identify the proxy in the logs; addr is host:port, never the
// proxy key. A proxy reached through SOCKS skips the transports that behave
// identically there.
func startSmartDialerProbes(ctx context.Context, strategy *connect.ClientStrategy, apiUrl string, tag string, index int, addr string, viaSocks bool) {
	delay := smartDialerProbeMinDelay + time.Duration(rand.Int63n(int64(smartDialerProbeStartJitter)))
	owner := tag + "[" + strconv.Itoa(index) + "]"
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
	go connect.HandleError(func() {
		runSmartDialerProbes(ctx, delay, smartDialerProbeCheckInterval, func(ctx context.Context) int {
			return strategy.ProbeDialersWith(ctx, apiUrl, options)
		})
	})
}

// runSmartDialerProbes waits out the initial delay, then runs probe on every
// interval until ctx ends. What a round found is reported by the probe's own
// OnRound hook, not here.
func runSmartDialerProbes(ctx context.Context, initialDelay time.Duration, interval time.Duration, probe func(context.Context) int) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		probe(ctx)
		timer.Reset(interval)
	}
}
