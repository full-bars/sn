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
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
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
	Cmd    string `json:"cmd"` // "set", "clear", "get", "status", "history", "version", or "shutdown"
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Limit  int    `json:"limit,omitempty"`  // for "history" command
	Cursor string `json:"cursor,omitempty"` // for "history" command
	V      int    `json:"v,omitempty"`      // protocol version; 0 = legacy
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
	MetricsAddrs []string `json:"metrics_addrs,omitempty"`
}

// Source identifies where a control setting value originated.
type Source string

const (
	SourceSocket  Source = "socket"
	SourceEnv     Source = "env"
	SourcePending Source = "pending"
	SourceLegacy  Source = "legacy"
	SourceDefault Source = "default"
)

// configMeta records the provenance of a single control setting.
type configMeta struct {
	Source Source    `json:"source"`
	SetAt  time.Time `json:"set_at,omitempty"`
}

// controlStateEnvelope is the v2 on-disk format for provider_state.json.
// It wraps the values map with a version tag and per-key metadata.
type controlStateEnvelope struct {
	Version int                   `json:"version"`
	Values  map[string]string     `json:"values"`
	Meta    map[string]configMeta `json:"meta,omitempty"`
}

// controlState is the provider's own in-memory record of every runtime
// setting an operator can change via the control socket (control_socket.go).
// It is the ONLY thing that writes controlStatePath() — unlike the legacy
// per-file overrides, there is exactly one writer here, so none of the
// read-modify-write or stale-cache problems that came with a shared,
// externally-written file apply. All access goes through this type's
// methods, which hold mu for their whole body.
type controlState struct {
	mu     sync.RWMutex
	values map[string]string
	meta   map[string]configMeta
	// passthrough preserves unrecognized keys from v2 envelopes across
	// load -> persist round-trips so a rollback doesn't drop them.
	passthroughValues map[string]string
	passthroughMeta   map[string]configMeta
	// txMu serializes the read-modify-write-persist-rollback sequence that
	// makes up one logical `set`/`clear` operation. Each control-socket
	// connection is served on its own goroutine (handleControlConn), and the
	// individual s.mu lock in set/clear/get/persist does NOT span that whole
	// sequence — without txMu, two concurrent sets on the same key could
	// interleave. txMu makes the get-old → set/clear → persist → rollback unit
	// atomic for this state.
	txMu sync.Mutex
	// shutdownFn, when non-nil, triggers the same graceful shutdown path
	// as SIGTERM: cancel the main context so all goroutines drain.
	shutdownFn func()
}

// controlKeys are the only settings the socket accepts.
var controlKeys = map[string]bool{
	"node_name":                   true,
	"report_url":                  true,
	"report_interval":             true,
	"fast_auth":                   true,
	"proxy_self_heal":             true,
	"proxy_url_max":               true,
	"proxy_url_refresh":           true,
	"proxy_dead_cleanup_scope":    true,
	"proxy_dead_cleanup_interval": true,
	"hot_restart":                 true,
	"gomemlimit":                  true,
	"gogc":                        true,
	"profile":                     true,
	"ramlogs":                     true,
	"metrics":                     true,
	"metrics_listen":              true,
}

// globalControlState is the single provider-wide instance.
var globalControlState = newControlState()

func newControlState() *controlState {
	return &controlState{
		values:            map[string]string{},
		meta:              map[string]configMeta{},
		passthroughValues: map[string]string{},
		passthroughMeta:   map[string]configMeta{},
	}
}

func (s *controlState) get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

func (s *controlState) getWithMeta(key string) (string, configMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, s.meta[key], ok
}

func (s *controlState) set(key, value string) error {
	return s.setWithValue(key, value, SourceSocket)
}

