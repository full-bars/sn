package miner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// profile_listen.go ports the 3.23-fix provider's live profiling
// (provider/provide.go: EnableProfiling, provider/profile_retry.go) onto the
// miner, so a running miner can be profiled the way a provider can: a CPU
// profile of a node that is misbehaving in the field is the one diagnostic
// that shows where the time actually goes, and the miner had no listener at
// all.
//
// Enable path. The provider enables profiling from URNETWORK_PPROF, read once
// at startup in provide(). There is no control key and no CLI flag for it, so
// this port uses exactly the same variable and no second mechanism. That also
// means the miner's control surface does not advertise it: the control-key
// table (controlKeys, with its live/restart/none classification) is for
// settings the socket accepts, and pprof is not one of them. The unrelated
// `profile` control key is the transport profile (eco/lowmem/turbo-v4), not
// this.
//
// Binding. Stricter than the provider, deliberately. provider.EnableProfiling
// passes whatever string it is given straight to net.Listen, so
// URNETWORK_PPROF=0.0.0.0:6060 publishes heap and goroutine dumps on every
// interface. The miner refuses that: the address must be a literal loopback
// IP with a port, and the bound socket is re-checked after Listen, so a
// misconfigured enable path cannot put an unauthenticated diagnostics
// endpoint on a public interface. net/http/pprof has no authentication and a
// heap profile contains whatever the process holds in memory, including
// session tokens, so "off unless explicitly enabled, and only on loopback"
// is the contract, not a default.
//
// HotSwap. A spawned candidate starts while its parent still holds the
// loopback port for the whole drain, so the bind is retried in the background
// (only on EADDRINUSE) exactly as the provider does; without the retry the
// promoted process would lose profiling on every other swap. The retry is
// cancelled by stopProfiling and by the run context, so an exit that lands
// while it waits cannot leave a listener behind.

// envProfiling is the provider's enable variable, reused unchanged.
const envProfiling = "URNETWORK_PPROF"

// pprofProfileNames are the named profiles registered explicitly on the
// private mux, mirroring the provider's routes. The list is not an allowlist:
// http.ServeMux sends every other /debug/pprof/ path to pprof.Index, which
// dispatches dynamically through runtime/pprof.Lookup. A profile added in a
// later Go release (goroutineleak, say) is therefore served whether or not it
// is named here, and an unknown name gets pprof.Index's own 404.
var pprofProfileNames = []string{"allocs", "block", "goroutine", "mutex", "heap", "threadcreate"}

// profilingRetryLoop is one background bind retry. cancel ends the loop and
// done closes once its goroutine has returned, so stopProfiling can both stop
// a retry that has not bound yet and wait for it to be gone: a stopped
// listener must not come back when the HotSwap parent releases the port.
type profilingRetryLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

var (
	// profilingMu guards the listener, server and retry loop: the HotSwap
	// retry goroutine and stopProfiling can both touch them, and tests stop
	// and restart the listener. enableProfiling checks its context under this
	// mutex and stopProfiling cancels under it, which is what makes a bind
	// that races a stop either register (and be shut down) or close itself.
	profilingMu       sync.Mutex
	profilingServer   *http.Server
	profilingListener net.Listener
	profilingRetry    *profilingRetryLoop
)

// pprofLoopbackAddr validates spec and returns the address to bind. Only a
// literal loopback IP is accepted: a hostname is refused because it can
// resolve to a public address (and can change between check and bind), and an
// empty or wildcard host is refused because it means every interface.
func pprofLoopbackAddr(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", fmt.Errorf("pprof: %s is empty", envProfiling)
	}
	host, port, err := net.SplitHostPort(spec)
	if err != nil {
		return "", fmt.Errorf("pprof: %q must be a loopback host:port such as 127.0.0.1:6060: %w", spec, err)
	}
	if host == "" {
		return "", fmt.Errorf("pprof: %q has no host, which would bind every interface; use 127.0.0.1:%s", spec, port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("pprof: %q host %q is not a literal IP; hostnames are refused because one can resolve to a public address (use 127.0.0.1)", spec, host)
	}
	if !ip.IsLoopback() {
		return "", fmt.Errorf("pprof: %q is not loopback; pprof has no authentication and dumps heap contents, so it may only bind 127.0.0.1 or ::1", spec)
	}
	// A port of 0 is allowed: the kernel picks a free one, which is what the
	// tests and a host with several miners use.
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("pprof: %q port %q is not 0-65535", spec, port)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// enableProfiling starts the pprof HTTP listener on addr. It refuses any
// address that is not a literal loopback IP, and re-checks the socket it got,
// so a caller cannot widen the bind by passing something else.
//
// A context that is already cancelled closes whatever was bound instead of
// registering it: a stop can land while this function is between net.Listen
// and the bookkeeping below, and nothing else would ever close that socket.
func enableProfiling(ctx context.Context, addr string) error {
	listenAddr, err := pprofLoopbackAddr(addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("pprof listen on %s: %w", listenAddr, err)
	}
	// Belt and braces: even if the parse above were bypassed, the socket that
	// is actually bound must be loopback.
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return fmt.Errorf("pprof listen on %s bound %s, which is not loopback; refusing", listenAddr, ln.Addr())
	}

	server := &http.Server{
		Handler:           pprofHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	profilingMu.Lock()
	if ctx.Err() != nil {
		// stopProfiling cancels under this mutex, so a stop that raced the
		// bind is visible here: close the socket instead of registering a
		// listener that nothing will stop.
		profilingMu.Unlock()
		_ = ln.Close()
		return fmt.Errorf("pprof listen on %s: %w", listenAddr, ctx.Err())
	}
	if profilingServer != nil {
		running := profilingListener
		profilingMu.Unlock()
		_ = ln.Close()
		if running != nil {
			return fmt.Errorf("pprof is already listening on %s", running.Addr())
		}
		return fmt.Errorf("pprof is already listening")
	}
	profilingServer, profilingListener = server, ln
	profilingMu.Unlock()

	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			tlog("[profile] pprof listener failed: %v\n", err)
		}
	}()
	tlog("[profile] pprof listening on %s (loopback only)\n", ln.Addr())
	return nil
}

