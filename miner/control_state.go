package miner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// control_state.go ports the 3.23-fix v33 provider's control-state store
// (provider/control_state.go) onto the miner. It is the one record of every
// runtime setting an operator can change through the control socket
// (control_socket.go). The provider process is the only writer, so there is
// none of the read-modify-write trouble of the legacy per-key override files
// the resolve* functions in the miner's proxy manager still read as a
// fallback.
//
// The file format is the v33 format (provider_state.json, version 2 envelope
// with per-key metadata) and it lives in the miner's state directory, so
// URNETWORK_STATE_DIR keeps working and a state directory written by a
// 3.23-fix provider is adopted as-is.

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
type controlStateEnvelope struct {
	Version int                   `json:"version"`
	Values  map[string]string     `json:"values"`
	Meta    map[string]configMeta `json:"meta,omitempty"`
}

// controlState is the provider's own in-memory record of every runtime
// setting an operator can change via the control socket. All access goes
// through this type's methods, which hold mu for their whole body.
type controlState struct {
	mu     sync.RWMutex
	values map[string]string
	meta   map[string]configMeta
	// passthrough preserves unrecognized keys from v2 envelopes across
	// load to persist round-trips so a rollback does not drop them.
	passthroughValues map[string]string
	passthroughMeta   map[string]configMeta
	// txMu serializes the read-modify-write-persist-rollback sequence that
	// makes up one logical set/clear operation; the per-method mu does not
	// span that sequence.
	txMu sync.Mutex
	// shutdownFn triggers the same graceful shutdown path as SIGTERM: it is
	// set by the provide run before the socket is opened.
	shutdownFn func()
}

// controlKeys are the settings the socket accepts. This is the v33 key
// surface, unchanged, so the unmodified urnet-tools client validates and
// sends exactly the keys it sends to a 3.23-fix provider. Keys the miner has
// no live consumer for are still persisted and reported; the provider log
// says which ones need a restart (see needsRestart in control_socket.go).
var controlKeys = map[string]bool{
	"node_name":                   true,
	"report_url":                  true,
	"report_interval":             true,
	"fast_auth":                   true,
	"baseline":                    true,
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
	"proxy_audit":                 true,
	"smart_dialer":                true,
	"h3":                          true,
	"h3_datagram":                 true,
	"h3_datagram_send":            true,
	"ip_version":                  true,
	"h1_plus":                     true,
	"oom_cap":                     true,
}

// globalControlState is the single provider-wide instance, set by
// loadControlState during provider startup, before the control socket or any
// resolve* function can be reached.
var globalControlState = newControlState()

func newControlState() *controlState {
	return &controlState{
		values:            map[string]string{},
		meta:              map[string]configMeta{},
		passthroughValues: map[string]string{},
		passthroughMeta:   map[string]configMeta{},
	}
}

// get returns a setting raw string value and whether it has been set via
// the control socket. found=false means the socket has no opinion on this
// key and the caller should fall back to its legacy file or startup default.
func (s *controlState) get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

// getWithMeta returns a setting value, its metadata, and whether it was found.
func (s *controlState) getWithMeta(key string) (string, configMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, s.meta[key], ok
}

// set validates key against controlKeys and stores value with SourceSocket.
// It does not persist; callers that want durability call persist().
func (s *controlState) set(key, value string) error {
	return s.setWithValue(key, value, SourceSocket)
}

