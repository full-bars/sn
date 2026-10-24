package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Ported from the 3.23-fix v33 provider's proxy_health_log.go (read-only
// source candidate/v33-ops). The health report types are fork-local
// (proxyHealthReport/proxyBandwidth/proxyEvent, declared at the bottom of this
// file) because the pinned full-bars/connect does not export
// connect.ProxyHealthReport, connect.ProxyBandwidth or connect.ProxyEvent.
// Everything that formats or writes the files is byte-for-byte v33.
//
// The retention half (appendRetentionEvent/flushRetentionEvents and the
// retentionLogWriter) has no call site in the miner yet: the pinned connect has
// no SendBufferSettings.RetentionEventCallback, so the port keeps the v33
// implementation and its test for the P2 wiring that adds one.

// usageHistoryMaxBytes the accumulated usage history file before it
// rotates to <name>.1. At ~90 bytes/row and hourly snapshots, 512 MiB is
// ~60+ years of history. The cap guards against a runaway writer. NOTE: if
// the file rotates, readUsageHistory still reads the rotated .1 file, so
// LIFETIME (segment-summed over the full history, see usageLifetime)
// continues to include pre-rotation rows.
const usageHistoryMaxBytes = 512 << 20 // 512 MiB

// usageHistoryPath is the persistent JSONL history of aggregate usage
// snapshots. One JSON object per line:
//
//	{"ts":"2026-08-31T05:00:00Z","rx":...,"tx":...,"billable_rx":...,"billable_tx":...}
//
// rx/tx are the cumulative TOTAL bytes across all proxies since the process
// started; billable_* are the cumulative billable bytes. The totals can dip on
// ordinary proxy churn (a removed proxy's bytes leave the aggregate sum), not
// just on a provider restart. Lifetime is computed by segment-summing the
// history (usageLifetime); "since start" is the latest snapshot's values.
func usageHistoryPath(dir string) string {
	return filepath.Join(dir, "usage_history.jsonl")
}

// usageHistoryLastHour tracks the last hour bucket we appended, so the health
// heartbeat (which fires far more often than hourly) only wires one snapshot
// per hour. Package-level so provider restarts within the same hour don't
// double-append startup rows.
var (
	usageHistoryMu       sync.Mutex
	usageHistoryLastHour int64 // UTC hour magnitude: unix/3600
)

