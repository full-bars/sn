package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
)

// Version is the build version of the provider binary. In production binaries
// this is set at link time via -ldflags "-X main.Version=...".
var Version string

// controlSocketPath returns ~/.urnetwork/provider.sock — the Unix domain
// socket urnet-tools talks to instead of writing override files directly.
// The provider is the only writer of its own settings (see controlState);
// this socket is how another process (urnet-tools) asks it to change one.
func controlSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "provider.sock"), nil
}

// controlRequest is one line of the socket protocol: newline-delimited JSON,
// one request per line, one response per line, in order.
type controlRequest struct {
	Cmd     string `json:"cmd"` // "set", "clear", "get", "status", "history", "version", or "shutdown"
	Key     string `json:"key"`
	Value   string `json:"value,omitempty"`
	Action  string `json:"action,omitempty"`
	Address string `json:"address,omitempty"`
	Limit   int    `json:"limit,omitempty"`  // for "history" command
	Cursor  string `json:"cursor,omitempty"` // for "history" command
	V       int    `json:"v,omitempty"`      // protocol version; 0 = legacy
}

// settingInfo is the per-key detail returned by the "status" command.
type settingInfo struct {
	Value  string     `json:"value"`
	Source string     `json:"source"`
	SetAt  *time.Time `json:"set_at,omitempty"`
}

type controlResponse struct {
	OK           bool                   `json:"ok"`
	Value        string                 `json:"value,omitempty"`
	Found        bool                   `json:"found,omitempty"`
	Error        string                 `json:"error,omitempty"`
	NeedsRestart bool                   `json:"needs_restart,omitempty"`
	Entries      []CommandAudit         `json:"entries,omitempty"`
	NextCursor   string                 `json:"next_cursor,omitempty"`
	Settings     map[string]settingInfo `json:"settings,omitempty"`
	// StartupValues maps restart-required keys to the values the running
	// process actually started with (from env vars set by
	// seedEnvFromControlState). The dashboard compares these against the
	// current control-state values to decide whether a restart banner is
	// warranted — only a VALUE CHANGE since startup, not mere presence,
	// should trigger the warning.
	StartupValues map[string]string `json:"startup_values,omitempty"`
	Version       int               `json:"v,omitempty"` // protocol version echoed back
	// BuildVersion is the provider's own release version, answered by the
	// "version" command. Distinct from Version, which is the control
	// protocol's version, not the binary's.
	BuildVersion string `json:"build_version,omitempty"`
	// MetricsAddrs are the addresses /metrics is listening on, answered by
	// "status". Empty when metrics is off.
	MetricsAddrs []string          `json:"metrics_addrs,omitempty"`
	ProxyAudit   *proxyAuditStatus `json:"proxy_audit,omitempty"`
	Audit        *proxyAuditStatus `json:"audit,omitempty"`
	Governor     *proxyAuditStatus `json:"governor,omitempty"`
}

func controlLog(format string, args ...any) {
	fmt.Printf("%s "+format, append([]any{time.Now().Format("0102 15:04:05")}, args...)...)
}

