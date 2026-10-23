package miner

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// baseline_sampler.go ports the sampling half of the 3.23-fix v33 baseline
// recorder (provider/baseline.go). The operator-mark half was already here
// (baseline_mark.go), so a mark used to be the only line the miner wrote and an
// upgrade had no before-and-after. This file adds the recorder: a start mark at
// launch and one bounded sample per baselineInterval, in the same JSONL shape
// the unmodified `urnet-tools baseline show|compare` reads.
//
// The rules that matter, in order:
//
//   - An unknown field is OMITTED, never written as zero. A zero reads as
//     "measured, and the answer was nothing", which for host memory or PSI
//     would be a catastrophic misreading of a perfectly healthy box.
//   - Recording never blocks or fails the thing it observes. Write errors are
//     swallowed, except for one rate-limited line so a broken disk is visible
//     rather than silent.
//   - No proxy addresses, usernames or passwords ever reach the file: it holds
//     counts and totals, nothing that identifies a proxy or a customer.
//
// Fields the miner has no source for yet are omitted rather than stood in for:
// the error taxonomy and contract counts have no miner counter, and the OOM
// kill epoch arrives with the oom_cap port. desired and trim-cap are omitted
// until the manager surfaces that own them expose a value.

const (
	// baselineInterval is the steady-state cadence. baselineFirstSampleDelay is
	// longer than nothing because a sample at t=0 measures a pool that has not
	// launched yet, and those first minutes are exactly the ramp a compare
	// excludes from both sides.
	baselineInterval         = 15 * time.Minute
	baselineFirstSampleDelay = 5 * time.Minute

	// baselineMinInterval is the floor on the env override, so a typo or a
	// canary run cannot turn the recorder into a busy loop.
	baselineMinInterval = time.Minute

	// baselineErrorTopN caps the error taxonomy in one sample. It stays unused
	// until the miner has an error counter; keeping it here means the shape
	// does not drift when that lands.
	baselineErrorTopN = 8
)

