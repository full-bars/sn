package miner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
)

// control_socket.go ports the 3.23-fix v33 provider's control socket
// (provider/control_socket.go) onto the miner. This is the daemon surface the
// unmodified Go urnet-tools and urnet-docker clients drive:
//
//   - path <state>/provider.sock, where the state directory is the miner's
//     providerStateDir (so URNETWORK_STATE_DIR works exactly as it does for
//     the relay);
//   - newline-delimited JSON, one request per line, one response per line, in
//     order;
//   - the v33 command set: set, clear, get, status, history, version,
//     snapshot, traffic, internals, goroutines, trim_preview, ledger,
//     shutdown, audit (plus hotswap, whose trigger the daemon wires);
//   - socket mode 0600, platform ACL restriction and peer credential
//     verification on every connection;
//   - a 4 KiB cap on a "set" value and a 64 KiB cap on one request line.
//
// Responses are written as one JSON line each and are refused with a
// structured error if they would exceed the client's 1 MiB read cap.

// maxControlRequestLine is the largest accepted request line.
const maxControlRequestLine = 64 * 1024

// maxControlSetValue is the largest accepted "set" value.
const maxControlSetValue = 4 * 1024

// maxControlResponseBytes is the client-enforced cap on one response line
// (internal/urnettools/control_client.go). The server refuses to write a
// larger reply: a client would reject it as a possible hostile socket.
const maxControlResponseBytes = 1 << 20

// controlSocketPath returns <state>/provider.sock - the Unix domain socket
// urnet-tools talks to instead of writing override files directly. The
// provider is the only writer of its own settings (see controlState); this
// socket is how another process asks it to change one.
func controlSocketPath() (string, error) {
	return providerStatePath("provider.sock")
}

// controlRequest is one line of the socket protocol.
type controlRequest struct {
	Cmd     string `json:"cmd"` // "set", "clear", "get", "status", "history", "version", "snapshot", "traffic", "internals", "goroutines", "trim_preview", "ledger", "shutdown", "audit", "hotswap"
	Key     string `json:"key"`
	Value   string `json:"value,omitempty"`
	Limit   int    `json:"limit,omitempty"`   // for "history" and "ledger"
	Cursor  string `json:"cursor,omitempty"`  // for "history"
	V       int    `json:"v,omitempty"`       // protocol version; 0 = legacy
	Action  string `json:"action,omitempty"`  // for "audit": status, on, off, release
	Address string `json:"address,omitempty"` // for "audit" release
}

// settingInfo is the per-key detail returned by the "status" command.
type settingInfo struct {
	Value  string     `json:"value"`
	Source string     `json:"source"`
	SetAt  *time.Time `json:"set_at,omitempty"`
}

// controlResponse is one line of the socket protocol's reply.
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
	// process actually started with; the dashboard compares them against the
	// current values to decide whether a restart banner is warranted.
	StartupValues map[string]string `json:"startup_values,omitempty"`
	Version       int               `json:"v,omitempty"` // protocol version echoed back
	// BuildVersion is the provider's own release version, answered by the
	// "version" command.
	BuildVersion string `json:"build_version,omitempty"`
	// MetricsAddrs are the addresses /metrics listens on. Empty when metrics
	// is off (or, for the miner so far, not implemented).
	MetricsAddrs []string `json:"metrics_addrs,omitempty"`
	// Snapshot is the live node picture, answered by "snapshot".
	Snapshot *nodeSnapshot `json:"snapshot,omitempty"`
	// Traffic is the light live-counter reply, answered by "traffic".
	Traffic *liveTraffic `json:"traffic,omitempty"`
	// Internals and Goroutines are the runtime views, answered by "internals"
	// and "goroutines".
	Internals  *nodeInternals   `json:"internals,omitempty"`
	Goroutines *goroutineGroups `json:"goroutines,omitempty"`
	// ProxyAudit is the proxy audit engine's last completed tick. Nil when
	// the provider has no audit engine.
	ProxyAudit *proxyAuditStatus `json:"proxy_audit,omitempty"`
	Audit      *proxyAuditStatus `json:"audit,omitempty"`
}

