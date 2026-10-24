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
//   - metricsSeamDiskBench: disk_bench_metrics.go (the startup disk audit).
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

	// UNSUPPORTED: v33 tracks contract outcomes per proxy inside connect. The
	// pinned fork exports no such counter and the miner keeps no equivalent
	// (baseline_sampler.go omits contract counts for the same reason). The
	// all-time totals in lifetime_metrics.go are not a substitute: reporting
	// them as urnet_contracts_total would present a persisted lifetime figure
	// as this process's counter.
	metricsSeamContracts func() (acquired, denied int64, ok bool)

	// Wired by lifetime_metrics.go's init: the persisted all-time store.
	metricsSeamLifetime func() *seamLifetime

	// UNSUPPORTED: there is no persistent per-category error store in the
	// miner; lifetime_metrics.go keeps session, contract, proxy-transition and
	// billable-byte totals only.
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

// seamLifetime is one persisted all-time reading.
type seamLifetime struct {
	SessionsPQE, SessionsClas          uint64
	ContractsAcquired, ContractsDenied uint64
	ProxiesRecovered, ProxiesLost      uint64
	BillableBytes                      uint64
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