// baselineSample is one line of the file. Every optional field is a pointer or
// carries omitempty, so "unknown" and "zero" stay distinguishable on disk.
type baselineSample struct {
	V               int    `json:"v"`
	Kind            string `json:"kind"` // start | sample | mark
	Label           string `json:"label,omitempty"`
	TS              string `json:"ts"`
	Version         string `json:"version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	UptimeSeconds   *int64 `json:"uptime_seconds,omitempty"`

	State       string `json:"state,omitempty"`
	StateReason string `json:"state_reason,omitempty"`

	Proxies baselineProxies `json:"proxies"`
	// Desired is the configured pool size, and TrimCap the effective cap with
	// its source, so a later comparison can see that capacity changed rather
	// than reading a trimmed box as a regression.
	Desired       *int   `json:"desired,omitempty"`
	TrimCap       *int   `json:"trim_cap,omitempty"`
	TrimCapSource string `json:"trim_cap_source,omitempty"`

	Clients  int64            `json:"clients"`
	Sessions baselineSessions `json:"sessions"`
	Rate     baselineRate     `json:"rate"`

	Traffic   *baselineTraffic   `json:"traffic,omitempty"`
	Contracts *baselineContracts `json:"contracts,omitempty"`
	Pressure  *float64           `json:"pressure,omitempty"`

	Resources baselineResources `json:"resources"`
	Host      *baselineHost     `json:"host,omitempty"`
	// GC and Net are observation-only signals (metrics_resources.go). Pointers
	// so a box that could read neither writes no block rather than an empty one.
	GC  *baselineGC  `json:"gc,omitempty"`
	Net *baselineNet `json:"net,omitempty"`

	// Pointers because encoding/json's omitempty does nothing for a struct
	// value: a plain struct would serialise as {} even when nothing was known.
	OOM     *baselineOOM     `json:"oom,omitempty"`
	Restart *baselineRestart `json:"restart,omitempty"`
	Errors  []baselineError  `json:"errors,omitempty"`
}

type baselineProxies struct {
	Up         int `json:"up"`
	Degraded   int `json:"degraded"`
	Connecting int `json:"connecting"`
	Dead       int `json:"dead"`
}

type baselineSessions struct {
	PQE       int64 `json:"pqe"`
	Classical int64 `json:"classical"`
}

type baselineRate struct {
	Avg5mBps *int64 `json:"avg5m_bps,omitempty"`
}

type baselineTraffic struct {
	BillableBytes         uint64  `json:"billable_bytes"`
	TotalBytes            uint64  `json:"total_bytes"`
	LifetimeBillableBytes *uint64 `json:"lifetime_billable_bytes,omitempty"`
}

type baselineContracts struct {
	Acquired int64 `json:"acquired"`
	Denied   int64 `json:"denied"`
}

type baselineResources struct {
	RSSBytes       *uint64 `json:"rss_bytes,omitempty"`
	HeapInuseBytes *uint64 `json:"heap_inuse_bytes,omitempty"`
	MemLimitBytes  *uint64 `json:"mem_limit_bytes,omitempty"`
	Goroutines     *int    `json:"goroutines,omitempty"`
	OpenFDs        *uint64 `json:"open_fds,omitempty"`
	FDLimit        *uint64 `json:"fd_limit,omitempty"`
}

// baselineHost is the host-level view. MemAvailableMiB is the TIGHTER of host
// and cgroup availability: in Docker /proc/meminfo shows the host's free RAM,
// so a container near its limit would otherwise read healthy. AvailSource says
// which reader supplied it, so a comparison across a systemd box and a Docker
// box is not read as like-for-like.
type baselineHost struct {
	MemAvailableMiB *int64   `json:"mem_available_mib,omitempty"`
	AvailSource     string   `json:"avail_source,omitempty"`
	SwapUsedMiB     *int64   `json:"swap_used_mib,omitempty"`
	PSISomeAvg60    *float64 `json:"psi_some_avg60,omitempty"`
	PSIFullAvg60    *float64 `json:"psi_full_avg60,omitempty"`
	PSIFullAvg300   *float64 `json:"psi_full_avg300,omitempty"`
	Load1           *float64 `json:"load1,omitempty"`
}

// baselineOOM is the OOM-kill counter for the scope it was read from. Scope
// matters: the parent cgroup's subtree counter also covers other services.
type baselineOOM struct {
	Kills int64  `json:"kills"`
	Scope string `json:"scope,omitempty"`
}

type baselineRestart struct {
	Reason        string `json:"reason,omitempty"`
	CleanShutdown bool   `json:"clean_shutdown"`
}

type baselineError struct {
	Category string `json:"category"`
	Count    uint64 `json:"count"`
}

// baselineInputs is everything the sample builder needs, passed in rather than
// read from the process, so the builder is a pure function and its tests need
// no clock, no goroutines and no fixtures on disk.
type baselineInputs struct {
	now  time.Time
	snap *nodeSnapshot

	host      baselineHost
	hostKnown bool
	hostErr   error

	desired int
	trimCap int
	trimSrc string

	oom baselineOOM

	gc  *baselineGC
	net *baselineNet

	contracts [2]int64 // acquired, denied
	errs      map[string]uint64
	errsKnown bool
}

// buildBaselineSample turns its inputs into one line. It reads no clock and
// touches no file, which is what makes the omission rules testable.
func buildBaselineSample(in baselineInputs) baselineSample {
	s := baselineSample{V: 1, Kind: "sample", TS: in.now.UTC().Format(time.RFC3339)}
	if in.snap == nil {
		return s
	}
	snap := in.snap
	s.Version = snap.Version
	s.PreviousVersion = snap.PreviousVersion
	if !snap.StartedAt.IsZero() {
		s.StartedAt = snap.StartedAt.UTC().Format(time.RFC3339)
	}
	if snap.UptimeSeconds != 0 {
		u := int64(snap.UptimeSeconds)
		s.UptimeSeconds = &u
	}
	s.State = snap.State
	s.StateReason = snap.StateReason
	s.Proxies = baselineProxies{
		Up:         snap.Proxies.Up,
		Degraded:   snap.Proxies.Degraded,
		Connecting: snap.Proxies.Connecting,
		Dead:       snap.Proxies.Dead,
	}
	s.Clients = snap.Clients
	s.Sessions = baselineSessions{PQE: snap.Sessions.PQE, Classical: snap.Sessions.Classical}
	if snap.Rate.Avg5mBps != 0 {
		r := int64(snap.Rate.Avg5mBps)
		s.Rate.Avg5mBps = &r
	}
	if snap.Traffic != nil {
		t := &baselineTraffic{
			BillableBytes: snap.Traffic.BillableBytes,
			TotalBytes:    snap.Traffic.TotalBytes,
		}
		if snap.Traffic.LifetimeBillableBytes != nil {
			l := *snap.Traffic.LifetimeBillableBytes
			t.LifetimeBillableBytes = &l
		}
		s.Traffic = t
	}
	if in.contracts != [2]int64{} {
		s.Contracts = &baselineContracts{Acquired: in.contracts[0], Denied: in.contracts[1]}
	}
	if snap.Pressure != 0 {
		p := snap.Pressure
		s.Pressure = &p
	}
	// Resources: a zero that was actually measured is kept, an unknown is not
	// set at all. The snapshot's optional fields already distinguish the two.
	s.Resources = baselineResources{
		Goroutines:     intPtrOrNil(snap.Resources.Goroutines, snap.Resources.Goroutines != 0),
		HeapInuseBytes: uint64PtrOrNil(snap.Resources.HeapInuseBytes, snap.Resources.HeapInuseBytes != 0),
	}
	if snap.Resources.RSSBytes != nil && *snap.Resources.RSSBytes != 0 {
		v := *snap.Resources.RSSBytes
		s.Resources.RSSBytes = &v
	}
	if snap.Resources.MemLimitBytes != nil && *snap.Resources.MemLimitBytes != 0 {
		v := *snap.Resources.MemLimitBytes
		s.Resources.MemLimitBytes = &v
	}
	if snap.Resources.OpenFDs != nil && *snap.Resources.OpenFDs > 0 {
		v := uint64(*snap.Resources.OpenFDs)
		s.Resources.OpenFDs = &v
	}
	if snap.Resources.FDLimit != nil && *snap.Resources.FDLimit > 0 {
		v := uint64(*snap.Resources.FDLimit)
		s.Resources.FDLimit = &v
	}
	if in.hostKnown {
		h := in.host
		s.Host = &h
	}
	if in.oom.Kills != 0 || in.oom.Scope != "" {
		o := in.oom
		s.OOM = &o
	}
	s.GC = in.gc
	s.Net = in.net
	// Restart always describes this start, so it is known even when the reason
	// is empty: clean_shutdown is the load-bearing field.
	s.Restart = &baselineRestart{Reason: snap.Restart.Reason, CleanShutdown: snap.Restart.CleanShutdown}
	if in.errsKnown {
		s.Errors = topBaselineErrors(in.errs, baselineErrorTopN)
	}
	if in.desired != 0 {
		d := in.desired
		s.Desired = &d
	}
	if in.trimCap != 0 {
		c := in.trimCap
		s.TrimCap = &c
		s.TrimCapSource = in.trimSrc
	}
	return s
}

func intPtrOrNil(v int, known bool) *int {
	if !known {
		return nil
	}
	return &v
}

func uint64PtrOrNil(v uint64, known bool) *uint64 {
	if !known {
		return nil
	}
	return &v
}

// topBaselineErrors keeps the N largest counts, descending, so the file cannot
// grow with the error taxonomy and the most useful rows survive the trim.
func topBaselineErrors(m map[string]uint64, n int) []baselineError {
	if len(m) == 0 {
		return nil
	}
	out := make([]baselineError, 0, len(m))
	for k, v := range m {
		out = append(out, baselineError{Category: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		// A stable tie-break keeps the output deterministic across runs, which
		// matters for a diffable file.
		return out[i].Category < out[j].Category
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// baselineStartSample is written at launch and is what compare finds an
// upgrade by.
func baselineStartSample(version, previous string, now time.Time) baselineSample {
	return baselineSample{
		V:               1,
		Kind:            "start",
		TS:              now.UTC().Format(time.RFC3339),
		Version:         version,
		PreviousVersion: previous,
	}
}

// resolveBaselineInterval reads the env override with a floor, so a canary can
// sample faster but nothing can turn the recorder into a busy loop.
func resolveBaselineInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("URNETWORK_BASELINE_INTERVAL"))
	if v == "" {
		return baselineInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Unparseable falls back to the default rather than the floor: a typo
		// must not silently turn a 15-minute recorder into a 1-minute one.
		return baselineInterval
	}
	if d < baselineMinInterval {
		return baselineMinInterval
	}
	return d
}

// baselineEnabled is the recorder switch, on by default: an upgrade baseline is
// the point, so it must not depend on anyone remembering to configure it. The
// key lives in the control socket (controlLiveEffectKeys).
var baselineEnabled atomic.Bool

func init() { baselineEnabled.Store(true) }

func baselineIsEnabled() bool { return baselineEnabled.Load() }

// baselineWarnLast is the rate limiter for the "cannot write" line. Recording
// must never fail the thing it records, so the error is swallowed, but a
// silently broken recorder is worse than a loud one: an operator comparing an
// upgrade would find an empty file and blame the upgrade.
var baselineWarnLast atomic.Int64

// baselineRecord appends one sample through the same bounded, locked writer the
// operator marks use.
func baselineRecord(e baselineSample) {
	if !baselineIsEnabled() {
		return
	}
	path, err := baselinePath()
	if err != nil {
		baselineWarnWrite(err)
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		baselineWarnWrite(err)
		return
	}
	if err := appendBoundedLine(path, line, baselineMaxBytes); err != nil {
		baselineWarnWrite(err)
	}
}

// baselineWarnWrite logs at most one line per hour. A full disk would otherwise
// produce one warning per sample forever.
func baselineWarnWrite(err error) {
	now := time.Now().Unix()
	last := baselineWarnLast.Load()
	if now-last < 3600 {
		return
	}
	if !baselineWarnLast.CompareAndSwap(last, now) {
		return
	}
	importantLogf("[baseline] cannot write %s: %v; recording is off until this clears\n",
		baselineFileName, err)
}

// baselineStart writes the start mark and launches the sampler. The sampler is
// launched even when the key is off: it checks the flag on every tick, so
// `set baseline on` takes effect live. Returns a channel closed when it exits.
func baselineStart(ctx context.Context, version, previous string) <-chan struct{} {
	// baselineRecord is a no-op while the key is off, so no start mark is
	// written in that case.
	baselineRecord(baselineStartSample(version, previous, time.Now()))
	done := make(chan struct{})
	go baselineRun(ctx, done)
	return done
}

// baselineRun samples until ctx is cancelled. The first sample is delayed, so a
// sample at t=0 does not measure a pool that has not launched yet.
func baselineRun(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	interval := resolveBaselineInterval()
	first := time.NewTimer(baselineFirstSampleDelay)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	baselineRecord(buildBaselineSample(baselineCollect(time.Now())))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			baselineRecord(buildBaselineSample(baselineCollect(t)))
		}
	}
}

// baselineCollect gathers one sample's worth of inputs. Every host reader is
// best-effort: a reader that fails omits its field rather than zeroing it.
func baselineCollect(now time.Time) baselineInputs {
	snap, err := buildMinerSnapshot()
	if err != nil {
		snap = nil
	}
	in := baselineInputs{now: now, snap: snap}

	host, herr := readBaselineHost()
	in.host, in.hostKnown, in.hostErr = host, herr == nil, herr

	in.gc, in.net = readBaselineGC(now), readBaselineNet()
	return in
}

// currentVersionForBaseline is the running build, with the same "dev" fallback
// the audit ring uses so a build without -ldflags still records something
// readable.
func currentVersionForBaseline() string {
	if v := RequireVersion(); v != "" {
		return v
	}
	return "dev"
}

// previousVersionForBaseline is the version this process replaced, read through
// the same restart marker the node snapshot uses. Empty on a first start, which
// is correct: there was no previous version.
func previousVersionForBaseline() string {
	return readMinerRestartState().PreviousVersion
}