// writeUsageHistory appends an aggregate usage snapshot if we haven't written
// one for the current UTC hour bucket. Called from the health heartbeat with
// the freshly-built ProxyHealthReport.
func writeUsageHistory(dir string, r proxyHealthReport, now time.Time) {
	var billableRx, billableTx, totalRx, totalTx uint64
	for _, bw := range r.Bandwidth {
		billableRx += bw.BillableRx.Load()
		billableTx += bw.BillableTx.Load()
		totalRx += bw.TotalRx.Load()
		totalTx += bw.TotalTx.Load()
	}

	hour := now.Unix() / 3600
	usageHistoryMu.Lock()
	defer usageHistoryMu.Unlock()
	if usageHistoryLastHour == hour {
		return // already appended this hour
	}
	usageHistoryLastHour = hour

	row := struct {
		TS         time.Time `json:"ts"`
		RX         uint64    `json:"rx"`
		TX         uint64    `json:"tx"`
		BillableRX uint64    `json:"billable_rx"`
		BillableTX uint64    `json:"billable_tx"`
	}{
		TS: now.UTC(), RX: totalRx, TX: totalTx, BillableRX: billableRx, BillableTX: billableTx,
	}
	b, err := json.Marshal(row)
	if err != nil {
		return
	}

	path := usageHistoryPath(dir)
	rotateIfNeeded(path, usageHistoryMaxBytes)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// proxyHealthListCap bounds the stdout/combined-log detail lines. It does NOT
// apply to the persistent files, which always carry the complete list.
const proxyHealthListCap = 50

// capProxyList extracts the proxy index from items, joins them with ", ", and truncates to cap with a "(+N more)" suffix.
func capProxyList(items []string, cap int) string {
	if len(items) == 0 {
		return ""
	}
	var shortItems []string
	for _, item := range items {
		start := strings.Index(item, "[")
		end := strings.Index(item, "]")
		if start != -1 && end != -1 && end > start {
			shortItems = append(shortItems, item[start+1:end])
		} else {
			shortItems = append(shortItems, item)
		}
	}

	if len(shortItems) <= cap {
		return strings.Join(shortItems, ", ")
	}
	return strings.Join(shortItems[:cap], ", ") + fmt.Sprintf(", ... (+%d more)", len(shortItems)-cap)
}

func parseProxyString(s string) (string, string) {
	parts := strings.SplitN(s, " (", 2)
	if len(parts) == 2 {
		return parts[0], strings.TrimRight(parts[1], ")")
	}
	return s, ""
}

// formatStateFile renders the complete current-state snapshot (uncapped).
func formatStateFile(r proxyHealthReport, now time.Time) string {
	return formatStateFileWithHeartbeat(r, now, healthHeartbeatSnapshot())
}

// formatStateFileWithHeartbeat renders the status file with an explicit
// heartbeat, so the writer's output can be pinned by a test and the heartbeat
// cadence stays independent of this file's cadence.
//
// A nil heartbeat omits the line: no ` heartbeat: ` line must read as "this
// process has never pulsed", not as healthy.
func formatStateFileWithHeartbeat(r proxyHealthReport, now time.Time, hb *healthHeartbeat) string {
	var b strings.Builder
	down := len(r.Dead) + len(r.Degraded)
	fmt.Fprintf(&b, "=======================================================================\n")
	fmt.Fprintf(&b, " URNETWORK PROXY HEALTH REPORT\n")
	fmt.Fprintf(&b, " Updated: %s\n", now.UTC().Format(time.RFC3339))
	if line := healthHeartbeatStatusLine(hb); line != "" {
		b.WriteString(line)
	}
	// "Down" is the total of the two down buckets. Name them inline (dropped =
	// was up, now down; never-up = never connected) so the sum is obvious and
	// the word "degraded" is not reused for a hard gone state.
	fmt.Fprintf(&b, " Up: %d | Down: %d (dropped %d, never-up %d)\n", r.Up, down, len(r.Degraded), len(r.Dead))
	fmt.Fprintf(&b, " Lifetime Recovered: %d | Lifetime Lost: %d\n", r.LifetimeRecovered, r.LifetimeLost)
	fmt.Fprintf(&b, "=======================================================================\n")
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+------------+\n")
	fmt.Fprintf(&b, "| STATUS   | PROXY ID         | IP ADDRESS                              | AUTH ERRS  |\n")
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+------------+\n")
	for _, s := range r.Dead {
		p, ip := parseProxyString(s)
		fmt.Fprintf(&b, "| %-8s | %-16s | %-39s | %-10s |\n", "NEVER UP", p, ip, "")
	}
	for _, s := range r.Degraded {
		p, ip := parseProxyString(s)
		fmt.Fprintf(&b, "| %-8s | %-16s | %-39s | %-10s |\n", "DROPPED", p, ip, "")
	}
	fmt.Fprintf(&b, "+----------+------------------+-----------------------------------------+------------+\n")
	return b.String()
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := float64(unit), 0
	for n := float64(b) / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/div, "KMGTPE"[exp])
}

func formatAgeDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours()/24), int(d.Hours())%24)
}

