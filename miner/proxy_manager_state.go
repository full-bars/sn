package miner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// This file ports the 3.23-fix v33 provider's proxy.state store (proxy_state.go
// and proxy_id.go): the on-disk record of what the provider is currently
// running, keyed by proxy identity. The fork-only port keeps the v33 file
// format (version 2, identity keys) so state written by a 3.23-fix provider can
// be adopted, and uses the miner's providerStatePath so URNETWORK_STATE_DIR
// keeps working.

// proxyStateMu serializes all proxy.state read-modify-write cycles.
var proxyStateMu sync.Mutex

// proxyStateVersion is bumped whenever ProxyState.Proxies' map key changes
// format. 2 = keyed by proxy identity (ProxySettings.Key(): address, or
// address+user for a shared-gateway proxy like Decodo). Absent or 1 = the
// legacy format, keyed by bare address alone. writeProxyState always stamps
// the current version; a file with no "version" field (or 0/1) unmarshals
// with Version==0, which adoptLegacyProxyState and every reader treat as
// "may contain legacy bare-address keys, never rewrite or drop them
// blindly" - see adoptLegacyProxyState.
const proxyStateVersion = 2

// ProxyState is the on-disk record of what the provider is currently running.
// Written atomically at startup and after each reload.
type ProxyState struct {
	Version   int                   `json:"version,omitempty"`
	Source    string                `json:"source"`     // live source file path ("" = internal config)
	StartedAt time.Time             `json:"started_at"` // provider process start time
	NextID    int                   `json:"next_id"`    // snapshot of counter for display
	Proxies   map[string]ProxyEntry `json:"proxies"`    // identity key -> entry (see proxyStateVersion)
}

// proxyHealthParked is the Health of a proxy proxy audit is holding out.
// It is distinct from "inactive" on purpose: "inactive" means unseen for 7+ days
// and is collected for removal by dead-proxy cleanup and `proxy remove-dead`,
// which edit the operator's proxy file. A parked proxy is resting and comes
// back on its own, so nothing that removes proxies may treat it as dead.
const proxyHealthParked = "parked"

// ProxyEntry records the stable ID and last-known health for one proxy.
// Score/Graded/Failed/LastGraded/LastDecided/Pending mirror the v33 fields so
// the trimmed field set of P1(a) stays format compatible; the P1(c) grading
// sweep is what writes them, and P1(a) only carries them through.
type ProxyEntry struct {
	ID           int    `json:"id"`
	Health       string `json:"health"`                  // "up", "dead", "recently_offline", "offline", "long_offline", "inactive", "parked"
	DownSince    string `json:"down_since,omitempty"`    // RFC3339, set when not up
	Source       string `json:"source,omitempty"`        // "file", "internal", or "url" - where this address was first added from
	AuthFailures int64  `json:"auth_failures,omitempty"` // cumulative auth errors this run

	Score       float64   `json:"score,omitempty"`        // stage-1 table probe result (ok/total), 0 = never graded
	Graded      bool      `json:"graded,omitempty"`       // true once a stage-1 table probe recorded a DECIDABLE result
	Failed      []string  `json:"failed,omitempty"`       // target hostnames that did not answer the last stage-1 pass
	LastGraded  time.Time `json:"last_graded,omitempty"`  // when the last stage-1 pass ran
	LastDecided time.Time `json:"last_decided,omitempty"` // when the last DECIDABLE stage-1 pass landed
	Pending     bool      `json:"pending,omitempty"`      // reached but not decidable on the last stage-1 pass
}

func proxyStatePath() (string, error) {
	return providerStatePath("proxy.state")
}

func readProxyState() (*ProxyState, error) {
	path, err := proxyStatePath()
	if err != nil {
		return nil, err
	}
	return readProxyStateFrom(path)
}

func readProxyStateFrom(path string) (*ProxyState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ProxyState{Proxies: map[string]ProxyEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy.state: %w", err)
	}
	var s ProxyState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse proxy.state: %w", err)
	}
	if s.Proxies == nil {
		s.Proxies = map[string]ProxyEntry{}
	}
	return &s, nil
}

func writeProxyState(s *ProxyState) error {
	path, err := proxyStatePath()
	if err != nil {
		return err
	}
	return writeProxyStateTo(path, s)
}

