package miner

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// control_report.go holds the typed payloads the control socket's data
// commands answer with. Their JSON shape is the wire contract with
// urnet-tools (internal/urnettools/snapshot.go, internals.go): fields are
// only ever added, never renamed or removed, and optional fields are omitted
// when the miner cannot supply them. The P2b wiring in
// control_report_miner.go fills them from the miner's real state; the
// builders here are the honest fallbacks for the parts the miner has no
// source for.

// nodeSnapshot is the typed picture of the node served by the "snapshot"
// command. Schema v1, same shape as the 3.23-fix provider's NodeSnapshot.
type nodeSnapshot struct {
	V               int       `json:"v"`
	Version         string    `json:"version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	Now             time.Time `json:"now"`
	UptimeSeconds   float64   `json:"uptime_seconds"`

	// State is one of starting, degraded, idle, flowing.
	State string `json:"state"`
	// StateReason says why the node is starting or degraded.
	StateReason    string `json:"state_reason,omitempty"`
	Busy           bool   `json:"busy"`
	RestartPending bool   `json:"restart_pending"`

	Rate     snapshotRate     `json:"rate"`
	Clients  int64            `json:"clients"`
	Sessions snapshotSessions `json:"sessions"`
	// CountsKnown is false when the counters were unavailable. Clients and
	// Sessions are then zeros that mean "not read", not "none".
	CountsKnown bool            `json:"-"`
	Proxies     snapshotProxies `json:"proxies"`
	// ClientIDs is the live pool's identity provenance for this run, beside
	// the proxy counts. Omitted until a provide run has started.
	ClientIDs *snapshotClientIDs `json:"client_ids,omitempty"`
	// ProxiesKnown is false when the proxy state could not be read. The
	// counts are then zeros that mean "unknown", not "no proxies", and a
	// sample must not publish them as if they were measurements.
	ProxiesKnown bool            `json:"-"`
	Pressure     float64         `json:"pressure"`
	Restart      snapshotRestart `json:"restart"`

	Resources snapshotResources `json:"resources"`

	// IdleHint says why the node is idle. Set only when State is idle.
	IdleHint string `json:"idle_hint,omitempty"`

	// Traffic carries the byte totals and the total-traffic rate.
	Traffic *snapshotTraffic `json:"traffic,omitempty"`

	// MinerNotes is an additive field (clients ignore unknown fields) naming
	// data the miner cannot supply or qualifying what it does supply, so the
	// reply never reads as more complete than it is.
	MinerNotes []string `json:"miner_notes,omitempty"`
}

// total is the pool size the state derivation and hints use.
func (p snapshotProxies) total() int {
	return p.Up + p.Degraded + p.Connecting + p.Dead
}

// snapshotTraffic is billable versus total traffic.
type snapshotTraffic struct {
	BillableBytes         uint64    `json:"billable_bytes"`
	TotalBytes            uint64    `json:"total_bytes"`
	LifetimeBillableBytes *uint64   `json:"lifetime_billable_bytes,omitempty"`
	TotalNowBps           float64   `json:"total_now_bps"`
	TotalAvg1mBps         float64   `json:"total_avg_1m_bps"`
	TotalAvg5mBps         float64   `json:"total_avg_5m_bps"`
	TotalHistoryBps       []float64 `json:"total_history_bps"`
}

// liveTraffic is the light reply to the "traffic" command.
type liveTraffic struct {
	AtUnixNano    int64  `json:"at_unix_nano"`
	BillableBytes uint64 `json:"billable_bytes"`
	TotalBytes    uint64 `json:"total_bytes"`
}

// snapshotRate is billable throughput in bytes per second, with the per-second
// history ring (oldest first).
type snapshotRate struct {
	NowBps                 float64   `json:"now_bps"`
	Avg1mBps               float64   `json:"avg_1m_bps"`
	Avg5mBps               float64   `json:"avg_5m_bps"`
	HistoryIntervalSeconds float64   `json:"history_interval_seconds"`
	HistorySeq             uint64    `json:"history_seq,omitempty"`
	HistoryBps             []float64 `json:"history_bps"`
}

type snapshotSessions struct {
	PQE       int64 `json:"pqe"`
	Classical int64 `json:"classical"`
}

type snapshotProxies struct {
	Up         int `json:"up"`
	Degraded   int `json:"degraded"`
	Connecting int `json:"connecting"`
	Dead       int `json:"dead"`

	// A provider that predates trim reporting omits these; zeros then mean
	// "not reported", never "uncapped". TrimReported is the presence flag that
	// keeps a cleared cap from looking like an old build.
	TrimReported  bool   `json:"trim_reported,omitempty"`
	Configured    int    `json:"configured,omitempty"`
	TrimCap       int    `json:"trim_cap,omitempty"`
	TrimCapSource string `json:"trim_cap_source,omitempty"`
}

// snapshotClientIDs is the running slots split by how this run's identity
// started: reused from a stored login, or minted. It is per run, not a
// lifetime total. The node snapshot carries it as a pointer so a provider
// whose provide run has not started omits the block instead of reporting two
// zeros that would read as "every slot minted".
type snapshotClientIDs struct {
	Reused int `json:"reused"`
	Minted int `json:"minted"`
}

type snapshotRestart struct {
	Reason        string `json:"reason"`
	CleanShutdown bool   `json:"clean_shutdown"`
}

type snapshotResources struct {
	HeapInuseBytes uint64  `json:"heap_inuse_bytes"`
	MemLimitBytes  *uint64 `json:"mem_limit_bytes,omitempty"`
	RSSBytes       *uint64 `json:"rss_bytes,omitempty"`
	Goroutines     int     `json:"goroutines"`
	OpenFDs        *int    `json:"open_fds,omitempty"`
	FDLimit        *int    `json:"fd_limit,omitempty"`
}

// nodeInternals is the reply to "internals".
type nodeInternals struct {
	AtUnixNano int64 `json:"at_unix_nano"`

	Goroutines uint64 `json:"goroutines"`

	HeapObjectsBytes uint64 `json:"heap_objects_bytes"`
	HeapStacksBytes  uint64 `json:"heap_stacks_bytes"`
	HeapGoalBytes    uint64 `json:"heap_goal_bytes"`
	GOGC             int64  `json:"gogc"`

	GCCycles   uint64 `json:"gc_cycles"`
	AllocBytes uint64 `json:"alloc_bytes"`

	IntervalSeconds float64 `json:"interval_seconds"`
	GCPauseP99Ms    float64 `json:"gc_pause_p99_ms"`
	SchedLatP99Ms   float64 `json:"sched_latency_p99_ms"`
	GCCPUFraction   float64 `json:"gc_cpu_fraction"`
}

// goroutineGroup is one place goroutines are parked.
type goroutineGroup struct {
	Func  string `json:"func"`
	Count int    `json:"count"`
}

// goroutineGroups is the reply to "goroutines".
type goroutineGroups struct {
	AtUnixNano int64            `json:"at_unix_nano"`
	Total      int              `json:"total"`
	Groups     []goroutineGroup `json:"groups"`
}

// minerControlVersion is the version the "version" command reports: what the
// running process is, not what is on disk under its name.
func minerControlVersion() string {
	if v := RequireVersion(); v != "" {
		return v
	}
	return "dev"
}

// minerControlSnapshot builds the node snapshot. The real builder lives in
// control_report_miner.go; this indirection keeps the socket dispatch stable
// across the P2a and P2b commits.
var minerControlSnapshot = func() (*nodeSnapshot, error) {
	return nil, fmt.Errorf("snapshot is not available in this build")
}

// minerControlTraffic returns the live counter reply.
var minerControlTraffic = func() (*liveTraffic, error) {
	return nil, fmt.Errorf("traffic is not available in this build")
}

// minerControlInternals returns the runtime read.
var minerControlInternals = func() (*nodeInternals, error) {
	return nil, fmt.Errorf("internals is not available in this build")
}

// minerControlGoroutines returns the grouped goroutine profile.
var minerControlGoroutines = func() (*goroutineGroups, error) {
	return nil, fmt.Errorf("goroutines is not available in this build")
}

// minerControlTrimPreview renders what `proxy trim <count>` would shed. The
// running provider is the only process that knows its running set.
var minerControlTrimPreview = func(count int) (string, error) {
	return "", fmt.Errorf("the control socket is not available in this build")
}

// proxyAuditStatus and its parked-proxy element are defined in
// proxy_manager_audit_status.go (the P1(c) port). The control socket replies
// with that snapshot; the miner has no audit engine, so the builder below
// reports the gap explicitly.

// minerControlAuditStatus returns the proxy audit engine's last tick, or
// (nil, nil) when the miner has no audit engine. A non-nil error is reported
// to the client verbatim.
var minerControlAuditStatus = func() (*proxyAuditStatus, error) {
	return nil, nil
}

// minerLedgerTail returns the newest capacity-decision ledger entries
// (<state>/autopilot.jsonl), newest last. An absent file is an empty list.
func minerLedgerTail(limit int) (string, error) {
	path, err := providerStatePath("autopilot.jsonl")
	if err != nil {
		return "", err
	}
	return ledgerTailFromFile(path, limit)
}

// ledgerTailFromFile reads at most the last limit entries from path and renders
// them as the JSON array the control socket's Value carries. It is the miner's
// port of v33's ledgerTail: an absent file is an empty list, and a line that
// does not parse (or carries no actor) is skipped rather than poisoning the
// reply. The encoder re-emits the same {t,actor,action,from,to,mode,reason}
// objects the writer appended, so `urnet-tools autopilot log` reads either
// build's file.
//
// A scanner error after some entries parsed (an overlong or torn trailing line)
// must not hide them: the ledger's promise is that one corrupt line cannot bury
// the rest of the timeline. Only a file that yielded nothing is an error.
func ledgerTailFromFile(path string, limit int) (string, error) {
	entries, err := ledgerTail(path, limit)
	if err != nil {
		if len(entries) == 0 {
			return "", err
		}
		tlog("[ledger] warn: %s: %v (serving the %d entries read before it)\n", path, err, len(entries))
	}
	if entries == nil {
		entries = []ledgerEntry{}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// minerLedgerLimit clamps a ledger request the way v33 does: default 20, at
// most 200.
func minerLedgerLimit(n int) int {
	if n <= 0 {
		n = 20
	}
	if n > 200 {
		n = 200
	}
	return n
}

// atomize is a tiny helper so callers can express "unsigned counter reading"
// from a signed source without a branch at every site.
func atomize(v uint64) uint64 { return v }

// itoa is strconv.Itoa spelled once so the socket file stays short.
func itoa(n int) string { return strconv.Itoa(n) }