// setWithValue validates key and stores value plus meta under the write lock.
func (s *controlState) setWithValue(key, value string, src Source) error {
	if !controlKeys[key] {
		return fmt.Errorf("unknown control key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	s.meta[key] = configMeta{Source: src, SetAt: time.Now()}
	// Remove from passthrough if it was there - prevents stale passthrough
	// values from clobbering a live set at persist time.
	delete(s.passthroughValues, key)
	delete(s.passthroughMeta, key)
	return nil
}

// clear validates key against controlKeys and removes it, so a later get
// reports found=false and every reader falls back to its legacy file or
// startup default exactly as if it had never been set.
func (s *controlState) clear(key string) error {
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

// snapshotV2 returns the full v2 envelope with values and meta, suitable for
// serializing to the v2 on-disk format.
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
	return controlStateEnvelope{Version: 2, Values: vals, Meta: meta}
}

// statusSnapshot returns every known setting with its value and metadata so
// the operator always sees the full picture, not just socket-overridden keys.
func (s *controlState) statusSnapshot() map[string]struct {
	Value string
	Meta  configMeta
} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Pre-populate with env or code defaults for all known keys.
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

// startupValues returns the env-var values for restart-required keys - the
// values the running process actually started with. The dashboard compares
// these against the current control-state values to decide whether a restart
// banner is warranted.
func startupValues() map[string]string {
	out := map[string]string{
		"ramlogs": os.Getenv("URNETWORK_RAMLOGS"),
		"profile": os.Getenv("URNETWORK_PROFILE"),
	}
	return out
}

// controlStatePath returns <state>/provider_state.json, the provider's own
// private, atomically-written record of every socket-set control key. The
// state directory honours URNETWORK_STATE_DIR through providerStatePath.
func controlStatePath() (string, error) {
	return providerStatePath("provider_state.json")
}

// loadControlState reads controlStatePath() into a fresh controlState. A
// missing file means no setting has ever been changed via the socket. It
// handles both v2 envelopes and legacy flat maps.
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

	// Try the v2 envelope first (detected by the version field).
	var envelope controlStateEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Version > 0 {
		for k, v := range envelope.Values {
			if controlKeys[k] {
				s.values[k] = v
				if m, ok := envelope.Meta[k]; ok {
					s.meta[k] = m
				}
			} else {
				// Unrecognized keys in v2 are preserved in a passthrough
				// section so a rollback does not drop them.
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

	// Legacy flat map (no version field).
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("provider_state.json: %w", err)
	}
	for k, v := range values {
		if controlKeys[k] {
			s.values[k] = v
			// Legacy-loaded values get SourceLegacy with zero time.
			s.meta[k] = configMeta{Source: SourceLegacy}
		}
		// Silently drop any key this binary no longer recognizes rather
		// than failing startup over it.
	}
	return s, nil
}

// pendingControlOp is one entry in <state>/pending_overrides.json, the queue
// urnet-tools writes when the provider is not running.
type pendingControlOp struct {
	Op    string `json:"op"` // "set" or "clear"
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// mergePendingControlOverrides applies the queue urnet-tools writes while the
// provider was stopped, then removes it. A malformed queue is left on disk
// for inspection rather than silently discarded. The log lines are the v33
// mergePendingOverrides ones verbatim (provider/pending_overrides.go), so the
// daemon log reads the same under either binary.
func mergePendingControlOverrides(state *controlState) {
	path, err := providerStatePath("pending_overrides.json")
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			tlog("[control] failed to read pending_overrides.json, leaving it in place: %s\n", err)
		}
		return
	}
	var ops []pendingControlOp
	if err := json.Unmarshal(data, &ops); err != nil {
		tlog("[control] pending_overrides.json is malformed, leaving it in place for inspection: %s\n", err)
		return
	}
	// Name each key that lands, not just the count: an operator who ran
	// `urnet-tools set` while the provider was stopped needs to see THEIR
	// setting registered on the next start.
	applied := 0
	var appliedDesc []string
	for _, op := range ops {
		var applyErr error
		switch op.Op {
		case "set":
			applyErr = state.setWithValue(op.Key, op.Value, SourcePending)
		case "clear":
			applyErr = state.clear(op.Key)
		default:
			applyErr = fmt.Errorf("unknown op %q", op.Op)
		}
		if applyErr != nil {
			tlog("[control] skipping invalid pending override (op=%q key=%q): %s\n", op.Op, op.Key, applyErr)
			continue
		}
		if op.Op == "clear" {
			appliedDesc = append(appliedDesc, "cleared "+op.Key)
		} else {
			appliedDesc = append(appliedDesc, op.Key+"="+op.Value)
		}
		applied++
	}
	if applied > 0 {
		if err := state.persist(); err != nil {
			tlog("[control] applied %d pending override(s) in memory but failed to persist; leaving pending_overrides.json in place to retry next start: %s\n", applied, err)
			return
		}
		tlog("⚙️ [control] applied %d queued override(s) from pending_overrides.json: %s\n",
			applied, strings.Join(appliedDesc, ", "))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		tlog("[control] applied pending overrides but failed to remove pending_overrides.json: %s\n", err)
	}
}

// persist atomically writes the current snapshot to controlStatePath() in v2
// envelope format: temp file in the same directory, fsync, rename, then
// fsync the parent directory. No flock is needed: the provider is the only
// process that ever writes this file.
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
	defer os.Remove(tmpName) // no-op once the rename below succeeds

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
	// Best-effort directory fsync - do not fail after a successful rename.
	if parent, err := os.Open(filepath.Dir(path)); err == nil {
		parent.Sync()
		parent.Close()
	}
	return nil
}
