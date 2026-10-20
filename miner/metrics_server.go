package miner

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// metrics_server.go ports the 3.23-fix v33 provider's /metrics lifecycle
// (provider/control_socket.go: applyMetricsLive, serveMetrics, stopMetrics,
// startMetricsAfterTakeover, metricsEnabledByState, startDefaultMetrics) onto
// the miner, plus the HTTP handler.
//
// v33 serves connect's own Prometheus handler and appends the provider's
// families through connect.SetExtraMetricsProvider. The pinned connect fork
// has neither hook, so the miner serves the whole payload from
// minerExtraMetrics() itself — the same adaptation the legacy provider in this
// repository made (provider/metrics_stubs.go).

var (
	// metricsMu guards metricsServer and metricsListener: the control socket
	// serves connections concurrently, so "set metrics" from two clients must
	// not race the rebind path.
	metricsMu sync.Mutex

	// metricsListener is the socket metricsServer serves on. stopMetrics
	// closes it directly: Shutdown alone does not free the port when it
	// returns if Serve has not registered the listener yet, and a HotSwap
	// candidate needs the port released by the time the parent has yielded.
	metricsListener net.Listener

	// metricsServer is the running /metrics server, nil when off.
	metricsServer *http.Server

	// metricsUnregCloser unregisters the hotswap yield closer serveMetrics
	// installs. It is cleared by stopMetrics so a re-served listener does not
	// leave a stale closer behind. Guarded by metricsMu.
	metricsUnregCloser func()
)

// metricsHandoffPending is true while this process is a HotSwap candidate
// that has not taken over yet. The parent holds the metrics port until it
// yields, so a listener started earlier either fails to bind or lands on a
// different port. startMetricsAfterTakeover clears it.
//
// run.go sets it at startup when this process is a spawned HotSwap candidate
// (startMetricsAtStartup), and the candidate's takeover continuation clears it
// through startMetricsAfterTakeover.
var metricsHandoffPending atomic.Bool

// serveMetrics serves /metrics on ln. It is the miner's port of v33's
// serveMetrics, including the HotSwap coordinator registration: a retiring
// parent releases the listener when it yields at the handoff commit point, so
// the promoted candidate can rebind the same address. The miner's run path
// still owns stopMetrics on ordinary shutdown.
func serveMetrics(ln net.Listener) {
	server := &http.Server{
		Handler:           metricsHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	metricsMu.Lock()
	metricsServer = server
	metricsListener = ln
	if metricsUnregCloser != nil {
		metricsUnregCloser()
	}
	metricsUnregCloser = RegisterCoordinatorCloser(func() {
		if err := stopMetrics(); err != nil {
			tlog("[metrics] releasing /metrics for hotswap: %v\n", err)
		}
	})
	metricsMu.Unlock()
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			tlog("[metrics] listener failed: %v\n", err)
		}
	}()
	tlog("[metrics] started Prometheus /metrics on %s\n", ln.Addr())
}

// metricsHandler serves the Prometheus text exposition. Only /metrics is
// served: the payload carries per-proxy series, so nothing else on the
// listener should answer at all.
func metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		body := minerExtraMetrics()

		// Prometheus asks for gzip. A node with thousands of proxies serves
		// megabytes of text per scrape; compressed it is about a twelfth of
		// the size.
		w.Header().Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			_, _ = io.WriteString(zw, body)
			_ = zw.Close()
			return
		}
		_, _ = io.WriteString(w, body)
	})
}

// acceptsGzip reports whether the Accept-Encoding header allows gzip.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		token := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(token, "gzip") {
			return true
		}
	}
	return false
}