// formatTrafficStateFile renders the complete traffic usage report.
func formatTrafficStateFile(r proxyHealthReport, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "======================================================================================================\n")
	fmt.Fprintf(&b, " URNETWORK PROXY TRAFFIC REPORT\n")
	fmt.Fprintf(&b, " Updated: %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "======================================================================================================\n")
	fmt.Fprintf(&b, "+------------------+-----------------------+---------+---------+-------------------+-------------------+\n")
	fmt.Fprintf(&b, "| PROXY ID         | IP ADDRESS            | CLIENTS | MAX AGE | BILLABLE (TX/RX)  | TOTAL (TX/RX)     |\n")
	fmt.Fprintf(&b, "+------------------+-----------------------+---------+---------+-------------------+-------------------+\n")

	type proxyEntry struct {
		Proxy string
		Index int
		IP    string
		Bw    *proxyBandwidth
	}
	var entries []proxyEntry
	for k, bw := range r.Bandwidth {
		p, ip := parseProxyString(k)
		var index int
		fmt.Sscanf(p, "proxy[%d]", &index)
		entries = append(entries, proxyEntry{Proxy: p, Index: index, IP: ip, Bw: bw})
	}
	sort.Slice(entries, func(i, j int) bool {
		sumI := entries[i].Bw.BillableTx.Load() + entries[i].Bw.BillableRx.Load()
		sumJ := entries[j].Bw.BillableTx.Load() + entries[j].Bw.BillableRx.Load()
		if sumI != sumJ {
			return sumI > sumJ // descending order
		}
		return entries[i].Index < entries[j].Index
	})

	for _, e := range entries {
		billableStr := fmt.Sprintf("%s / %s", formatBytes(e.Bw.BillableTx.Load()), formatBytes(e.Bw.BillableRx.Load()))
		totalStr := fmt.Sprintf("%s / %s", formatBytes(e.Bw.TotalTx.Load()), formatBytes(e.Bw.TotalRx.Load()))
		fmt.Fprintf(&b, "| %-16s | %-21s | %-7d | %-7s | %-17s | %-17s |\n", e.Proxy, e.IP, e.Bw.Clients.Load(), formatAgeDuration(e.Bw.MaxAge()), billableStr, totalStr)
	}

	return b.String()
}

// formatEventLines renders one append-line per transition (complete, uncapped).
func formatEventLines(r proxyHealthReport, now time.Time) []string {
	ts := now.UTC().Format(time.RFC3339)
	var lines []string
	for _, e := range r.Recovered {
		p := fmt.Sprintf("proxy[%d]", e.Index)
		if e.After > 0 {
			lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | after=%-7s |", ts, "RECOVERED", p, e.Address, e.After.Round(time.Second)))
		} else {
			lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | %-13s |", ts, "RECOVERED", p, e.Address, ""))
		}
	}
	for _, e := range r.NewlyDegraded {
		p := fmt.Sprintf("proxy[%d]", e.Index)
		lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | %-13s |", ts, "DROPPED", p, e.Address, ""))
	}
	for _, e := range r.NewlyDead {
		p := fmt.Sprintf("proxy[%d]", e.Index)
		lines = append(lines, fmt.Sprintf("| %s | %-9s | %-16s | %-21s | %-13s |", ts, "NEVER UP", p, e.Address, ""))
	}
	return lines
}

const proxyHealthLogMaxBytes = 20 * 1024 * 1024 // 20 MB

// proxyHealthLogMu serialises every append and rotation of proxy_health.log.
// Two independent writers touch that file: the retention writer (a long-lived
// goroutine holding an open handle) and writeProxyHealthEvents (the heartbeat).
// Without a shared lock, a rotation by one renames the file out from under the
// other's open descriptor, and the loser's subsequent writes land in the renamed
// generation; the next rotation then overwrites .1 and those events are gone.
var proxyHealthLogMu sync.Mutex

// proxyHealthDir (URNETWORK_PROXY_HEALTH_DIR, else the state directory) is the
// ported proxy_manager_commands.go helper; this file reuses it.