// startControlSocket opens the control socket and serves it until ctx is
// canceled. It returns once the listener is up and accepting; serving happens
// on a background goroutine. The returned cleanup closes the listener and
// removes the socket file; it is idempotent.
func startControlSocket(ctx context.Context, state *controlState) (func(), error) {
	// The history command reads the audit ring; a process that reached this
	// point without the startup wiring (tests, an embedded use) still gets a
	// working ring.
	if globalAuditRing == nil {
		initControlAuditRing()
	}
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

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket listen: %w", err)
	}
	// Unix sockets inherit umask at creation time rather than an explicit mode
	// argument to Listen, so lock it down explicitly: owner-only, no
	// group/other access. Anyone who can reach this socket can change provider
	// settings.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket chmod: %w", err)
	}
	// On Windows, os.Chmod 0600 does not set NTFS permissions - any local user
	// could connect. Apply platform-specific ACL restrictions after creation
	// so only the current user can reach the socket.
	if err := restrictSocketACL(path); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket ACL: %w", err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	go func() {
		var acceptBackoff time.Duration
		for {
			conn, err := ln.Accept()
			if err != nil {
				if acceptLoopShouldStop(err) {
					return
				}
				// Anything else is transient (descriptor exhaustion during a
				// connection spike is the realistic one). Returning here would
				// leave the socket file on disk with nothing listening.
				if acceptBackoff == 0 {
					acceptBackoff = 5 * time.Millisecond
				} else if acceptBackoff < time.Second {
					acceptBackoff *= 2
				}
				tlog("⚠️ [control] accept failed, retrying in %s: %v\n", acceptBackoff, err)
				time.Sleep(acceptBackoff)
				continue
			}
			acceptBackoff = 0
			go handleControlConn(conn, state)
		}
	}()

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			ln.Close()
			os.Remove(path)
		})
	}
	return cleanup, nil
}

// removeStaleSocket removes path if nothing is actually listening on it - a
// leftover from a previous process that did not shut down cleanly. If a live
// process IS listening it leaves the file alone and returns an error instead
// of stealing the socket out from under a running instance.
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
	// ECONNREFUSED (or similar): the file exists but nothing is listening.
	return os.Remove(path)
}

// acceptLoopShouldStop reports whether an Accept error means the listener is
// gone for good. Only a closed listener ends the loop.
func acceptLoopShouldStop(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func handleControlConn(conn net.Conn, state *controlState) {
	defer conn.Close()

	// Read deadline: 5 seconds per line - prevents slowloris-style
	// connections that hold the socket open without sending data. Writes get
	// their own deadline: without it a client that sends a request and stops
	// reading blocks the response write forever.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	// Peer credential check (Linux/FreeBSD): verify the connecting process
	// UID matches the provider UID, or is root. Defense in depth alongside the
	// 0600 permissions.
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := verifyPeerCredentials(uc); err != nil {
			// Send a structured error so the CLI prints a real message
			// instead of "connection reset by peer".
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			writeControlResponse(conn, controlResponse{OK: false, Error: err.Error()})
			tlog("🔒 [control] rejected connection: %s\n", err)
			return
		}
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4*1024), maxControlRequestLine)
	for scanner.Scan() {
		// Reset deadlines for each line on a keep-alive connection.
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

		var req controlRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeControlResponse(conn, controlResponse{OK: false, Error: "invalid request: " + err.Error()})
			continue
		}
		// Max value size: 4 KiB for set values.
		if req.Cmd == "set" && len(req.Value) > maxControlSetValue {
			writeControlResponse(conn, controlResponse{OK: false, Error: "value too large (max 4 KiB)"})
			continue
		}

		// Version negotiation: v=1 is current; v=0 is legacy (accepted).
		if req.V > 1 {
			writeControlResponse(conn, controlResponse{OK: false, Error: "unsupported_version", Version: 1})
			continue
		}

		resp := handleControlRequest(state, req)
		if req.V >= 1 {
			resp.Version = 1
		}
		writeControlResponse(conn, resp)
	}
}

