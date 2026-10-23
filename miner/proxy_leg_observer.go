package miner

// proxy_leg_observer.go: the producer for the leg evidence in proxy_leg.go.
//
// The connect library hands the transport's Logger the typed result of every
// control dial when that logger implements connect.ControlDialObserver. The
// miner builds one device settings object per provider slot, so wrapping its
// logger there gives each slot its own observer, and a failure inside that
// slot's proxy connect step lands on that slot's streak.

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// minerProxyLeg is the process-wide evidence store. Slots are keyed by the
// proxy identity where there is one, so two accounts behind one gateway are
// still two proxies, exactly as the bandwidth counters key them.
var minerProxyLeg proxyLegTracker

// isProxyLegError reports whether a control dial failed inside the proxy's own
// connect step. The socks dialer wraps that failure in a *net.OpError whose Op
// names the socks command, which is what separates "this proxy cannot carry
// traffic" from "the platform is unreachable".
func isProxyLegError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && strings.HasPrefix(opErr.Op, "socks")
}

// proxyLegObservingLogger forwards to the logger the settings already carried,
// and reports control dials to the tracker. It must not block: the tracker's
// work is a short mutex and a counter.
type proxyLegObservingLogger struct {
	connect.Logger
	proxyLegId string
}

func (self *proxyLegObservingLogger) ObserveControlDial(tag string, network string, addr string, err error) {
	if tag != "platform" {
		return
	}
	switch {
	case err == nil:
		minerProxyLeg.RecordSuccess(self.proxyLegId, time.Now())
	case isProxyLegError(err):
		minerProxyLeg.RecordFailure(self.proxyLegId, time.Now())
	}
}

// withProxyLegObserver wraps a device settings logger so the slot's control
// dials reach the leg evidence. A nil logger keeps its nil behaviour, since the
// library falls back to its own default.
func withProxyLegObserver(log connect.Logger, proxyLegId string) connect.Logger {
	if log == nil {
		return nil
	}
	if _, ok := log.(connect.ControlDialObserver); ok {
		// an embedder logger that already observes is left alone
		return log
	}
	return &proxyLegObservingLogger{Logger: log, proxyLegId: proxyLegId}
}

// proxyLegTotals is what the scrape reports: summed streak and the number of
// proxies the shadow decision currently flags.
func proxyLegTotals() (int64, int64) {
	return minerProxyLeg.Totals()
}