// startControlSocket opens the control socket and serves it until ctx is
// canceled. Returns once the listener is up and accepting; serving happens
// on a background goroutine. The returned cleanup func closes the listener
// and removes the socket file — call it (or just let ctx cancellation do
// the equivalent) on shutdown.
func startControlSocket(ctx context.Context, state *controlState) (func(), error) {
	path, err := controlSocketPath()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}

	// Create the socket with restrictive permissions from the start
	// by setting umask to 0177 before bind(). This avoids a TOCTOU
	// window where the file exists with default permissions.
	// (Unix socket bind() uses mode 0777, so 0777 & ~0177 = 0600.)
	oldUmask := setUmask(0o177)
	ln, err := net.Listen("unix", path)
	setUmask(oldUmask)
	if err != nil {
		return nil, fmt.Errorf("control socket listen: %w", err)
	}
	// Belt-and-suspenders: ensure owner-only even if umask was
	// overridden by a parent process or kernel quirk.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket chmod: %w", err)
	}
	if err := restrictSocketACL(path); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket ACL: %w", err)
	}

	// cleanOnce ensures the listener close and socket removal happen
	// exactly once, even when called from both the ctx.Done goroutine
	// and the returned cleanup function concurrently.
	var cleanOnce sync.Once
	doClean := func() {
		ln.Close()
		os.Remove(path)
	}

	go func() {
		<-ctx.Done()
		cleanOnce.Do(doClean)
	}()

	go func() {
		var acceptBackoff time.Duration
		for {
			conn, err := ln.Accept()
			if err != nil {
				if acceptLoopShouldStop(err) {
					return
				}
				if acceptBackoff == 0 {
					acceptBackoff = 5 * time.Millisecond
				} else if acceptBackoff < time.Second {
					acceptBackoff *= 2
				}
				controlLog("⚠️ [control] accept failed, retrying in %s: %v\n", acceptBackoff, err)
				time.Sleep(acceptBackoff)
				continue
			}
			acceptBackoff = 0
			go handleControlConn(conn, state)
		}
	}()

	cleanup := func() {
		cleanOnce.Do(doClean)
	}
	return cleanup, nil
}

// removeStaleSocket removes path if nothing is actually listening on it —
// i.e. it's a leftover from a previous process that didn't shut down
// cleanly. If a live process IS listening (this provider is somehow already
// running), it leaves the file alone and returns an error instead of
// stealing the socket out from under a running instance.
func removeStaleSocket(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	conn, err := net.Dial("unix", path)
	if err == nil {
		conn.Close()
		return fmt.Errorf("control socket %s already has a live listener; is another provider instance running?", path)
	}
	return os.Remove(path)
}

func acceptLoopShouldStop(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func handleControlConn(conn net.Conn, state *controlState) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	if uc, ok := conn.(*net.UnixConn); ok {
		if err := verifyPeerCredentials(uc); err != nil {
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: false, Error: err.Error()})
			controlLog("🔒 [control] rejected connection: %s\n", err)
			return
		}
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4*1024), 64*1024)
	enc := json.NewEncoder(conn)
	for scanner.Scan() {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

		raw := scanner.Bytes()
		if len(raw) > 64*1024 {
			enc.Encode(controlResponse{OK: false, Error: "request too large (max 64 KiB)"})
			continue
		}

		var req controlRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			enc.Encode(controlResponse{OK: false, Error: "invalid request: " + err.Error()})
			continue
		}
		if req.Cmd == "set" && len(req.Value) > 4*1024 {
			enc.Encode(controlResponse{OK: false, Error: "value too large (max 4 KiB)"})
			continue
		}

		if req.V > 1 {
			enc.Encode(controlResponse{
				OK:      false,
				Error:   "unsupported_version",
				Version: 1,
			})
			continue
		}

		resp := handleControlRequest(state, req)
		if req.V >= 1 {
			resp.Version = 1
		}
		enc.Encode(resp)
	}
}

func formerValue(old string, had bool) string {
	if !had {
		return "unset"
	}
	if old == "" {
		return `""`
	}
	return old
}

var liveEffectKeys = map[string]bool{
	"gomemlimit":                  true,
	"gogc":                        true,
	"fast_auth":                   true,
	"proxy_self_heal":             true,
	"proxy_audit":                 true,
	"report_url":                  true,
	"report_interval":             true,
	"proxy_url_refresh":           true,
	"proxy_url_max":               true,
	"proxy_dead_cleanup_scope":    true,
	"proxy_dead_cleanup_interval": true,
	"node_name":                   true,
	"hot_restart":                 true,
	"metrics":                     true,
	"metrics_listen":              true,
}

func needsRestart(key string) bool {
	return !liveEffectKeys[key]
}