// stopMetrics shuts the /metrics listener down and forgets it. Safe to call
// when nothing is running.
func stopMetrics() error {
	metricsMu.Lock()
	if metricsUnregCloser != nil {
		metricsUnregCloser()
		metricsUnregCloser = nil
	}
	server := metricsServer
	ln := metricsListener
	metricsServer = nil
	metricsListener = nil
	metricsMu.Unlock()
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

// applyMetricsLive starts or stops the Prometheus /metrics listener.
func applyMetricsLive(value string) error {
	enabled := strings.EqualFold(value, "on")
	metricsMu.Lock()
	running := metricsServer != nil
	metricsMu.Unlock()
	if enabled && !running {
		// Bind before reporting success: a port that cannot be bound is an
		// error the caller sees, not a log line from a goroutine after the
		// command already returned OK.
		ln, err := listenMetrics(0)
		if err != nil {
			return fmt.Errorf("metrics on: %w", err)
		}
		serveMetrics(ln)
	} else if !enabled && running {
		if err := stopMetrics(); err != nil {
			return fmt.Errorf("metrics off: %w", err)
		}
		tlog("[metrics] stopped Prometheus /metrics\n")
	}
	return nil
}

// metricsEnabledByState reports whether /metrics should run according to the
// persisted `metrics` setting. It defaults to ON: a node whose operator never
// switched it on went dark (a fleet node had no listener and no one noticed).
// The listener in "auto" mode binds loopback plus the box's Tailscale
// addresses (inside a container, 0.0.0.0 with only published ports reachable)
// and never a public interface, and rendering costs nothing until something
// scrapes it. An explicit `urnet-tools metrics off` is persisted and wins.
func metricsEnabledByState(state *controlState) bool {
	v, ok := state.get("metrics")
	if !ok || strings.TrimSpace(v) == "" {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(v), "off")
}

// startDefaultMetrics starts /metrics at miner startup for a node that did
// not choose: URNETWORK_METRICS (startMetricsAtStartup binds that address
// itself), a persisted `metrics off` and a HotSwap candidate all take
// precedence. A start failure is logged and never stops the miner.
func startDefaultMetrics(state *controlState, hotSwapCandidate bool) {
	if hotSwapCandidate || os.Getenv("URNETWORK_METRICS") != "" || !metricsEnabledByState(state) {
		return
	}
	if err := applyMetricsLive("on"); err != nil {
		tlog("[metrics] default /metrics could not start: %v\n", err)
	}
}

// startMetricsAtStartup is the miner's port of the v33 startup wiring in
// provider/main.go: URNETWORK_METRICS binds exactly that address (typically a
// Tailscale IP like "100.64.0.10:9100") so a remote Prometheus can scrape
// without an agent on the fleet server; otherwise the persisted `metrics`
// setting rules and defaults to on.
//
// A HotSwap candidate skips both: the parent still holds the address until it
// yields, and startMetricsAfterTakeover binds it after takeover.
func startMetricsAtStartup(state *controlState, hotSwapCandidate bool) {
	if hotSwapCandidate {
		metricsHandoffPending.Store(true)
		return
	}
	if metricsAddr := os.Getenv("URNETWORK_METRICS"); metricsAddr != "" {
		tlog("[metrics] enabling Prometheus /metrics on %s\n", metricsAddr)
		if ln, err := net.Listen("tcp", metricsAddr); err != nil {
			tlog("[metrics] listener failed: %v\n", err)
		} else {
			serveMetrics(ln)
		}
	}
	startDefaultMetrics(state, false)
}

// startMetricsAfterTakeover starts /metrics in a promoted HotSwap candidate,
// once the parent has yielded. The parent's closers run in no fixed order, so
// its metrics port can still be held for a moment; wait briefly for the same
// address instead of moving to another port.
func startMetricsAfterTakeover(state *controlState) {
	metricsHandoffPending.Store(false)
	metricsMu.Lock()
	running := metricsServer != nil
	metricsMu.Unlock()
	if running {
		return
	}
	enabled := os.Getenv("URNETWORK_METRICS") != "" || metricsEnabledByState(state)
	if !enabled {
		return
	}
	ln, err := listenMetrics(5 * time.Second)
	if err != nil {
		tlog("[metrics] could not bind /metrics after hotswap takeover: %v\n", err)
		return
	}
	serveMetrics(ln)
}
