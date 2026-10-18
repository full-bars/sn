package miner

import "strings"

// metrics_seams.go is the registration point for the v33 Prometheus families
// the miner cannot source yet. The v33 provider (candidate/v33-ops,
// provider/metrics_*.go) and the v33 connect handler (metrics_prometheus.go)
// emit these; the miner's port names them here, and the emission stays with
// the work that owns the data:
//
//   - grading/earnings (parallel P2): sessions, contract outcomes, the
//     persisted lifetime store, and the per-category error counts;
//   - connect-owned accounting the pinned fork no longer exports: the
//     reconnect governor, transfer/dead-client/socket/transport/speedtest
//     series, H1+ counters, H3 and the dual-stack family transport;
//   - miner paths not ported yet: the startup disk benchmark, the enforcement
//     log, the peer-key cross-check, and the supervised-loop registry.
//
// Every hook below is nil until its owner wires it. minerExtraMetrics omits a
// family while its hook is nil, so a missing series means "not implemented",
// never a fabricated zero. When a port lands, it sets the hook (in its own
// file) and the family appears; nothing in the scrape has to change.
var (
	// metricsSeamSessions is connect's PQE/classical session accounting:
	// urnet_sessions_pqe, urnet_sessions_classical, urnet_sessions_opened_total
	// and urnet_sessions_opened_recent. The miner's SDK does not expose the
	// session split yet (see control_report_miner.go).
	metricsSeamSessions func() *seamSessions

	// metricsSeamContracts is the contract outcome registry:
	// urnet_contracts_total{result="acquired"|"denied"} (and the lifetime
	// urnet_lifetime_contracts_total). v33 tracks it per proxy in
	// contract_metrics.go; the earnings port owns the miner's copy.
	metricsSeamContracts func() (acquired, denied int64, ok bool)

	// metricsSeamLifetime is the persisted all-time store:
	// urnet_lifetime_sessions_total, urnet_lifetime_contracts_total,
	// urnet_lifetime_proxies_total and urnet_lifetime_billable_bytes_total.
	metricsSeamLifetime func() *seamLifetime

	// metricsSeamErrors is the persistent per-category error counts:
	// urnet_lifetime_errors_total{category} and urnet_errors_total{category}.
	metricsSeamErrors func() map[string]uint64

	// metricsSeamLoops is the supervised background loop registry:
	// urnet_loop_restarts_total{loop} and urnet_loop_up{loop}.
	metricsSeamLoops func() []seamLoop

	// metricsSeamDiskBench is the startup disk benchmark:
	// urnet_disk_write_mbps, urnet_disk_slow and urnet_disk_bench_age_seconds.
	metricsSeamDiskBench func() *seamDiskBench

	// metricsSeamDohFailures is connect's DNS-over-HTTPS failure counter:
	// urnet_doh_failures_total. The pinned fork keeps DoH but exports no
	// counter.
	metricsSeamDohFailures func() int64

	// metricsSeamH1Plus is the H1+ upgrade accounting:
	// urnet_h1_max_message_bytes and the urnet_h1plus_* counters. The fork
	// keeps a per-settings H1PlusStats object but no process-global accessor.
	metricsSeamH1Plus func(b *strings.Builder) bool

	// metricsSeamDualstack is the dual-stack family transport block:
	// urnet_dualstack_* and urnet_ip_version.
	metricsSeamDualstack func(b *strings.Builder) bool

	// metricsSeamEnforcement is the durable enforcement log:
	// urnet_enforcement_events, urnet_enforcement_events_total and the
	// log-health counters.
	metricsSeamEnforcement func(b *strings.Builder) bool

	// metricsSeamPeerKeyCrosscheck is the out-of-band peer key cross-check:
	// urnet_peer_key_crosscheck_total{result}.
	metricsSeamPeerKeyCrosscheck func(b *strings.Builder) bool

	// metricsSeamGovern is the v33 connect block the pinned fork dropped:
	// the reconnect governor (urnet_govern_*), transfer accounting
	// (urnet_transfer_*), dead-client release (urnet_dead_client_*), the
	// socket/connect-attempt byte accounting (urnet_socket_bytes_total,
	// urnet_connect_attempt*_total), the runtime connection count
	// (urnet_connections_active) and the categorized error counter
	// (urnet_errors_total).
	metricsSeamGovern func(b *strings.Builder) bool

	// metricsSeamH3 is the H3 identity set and DATAGRAM accounting:
	// urnet_h3_mode_info, urnet_h3_set_size and urnet_h3_datagram_*.
	metricsSeamH3 func(b *strings.Builder) bool
)

// The typed seam payloads are the shapes the v33 writer functions consume;
// they are defined here so a landing port has the contract ready.

// seamSessions is one session-count reading.
type seamSessions struct {
	ActivePQE, ActiveClas       int64
	PQELifetime, ClasLifetime   int64
	PQEHour, PQEDay, PQEWeek    int64
	ClasHour, ClasDay, ClasWeek int64
}

// seamLifetime is one persisted all-time reading.
type seamLifetime struct {
	SessionsPQE, SessionsClas          uint64
	ContractsAcquired, ContractsDenied uint64
	ProxiesRecovered, ProxiesLost      uint64
	BillableBytes                      uint64
}

// seamLoop is one supervised background loop.
type seamLoop struct {
	Name     string
	Up       bool
	Restarts uint64
}

// seamDiskBench is the last startup disk benchmark reading.
type seamDiskBench struct {
	WriteMBps  float64
	Slow       bool
	AgeSeconds int64
}