func validateControlValue(key, value string) error {
	valLower := strings.ToLower(value)
	switch key {
	case "report_interval", "proxy_url_refresh", "proxy_dead_cleanup_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: invalid duration %q (use e.g. 30s, 5m, 1h)", key, value)
		}
		min := 10 * time.Second
		if key == "proxy_dead_cleanup_interval" {
			min = time.Minute
		}
		if d < min {
			return fmt.Errorf("%s: %s is below the minimum %s", key, value, min)
		}
	case "proxy_url_max":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("%s: must be a non-negative integer (got %q)", key, value)
		}
	case "proxy_dead_cleanup_scope":
		switch value {
		case "none", "url", "all":
		default:
			return fmt.Errorf("%s: must be none, url, or all (got %q)", key, value)
		}
	case "fast_auth", "proxy_self_heal", "proxy_audit":
		switch valLower {
		case "on", "off":
		default:
			return fmt.Errorf("%s: must be on or off (got %q)", key, value)
		}
	case "hot_restart":
		switch valLower {
		case "on", "off", "1", "0", "true", "false", "yes", "no":
		default:
			return fmt.Errorf("hot_restart: must be on or off (got %q)", value)
		}
	case "ramlogs":
		switch valLower {
		case "on", "off", "1", "0", "true", "false":
		default:
			return fmt.Errorf("ramlogs: must be on or off (got %q)", value)
		}
	case "profile":
		switch valLower {
		case "auto", "eco", "lowmem", "turbo-v4", "turbo-v8", "v4", "v8":
		default:
			return fmt.Errorf("profile: must be auto, eco, lowmem, turbo-v4, turbo-v8, v4, or v8 (got %q)", value)
		}
	case "gomemlimit":
		if _, err := connect.ParseByteCount(value); err != nil {
			return fmt.Errorf("gomemlimit: invalid byte count %q: %w", value, err)
		}
	case "gogc":
		if valLower != "off" && valLower != "disabled" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("gogc: must be an integer percentage, 'off' to clear, or 'disabled' to turn collection off (got %q)", value)
			}
			if n < 0 {
				return fmt.Errorf("gogc: must be a non-negative percentage, 'off' to clear, or 'disabled' to turn collection off (got %q)", value)
			}
		}
	case "metrics":
		switch valLower {
		case "on", "off":
		default:
			return fmt.Errorf("metrics: must be on or off (got %q)", value)
		}
	case "metrics_listen":
		return validateMetricsListen(value)
	case "node_name":
		if value == "" {
			return fmt.Errorf("node_name: must not be empty")
		}
		for _, c := range value {
			if c < 32 || c > 126 {
				return fmt.Errorf("node_name: must be printable ASCII (got 0x%02x)", c)
			}
		}
	case "report_url":
		if value != "" {
			for _, c := range value {
				if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
					return fmt.Errorf("report_url: must not contain whitespace (got %q)", value)
				}
			}
		}
	}
	return nil
}

var liveDefaults = map[string]string{
	"gomemlimit":     "0",
	"gogc":           "100",
	"metrics":        "off",
	"metrics_listen": "auto",
}

func applyLiveDefault(key string) error {
	def, ok := liveDefaults[key]
	if !ok {
		return nil
	}
	return applyLiveSideEffect(key, def)
}