func (s *controlState) setWithValue(key, value string, src Source) error {
	if !controlKeys[key] {
		return fmt.Errorf("unknown control key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	s.meta[key] = configMeta{Source: src, SetAt: time.Now()}
	delete(s.passthroughValues, key)
	delete(s.passthroughMeta, key)
	return nil
}

func (s *controlState) clear(key string) error {
	return s.clearWithValue(key)
}

func (s *controlState) clearWithValue(key string) error {
	if !controlKeys[key] {
		return fmt.Errorf("unknown control key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	delete(s.meta, key)
	delete(s.passthroughValues, key)
	delete(s.passthroughMeta, key)
	return nil
}

func (s *controlState) replaceAll(values map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = values
	s.meta = make(map[string]configMeta, len(values))
	s.passthroughValues = map[string]string{}
	s.passthroughMeta = map[string]configMeta{}
}

func (s *controlState) replaceAllWithMeta(values map[string]string, meta map[string]configMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = values
	s.meta = meta
}

func (s *controlState) snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

func (s *controlState) snapshotV2() controlStateEnvelope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vals := make(map[string]string, len(s.values)+len(s.passthroughValues))
	for k, v := range s.values {
		vals[k] = v
	}
	for k, v := range s.passthroughValues {
		vals[k] = v
	}
	meta := make(map[string]configMeta, len(s.meta)+len(s.passthroughMeta))
	for k, m := range s.meta {
		meta[k] = m
	}
	for k, m := range s.passthroughMeta {
		meta[k] = m
	}
	return controlStateEnvelope{
		Version: 2,
		Values:  vals,
		Meta:    meta,
	}
}

func (s *controlState) statusSnapshot() map[string]struct {
	Value string
	Meta  configMeta
} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	envDefaults := map[string]string{
		"ramlogs":    os.Getenv("URNETWORK_RAMLOGS"),
		"fast_auth":  os.Getenv("URNETWORK_FAST_AUTH"),
		"metrics":    os.Getenv("URNETWORK_METRICS"),
		"profile":    os.Getenv("URNETWORK_PROFILE"),
		"gogc":       os.Getenv("GOGC"),
		"gomemlimit": os.Getenv("GOMEMLIMIT"),
	}

	out := make(map[string]struct {
		Value string
		Meta  configMeta
	}, len(controlKeys))
	for k := range controlKeys {
		if v, ok := s.values[k]; ok {
			out[k] = struct {
				Value string
				Meta  configMeta
			}{Value: v, Meta: s.meta[k]}
		} else if v := envDefaults[k]; v != "" {
			out[k] = struct {
				Value string
				Meta  configMeta
			}{Value: v, Meta: configMeta{Source: SourceEnv}}
		} else {
			out[k] = struct {
				Value string
				Meta  configMeta
			}{Value: "", Meta: configMeta{Source: SourceDefault}}
		}
	}
	return out
}

func startupValues() map[string]string {
	out := map[string]string{
		"ramlogs": os.Getenv("URNETWORK_RAMLOGS"),
		"profile": os.Getenv("URNETWORK_PROFILE"),
	}
	return out
}

func controlStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "provider_state.json"), nil
}

func loadControlState() (*controlState, error) {
	path, err := controlStatePath()
	if err != nil {
		return nil, err
	}
	s := newControlState()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}

	var envelope controlStateEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Version > 0 {
		for k, v := range envelope.Values {
			if controlKeys[k] {
				s.values[k] = v
				if m, ok := envelope.Meta[k]; ok {
					s.meta[k] = m
				}
			} else {
				s.passthroughValues[k] = v
				if m, ok := envelope.Meta[k]; ok {
					s.passthroughMeta[k] = m
				}
			}
		}
		for k, m := range envelope.Meta {
			if !controlKeys[k] {
				s.passthroughMeta[k] = m
			}
		}
		return s, nil
	}

	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("provider_state.json: %w", err)
	}
	for k, v := range values {
		if controlKeys[k] {
			s.values[k] = v
			s.meta[k] = configMeta{Source: SourceLegacy}
		}
	}
	return s, nil
}

func (s *controlState) persist() error {
	path, err := controlStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	envelope := s.snapshotV2()
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".provider_state.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if parent, err := os.Open(filepath.Dir(path)); err == nil {
		parent.Sync()
		parent.Close()
	}
	return nil
}

