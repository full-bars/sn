package miner

import (
	"strings"
	"time"
)

// metrics_seams.go is the registration point for the v33 Prometheus families
// the miner cannot source. The v33 provider (candidate/v33-ops,
// provider/metrics_*.go) and the v33 connect handler (metrics_prometheus.go)
// emit these; the miner names them here so a missing family has one obvious
// place to look.
//
// Wired in this build:
//
//   - metricsSeamLifetime: lifetime_metrics.go (the persisted all-time store);
//   - metricsSeamLoops: loop_supervisor.go (the supervised-loop registry);
//   - metricsSeamErrors: metrics_errors.go (the persistent error counts);
//   - metricsSeamDiskBench: disk_bench_metrics.go (the startup disk audit);
//   - metricsSeamContracts: contract_metrics.go (the provider contract feed:
//     cumulative acquired plus the open book -- count, bytes, utilization.
//     Denials have no signal at all; the hook comment says why).
//
// Every other hook below is UNSUPPORTED, not merely pending. Each comment says
// why, and the reason is one of two kinds: the data lives inside the pinned
// connect fork and is not exported, or the miner never built the subsystem at
// all. None of them can be filled from a value the miner already holds, so
// wiring one would need new instrumentation rather than a registration.
//
// A hook stays nil until its owner sets it (in its own file). minerExtraMetrics
// omits a family while its hook is nil, so a missing series means "not
// implemented", never a fabricated zero.
var (
	// UNSUPPORTED: the pinned SDK does not expose connect's PQE/classical
	// session split (control_report_miner.go names the same gap for the
	// snapshot), so the miner has no active or lifetime session counter to
	// read. urnet_sessions_* stays absent.
	metricsSeamSessions func() *seamSessions

	// Wired by contract_metrics.go's init: this process's acquired contracts
	// and the open book (count, bytes, utilization), all read from the provider
	// contract feed the SDK publishes per slot.
	//
	// UNSUPPORTED here: denials. A refusal never opens, so the manager never
	// emits a stats event and the feed the miner can see never names one. The
	// only refusal signal the pinned SDK exports is DeviceLocal.GetContractStatus,
	// a windowed Premium/InsufficientBalance/NoPermission summary with no count;
	// a real count would need an SDK-exported denial event (a
	// ContractManager.AddContractStatusCallback-grade hook), which the pinned
	// revision does not expose. So no denied series is published and none is
	// passed up for rendering: a zero would read as "nothing was refused" and
	// an "unknown" on every line would read as "checked, cannot tell" -- both
	// claims the miner cannot back.
	//
	// On a fleet-wide scrape that makes this family's label set differ by one
	// result: the provider publishes urnet_contracts_total{result="denied"}
	// from its own denial counter, and the miner has no such series. A
	// dashboard must read a miner's missing denied result as "not measured",
	// never as zero.
	metricsSeamContracts func() *seamContracts

	// Wired by lifetime_metrics.go's init: the persisted all-time store.
	metricsSeamLifetime func() *seamLifetime

	// Wired by metrics_errors.go's init: the persistent per-category error
	// store, counted from the provider diagnostics failure codes.
	metricsSeamErrors func() map[string]uint64

	// Wired by loop_supervisor.go's init: the supervised-loop registry that
	// every supervised loop in the run path registers into.
	metricsSeamLoops func() []seamLoop

	// Wired by disk_bench_metrics.go's init: the startup disk audit's last
	// reading, or nil when no benchmark has been recorded.
	metricsSeamDiskBench func() *seamDiskBench

	// UNSUPPORTED: the miner has no DNS-over-HTTPS dial path of its own (name
	// resolution goes through the host stack and the pinned connect), so there
	// is no failure counter to export.
	metricsSeamDohFailures func() int64

	// UNSUPPORTED: the pinned fork keeps H1PlusStats per client settings with
	// no process-global accessor. The switch is exported on its own as
	// urnet_h1plus_enabled (metrics_provider.go); the per-message counters are
	// not reachable.
	metricsSeamH1Plus func(b *strings.Builder) bool

	// UNSUPPORTED: the dual-stack family-transport block belongs to connect's
	// client internals, which the pinned revision does not export. The miner's
	// own dual-stack behaviour (the direct slot and the proxy UDP checks) keeps
	// no per-family counters.
	metricsSeamDualstack func(b *strings.Builder) bool

	// UNSUPPORTED: the miner has no durable enforcement log. Its
	// enforcement-shaped decisions (pressure shed, OOM cap, proxy audit) log
	// through the ramlog and events.log and keep no graded event counter.
	metricsSeamEnforcement func(b *strings.Builder) bool

	// UNSUPPORTED: the miner has no out-of-band peer-key cross-check.
	metricsSeamPeerKeyCrosscheck func(b *strings.Builder) bool

	// UNSUPPORTED: the reconnect governor, the transfer/dead-client/socket
	// accounting and urnet_connections_active are connect-internal in v33. The
	// pinned fork exports none of them and the miner keeps no equivalent
	// counters.
	metricsSeamGovern func(b *strings.Builder) bool

	// UNSUPPORTED: the H3 mode (resolveH3Mode) and the launched proxy set
	// (h3LaunchEligible) do exist, but the direct identity's slot is a local in
	// the provide run (run.go) with no scrape-reachable accessor, so
	// urnet_h3_set_size cannot be computed from outside that scope without new
	// plumbing. The urnet_h3_datagram_* counters live inside connect and the
	// pinned revision does not export them either. Emitting the mode alone
	// would advertise an identity set the scrape cannot measure, so the family
	// stays absent as a whole.
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

// seamContracts is one process-lifetime contract reading: the cumulative
// acquired count, plus the open book (see contract_metrics.go).
//
// There is no denied field on purpose. The miner has no denial source at all
// (see the metricsSeamContracts hook comment), so a constant denied=unknown --
// or a zero -- would be a claim it cannot back. The gap lives in that comment
// rather than on every rendered line.
type seamContracts struct {
	// Acquired is every contract the platform established since this process
	// started, open or since closed. Cumulative; the [profit] contracts= field
	// and urnet_contracts_total{result="acquired"} both mean this number.
	Acquired int64
	// Open is the contracts open right now. Open > 0 is what makes the open
	// gauges a reading; at 0 they are absent, not a zero.
	Open int
	// OpenLimitBytes and OpenUsedBytes are summed over the open contracts
	// only. OpenUsedBytes is per-contract, point-in-time usage as the feed
	// last reported it -- not the cumulative settled bytes.
	OpenLimitBytes int64
	OpenUsedBytes  int64
	// AvgUtilPct is the mean used/purchased over the open contracts that
	// report a positive purchased size, as a percentage. NaN when undefined;
	// the writer omits the gauge then rather than publishing 0%.
	AvgUtilPct float64
}

// seamLifetime is one persisted all-time reading.
type seamLifetime struct {
	SessionsPQE, SessionsClas uint64
	// ContractsAcquired is the persisted all-time acquired total. There is no
	// denied total here: the miner has no denial source (see the
	// metricsSeamContracts hook), so the store's field is kept only to
	// round-trip a v33 file and is never published.
	ContractsAcquired             uint64
	ProxiesRecovered, ProxiesLost uint64
	BillableBytes                 uint64
	// Since is when the totals began accumulating; SinceApprox says the start
	// predates tracking, so the totals include earlier history.
	Since       time.Time
	SinceApprox bool
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