func handleControlRequest(state *controlState, req controlRequest) controlResponse {
	IncrControlCmd(req.Cmd)

	switch req.Cmd {
	case "version":
		v := Version
		if v == "" {
			v = "dev"
		}
		return controlResponse{OK: true, BuildVersion: v}

	case "get":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		value, found := state.get(req.Key)
		if !controlKeys[req.Key] {
			return controlResponse{OK: false, Error: fmt.Sprintf("unknown control key %q", req.Key)}
		}
		return controlResponse{OK: true, Value: value, Found: found}

	case "set":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		if err := validateControlValue(req.Key, req.Value); err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		if req.Key == "profile" {
			switch strings.ToLower(req.Value) {
			case "v4":
				req.Value = "turbo-v4"
			case "v8":
				req.Value = "turbo-v8"
			}
		}
		state.txMu.Lock()
		defer state.txMu.Unlock()
		oldValue, oldMeta, hadOld := state.getWithMeta(req.Key)
		if err := state.set(req.Key, req.Value); err != nil {
			controlLog("❌ [control] set %s=%s rejected: %s\n", req.Key, req.Value, err)
			return controlResponse{OK: false, Error: err.Error()}
		}
		if err := state.persist(); err != nil {
			state.mu.Lock()
			if hadOld {
				state.values[req.Key] = oldValue
				state.meta[req.Key] = oldMeta
			} else {
				delete(state.values, req.Key)
				delete(state.meta, req.Key)
			}
			state.mu.Unlock()
			controlLog("❌ [control] set %s=%s failed to persist, rolled back: %s\n", req.Key, req.Value, err)
			return controlResponse{OK: false, Error: "set applied in memory but failed to persist: " + err.Error()}
		}
		if err := applyLiveSideEffect(req.Key, req.Value); err != nil {
			controlLog("⚠️ [control] set %s=%s (was %s) persisted but live apply failed, takes effect on restart: %s\n",
				req.Key, req.Value, formerValue(oldValue, hadOld), err)
			return controlResponse{OK: false, Error: "persisted, but failed to apply live: " + err.Error()}
		}
		controlLog("⚙️ [control] set %s=%s (was %s)\n", req.Key, req.Value, formerValue(oldValue, hadOld))
		recordAndPersist(CommandAudit{
			Timestamp: time.Now(),
			Cmd:       req.Cmd,
			Key:       req.Key,
			Value:     req.Value,
			OK:        true,
		})
		return controlResponse{OK: true, NeedsRestart: needsRestart(req.Key)}

	case "clear":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		state.txMu.Lock()
		defer state.txMu.Unlock()
		oldValue, oldMeta, hadOld := state.getWithMeta(req.Key)
		if err := state.clear(req.Key); err != nil {
			controlLog("❌ [control] clear %s rejected: %s\n", req.Key, err)
			return controlResponse{OK: false, Error: err.Error()}
		}
		if err := state.persist(); err != nil {
			if hadOld {
				state.mu.Lock()
				state.values[req.Key] = oldValue
				state.meta[req.Key] = oldMeta
				state.mu.Unlock()
			}
			controlLog("❌ [control] clear %s failed to persist, rolled back: %s\n", req.Key, err)
			return controlResponse{OK: false, Error: "clear applied in memory but failed to persist: " + err.Error()}
		}
		liveCleared := liveEffectKeys[req.Key]
		if liveCleared {
			if err := applyLiveDefault(req.Key); err != nil {
				controlLog("⚠️ [control] clear %s persisted but live default apply failed: %s\n", req.Key, err)
				return controlResponse{OK: false, NeedsRestart: true, Error: "cleared, but failed to reapply live default: " + err.Error()}
			}
		}
		controlLog("⚙️ [control] cleared %s (was %s)\n", req.Key, formerValue(oldValue, hadOld))
		recordAndPersist(CommandAudit{
			Timestamp: time.Now(),
			Cmd:       req.Cmd,
			Key:       req.Key,
			Value:     oldValue,
			OK:        true,
		})
		return controlResponse{OK: true, NeedsRestart: !liveCleared}

	case "status":
		raw := state.statusSnapshot()
		settings := make(map[string]settingInfo, len(raw))
		for k, v := range raw {
			si := settingInfo{Value: v.Value, Source: string(v.Meta.Source)}
			if !v.Meta.SetAt.IsZero() {
				si.SetAt = &v.Meta.SetAt
			}
			settings[k] = si
		}
		snap := proxyAuditStatusSnapshot()
		return controlResponse{OK: true, Settings: settings, StartupValues: startupValues(), MetricsAddrs: metricsServedAddrs(), ProxyAudit: snap, Audit: snap, Governor: snap}

	case "audit":
		switch req.Action {
		case "status", "":
			snap := proxyAuditStatusSnapshot()
			return controlResponse{OK: true, ProxyAudit: snap, Audit: snap, Governor: snap}

		case "on":
			_ = state.set("proxy_audit", "on")
			setProxyAuditOverride(true)
			controlLog("✓ [proxy][audit] proxy audit enabled via control socket\n")
			if a := currentProxyAuditor.Load(); a != nil {
				go a.runOnce()
			}
			return controlResponse{OK: true, Value: "enabled"}

		case "off":
			_ = state.set("proxy_audit", "off")
			setProxyAuditOverride(false)
			controlLog("✓ [proxy][audit] proxy audit disabled via control socket\n")
			if a := currentProxyAuditor.Load(); a != nil {
				go a.runOnce()
			}
			return controlResponse{OK: true, Value: "disabled"}

		case "release":
			a := currentProxyAuditor.Load()
			if a == nil {
				return controlResponse{OK: false, Error: "proxy auditor not active"}
			}
			if req.Address == "" || req.Address == "--all" || req.Address == "all" {
				released := a.st.releaseAll()
				for _, addr := range released {
					a.releaseBackoff(addr)
				}
				a.publish(a.env.now(), a.env.act(), proxyAuditResult{})
				controlLog("✓ [proxy][audit] released all %d parked proxies via control socket\n", len(released))
				return controlResponse{OK: true, Value: fmt.Sprintf("released %d proxies", len(released))}
			}
			addr := req.Address
			wasParked := a.st.isParked(addr)
			if wasParked {
				a.st.release(addr)
			}
			a.releaseBackoff(addr)
			if globalProxyFailureHistory != nil {
				globalProxyFailureHistory.Reset(addr)
			}
			a.publish(a.env.now(), a.env.act(), proxyAuditResult{})
			if wasParked {
				controlLog("✓ [proxy][audit] released parked proxy %s via control socket\n", addr)
				return controlResponse{OK: true, Value: fmt.Sprintf("released proxy %s", addr)}
			}
			controlLog("✓ [proxy][audit] cleared backoff for proxy %s via control socket (was not parked)\n", addr)
			return controlResponse{OK: true, Value: fmt.Sprintf("cleared backoff for proxy %s", addr)}

		default:
			return controlResponse{OK: false, Error: fmt.Sprintf("unknown audit action %q (status|on|off|release)", req.Action)}
		}

	case "history":
		if globalAuditRing == nil {
			return controlResponse{OK: false, Error: "audit ring not initialized"}
		}
		limit := req.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 100 {
			limit = 100
		}
		entries, nextCursor := globalAuditRing.Entries(limit, req.Cursor)
		return controlResponse{OK: true, Entries: entries, NextCursor: nextCursor}

	case "shutdown":
		if state.shutdownFn == nil {
			return controlResponse{OK: false, Error: "shutdown not available (no shutdown function configured)"}
		}
		controlLog("🛑 [control] shutdown requested via control socket\n")
		go func() {
			time.Sleep(50 * time.Millisecond)
			state.shutdownFn()
		}()
		return controlResponse{OK: true, Value: "shutting down"}

	case "hotswap":
		trigger := getHotSwapTrigger()
		if trigger == nil {
			return controlResponse{OK: false, Error: "hotswap trigger not available"}
		}
		go func() {
			if err := trigger(); err != nil {
				controlLog("[hotswap] background handoff failed: %v\n", err)
			}
		}()
		return controlResponse{OK: true, Value: "hotswap triggered"}

	default:
		return controlResponse{OK: false, Error: fmt.Sprintf("unknown command %q", req.Cmd)}
	}
}