// pprofHandler serves the standard pprof endpoints on a private mux, so
// http.DefaultServeMux is not polluted with handlers that would then answer on
// any other listener that uses it.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range pprofProfileNames {
		name := name // capture the loop variable
		mux.HandleFunc("/debug/pprof/"+name, func(w http.ResponseWriter, r *http.Request) {
			pprof.Handler(name).ServeHTTP(w, r)
		})
	}
	return mux
}

// profilingAddr returns the address the pprof listener is bound to, or "" when
// profiling is off. Tests and the status path use it to report the real port
// (port 0 is a valid request).
func profilingAddr() string {
	profilingMu.Lock()
	defer profilingMu.Unlock()
	if profilingListener == nil {
		return ""
	}
	return profilingListener.Addr().String()
}

// stopProfiling shuts the pprof listener down and forgets it. Safe to call
// when nothing is running.
//
// It also cancels a retry that has not bound yet and waits for that goroutine
// to return, so a stop cannot be undone by a retry that was still waiting for
// the HotSwap parent to release the port.
func stopProfiling() error {
	profilingMu.Lock()
	server := profilingServer
	ln := profilingListener
	retry := profilingRetry
	profilingServer, profilingListener, profilingRetry = nil, nil, nil
	if retry != nil {
		// Cancelled under the same mutex enableProfiling checks its context
		// under: a bind in flight either registered above (and is shut down
		// below) or will see the cancellation and close what it bound.
		retry.cancel()
	}
	profilingMu.Unlock()
	if retry != nil {
		// A cancelled loop returns at once; the bound only keeps a wedged
		// bind from stalling shutdown.
		select {
		case <-retry.done:
		case <-time.After(2 * time.Second):
		}
	}
	if server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := server.Shutdown(ctx)
	if ln != nil {
		// Already closed by Shutdown in the common case; closing again just
		// returns net.ErrClosed.
		_ = ln.Close()
	}
	return err
}

// startProfilingAtStartup enables pprof when URNETWORK_PPROF is set. It is the
// miner's counterpart of the provider's block in provide(): same variable,
// same retry, read once at startup. Off unless the operator asked for it, and
// a bad value is one clear line, not a retry loop that cannot succeed.
func startProfilingAtStartup(ctx context.Context) {
	spec := strings.TrimSpace(os.Getenv(envProfiling))
	if spec == "" {
		return
	}
	// Validate once and hand the normalized address to the listener and the
	// retry logs, so [::ffff:127.0.0.1]:6060 is reported as 127.0.0.1:6060
	// everywhere instead of being re-parsed and logged as it was typed.
	addr, err := pprofLoopbackAddr(spec)
	if err != nil {
		tlog("[profile] %v\n", err)
		return
	}
	// The retry outlives this call and can outlive a shutdown that lands while
	// it waits for the HotSwap parent, so it gets its own cancel, derived from
	// the run context (a run shutdown ends it too) and triggered by
	// stopProfiling.
	retryCtx, cancelRetry := context.WithCancel(ctx)
	loop := &profilingRetryLoop{cancel: cancelRetry, done: make(chan struct{})}

	profilingMu.Lock()
	if profilingRetry != nil {
		profilingMu.Unlock()
		cancelRetry()
		tlog("[profile] pprof is already starting\n")
		return
	}
	profilingRetry = loop
	profilingMu.Unlock()

	tlog("[profile] enabling pprof on %s\n", addr)
	goSafe("pprof_listener", func() {
		defer func() {
			profilingMu.Lock()
			if profilingRetry == loop {
				profilingRetry = nil
			}
			profilingMu.Unlock()
			cancelRetry()
			close(loop.done)
		}()
		enableProfilingWithRetry(retryCtx, addr, enableProfiling, 90*time.Second, time.Second, tlog)
	})
}

// enableProfilingWithRetry keeps trying to bind the pprof address until it is
// free. A HotSwap candidate starts while its parent still holds the loopback
// port, and the parent keeps it for the whole stream drain (up to 30s) until it
// exits; trying once left the promoted process without profiling for the rest
// of its life, and only on every other swap.
//
// Only "address in use" is retried; any other failure (a non-loopback or
// malformed address) will not fix itself and is reported once. A cancelled
// context ends the loop quietly, including before the first attempt: a
// shutdown that lands between launching this goroutine and the first bind must
// not leave a listener behind.
func enableProfilingWithRetry(
	ctx context.Context,
	addr string,
	enable func(context.Context, string) error,
	wait, poll time.Duration,
	logf func(format string, args ...any),
) {
	deadline := time.Now().Add(wait)
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		err := enable(ctx, addr)
		if err == nil {
			if 1 < attempt {
				logf("[profile] pprof enabled on %s after the hotswap parent released it\n", addr)
			}
			return
		}
		// A cancel that landed while enable was binding is not a bind failure:
		// enable closed what it bound and reported the context error.
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			logf("[profile] failed to enable pprof: %v\n", err)
			return
		}
		if !time.Now().Before(deadline) {
			logf("[profile] gave up enabling pprof on %s after %s: %v\n", addr, wait, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}