// writeControlResponse writes exactly one JSON line. A reply over the
// client's 1 MiB read cap is replaced with a structured error: the client
// treats an oversized line as a possible hostile socket and refuses it.
func writeControlResponse(conn net.Conn, resp controlResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		data, _ = json.Marshal(controlResponse{OK: false, Error: "response marshal failed: " + err.Error()})
	}
	if len(data) > maxControlResponseBytes {
		data, _ = json.Marshal(controlResponse{OK: false, Error: fmt.Sprintf("response too large (%d bytes, max %d)", len(data), maxControlResponseBytes)})
	}
	data = append(data, '\n')
	conn.Write(data)
}

// formerValue renders the previous value of a control key for the log line,
// distinguishing "was empty" from "was never set".
func formerValue(old string, had bool) string {
	if !had {
		return "unset"
	}
	if old == "" {
		return `""`
	}
	return old
}

// controlLiveEffectKeys tracks which control keys the miner can apply at
// runtime without a restart. It is the subset of the v33 live set the miner
// has a live consumer for: the Go runtime knobs, the dialer/upgrade toggles
// the pinned connect exports, and the proxy-manager overrides the running
// fetcher/reaper re-read on every cycle. Keys the miner has no consumer for
// still persist (and the provider log says so); they report needs_restart.
var controlLiveEffectKeys = map[string]bool{
	"gomemlimit":                  true,
	"gogc":                        true,
	"smart_dialer":                true,
	"h1_plus":                     true,
	"metrics":                     true,
	"metrics_listen":              true,
	"proxy_self_heal":             true,
	"proxy_url_max":               true,
	"proxy_url_refresh":           true,
	"proxy_dead_cleanup_scope":    true,
	"proxy_dead_cleanup_interval": true,
	"hot_restart":                 true,
}

// controlUnsupportedKeys are accepted and persisted but have no miner
// consumer at all yet; applyLiveSideEffect logs the gap explicitly so an
// operator never believes a value took effect when nothing reads it.
var controlUnsupportedKeys = map[string]bool{
	"node_name":        true,
	"report_url":       true,
	"report_interval":  true,
	"fast_auth":        true,
	"baseline":         true,
	"proxy_audit":      true,
	"h3":               true,
	"h3_datagram":      true,
	"h3_datagram_send": true,
	"ip_version":       true,
	"oom_cap":          true,
	"profile":          true,
	"ramlogs":          true,
}

// controlNeedsRestart returns true if setting this key requires a provider
// restart to take effect: any key that cannot be applied live.
func controlNeedsRestart(key string) bool {
	return !controlLiveEffectKeys[key]
}

// controlLiveDefaults maps live-applied keys to the runtime values clearing
// them reapplies. Values absent here fall through to the next layer (legacy
// override file, then startup default), which is exactly what v33 does.
var controlLiveDefaults = map[string]string{
	// Zero is a zero-byte limit to the Go runtime, not "unlimited": the apply
	// path maps any non-positive value to math.MaxInt64.
	"gomemlimit": "0",
	"gogc":       "100",
	// Without an entry here, clearing metrics would report success while the
	// (hypothetical) listener kept serving. There is no listener in the
	// miner, so clearing is a no-op that must still be recorded.
	"metrics":        "off",
	"metrics_listen": "auto",
	// Clearing smart_dialer must restore the measured-cost preference to its
	// off default.
	"smart_dialer": "off",
	// Clearing h1_plus restores the default: H1+ upgrades allowed.
	"h1_plus": "on",
}