var errNoProvider = errors.New("no provider listening on control socket")

func dialControlSocket(req controlRequest) (controlResponse, error) {
	path, err := controlSocketPath()
	if err != nil {
		return controlResponse{}, err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return controlResponse{}, errNoProvider
		}
		return controlResponse{}, err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return controlResponse{}, err
	}
	var resp controlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return controlResponse{}, err
	}
	return resp, nil
}

var controlApplyLog = func(format string, args ...any) { controlLog(format, args...) }

func applyLiveSideEffect(key, value string) error {
	switch key {
	case "gomemlimit":
		limit, err := connect.ParseByteCount(value)
		if err != nil {
			return fmt.Errorf("gomemlimit: %w", err)
		}
		if limit <= 0 {
			limit = math.MaxInt64
		}
		debug.SetMemoryLimit(int64(limit))
		if limit == math.MaxInt64 {
			controlApplyLog("⚙️ [control] applied gomemlimit=unlimited (no soft memory limit)\n")
		} else {
			controlApplyLog("⚙️ [control] applied gomemlimit=%s\n", value)
		}
	case "gogc":
		if strings.EqualFold(value, "disabled") {
			debug.SetGCPercent(-1)
			controlApplyLog("⚙️ [control] applied gogc=disabled (garbage collection off; heap grows unbounded)\n")
		} else if strings.EqualFold(value, "off") {
			debug.SetGCPercent(100)
			controlApplyLog("⚙️ [control] applied gogc=100 (off restores the default)\n")
		} else {
			percent, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("gogc: %w", err)
			}
			if percent < 0 {
				return fmt.Errorf("gogc: must be a non-negative percentage (got %d)", percent)
			}
			debug.SetGCPercent(percent)
		}
	case "metrics":
		return applyMetricsLive(value)
	case "metrics_listen":
		return applyMetricsListenLive()
	case "proxy_audit":
		enabled := isTruthyOn(value)
		setProxyAuditOverride(enabled)
		if a := currentProxyAuditor.Load(); a != nil {
			go a.runOnce()
		}
		return nil
	}
	return nil
}