// CommandAudit records one control socket operation.
type CommandAudit struct {
	Timestamp time.Time `json:"timestamp"`
	Cmd       string    `json:"cmd"`
	Key       string    `json:"key"`
	Value     string    `json:"value,omitempty"`
	Source    string    `json:"source"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

// AuditRing is a fixed-size circular buffer of command audit entries.
type AuditRing struct {
	mu      sync.Mutex
	entries [1000]CommandAudit
	head    int
	size    int
}

var globalAuditRing = &AuditRing{}

func (r *AuditRing) Append(entry CommandAudit) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries[r.head] = entry
	r.head = (r.head + 1) % len(r.entries)
	if r.size < len(r.entries) {
		r.size++
	}
	r.mu.Unlock()
}

func (r *AuditRing) Entries(limit int, cursor string) ([]CommandAudit, string) {
	if r == nil {
		return nil, ""
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size == 0 {
		return nil, ""
	}

	ordered := make([]CommandAudit, r.size)
	if r.size < len(r.entries) {
		copy(ordered, r.entries[:r.size])
	} else {
		n := copy(ordered, r.entries[r.head:])
		copy(ordered[n:], r.entries[:r.head])
	}

	if cursor != "" {
		cursorTime, err := time.Parse(time.RFC3339, cursor)
		if err == nil {
			i := 0
			for i < len(ordered) && !ordered[i].Timestamp.After(cursorTime) {
				i++
			}
			ordered = ordered[i:]
		}
	}

	total := len(ordered)
	if limit > total {
		limit = total
	}

	result := make([]CommandAudit, limit)
	for i := 0; i < limit; i++ {
		result[i] = ordered[total-1-i]
	}

	nextCursor := ""
	if limit < total {
		nextCursor = result[limit-1].Timestamp.Format(time.RFC3339)
	}

	return result, nextCursor
}

func recordAndPersist(entry CommandAudit) {
	if globalAuditRing == nil {
		return
	}
	globalAuditRing.Append(entry)
}

func tlog(format string, args ...any) {
	fmt.Printf("%s "+format, append([]any{time.Now().Format("0102 15:04:05")}, args...)...)
}

var (
	controlCmdsAcked atomic.Int64
	controlCmdsSet   atomic.Int64
	controlCmdsGet   atomic.Int64
	controlCmdsClear atomic.Int64
)

func IncrControlCmd(cmd string) {
	controlCmdsAcked.Add(1)
	switch cmd {
	case "set":
		controlCmdsSet.Add(1)
	case "get":
		controlCmdsGet.Add(1)
	case "clear":
		controlCmdsClear.Add(1)
	}
}

func restrictSocketACL(_ string) error {
	return nil
}

func verifyPeerCredentials(_ *net.UnixConn) error {
	return nil
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

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket chmod: %w", err)
	}
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

	cleanup := func() {
		ln.Close()
		os.Remove(path)
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
			tlog("🔒 [control] rejected connection: %s\n", err)
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
	case "fast_auth", "proxy_self_heal":
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
		if err := applyLiveSideEffect(req.Key, req.Value); err != nil {
			tlog("⚠️ [control] set %s=%s (was %s) persisted but live apply failed, takes effect on restart: %s\n",
				req.Key, req.Value, formerValue(oldValue, hadOld), err)
			return controlResponse{OK: false, Error: "persisted, but failed to apply live: " + err.Error()}
		}
		tlog("⚙️ [control] set %s=%s (was %s)\n", req.Key, req.Value, formerValue(oldValue, hadOld))
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
		liveCleared := liveEffectKeys[req.Key]
		if liveCleared {
			if err := applyLiveDefault(req.Key); err != nil {
				tlog("⚠️ [control] clear %s persisted but live default apply failed: %s\n", req.Key, err)
				return controlResponse{OK: false, NeedsRestart: true, Error: "cleared, but failed to reapply live default: " + err.Error()}
			}
		}
		tlog("⚙️ [control] cleared %s (was %s)\n", req.Key, formerValue(oldValue, hadOld))
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
		return controlResponse{OK: true, Settings: settings, StartupValues: startupValues(), MetricsAddrs: metricsServedAddrs()}

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
		tlog("🛑 [control] shutdown requested via control socket\n")
		go func() {
			time.Sleep(50 * time.Millisecond)
			state.shutdownFn()
		}()
		return controlResponse{OK: true, Value: "shutting down"}

	case "hotswap":
		if hotSwapTrigger == nil {
			return controlResponse{OK: false, Error: "hotswap trigger not available"}
		}
		go func() {
			if err := hotSwapTrigger(); err != nil {
				tlog("[hotswap] background handoff failed: %v\n", err)
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

var controlApplyLog = func(format string, args ...any) { tlog(format, args...) }

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
		tlog("[metrics] stopped Prometheus /metrics\n")
	}
	return nil
}

// Stubs/adapters for connect metrics functions not present in v2026 connect.
type ExtraMetricsProvider func() string

var (
	extraMetricsMu       sync.RWMutex
	extraMetricsProvider ExtraMetricsProvider
	persistentErrorFunc  func()
	providerExtraMetrics = func() string { return "" }
	IncrPersistentError  = func() {}
)

func setExtraMetricsProvider(fn ExtraMetricsProvider) {
	extraMetricsMu.Lock()
	defer extraMetricsMu.Unlock()
	extraMetricsProvider = fn
}

func setPersistentErrorFunc(fn func()) {
	persistentErrorFunc = fn
}

func prometheusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder
		fmt.Fprintf(&b, "# HELP urnet_control_socket_up Provider control socket is up.\n")
		fmt.Fprintf(&b, "# TYPE urnet_control_socket_up gauge\n")
		fmt.Fprintf(&b, "urnet_control_socket_up 1\n")
		extraMetricsMu.RLock()
		fn := extraMetricsProvider
		extraMetricsMu.RUnlock()
		if fn != nil {
			if extra := fn(); extra != "" {
				b.WriteString(extra)
				if !strings.HasSuffix(extra, "\n") {
					b.WriteByte('\n')
				}
			}
		}
		w.Write([]byte(b.String()))
	})
}

var (
	metricsMu             sync.Mutex
	metricsServer         *http.Server
	metricsListener       net.Listener
	metricsUnregCloser    func()
	metricsHandoffPending atomic.Bool
	hotSwapTrigger        func() error

	coordinatorClosersMu  sync.Mutex
	coordinatorCloserSeq uint64
	coordinatorClosersMap map[uint64]func()
)

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
	setExtraMetricsProvider(providerExtraMetrics)
	setPersistentErrorFunc(IncrPersistentError)
	server := &http.Server{
		Handler:           prometheusHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	metricsMu.Lock()
	metricsServer = server
	metricsListener = ln
	metricsMu.Unlock()

	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			tlog("[metrics] listener failed: %v\n", err)
		}
	}()
	if metricsUnregCloser != nil {
		metricsUnregCloser()
	}
	metricsUnregCloser = RegisterCoordinatorCloser(func() {
		if err := stopMetrics(); err != nil {
			tlog("[metrics] releasing /metrics for hotswap: %v\n", err)
		}
	})
	tlog("[metrics] started Prometheus /metrics on %s\n", ln.Addr())
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
		tlog("[metrics] could not bind /metrics after hotswap takeover: %v\n", err)
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
			tlog("[control] failed to apply persisted gomemlimit=%s: %s\n", v, err)
		}
	}
	if v, ok := state.get("gogc"); ok && v != "" && !strings.EqualFold(v, "off") {
		if err := applyLiveSideEffect("gogc", v); err != nil {
			tlog("[control] failed to apply persisted gogc=%s: %s\n", v, err)
		}
	}
	if v, ok := state.get("metrics"); ok && strings.EqualFold(v, "on") && os.Getenv("URNETWORK_METRICS") == "" && !metricsHandoffPending.Load() {
		if err := applyMetricsLive("on"); err != nil {
			tlog("[control] failed to apply persisted metrics=on: %s\n", err)
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
			tlog("[control] timed out waiting for parent to release control socket after takeover; proceeding anyway\n")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var metricsAutoPorts = []int{9100, 9101, 9102, 9103}
var metricsContainerMarkers = []string{"/.dockerenv", "/run/.containerenv"}
var tailscaleAddrsFunc = tailscaleIPv4Addrs
var tailscaleRescanInterval = 30 * time.Second
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

func tailscaleIPv4Addrs() []netip.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || !isTailscaleInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.Is4() && tailscaleCGNAT.Contains(ip) {
				out = append(out, ip)
			}
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}

func isTailscaleInterface(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "tailscale") {
		return true
	}
	return runtime.GOOS == "darwin" && strings.HasPrefix(lower, "utun")
}

func runningInContainer() bool {
	for _, marker := range metricsContainerMarkers {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

func metricsListenSetting() string {
	v, ok := globalControlState.get("metrics_listen")
	if !ok {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "auto") || strings.EqualFold(v, "off") {
		return ""
	}
	return v
}

func validateMetricsListen(value string) error {
	if strings.EqualFold(value, "auto") || strings.EqualFold(value, "off") {
		return nil
	}
	ap, err := netip.ParseAddrPort(value)
	if err != nil {
		return fmt.Errorf("metrics_listen: must be auto or an IP address with a port, like 100.64.0.10:9100 or 0.0.0.0:9100 (got %q)", value)
	}
	if ap.Port() == 0 {
		return fmt.Errorf("metrics_listen: port must be 1-65535 (got %q)", value)
	}
	return nil
}

func listenMetrics(wait time.Duration) (net.Listener, error) {
	if addr := os.Getenv("URNETWORK_METRICS"); addr != "" {
		return listenOrWait(addr, wait)
	}
	if addr := metricsListenSetting(); addr != "" {
		return listenOrWait(addr, wait)
	}

	container := runningInContainer()
	host := "127.0.0.1"
	if container {
		host = "0.0.0.0"
	}
	var lastErr error
	for i, port := range metricsAutoPorts {
		portWait := time.Duration(0)
		if i == 0 {
			portWait = wait
		}
		ln, err := listenOrWait(net.JoinHostPort(host, fmt.Sprint(port)), portWait)
		if err != nil {
			lastErr = err
			tlog("[metrics] port %d in use, trying next\n", port)
			continue
		}
		if container {
			return ln, nil
		}
		multi := newMetricsMultiListener(port, ln)
		multi.addTailscale(portWait)
		go multi.watchTailscale(tailscaleRescanInterval)
		return multi, nil
	}
	return nil, fmt.Errorf("no free port in %s:%d-%d: %w", host, metricsAutoPorts[0], metricsAutoPorts[len(metricsAutoPorts)-1], lastErr)
}

func metricsServedAddrs() []string {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	ln := metricsListener
	if ln == nil {
		return nil
	}
	if multi, ok := ln.(*metricsMultiListener); ok {
		return multi.Addrs()
	}
	return []string{ln.Addr().String()}
}

func applyMetricsListenLive() error {
	metricsMu.Lock()
	srv := metricsServer
	metricsMu.Unlock()
	if srv == nil {
		return nil
	}
	if err := stopMetrics(); err != nil {
		return fmt.Errorf("metrics_listen: stopping the old listener: %w", err)
	}
	ln, err := listenMetrics(2 * time.Second)
	if err != nil {
		return fmt.Errorf("metrics_listen: %w", err)
	}
	serveMetrics(ln)
	return nil
}

type metricsMultiListener struct {
	port  int
	conns chan net.Conn
	done  chan struct{}

	mu        sync.Mutex
	listeners []net.Listener
	failed    map[string]bool
	closed    bool
}

func newMetricsMultiListener(port int, first net.Listener) *metricsMultiListener {
	m := &metricsMultiListener{
		port:   port,
		conns:  make(chan net.Conn),
		done:   make(chan struct{}),
		failed: map[string]bool{},
	}
	m.add(first)
	return m
}

func (m *metricsMultiListener) add(ln net.Listener) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		ln.Close()
		return
	}
	m.listeners = append(m.listeners, ln)
	go m.acceptLoop(ln)
}

func (m *metricsMultiListener) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case m.conns <- conn:
		case <-m.done:
			conn.Close()
			return
		}
	}
}

func (m *metricsMultiListener) has(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ln := range m.listeners {
		if ln.Addr().String() == addr {
			return true
		}
	}
	return false
}

func (m *metricsMultiListener) addTailscale(wait time.Duration) {
	for _, ip := range tailscaleAddrsFunc() {
		addr := netip.AddrPortFrom(ip, uint16(m.port)).String()
		if m.has(addr) {
			continue
		}
		ln, err := listenOrWait(addr, wait)
		if err != nil {
			m.mu.Lock()
			first := !m.failed[addr]
			m.failed[addr] = true
			m.mu.Unlock()
			if first {
				tlog("[metrics] could not serve /metrics on Tailscale address %s: %v\n", addr, err)
			}
			continue
		}
		m.add(ln)
		tlog("[metrics] serving /metrics on Tailscale address %s\n", addr)
	}
}

func (m *metricsMultiListener) watchTailscale(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.addTailscale(0)
		}
	}
}

func (m *metricsMultiListener) Accept() (net.Conn, error) {
	select {
	case conn := <-m.conns:
		return conn, nil
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *metricsMultiListener) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return net.ErrClosed
	}
	m.closed = true
	close(m.done)
	for _, ln := range m.listeners {
		ln.Close()
	}
	return nil
}

func (m *metricsMultiListener) Addr() net.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listeners[0].Addr()
}

func (m *metricsMultiListener) Addrs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.listeners))
	for _, ln := range m.listeners {
		out = append(out, ln.Addr().String())
	}
	return out
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
		tlog("🏷️ [identity] dashboard label: %s\n", description)
	} else {
		tlog("🏷️ [identity] dashboard label changed: %s -> %s\n", lastDashboardLabel, description)
	}
	lastDashboardLabel = description
}