func writeProxyStateTo(path string, s *ProxyState) error {
	// Every write moves the file forward to the current key format, even if
	// it was read as legacy (Version 0/1). adoptLegacyProxyState is what
	// actually migrates entries; this just records that a write in the
	// current format happened, so a reader downstream (or an operator
	// inspecting the file) knows the map keys are identity keys, not bare
	// addresses, from this point on.
	s.Version = proxyStateVersion
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// adoptLegacyProxyState migrates legacy (bare-address-keyed) entries in
// state.Proxies to the new identity-keyed format (ProxySettings.Key()) for
// every address the given desired settings actually claim right now,
// leaving every other entry untouched - already-migrated, or a legacy
// address with no current claimant (left alone for a later release to age
// out normally, per proxyStateVersion's doc comment).
//
// Idempotent and safe to call on every reload. An address claimed by exactly
// one identity carries its legacy entry over intact; an address claimed by two
// or more identities adopts the legacy entry to the identity with the
// lexicographically smallest Key() - deterministic and stable across restarts.
func adoptLegacyProxyState(state *ProxyState, desired []*connect.ProxySettings) (adopted, split int) {
	byAddress := make(map[string][]*connect.ProxySettings, len(desired))
	for _, s := range desired {
		if s == nil || s.Address == "" {
			continue
		}
		byAddress[s.Address] = append(byAddress[s.Address], s)
	}

	for address, settingsAtAddress := range byAddress {
		legacy, hasLegacy := state.Proxies[address]
		if !hasLegacy {
			continue
		}

		seen := map[string]bool{}
		var keys []string
		for _, s := range settingsAtAddress {
			k := s.Key()
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		winner := keys[0]

		if winner == address {
			// The winning (and, if len(keys)==1, only) identity carries no
			// distinguishing user: its Key() IS the bare address, so the
			// legacy entry is already correctly keyed. Nothing to adopt.
			continue
		}

		delete(state.Proxies, address)
		state.Proxies[winner] = legacy
		adopted++

		if len(keys) > 1 {
			split++
			tlog("[proxy][identity] %s split into %d identities on adoption; %s kept id=%d and history, the rest start fresh\n",
				address, len(keys), proxyKeyDisplay(winner), legacy.ID)
		}
	}
	return adopted, split
}

// proxyKeyDisplay renders a proxy identity key for operator-facing output
// ("addr" for no auth, "addr (user ab***yz)" for a shared-gateway
// identity), never the raw key - Key()'s \x1f separator must never reach a
// log line or terminal verbatim, and the password is never in the key to
// begin with (see ProxySettings.Key()).
func proxyKeyDisplay(key string) string {
	address, user := connect.SplitProxyKey(key)
	if user == "" {
		return address
	}
	return fmt.Sprintf("%s (user %s)", address, obfuscateUser(user))
}

// resolveProxyID returns the stable ID for an address.
// Known addresses keep their existing ID; new ones get the next counter value.
func resolveProxyID(state *ProxyState, address string) int {
	if entry, ok := state.Proxies[address]; ok {
		return entry.ID
	}
	id := nextProxyID()
	state.Proxies[address] = ProxyEntry{ID: id}
	return id
}

// tagProxySourceIfUnset records where a proxy address was first added from
// ("file", "internal", or "url"). Once set, the tag is never overwritten -
// an address keeps its original provenance across reloads and restarts, so
// source-scoped dead-proxy cleanup stays accurate even if the same address
// later also appears in a different source. (v33 additionally registers the
// source with the recovery scout; the scout is P1(c), so the port records the
// tag only.)
func tagProxySourceIfUnset(state *ProxyState, address, source string) {
	entry := state.Proxies[address]
	if entry.Source == "" {
		entry.Source = source
	}
	state.Proxies[address] = entry
}

// proxyIDCounter is the global monotonic proxy ID counter.
// IDs are never reused - even after removal and re-addition.
var proxyIDCounter int64

// nextProxyID allocates the next never-reused proxy ID.
func nextProxyID() int {
	return int(atomic.AddInt64(&proxyIDCounter, 1) - 1)
}

// initProxyIDCounter fast-forwards the monotonic counter above highestExistingID
// so IDs from previous runs stored in proxy.state are never reused.
func initProxyIDCounter(highestExistingID int) {
	// ID 0 is reserved for the native [direct] connection.
	// All external proxies must start at 1 or higher.
	if highestExistingID < 0 {
		highestExistingID = 0
	}

	target := int64(highestExistingID + 1)
	for {
		cur := atomic.LoadInt64(&proxyIDCounter)
		if cur >= target {
			return
		}
		if atomic.CompareAndSwapInt64(&proxyIDCounter, cur, target) {
			return
		}
	}
}

// currentProxyIDCounter returns the current counter value, for state snapshots.
func currentProxyIDCounter() int {
	return int(atomic.LoadInt64(&proxyIDCounter))
}