// applyControlLiveDefault reapplies the runtime default for a live-applied
// key when it is cleared. Manager-owned proxy keys drop the in-process
// override so the P1 resolvers fall through to the legacy override file and
// then the startup default, exactly as v33 does.
func applyControlLiveDefault(key string) error {
	switch key {
	case "proxy_url_max", "proxy_url_refresh", "proxy_dead_cleanup_scope", "proxy_dead_cleanup_interval", "proxy_self_heal":
		globalProxyControlState.clear(key)
		return nil
	}
	def, ok := controlLiveDefaults[key]
	if !ok {
		return nil
	}
	return applyControlLiveSideEffect(key, def)
}

// validateControlValue validates a value server-side before persisting, with
// the same grammar the unmodified client enforces so a raw socket client
// cannot persist a value the next start cannot read.
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
	case "ip_version":
		switch valLower {
		case "dual", "dualstack", "dual-stack", "ipv4", "v4", "4", "ipv6", "v6", "6", "legacy":
		default:
			return fmt.Errorf("ip-version: must be dual, ipv4, ipv6 or legacy (got %q)", value)
		}
	case "fast_auth", "proxy_self_heal", "proxy_audit", "smart_dialer", "baseline", "h3_datagram", "h3_datagram_send", "h1_plus":
		switch valLower {
		case "on", "off":
		default:
			return fmt.Errorf("%s: must be on or off (got %q)", key, value)
		}
	case "h3":
		if !validH3ControlValue(value) {
			return fmt.Errorf("h3: must be off, direct, on, a positive proxy count, or all (got %q)", value)
		}
	case "oom_cap":
		switch valLower {
		case "on", "off", "shadow":
		default:
			return fmt.Errorf("oom_cap: must be on, off, or shadow (got %q)", value)
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
		// "off" clears the override, the same as every other tuning key.
		// "disabled" is the explicit value that turns collection off, kept
		// distinct so a plain "off" never hands an operator an unbounded heap.
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

// validH3ControlValue accepts off, direct, on, all and a positive decimal
// proxy count, the same grammar the client enforces.
func validH3ControlValue(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "off", "direct", "on", "all":
		return true
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 && strconv.Itoa(n) == v {
		return true
	}
	return false
}

// validateMetricsListen accepts auto, off, or an address:port with a port.
func validateMetricsListen(value string) error {
	if strings.EqualFold(value, "auto") || strings.EqualFold(value, "off") {
		return nil
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("metrics_listen: must be auto or an IP address with a port, like 100.64.0.10:9100 or 0.0.0.0:9100 (got %q)", value)
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return fmt.Errorf("metrics_listen: must be auto or an IP address with a port, like 100.64.0.10:9100 or 0.0.0.0:9100 (got %q)", value)
	}
	return nil
}

// peerAllowed is the peer admission rule shared by the platform peercred
// files: the provider's own UID, or root.
func peerAllowed(peerUID, providerUID uint32) bool {
	return peerUID == providerUID || peerUID == 0
}

// hotSwapTrigger is set during provider startup to a closure that performs
// the in-process restart. The control socket's "hotswap" command calls it.
// Nil when not set (a command path that did not wire the full startup).
var (
	hotSwapTriggerMu sync.RWMutex
	hotSwapTrigger   func() error
)

// setHotSwapTrigger stores the hot-restart trigger function.
func setHotSwapTrigger(fn func() error) {
	hotSwapTriggerMu.Lock()
	defer hotSwapTriggerMu.Unlock()
	hotSwapTrigger = fn
}

// getHotSwapTrigger returns the current hot-restart trigger (nil if unset).
func getHotSwapTrigger() func() error {
	hotSwapTriggerMu.RLock()
	defer hotSwapTriggerMu.RUnlock()
	return hotSwapTrigger
}

// resolveHotRestartEnabled reports whether the restart machinery is enabled:
// the persisted hot_restart key when set, else URNETWORK_HOT_RESTART (on
// unless it is "0"). This is the v33 default-on semantics. The audit/restart
// callers go through the overridable hotRestartEnabled var
// (proxy_manager_audit_run.go).
func resolveHotRestartEnabled() bool {
	if v, ok := globalControlState.get("hot_restart"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "off", "0", "false", "no":
			return false
		case "on", "1", "true", "yes":
			return true
		default:
			// Unknown stored value: do not silently flip behavior off a bad
			// write; keep the historical default-on baseline.
			return true
		}
	}
	return os.Getenv("URNETWORK_HOT_RESTART") != "0"
}

