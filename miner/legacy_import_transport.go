// The proof client's transport. The import proves saved identities with small
// control-plane HTTPS reads, so it gets its own client whose dialing is
// bounded and family-aware: the host's working family first (IPv4 first when
// both work, which is what a dual-stack host with a broken IPv6 path needs),
// a fast fallback to the other family, and no dial that can outlive the
// per-proof deadline. This is the fix for the silent multi-minute stall: the
// same proof reads used to ride the process-wide strategy, whose control dials
// can sit on an IPv6-first connection well past the point an operator would
// call the start dead.
package miner

import (
	"context"
	"net"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// The proof client's bounds. The per-proof deadline (legacyImportProofTimeout)
// is the outer limit; these keep one bad family or one stalled handshake from
// eating it.
const (
	// legacyImportProverConnectTimeout bounds one TCP connect attempt.
	legacyImportProverConnectTimeout = 5 * time.Second
	// legacyImportProverTlsTimeout bounds the TLS handshake of the api client.
	legacyImportProverTlsTimeout = 8 * time.Second
	// legacyImportProverDialTimeout bounds the FIRST family attempt; the
	// second attempt gets whatever the per-proof deadline has left, so a
	// blackholed first family costs seconds, never minutes.
	legacyImportProverDialTimeout = 4 * time.Second
	// legacyImportProverHandshakeBound is the first-handshake family-demotion
	// bound (connect's ControlFamilyFirstHandshakeTimeout): a connection that
	// succeeds but stalls in the handshake is retried over the other family
	// inside the proof, instead of hanging until the caller gives up.
	legacyImportProverHandshakeBound = 5 * time.Second
	// legacyImportProverRetryReserve must remain after the bound for that
	// retry to have room, or connect leaves the handshake unbounded.
	legacyImportProverRetryReserve = 2 * time.Second
)

// legacyImportProofDialWait is the transport's narration hook: it is called
// just before a family attempt starts.
type legacyImportProofDialWait func(family string, attempt int, timeout time.Duration)

// legacyImportProofDialer dials the proof client: an explicit family when
// connect narrowed the network, otherwise the host's working family first
// with a fast fallback to the other.
type legacyImportProofDialer struct {
	dial         connect.DialContextFunction
	family4      bool
	family6      bool
	firstTimeout time.Duration
	wait         legacyImportProofDialWait
}

type legacyImportDialFamily struct {
	network string
	name    string
}

func newLegacyImportProofDialer(settings *connect.ConnectSettings, wait legacyImportProofDialWait) *legacyImportProofDialer {
	var dial connect.DialContextFunction
	if settings != nil {
		dial = settings.NetDialer().DialContext
	} else {
		dial = (&net.Dialer{}).DialContext
	}
	return &legacyImportProofDialer{
		dial:         dial,
		family4:      connect.FamilySupported(4),
		family6:      connect.FamilySupported(6),
		firstTimeout: legacyImportProverDialTimeout,
		wait:         wait,
	}
}

// familyOrder is the proof's family preference: IPv4 first when the host has a
// working IPv4 path (the reference host shape: dual stack with a broken IPv6
// route), IPv6 first when it is the only working path, and no preference when
// the probe found neither (the dial then keeps connect's own race, still
// bounded by the per-proof context).
func (self *legacyImportProofDialer) familyOrder() (legacyImportDialFamily, legacyImportDialFamily) {
	switch {
	case self.family4 && self.family6:
		return legacyImportDialFamily{"tcp4", "v4"}, legacyImportDialFamily{"tcp6", "v6"}
	case self.family4:
		return legacyImportDialFamily{"tcp4", "v4"}, legacyImportDialFamily{}
	case self.family6:
		return legacyImportDialFamily{"tcp6", "v6"}, legacyImportDialFamily{}
	default:
		return legacyImportDialFamily{}, legacyImportDialFamily{}
	}
}

func (self *legacyImportProofDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp4", "tcp6":
		// connect narrowed the network (force or a learned demotion). Honor
		// it, still under the first-attempt bound.
		name := "v4"
		if network == "tcp6" {
			name = "v6"
		}
		return self.attempt(ctx, network, name, 1, self.attemptTimeout(ctx, self.firstTimeout), addr)
	}

	first, second := self.familyOrder()
	if first.network == "" {
		// No family evidence: one family-agnostic attempt, bounded by the
		// caller's own (per-proof) deadline.
		return self.dial(ctx, network, addr)
	}
	conn, err := self.attempt(ctx, first.network, first.name, 1, self.attemptTimeout(ctx, self.firstTimeout), addr)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil || second.network == "" {
		return nil, err
	}
	// Fast fallback: the second family gets whatever the per-proof deadline
	// has left. A refused or blackholed first family costs at most
	// firstTimeout before this runs.
	return self.attempt(ctx, second.network, second.name, 2, self.attemptTimeout(ctx, 0), addr)
}

// attemptTimeout caps one attempt at cap, never past the caller's deadline.
func (self *legacyImportProofDialer) attemptTimeout(ctx context.Context, cap time.Duration) time.Duration {
	timeout := cap
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return time.Millisecond
		}
		if timeout <= 0 || remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		timeout = self.firstTimeout
	}
	return timeout
}

func (self *legacyImportProofDialer) attempt(ctx context.Context, network, family string, attempt int, timeout time.Duration, addr string) (net.Conn, error) {
	if self.wait != nil {
		self.wait(family, attempt, timeout)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return self.dial(attemptCtx, network, addr)
}

// newSdkLegacyImportProver builds the proof client: one direct control-plane
// API whose dials prefer the host's working family and are bounded at every
// step. The proof reads are small public/self reads, like the login the
// operator already runs from this host; a per-slot proxy is deliberately not
// used so a dead proxy slot cannot silently discard the other slots.
func newSdkLegacyImportProver(ctx context.Context, apiUrl string, wait legacyImportProofDialWait) (legacyImportProver, func(), error) {
	settings := connect.DefaultClientStrategySettings()
	settings.ConnectSettings.ConnectTimeout = legacyImportProverConnectTimeout
	settings.ConnectSettings.TlsTimeout = legacyImportProverTlsTimeout
	settings.ConnectSettings.ControlFamilyFirstHandshakeTimeout = legacyImportProverHandshakeBound
	settings.ConnectSettings.ControlFamilyRetryReserve = legacyImportProverRetryReserve
	dialer := newLegacyImportProofDialer(&settings.ConnectSettings, wait)
	settings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{DialContext: dialer.DialContext}
	clientStrategy := connect.NewClientStrategy(ctx, settings)
	api := sdk.NewApi(ctx, clientStrategy, apiUrl)
	return &sdkLegacyImportProver{api: api}, func() {
		clientStrategy.Close()
		api.CloseAndWait(context.Background())
	}, nil
}