func applyMetricsLive(value string) error {
	enabled := strings.EqualFold(value, "on")
	metricsMu.Lock()
	srv := metricsServer
	metricsMu.Unlock()

	if enabled && srv == nil {
		ln, err := listenMetrics(0)
		if err != nil {
			return fmt.Errorf("metrics on: %w", err)
		}
		serveMetrics(ln)
	} else if !enabled && srv != nil {
		if err := stopMetrics(); err != nil {
			return fmt.Errorf("metrics off: %w", err)
		}
		controlLog("[metrics] stopped Prometheus /metrics\n")
	}
	return nil
}

var (
	metricsMu             sync.Mutex
	metricsServer         *http.Server
	metricsListener       net.Listener
	metricsUnregCloser    func()
	metricsHandoffPending atomic.Bool
	hotSwapTriggerMu      sync.RWMutex
	hotSwapTrigger        func() error

	coordinatorClosersMu  sync.Mutex
	coordinatorCloserSeq  uint64
	coordinatorClosersMap map[uint64]func()
)

// setHotSwapTrigger stores the hot-swap trigger function, safe for concurrent use.
func setHotSwapTrigger(fn func() error) {
	hotSwapTriggerMu.Lock()
	defer hotSwapTriggerMu.Unlock()
	hotSwapTrigger = fn
}

// getHotSwapTrigger returns the current hot-swap trigger function (nil if unset).
func getHotSwapTrigger() func() error {
	hotSwapTriggerMu.RLock()
	defer hotSwapTriggerMu.RUnlock()
	return hotSwapTrigger
}

func RegisterCoordinatorCloser(closer func()) func() {
	if closer == nil {
		return func() {}
	}
	coordinatorClosersMu.Lock()
	defer coordinatorClosersMu.Unlock()
	coordinatorCloserSeq++
	id := coordinatorCloserSeq
	if coordinatorClosersMap == nil {
		coordinatorClosersMap = make(map[uint64]func())
	}
	coordinatorClosersMap[id] = closer

	var once sync.Once
	return func() {
		once.Do(func() {
			coordinatorClosersMu.Lock()
			delete(coordinatorClosersMap, id)
			coordinatorClosersMu.Unlock()
		})
	}
}

func serveMetrics(ln net.Listener) {
	stubSetExtraMetricsProvider(providerExtraMetrics)
	stubSetPersistentErrorFunc(IncrPersistentError)
	server := &http.Server{
		Handler:           prometheusHandlerStub(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	metricsMu.Lock()
	metricsServer = server
	metricsListener = ln
	metricsMu.Unlock()

	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			controlLog("[metrics] listener failed: %v\n", err)
		}
	}()
	if metricsUnregCloser != nil {
		metricsUnregCloser()
	}
	metricsUnregCloser = RegisterCoordinatorCloser(func() {
		if err := stopMetrics(); err != nil {
			controlLog("[metrics] releasing /metrics for hotswap: %v\n", err)
		}
	})
	controlLog("[metrics] started Prometheus /metrics on %s\n", ln.Addr())
}

