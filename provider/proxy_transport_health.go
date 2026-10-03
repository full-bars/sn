package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/urnetwork/connect"
)

// Connection-driven proxy health.
//
// A proxy's health entry must follow its platform transport's REAL connection
// state: up only once the transport has authenticated and registered routes,
// down when that connection drops, and every failed connect attempt counted as
// a transport auth failure. The revocation watcher (provider_auth.go) and the
// renewal watcher's transport-auth trigger (renewal_watcher.go) read exactly
// these inputs; marking a proxy up when its transport is merely constructed
// made both of them inert.
//
// The pinned connect engine exposes the connection state through
// PlatformTransport.IsConnected and ConnectedNotify. It has no per-attempt
// connect-failure callback; the only per-transport signal for a failed connect
// is the "[t]auth error" line the transport logs through its settings Log.
// transportFailureLogger observes that line (by its format string, not its
// rendered text) and forwards every call unchanged to the real logger.
// TestTransportAuthErrorFormatsMatchPinnedEngine pins the format strings
// against the engine source so an engine bump that rewords them fails loudly
// instead of silently zeroing the failure count.

// transportConnState is the slice of *connect.PlatformTransport that
// connection-driven health needs. An interface so tests can drive it.
type transportConnState interface {
	IsConnected() bool
	ConnectedNotify() <-chan struct{}
}

// errProxyTransportDropped is recorded as the last error when a live platform
// transport connection drops.
var errProxyTransportDropped = errors.New("platform transport connection dropped")

// The engine's per-attempt connect failure lines. Both the throttled INFO form
// and the V(1) form use these formats; see transport.go in the pinned engine.
const (
	transportAuthErrFormat           = "[t]auth error %s = %s\n"
	transportAuthErrSuppressedFormat = "[t]auth error %s = %s (%d suppressed)\n"
)

// proxyTransportHealth binds one proxy instance's health entry to its platform
// transport. Create it before the transport (its logger goes into the
// transport settings), then call start with the constructed transport.
type proxyTransportHealth struct {
	ctx   context.Context
	index int

	mu        sync.Mutex
	transport transportConnState
}

func newProxyTransportHealth(ctx context.Context, index int) *proxyTransportHealth {
	return &proxyTransportHealth{ctx: ctx, index: index}
}

// logger wraps base so failed connect attempts reach noteConnectFailure.
func (h *proxyTransportHealth) logger(base connect.Logger) connect.Logger {
	return &transportFailureLogger{Logger: base, onAuthError: h.noteConnectFailure}
}

// start begins following t's connection state. It never marks the proxy up
// before t reports a live connection. The returned stop cancels the watcher
// and waits for it to exit, so a caller that defers stop after deferring
// markProxyDown cannot have a late markProxyUp land after its final down.
// The watcher also exits on its own when the proxy context ends.
func (h *proxyTransportHealth) start(t transportConnState) (stop func()) {
	h.mu.Lock()
	h.transport = t
	h.mu.Unlock()
	watchCtx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go connect.HandleError(func() {
		defer close(done)
		watchTransportConnection(watchCtx, h.index, t)
	})
	return func() {
		cancel()
		<-done
	}
}

// noteConnectFailure records one failed connect attempt as a transport auth
// failure. Attempts that fail because the proxy is shutting down are local
// teardown, not a backend signal, and an attempt that fails while the
// transport is still connected over another mode (an H3 race lost while H1
// serves) is not a failure to connect.
func (h *proxyTransportHealth) noteConnectFailure(err error) {
	if h.ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	h.mu.Lock()
	t := h.transport
	h.mu.Unlock()
	if t != nil && t.IsConnected() {
		return
	}
	RecordProxyAuthFailure(h.index, err)
}

// watchTransportConnection marks proxyIndex up on each transition to
// connected and down (with a transport drop) on each transition back to
// disconnected. The notify channel is captured before IsConnected is read, as
// the engine requires, so no transition is missed.
func watchTransportConnection(ctx context.Context, proxyIndex int, t transportConnState) {
	connected := false
	for {
		notify := t.ConnectedNotify()
		if ctx.Err() != nil {
			// Teardown is not a drop; provideWithProxy marks the proxy
			// down on exit.
			return
		}
		now := t.IsConnected()
		switch {
		case now && !connected:
			markProxyUp(proxyIndex)
		case !now && connected:
			markProxyDown(proxyIndex)
			RecordProxyTransportDrop(proxyIndex, errProxyTransportDropped)
		}
		connected = now
		select {
		case <-ctx.Done():
			return
		case <-notify:
		}
	}
}

// transportFailureLogger forwards everything to the wrapped logger and calls
// onAuthError for each "[t]auth error" line. V(1) always reports enabled so
// the engine takes its per-attempt branch even when the INFO line is
// throttled; V(1) output is still only written when the wrapped logger has
// V(1) enabled.
type transportFailureLogger struct {
	connect.Logger
	onAuthError func(err error)
}

func (l *transportFailureLogger) observe(format string, args []any) {
	if format != transportAuthErrFormat && format != transportAuthErrSuppressedFormat {
		return
	}
	var err error
	if 2 <= len(args) {
		if e, ok := args[1].(error); ok {
			err = e
		} else {
			err = fmt.Errorf("%v", args[1])
		}
	}
	l.onAuthError(err)
}

func (l *transportFailureLogger) Infof(format string, args ...any) {
	l.observe(format, args)
	l.Logger.Infof(format, args...)
}

func (l *transportFailureLogger) V(level int32) connect.Verbose {
	inner := l.Logger.V(level)
	if level != 1 {
		return inner
	}
	return &transportFailureVerbose{inner: inner, l: l}
}

type transportFailureVerbose struct {
	inner connect.Verbose
	l     *transportFailureLogger
}

func (v *transportFailureVerbose) Enabled() bool { return true }

func (v *transportFailureVerbose) Info(args ...any) {
	if v.inner.Enabled() {
		v.inner.Info(args...)
	}
}

func (v *transportFailureVerbose) Infof(format string, args ...any) {
	v.l.observe(format, args)
	if v.inner.Enabled() {
		v.inner.Infof(format, args...)
	}
}