// writeProxyHealthState atomically rewrites the current-state snapshot file.
func writeProxyHealthState(dir string, r proxyHealthReport, now time.Time) {
	path := filepath.Join(dir, "proxy_health.state")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(formatStateFile(r, now)), 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// writeProxyTrafficState atomically rewrites the proxy traffic snapshot file.
func writeProxyTrafficState(dir string, r proxyHealthReport, now time.Time) {
	path := filepath.Join(dir, "proxy_traffic.state")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(formatTrafficStateFile(r, now)), 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// writeProxyHealthEvents appends transition lines (if any) to the event log,
// rotating first when it would exceed the size cap.
func writeProxyHealthEvents(dir string, r proxyHealthReport, now time.Time) {
	lines := formatEventLines(r, now)
	if len(lines) == 0 {
		return
	}
	proxyHealthLogMu.Lock()
	defer proxyHealthLogMu.Unlock()
	path := filepath.Join(dir, "proxy_health.log")
	rotateIfNeeded(path, proxyHealthLogMaxBytes)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strings.Join(lines, "\n") + "\n")
}

// rotateIfNeeded renames path to path.1 (replacing any prior .1) when it exceeds
// maxBytes, keeping one generation of history.
func rotateIfNeeded(path string, maxBytes int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}

// retentionEventBuffer bounds the pending retention events before the writer
// drops new entries under sustained backpressure (events are telemetry, not
// correctness-critical — dropping is preferable to blocking the transfer
// sequence goroutine that fires the callback).
const retentionEventBuffer = 256

// retentionEventCh is the buffered channel feeding the single retention-event
// writer goroutine. Created lazily on first use so CLI subcommands that never
// wire RetentionEventCallback pay nothing.
var (
	retentionEventOnce sync.Once
	retentionEventCh   chan string
	retentionEventDone chan struct{}

	// retentionEventMu guards retentionEventClosed against the race between
	// flushRetentionEvents closing retentionEventCh and a SendSequence still
	// draining on shutdown calling appendRetentionEvent concurrently — the
	// teardown drain (transfer.go Run() exit path) fires RetentionEventCallback
	// for every retained item, which is exactly the same window flush runs in.
	// Without this guard a send on the closed channel panics the process
	// during what should be a graceful shutdown.
	retentionEventMu     sync.Mutex
	retentionEventClosed bool

	// retentionEventDropped counts events dropped because the buffer was
	// full. Incremented with a non-blocking atomic op on the hot path;
	// reported from the writer goroutine (off the hot path) instead of via
	// a synchronous stderr write at drop time, which could block on a slow
	// pipe/log collector and stall the transfer sequence that called
	// appendRetentionEvent.
	retentionEventDropped atomic.Uint64

	// retentionEventRunCtx is the context of the run that owns the writer. The
	// writer is process-wide and started lazily from the event path, so it
	// cannot take a run context as an argument; the run registers its context
	// here (setRetentionEventRunContext) and the writer derives from it. Without
	// that derivation a cancelled run could not stop the writer's supervisor,
	// and the supervisor is what closes retentionEventDone: a writer panicking
	// in its backoff would leave flushRetentionEvents waiting forever.
	retentionEventRunMu        sync.Mutex
	retentionEventRunCtx       context.Context
	retentionEventWriterCancel context.CancelFunc

	// newRetentionWriter is the writer constructor. A seam for the tests that
	// exercise the shutdown contract: a panicking constructor stands in for a
	// log directory that cannot be opened.
	newRetentionWriter = func() retentionWriter { return newRetentionLogWriter() }
)

// retentionFlushTimeout and retentionFlushGrace bound how long shutdown waits
// for the writer. The flush closes the channel and waits for the final drain;
// if the writer is wedged anyway (a stuck write, a panic-and-backoff cycle) the
// wait must still end, or shutdown and the hotswap drain monitor — which exits
// the parent — hang before exit.
const (
	retentionFlushTimeout = 10 * time.Second
	retentionFlushGrace   = 5 * time.Second
)

// retentionWriter is the sink the writer loop appends batches to.
type retentionWriter interface {
	write(events []string)
	close()
}

// retentionEventBatch is how many buffered events the writer drains per file
// write. Draining one at a time cost a full open/stat/write/close cycle PER
// EVENT, so on a slow disk the single writer could not keep up with the event
// rate and the 256-slot buffer went permanently full, dropping most of the
// retention log on a large box. Batching turns draining a full buffer into one
// open and one write.
const retentionEventBatch = 64

// setRetentionEventRunContext records the context of the run that owns the
// retention writer. The writer lives as long as the process (it is started
// lazily from the event path, and its flush is terminal), so it cannot be
// handed a run context directly. The run registers its context here before it
// starts serving, and a cancelled run then always ends the writer's supervisor
// — which is what closes retentionEventDone, on the panic path too, where
// nothing else can stop the restart backoff.
func setRetentionEventRunContext(ctx context.Context) {
	retentionEventRunMu.Lock()
	defer retentionEventRunMu.Unlock()
	retentionEventRunCtx = ctx
	if ctx == nil || retentionEventWriterCancel == nil {
		// No writer yet: retentionEventWriterContext derives the writer's
		// context from the run context when it starts.
		return
	}
	// The writer started before the run registered (it is created from the
	// ingest path), so it derived from whatever was registered then. Wire this
	// run's end to it explicitly. AfterFunc runs immediately if ctx already
	// ended, which is the same outcome.
	context.AfterFunc(ctx, retentionEventWriterCancel)
}

// retentionEventRunContext returns the registered run context, or a background
// context when no run wired one (a CLI subcommand, a unit test). Background's
// Done is nil, so a waiter on it blocks until its own timeout.
func retentionEventRunContext() context.Context {
	retentionEventRunMu.Lock()
	defer retentionEventRunMu.Unlock()
	if retentionEventRunCtx == nil {
		return context.Background()
	}
	return retentionEventRunCtx
}

// retentionEventWriterContext builds the writer's context: a child of the run's,
// so the writer's loop ends when the run does.
func retentionEventWriterContext() (context.Context, context.CancelFunc) {
	retentionEventRunMu.Lock()
	defer retentionEventRunMu.Unlock()
	parent := retentionEventRunCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	retentionEventWriterCancel = cancel
	return ctx, cancel
}

// startRetentionEventWriter launches the single goroutine that performs the
// retention file I/O off the hot path. It takes the first event blocking, then
// drains whatever else is already queued without waiting, so a burst becomes
// one write while an idle stream still costs nothing.
func startRetentionEventWriter() {
	retentionEventOnce.Do(func() {
		retentionEventCh = make(chan string, retentionEventBuffer)
		retentionEventDone = make(chan struct{})
		// The writer is a long-running loop, so it runs under the supervisor: a
		// panic restarts it with backoff instead of ending retention logging
		// for the rest of the process. Its one orderly end is the channel
		// closing at shutdown, which is not a failure — that path cancels this
		// context so the supervisor exits instead of reopening the log and
		// ranging over a closed channel. The context derives from the run's, so
		// a cancelled run ends the loop even when the writer is panicking and
		// the supervisor is asleep in its backoff.
		ctx, cancel := retentionEventWriterContext()
		ref := newLoopRef("retention_event_writer")
		go func() {
			defer close(retentionEventDone)
			superviseRetentionEventWriter(ctx, ref, retentionEventCh, cancel)
		}()
	})
}

// superviseRetentionEventWriter drains events to the retention log until the
// channel closes or ctx ends. Split out of startRetentionEventWriter so the
// shutdown contract — a panicking writer must still end with the run rather
// than sit in the supervisor's backoff while flushRetentionEvents waits — is
// testable without the process-wide singleton.
func superviseRetentionEventWriter(ctx context.Context, ref loopRef, events <-chan string, cancel context.CancelFunc) {
	// The drop count is cumulative and outlives a run, so a restarted writer
	// does not report the whole total as if it were new.
	var lastReportedDropped uint64
	reportDrops := func() {
		if dropped := retentionEventDropped.Load(); dropped != lastReportedDropped {
			fmt.Fprintf(os.Stderr, "[provider] warn: retention event buffer was full, dropped %d event(s)\n", dropped-lastReportedDropped)
			lastReportedDropped = dropped
		}
	}
	superviseLoop(ctx, ref, func() {
		w := newRetentionWriter()
		defer w.close()
		retentionEventWriteLoop(ctx, events, w, reportDrops)
		// Either the channel closed (the orderly end) or the run ended.
		// Cancel in both cases: a close must end the supervisor rather than
		// restart it over a closed channel, and the cancel is idempotent.
		cancel()
	}, nil)
}

// retentionEventWriteLoop performs the file I/O: it takes the first event
// blocking, then drains whatever else is already queued without waiting, so a
// burst becomes one write while an idle stream still costs nothing. It ends
// when the channel closes or the run's context does; in both cases it writes
// what is already buffered first, so a flush that closes the channel after the
// run was cancelled still gets its final batch written.
func retentionEventWriteLoop(ctx context.Context, events <-chan string, w retentionWriter, reportDrops func()) {
	batch := make([]string, 0, retentionEventBatch)
	for {
		event, ok := nextRetentionEvent(ctx, events)
		if !ok {
			// Closed channel or an ended run: write the rest and stop.
			writeRetentionBatch(w, batch)
			reportDrops()
			return
		}
		batch = append(batch[:0], event)
		// Take whatever else is already queued, without blocking. A closed
		// channel drains to what is left and then yields ok=false; the next
		// pass through the loop sees that and exits after writing this batch.
	drain:
		for len(batch) < retentionEventBatch {
			select {
			case next, ok := <-events:
				if !ok {
					break drain
				}
				batch = append(batch, next)
			default:
				break drain
			}
		}
		writeRetentionBatch(w, batch)
		reportDrops()
		batch = batch[:0]
	}
}

// nextRetentionEvent waits for one event. ok is false when the channel closed
// or the run's context ended. An event that is already queued wins over a
// cancelled context, so a run that ends with events still buffered writes them
// instead of dropping them on the floor.
func nextRetentionEvent(ctx context.Context, events <-chan string) (event string, ok bool) {
	select {
	case event, ok = <-events:
		return event, ok
	default:
	}
	select {
	case event, ok = <-events:
		return event, ok
	case <-ctx.Done():
		return "", false
	}
}

func writeRetentionBatch(w retentionWriter, batch []string) {
	if len(batch) == 0 {
		return
	}
	w.write(batch)
}

// retentionLogWriter appends retention lines to proxy_health.log, keeping the
// file open across batches. The handle is reopened when the file is rotated:
// renaming a path that is still open would leave every later append going to
// the rotated generation, so the writer must notice the rename and reopen.
type retentionLogWriter struct {
	path   string
	f      *os.File
	bytes  int64 // size at the last check, for the rotation threshold
	opened bool
}

func newRetentionLogWriter() *retentionLogWriter { return &retentionLogWriter{} }

// write appends a batch of events as one write.
func (w *retentionLogWriter) write(events []string) {
	if len(events) == 0 {
		return
	}
	dir, ok := proxyHealthDir()
	if !ok {
		return
	}
	proxyHealthLogMu.Lock()
	defer proxyHealthLogMu.Unlock()
	// The other writer (writeProxyHealthEvents) rotates by renaming the file
	// under this lock, which leaves an open handle here pointing at the rotated
	// generation while the path string is unchanged. Compare the handle with
	// what the path names NOW, and reopen when they differ. The live size is
	// re-read at the same time, because the other writer's appends never pass
	// through w.bytes.
	if size, live := w.liveSize(filepath.Join(dir, "proxy_health.log")); live {
		w.bytes = size
	} else {
		w.reopen(dir)
		if w.f == nil {
			return
		}
	}
	// Rotate BEFORE writing, when the previous batch already filled the file.
	// Checking after the write (as an earlier version did, despite its own
	// comment) puts the batch that crosses the threshold into the generation
	// that is about to be renamed, so the newest events are the ones lost from
	// the live file.
	//
	// w.bytes is the live size as just read (or, after a reopen, seeded from the
	// file), incremented by exactly what this batch appends.
	if w.bytes >= proxyHealthLogMaxBytes {
		w.close()
		if _, err := os.Stat(w.path); err == nil {
			_ = os.Rename(w.path, w.path+".1")
		}
		w.bytes = 0
		if w.f == nil {
			w.reopen(filepath.Dir(w.path))
			if w.f == nil {
				return
			}
		}
	}
	var b strings.Builder
	ts := time.Now().UTC().Format(time.RFC3339)
	for _, e := range events {
		fmt.Fprintf(&b, "| %s | %-9s | %-16s | %-21s | %s |\n", ts, "RETAIN", "-", "", e)
	}
	n, err := w.f.WriteString(b.String())
	if err != nil {
		// The handle went bad (rotation by the OTHER writer, truncation, disk).
		// Drop it so the next batch reopens rather than writing into a dead
		// file forever.
		w.close()
		return
	}
	w.bytes += int64(n)
}

// liveSize reports whether the open handle is still the file path names, and
// that file's current size. It is false when there is no handle, the handle was
// opened for a different path, the path no longer exists, or the path now names
// a different file (the other writer rotated it). The caller holds
// proxyHealthLogMu.
func (w *retentionLogWriter) liveSize(path string) (int64, bool) {
	if w.f == nil || w.path != path {
		return 0, false
	}
	handle, err := w.f.Stat()
	if err != nil {
		return 0, false
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(handle, current) {
		return 0, false
	}
	return current.Size(), true
}

func (w *retentionLogWriter) reopen(dir string) {
	w.close()
	w.path = filepath.Join(dir, "proxy_health.log")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	w.f = f
	w.opened = true
	if info, err := f.Stat(); err == nil {
		w.bytes = info.Size()
	} else {
		w.bytes = 0
	}
}

func (w *retentionLogWriter) close() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	w.opened = false
}

// appendRetentionEvent buffers a retention telemetry line and returns
// immediately; a single writer goroutine performs the file I/O. When the
// buffer is full the event is dropped (with a stderr note) rather than
// blocking the caller, which runs on the transfer sequence hot path.
func appendRetentionEvent(event string) {
	startRetentionEventWriter()
	retentionEventMu.Lock()
	defer retentionEventMu.Unlock()
	if retentionEventClosed {
		// Lost the race with shutdown's flush; the writer goroutine has
		// already been told to drain and exit. Telemetry, not correctness —
		// drop rather than reopen a closed channel.
		return
	}
	select {
	case retentionEventCh <- event:
	default:
		// Non-blocking: never write to stderr synchronously here. This runs
		// on the transfer sequence hot path (RetentionEventCallback), and a
		// blocked stderr write (slow pipe/log collector) would stall
		// transfer sequences under sustained retention-event backpressure.
		// The writer goroutine reports the drop count once it next runs.
		retentionEventDropped.Add(1)
	}
}

// flushRetentionEvents drains all buffered events and waits for the writer
// goroutine to finish, so no pending entries are lost at shutdown. Safe to
// call multiple times (subsequent calls are no-ops) and safe to race against
// concurrent appendRetentionEvent calls (retentionEventMu makes the close
// and the closed-check atomic with respect to each other). Terminal: after
// this call, appendRetentionEvent silently drops all events.
//
// The wait is bounded. The writer's context derives from the run's, so a
// cancelled run ends the writer's supervisor; a writer that is wedged anyway (a
// stuck write, a panicking constructor that keeps the supervisor in backoff)
// must not hold shutdown — or the hotswap drain monitor, which owns the
// parent's exit — past the timeout.
func flushRetentionEvents() {
	startRetentionEventWriter()
	retentionEventMu.Lock()
	if !retentionEventClosed {
		retentionEventClosed = true
		close(retentionEventCh)
	}
	done := retentionEventDone
	retentionEventMu.Unlock()
	if !waitRetentionWriterDone(done, retentionEventRunContext(), retentionFlushTimeout, retentionFlushGrace) {
		tlog("[retention] event writer did not finish within %s; buffered events may be lost\n", retentionFlushTimeout)
	}
}

// waitRetentionWriterDone waits for the writer to close done, bounded by
// timeout. runCtx is the run whose cancel also stops the writer: once it has
// ended, the writer is already being stopped, so it only gets grace to finish
// the batch in hand rather than the full timeout.
func waitRetentionWriterDone(done <-chan struct{}, runCtx context.Context, timeout, grace time.Duration) bool {
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()
	select {
	case <-done:
		return true
	case <-timeoutTimer.C:
		return false
	case <-runCtx.Done():
		graceTimer := time.NewTimer(grace)
		defer graceTimer.Stop()
		select {
		case <-done:
			return true
		case <-timeoutTimer.C:
			return false
		case <-graceTimer.C:
			return false
		}
	}
}

// --- fork-local health report types (the pinned connect does not export the
// v33 connect.ProxyHealthReport / ProxyBandwidth / ProxyEvent) ---

// proxyEvent is one health transition: the proxy's index and display address,
// plus how long it was down for a recovery.
type proxyEvent struct {
	Index   int
	Address string
	After   time.Duration
}

// proxyHealthReport is the full per-heartbeat health result, shaped exactly
// like v33's connect.ProxyHealthReport.
type proxyHealthReport struct {
	Up       int
	Dead     []string // formatted "proxy[idx] (addr)", index-sorted, complete (uncapped)
	Degraded []string

	Recovered     []proxyEvent // down->up since the last heartbeat
	NewlyDegraded []proxyEvent // up->down since the last heartbeat
	NewlyDead     []proxyEvent // never-up proxies newly confirmed dead (logged once)

	LifetimeRecovered int
	LifetimeLost      int

	Bandwidth map[string]*proxyBandwidth
}

// proxyBandwidth is the per-proxy traffic snapshot the traffic report renders.
// The live counters are the identity-keyed *bandwidth.ProxyBandwidth the
// provide loop publishes (proxyEarnBandwidthRegistry,
// proxy_manager_earnings_loop.go), and proxy audit's client count reads them
// directly (proxyAuditClients). This fork-local shape exists only because the
// pinned connect does not export the v33 connect.ProxyHealthReport /
// ProxyBandwidth types; nothing fills the report's Bandwidth map yet, so the
// traffic report the miner writes is still empty.
type proxyBandwidth struct {
	TotalRx, TotalTx, BillableRx, BillableTx atomic.Uint64
	Clients                                  atomic.Int64
	LatencyNs                                atomic.Int64
	SocksLatencyNs                           atomic.Int64

	mu           sync.Mutex
	presenceFrom time.Time
}

// AddPresence marks a client session as active at start, extending the
// presence window the traffic report's MAX AGE column shows.
func (self *proxyBandwidth) AddPresence(start time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.presenceFrom.IsZero() {
		self.presenceFrom = start
	}
}

// MaxAge is how long the current continuous client presence window has run,
// or 0 when no client has been seen (the renderer prints "-" for 0).
func (self *proxyBandwidth) MaxAge() time.Duration {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.presenceFrom.IsZero() {
		return 0
	}
	return time.Since(self.presenceFrom)
}
