package miner

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// control_audit.go ports the 3.23-fix v33 provider's command audit ring
// (provider/audit_ring.go): the fixed-size circular buffer behind the control
// socket's "history" command. Entries are also persisted to <state>/audit.json
// so `urnet-tools history` shows lifecycle events across restarts, not just
// this process's commands.

// CommandAudit records one control socket operation. The JSON shape matches
// the v33 wire format (and the client's urnettools.AuditEntry).
type CommandAudit struct {
	Timestamp time.Time `json:"timestamp"`
	Cmd       string    `json:"cmd"`
	Key       string    `json:"key"`
	Value     string    `json:"value,omitempty"`
	Source    string    `json:"source"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

// auditRingCapacity is the fixed ring size: no backing-array growth, at most
// about 50 KB on disk.
const auditRingCapacity = 1000

// auditRing is a fixed-size circular buffer of command audit entries.
type auditRing struct {
	mu      sync.Mutex
	entries [auditRingCapacity]CommandAudit
	head    int
	size    int
	path    string
}

var globalAuditRing *auditRing

// initControlAuditRing loads or creates the audit ring at startup. A missing
// file is normal (a fresh state directory).
func initControlAuditRing() {
	path, err := providerStatePath("audit.json")
	if err != nil {
		return
	}
	ring := &auditRing{path: path}

	var saved struct {
		Entries []CommandAudit `json:"entries"`
	}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &saved); err == nil {
			for _, e := range saved.Entries {
				ring.appendLocked(e)
			}
		} else {
			tlog("[audit] ignoring unreadable audit log: %v\n", err)
		}
	}

	globalAuditRing = ring
}

// recordControlProcessStart appends this process's startup to the audit ring
// so `urnet-tools history` shows lifecycle events, not just config changes.
func recordControlProcessStart() {
	recordControlAudit(CommandAudit{
		Timestamp: time.Now(),
		Cmd:       "start",
		Key:       "version",
		Value:     minerControlVersion(),
		Source:    "boot",
		OK:        true,
	})
}

// appendLocked appends one entry to the ring. Caller holds r.mu.
func (r *auditRing) appendLocked(entry CommandAudit) {
	r.entries[r.head] = entry
	r.head = (r.head + 1) % len(r.entries)
	if r.size < len(r.entries) {
		r.size++
	}
}

// orderedLocked returns the ring's entries oldest to newest. Caller holds r.mu.
func (r *auditRing) orderedLocked() []CommandAudit {
	if r.size < len(r.entries) {
		return append([]CommandAudit(nil), r.entries[:r.size]...)
	}
	ordered := make([]CommandAudit, 0, len(r.entries))
	ordered = append(ordered, r.entries[r.head:]...)
	ordered = append(ordered, r.entries[:r.head]...)
	return ordered
}

// entries returns up to limit entries, starting after cursor (RFC3339).
// An empty cursor returns from the most recent entry. Results are newest-first.
func (r *auditRing) historyEntries(limit int, cursor string) ([]CommandAudit, string) {
	if r == nil {
		return nil, ""
	}
	if limit <= 0 {
		limit = 50
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size == 0 {
		return nil, ""
	}

	ordered := r.orderedLocked()

	// Filter by cursor: skip entries with timestamp <= cursor.
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

	// Return newest-first.
	total := len(ordered)
	if limit > total {
		limit = total
	}
	result := make([]CommandAudit, limit)
	for i := 0; i < limit; i++ {
		result[i] = ordered[total-1-i]
	}

	// nextCursor is the timestamp of the oldest returned entry.
	nextCursor := ""
	if limit < total {
		nextCursor = result[limit-1].Timestamp.Format(time.RFC3339)
	}
	return result, nextCursor
}

// persist writes the ring to disk atomically.
func (r *auditRing) persist() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	ordered := r.orderedLocked()
	r.mu.Unlock()

	payload, err := json.Marshal(map[string]any{"entries": ordered})
	if err != nil {
		return err
	}
	return atomicWriteFile(r.path, payload, 0o600)
}

// recordControlAudit appends an entry and persists at most every 30 seconds
// (the ring is at most about 50 KB, so writing it on every command would be
// wasteful). The persist gate is mutex-guarded because control-socket
// connections are served concurrently.
var (
	auditPersistMu   sync.Mutex
	lastAuditPersist time.Time
)

func recordControlAudit(entry CommandAudit) {
	if globalAuditRing == nil {
		return
	}
	globalAuditRing.mu.Lock()
	globalAuditRing.appendLocked(entry)
	globalAuditRing.mu.Unlock()

	auditPersistMu.Lock()
	defer auditPersistMu.Unlock()
	if time.Since(lastAuditPersist) <= 30*time.Second {
		return
	}
	lastAuditPersist = time.Now()
	if err := globalAuditRing.persist(); err != nil {
		tlog("[audit] persist failed: %v\n", err)
	}
}

// forceControlAuditPersist persists immediately (shutdown and restart paths).
func forceControlAuditPersist() {
	if globalAuditRing == nil {
		return
	}
	auditPersistMu.Lock()
	defer auditPersistMu.Unlock()
	lastAuditPersist = time.Now()
	if err := globalAuditRing.persist(); err != nil {
		tlog("[audit] shutdown persist failed: %v\n", err)
	}
}

// sortAuditEntries orders entries by timestamp, oldest first. It exists for
// tests that compare merged rings.
func sortAuditEntries(entries []CommandAudit) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
}
