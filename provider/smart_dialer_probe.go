package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"strings"
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

// applyPersistedSmartDialer hands the persisted preference to the dialer, once,
// before any identity starts. This is the only writer of the runtime flag from
// control state, and it is why smart_dialer is restart-required rather than
// live (see liveEffectKeys in control_socket.go): the probe loop that feeds a
// measurement is created per identity when it starts, so a `set` on a running
// provider cannot create one and only persists the value for the next start.
//
// Replayed in both directions, including the off default: an unset key must put
// the flag at off rather than leave whatever an earlier replay left behind.
func applyPersistedSmartDialer(state *controlState) {
	on := false
	if v, ok := state.get("smart_dialer"); ok {
		on = strings.EqualFold(strings.TrimSpace(v), "on")
	}
	connect.SetSmartDialer(on)
}

// smartDialerProbeOwner is the identity the shared-gateway hold keys on. It must
// be stable across a proxy reload: the hold is renewed while the same proxy
// keeps probing, so an identity built from the proxy's slot number would both
// free an address another proxy is still probing (when the slots move) and lock
// a proxy out of an address it already owns (when its slot number changes),
// for the whole smartDialerGatewayHold. The key is the identity the rest of the
// provider uses (see jwtStoreKey); it is hashed because the raw key carries the
// proxy's user name and a separator that does not belong in a log-adjacent
// string.
func smartDialerProbeOwner(identityKey string) string {
	if identityKey == "" || identityKey == "direct" {
		return "direct"
	}
	sum := sha256.Sum256([]byte(identityKey))
	return "proxy:" + hex.EncodeToString(sum[:8])
}

// startSmartDialerProbes measures, in the background, the connect cost of
// every transport the strategy has not been able to measure on its own (the
// first-choice transport always wins where it works, so the others are never
// dialed; see connect.ClientStrategy.ProbeDialersWith).
//
// A node with the smart dialer off starts nothing at all: no goroutine and no
// timer. The check inside ProbeDialersWith already made an idle round cost no
// network work, but the loop itself still cost one goroutine and its stack per
// proxy, and on a node with thousands of proxies that is thousands of
// goroutines that can never do anything. The preference is read here, when the
// identity starts, so a node that turns it on at runtime begins probing on its
// next start rather than within one interval.
//
// Call it only after the proxy authenticated: a proxy that never authenticates,
// or sits in slow retry, never probes.
//
// owner is smartDialerProbeOwner's identity for this proxy, index and addr
// identify the proxy in the logs; addr is host:port, never the proxy key. A
// proxy reached through SOCKS skips the transports that behave identically
// there.
func startSmartDialerProbes(ctx context.Context, strategy *connect.ClientStrategy, apiUrl string, owner string, index int, addr string, viaSocks bool) {
	if !connect.SmartDialerEnabled() {
		return
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