func stopMetrics() error {
	if metricsUnregCloser != nil {
		metricsUnregCloser()
		metricsUnregCloser = nil
	}
	metricsMu.Lock()
	srv := metricsServer
	ln := metricsListener
	metricsServer = nil
	metricsListener = nil
	metricsMu.Unlock()

	if srv == nil {
		if ln != nil {
			_ = ln.Close()
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	if ln != nil {
		_ = ln.Close()
	}
	return err
}

func startMetricsAfterTakeover(state *controlState) {
	metricsHandoffPending.Store(false)
	metricsMu.Lock()
	srv := metricsServer
	metricsMu.Unlock()
	if srv != nil {
		return
	}
	enabled := os.Getenv("URNETWORK_METRICS") != ""
	if !enabled {
		if v, ok := state.get("metrics"); ok && strings.EqualFold(v, "on") {
			enabled = true
		}
	}
	if !enabled {
		return
	}
	ln, err := listenMetrics(5 * time.Second)
	if err != nil {
		controlLog("[metrics] could not bind /metrics after hotswap takeover: %v\n", err)
		return
	}
	serveMetrics(ln)
}

func listenOrWait(addr string, wait time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(wait)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil || !time.Now().Before(deadline) {
			return ln, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func applyPersistedRuntimeTuning(state *controlState) {
	if v, ok := state.get("gomemlimit"); ok && v != "" && !strings.EqualFold(v, "off") {
		if err := applyLiveSideEffect("gomemlimit", v); err != nil {
			controlLog("[control] failed to apply persisted gomemlimit=%s: %s\n", v, err)
		}
	}
	if v, ok := state.get("gogc"); ok && v != "" && !strings.EqualFold(v, "off") {
		if err := applyLiveSideEffect("gogc", v); err != nil {
			controlLog("[control] failed to apply persisted gogc=%s: %s\n", v, err)
		}
	}
	if v, ok := state.get("metrics"); ok && strings.EqualFold(v, "on") && os.Getenv("URNETWORK_METRICS") == "" && !metricsHandoffPending.Load() {
		if err := applyMetricsLive("on"); err != nil {
			controlLog("[control] failed to apply persisted metrics=on: %s\n", err)
		}
	}
}

func persistedRuntimeTuningActive(key string) bool {
	v, ok := globalControlState.get(key)
	return ok && v != "" && v != "off"
}

func waitForControlSocketRelease(timeout time.Duration) {
	path, err := controlSocketPath()
	if err != nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.Dial("unix", path)
		if err != nil {
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			controlLog("[control] timed out waiting for parent to release control socket after takeover; proceeding anyway\n")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func isTruthyOn(v string) bool {
	switch v {
	case "on", "1", "true", "yes":
		return true
	default:
		return false
	}
}

func hotRestartEnabled() bool {
	if v, ok := globalControlState.get("hot_restart"); ok {
		switch v {
		case "off", "0", "false", "no":
			return false
		case "on", "1", "true", "yes":
			return true
		default:
			return true
		}
	}
	return os.Getenv("URNETWORK_HOT_RESTART") != "0"
}

func nodeNameOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "node_name"), nil
}

func resolveNodeName(startupName string) string {
	if v, ok := globalControlState.get("node_name"); ok && v != "" {
		return v
	}

	path, err := nodeNameOverridePath()
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return startupName
}

var (
	lastDashboardLabelMu sync.Mutex
	lastDashboardLabel   string
)

func logDashboardLabel(description string) {
	lastDashboardLabelMu.Lock()
	defer lastDashboardLabelMu.Unlock()
	if description == lastDashboardLabel {
		return
	}
	if lastDashboardLabel == "" {
		controlLog("🏷️ [identity] dashboard label: %s\n", description)
	} else {
		controlLog("🏷️ [identity] dashboard label changed: %s -> %s\n", lastDashboardLabel, description)
	}
	lastDashboardLabel = description
}