func handleControlRequest(state *controlState, req controlRequest) controlResponse {
	switch req.Cmd {
	case "version":
		return controlResponse{OK: true, BuildVersion: minerControlVersion()}

	case "snapshot":
		snap, err := minerControlSnapshot()
		if err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		return controlResponse{OK: true, Snapshot: snap}

	case "traffic":
		traffic, err := minerControlTraffic()
		if err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		return controlResponse{OK: true, Traffic: traffic}

	case "internals":
		internals, err := minerControlInternals()
		if err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		return controlResponse{OK: true, Internals: internals}

	case "goroutines":
		goroutines, err := minerControlGoroutines()
		if err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		return controlResponse{OK: true, Goroutines: goroutines}

	case "get":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		if !controlKeys[req.Key] {
			return controlResponse{OK: false, Error: fmt.Sprintf("unknown control key %q", req.Key)}
		}
		value, found := state.get(req.Key)
		return controlResponse{OK: true, Value: value, Found: found}

	case "set":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		if err := validateControlValue(req.Key, req.Value); err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		// Canonicalize profile aliases: v4/v8 pass validation but must be
		// stored as turbo-v4/turbo-v8 so startup code understands them.
		if req.Key == "profile" {
			switch strings.ToLower(req.Value) {
			case "v4":
				req.Value = "turbo-v4"
			case "v8":
				req.Value = "turbo-v8"
			}
		}
		// Apply, try to persist, roll back the in-memory change if persisting
		// fails - keeping memory and disk from disagreeing about what is
		// "set". txMu makes the whole get-old/set/persist/rollback unit atomic.
		state.txMu.Lock()
		defer state.txMu.Unlock()
		oldValue, oldMeta, hadOld := state.getWithMeta(req.Key)
		if err := state.set(req.Key, req.Value); err != nil {
			tlog("❌ [control] set %s=%s rejected: %s\n", req.Key, req.Value, err)
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
			tlog("❌ [control] set %s=%s failed to persist, rolled back: %s\n", req.Key, req.Value, err)
			return controlResponse{OK: false, Error: "set applied in memory but failed to persist: " + err.Error()}
		}
		if err := applyControlLiveSideEffect(req.Key, req.Value); err != nil {
			tlog("⚠️ [control] set %s=%s (was %s) persisted but live apply failed, takes effect on restart: %s\n",
				req.Key, req.Value, formerValue(oldValue, hadOld), err)
			return controlResponse{OK: false, Error: "persisted, but failed to apply live: " + err.Error()}
		}
		tlog("⚙️ [control] set %s=%s (was %s)\n", req.Key, req.Value, formerValue(oldValue, hadOld))
		recordControlAudit(CommandAudit{
			Timestamp: time.Now(),
			Cmd:       req.Cmd,
			Key:       req.Key,
			Value:     req.Value,
			OK:        true,
		})
		return controlResponse{OK: true, NeedsRestart: controlNeedsRestart(req.Key)}

	case "clear":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		if !controlKeys[req.Key] {
			return controlResponse{OK: false, Error: fmt.Sprintf("unknown control key %q", req.Key)}
		}
		state.txMu.Lock()
		defer state.txMu.Unlock()
		oldValue, oldMeta, hadOld := state.getWithMeta(req.Key)
		if err := state.clear(req.Key); err != nil {
			tlog("❌ [control] clear %s rejected: %s\n", req.Key, err)
			return controlResponse{OK: false, Error: err.Error()}
		}
		if err := state.persist(); err != nil {
			if hadOld {
				state.mu.Lock()
				state.values[req.Key] = oldValue
				state.meta[req.Key] = oldMeta
				state.mu.Unlock()
			}
			tlog("❌ [control] clear %s failed to persist, rolled back: %s\n", req.Key, err)
			return controlResponse{OK: false, Error: "clear applied in memory but failed to persist: " + err.Error()}
		}
		liveCleared := controlLiveEffectKeys[req.Key]
		if liveCleared {
			if err := applyControlLiveDefault(req.Key); err != nil {
				tlog("⚠️ [control] clear %s persisted but live default apply failed: %s\n", req.Key, err)
				return controlResponse{OK: false, NeedsRestart: true, Error: "cleared, but failed to reapply live default: " + err.Error()}
			}
		}
		tlog("⚙️ [control] cleared %s (was %s)\n", req.Key, formerValue(oldValue, hadOld))
		recordControlAudit(CommandAudit{
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
				setAt := v.Meta.SetAt
				si.SetAt = &setAt
			}
			settings[k] = si
		}
		snap, _ := minerControlAuditStatus()
		return controlResponse{OK: true, Settings: settings, StartupValues: startupValues(), MetricsAddrs: metricsServedAddrs(), ProxyAudit: snap, Audit: snap}

	case "audit":
		switch req.Action {
		case "status", "":
			snap, err := minerControlAuditStatus()
			if err != nil {
				return controlResponse{OK: false, Error: err.Error()}
			}
			return controlResponse{OK: true, ProxyAudit: snap, Audit: snap}
		case "on", "off":
			val := "off"
			if req.Action == "on" {
				val = "on"
			}
			// Same persist-then-commit transaction as "set": apply, persist,
			// roll back memory on failure so disk and memory never disagree.
			state.txMu.Lock()
			defer state.txMu.Unlock()
			oldValue, oldMeta, hadOld := state.getWithMeta("proxy_audit")
			if err := state.set("proxy_audit", val); err != nil {
				return controlResponse{OK: false, Error: err.Error()}
			}
			if err := state.persist(); err != nil {
				state.mu.Lock()
				if hadOld {
					state.values["proxy_audit"] = oldValue
					state.meta["proxy_audit"] = oldMeta
				} else {
					delete(state.values, "proxy_audit")
					delete(state.meta, "proxy_audit")
				}
				state.mu.Unlock()
				return controlResponse{OK: false, Error: "audit applied in memory but failed to persist: " + err.Error()}
			}
			// The miner has no audit engine; the value persists for parity and
			// the provider log says no engine consumes it.
			tlog("[control] proxy audit %s persisted; the miner does not run the v33 audit engine yet\n", req.Action)
			recordControlAudit(CommandAudit{
				Timestamp: time.Now(),
				Cmd:       "audit",
				Key:       "proxy_audit",
				Value:     val,
				OK:        true,
			})
			return controlResponse{OK: true, Value: val}
		case "release":
			// No audit engine means nothing is parked; say so rather than
			// pretending a release happened.
			return controlResponse{OK: false, Error: "proxy audit: the miner does not implement the v3.23 proxy audit engine, so no proxy is parked"}
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

	case "ledger":
		value, err := minerLedgerTail(minerLedgerLimit(req.Limit))
		if err != nil {
			return controlResponse{OK: false, Error: fmt.Sprintf("ledger: %v", err)}
		}
		return controlResponse{OK: true, Value: value}

	case "trim_preview":
		n, err := strconv.Atoi(strings.TrimSpace(req.Value))
		if err != nil || n <= 0 {
			return controlResponse{OK: false, Error: fmt.Sprintf("trim_preview: invalid count %q (want a positive number)", req.Value)}
		}
		text, err := minerControlTrimPreview(n)
		if err != nil {
			return controlResponse{OK: false, Error: fmt.Sprintf("trim_preview: %v", err)}
		}
		return controlResponse{OK: true, Value: text}

	case "shutdown":
		// Record the operator's remote stop in the audit ring.
		recordControlAudit(CommandAudit{
			Timestamp: time.Now(),
			Cmd:       "shutdown",
			Source:    "control-socket",
			OK:        true,
		})
		// Trigger the same graceful shutdown path as SIGTERM: cancel the run
		// context so all goroutines drain and the deferred cleanup runs.
		if state.shutdownFn == nil {
			return controlResponse{OK: false, Error: "shutdown not available (no shutdown function configured)"}
		}
		tlog("🛑 [control] shutdown requested via control socket\n")
		// The cancel runs asynchronously so the client gets its ACK first.
		go func() {
			time.Sleep(50 * time.Millisecond)
			state.shutdownFn()
		}()
		return controlResponse{OK: true, Value: "shutting down"}

	case "hotswap":
		if !resolveHotRestartEnabled() {
			return controlResponse{OK: false, Error: "hot restart is disabled (set hot_restart on or URNETWORK_HOT_RESTART=1 to enable)"}
		}
		hotSwap := getHotSwapTrigger()
		if hotSwap == nil {
			return controlResponse{OK: false, Error: "hotswap trigger not available"}
		}
		go func() {
			if err := hotSwap(); err != nil {
				tlog("[hotswap] background handoff failed: %v\n", err)
			}
		}()
		return controlResponse{OK: true, Value: "hotswap triggered"}

	default:
		return controlResponse{OK: false, Error: fmt.Sprintf("unknown command %q", req.Cmd)}
	}
}

// applyControlLiveSideEffect runs the immediate, no-restart-needed part of a
// socket set. Keys with no miner consumer are logged explicitly and left for
// the restart path.
func applyControlLiveSideEffect(key, value string) error {
	switch key {
	case "gomemlimit":
		limit, err := connect.ParseByteCount(value)
		if err != nil {
			return fmt.Errorf("gomemlimit: %w", err)
		}
		// Zero is not "unlimited" to the Go runtime, it is a zero-byte soft
		// limit: every allocation then reads as over budget and the runtime
		// GCs continuously. math.MaxInt64 is the value that means unlimited.
		if limit <= 0 {
			limit = math.MaxInt64
		}
		debug.SetMemoryLimit(limit)
	case "gogc":
		// Only "disabled" turns collection off. "off" is the clear value for
		// every tuning key, so a set carrying it restores the runtime default.
		if strings.EqualFold(value, "disabled") {
			debug.SetGCPercent(-1)
			importantLogf("[control] applied gogc=disabled (garbage collection off; heap grows unbounded)\n")
		} else if strings.EqualFold(value, "off") {
			debug.SetGCPercent(100)
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
	case "smart_dialer":
		// Transport choice keeps measuring either way (the measurement always
		// runs); this decides whether selection consults it.
		enabled := strings.EqualFold(value, "on")
		previous := connect.SetSmartDialer(enabled)
		was := "off"
		if previous {
			was = "on"
		}
		tlog("⚙️ [control] applied smart_dialer=%s (was %s)\n", value, was)
	case "h1_plus":
		enabled := strings.EqualFold(value, "on")
		connect.SetH1PlusDisabled(!enabled)
		tlog("⚙️ [control] applied h1_plus=%s (takes effect on the next connect)\n", onOffLabel(enabled))
	case "metrics":
		return applyControlMetricsLive(value)
	case "metrics_listen":
		// No listener exists in the miner yet, so there is nothing to move.
		// Persisting the address is all the miner can honestly do.
		return nil
	case "proxy_url_max", "proxy_url_refresh", "proxy_dead_cleanup_scope", "proxy_dead_cleanup_interval", "proxy_self_heal":
		// The running URL fetcher/reaper/cleanup loops re-read these on every
		// cycle through the P1 resolvers, which check this in-process map
		// first and the legacy override file second.
		globalProxyControlState.set(key, value)
		tlog("[control] applied %s=%s to the running proxy manager\n", key, value)
	case "hot_restart":
		tlog("[control] hot_restart=%s (restart machinery %s)\n", value, onOffLabel(resolveHotRestartEnabled()))
	default:
		if controlUnsupportedKeys[key] {
			// Persisted, but nothing in the miner reads it yet. Say so rather
			// than let the operator believe it took effect.
			importantLogf("[control] %s=%s persisted, but the miner has no consumer for this key yet; it is inert until the P2 remainder wires it\n", key, value)
			return nil
		}
	}
	return nil
}

// applyControlMetricsLive is the miner's honest stand-in for v33's
// applyMetricsLive. The miner has no Prometheus exporter yet, so "off" is
// already true (there is no listener), and "on" is refused by name instead of
// silently claiming a listener exists.
func applyControlMetricsLive(value string) error {
	if strings.EqualFold(value, "on") {
		return fmt.Errorf("the miner does not implement the v3.23 Prometheus /metrics exporter yet; the setting is persisted and will apply when the exporter lands")
	}
	// off: nothing to stop. (A future exporter stops here.)
	return nil
}

// metricsServedAddrs returns the /metrics listen addresses. The miner has no
// exporter yet, so this is always empty and `urnet-tools metrics` reports
// metrics off with an explanatory path.
func metricsServedAddrs() []string {
	return nil
}

// onOffLabel renders a boolean as the on/off labels the client uses.
func onOffLabel(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// --- the miner's own client side ---

// errNoProviderControlSocket distinguishes "provider is not running" from an
// actual protocol or application error.
var errNoProviderControlSocket = errors.New("no provider listening on control socket")

// dialControlSocket sends one request to the provider's control socket and
// reads one response. It is the miner's port of v33's dialControlSocket, used
// by `proxy trim --preview` so the preview is computed by the running
// provider, the only process that knows its running set.
func dialControlSocket(req controlRequest) (controlResponse, error) {
	path, err := controlSocketPath()
	if err != nil {
		return controlResponse{}, err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return controlResponse{}, errNoProviderControlSocket
		}
		return controlResponse{}, err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return controlResponse{}, err
	}
	// Cap the response read: provider.sock lives in a user-owned directory and
	// can be attacker-replaced, so an unbounded read lets a hostile socket
	// stream gigabytes into memory within the deadline. The provider's real
	// replies are short; a response that fills the budget without a newline is
	// a protocol error.
	reader := bufio.NewReader(io.LimitReader(conn, maxControlResponseBytes+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return controlResponse{}, err
	}
	if len(line) > maxControlResponseBytes {
		return controlResponse{}, fmt.Errorf("control socket response from %s exceeds %d bytes - refusing (possible hostile socket)", path, maxControlResponseBytes)
	}
	var resp controlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return controlResponse{}, err
	}
	return resp, nil
}

// previewViaControlSocketDial asks the running provider what a trim would
// shed. It is the wired body of the P1 seam in proxy_manager_commands.go.
func previewViaControlSocketDial(count int) (string, error) {
	resp, err := dialControlSocket(controlRequest{Cmd: "trim_preview", Value: itoa(count)})
	if err != nil {
		if errors.Is(err, errNoProviderControlSocket) {
			return "", fmt.Errorf("no provider is listening on the control socket")
		}
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("%s", resp.Error)
	}
	return resp.Value, nil
}

// controlSocketReachable reports whether the provider's control socket is
// accepting right now - the strongest liveness signal for the control plane.
func controlSocketReachable() bool {
	path, err := controlSocketPath()
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// controlStatusQuery fetches the provider's status reply for the miner's own
// commands (profile display, restart checks).
func controlStatusQuery() (controlResponse, error) {
	return dialControlSocket(controlRequest{Cmd: "status"})
}
