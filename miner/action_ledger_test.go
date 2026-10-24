package miner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests pin the action ledger ported from provider/action_ledger.go: the
// line format `urnet-tools autopilot log` reads, the size bound and rotation,
// the inter-process append, and the decision sites that must record (OOM cap,
// the kill switch, the thrash escalation and the launch trim hold).

var ledgerT0 = time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)

func ledgerFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ledgerFileName)
}

// Every decision leaves one machine-readable line an operator can audit later
// without grepping a rotating RAM log, in the provider's wire shape.
func TestLedgerAppendAndReadBack(t *testing.T) {
	path := ledgerFile(t)
	entries := []ledgerEntry{
		{Actor: "oomcap", Action: "reduce", From: 0, To: 3301, Mode: "on", Reason: "OOM kill since the last start (peak running 4127)"},
		{Actor: "trim", Action: "applied", From: 4127, To: 2000, Mode: "operator", Reason: "held 2127 worst-graded at launch until the cap is raised"},
	}
	for i, e := range entries {
		if err := ledgerAppend(path, e, ledgerT0.Add(time.Duration(i)*time.Minute), 1<<20); err != nil {
			t.Fatal(err)
		}
	}

	// The file is JSONL with the provider's keys, one object per line, so the
	// shared `urnet-tools autopilot log` reads it unchanged.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per entry, got %d: %q", len(lines), string(raw))
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &wire); err != nil {
		t.Fatalf("a ledger line must be one JSON object: %v (%q)", err, lines[0])
	}
	for _, key := range []string{"t", "actor", "action", "from", "to", "mode", "reason"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("wire line is missing %q: %s", key, lines[0])
		}
	}
	if wire["t"] != "2026-09-26T01:00:00Z" {
		t.Fatalf("t = %v, want the UTC RFC3339 instant recorded", wire["t"])
	}
	// from/to are numbers on the wire, because the tool renders "from -> to".
	if wire["from"] != float64(0) || wire["to"] != float64(3301) {
		t.Fatalf("from/to must be numbers, got %v/%v", wire["from"], wire["to"])
	}

	got, err := ledgerTail(path, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("tail: %v %v", got, err)
	}
	if got[0].Actor != "oomcap" || got[0].To != 3301 || got[0].Time != "2026-09-26T01:00:00Z" {
		t.Fatalf("first entry: %+v", got[0])
	}
	if got[1].Actor != "trim" || got[1].Time != "2026-09-26T01:01:00Z" || got[1].Mode != "operator" {
		t.Fatalf("second entry: %+v", got[1])
	}

	// Newest N only, in chronological order.
	last, _ := ledgerTail(path, 1)
	if len(last) != 1 || last[0].Actor != "trim" {
		t.Fatalf("tail(1): %+v", last)
	}
}

func TestLedgerIsSizeBoundedAndKeepsTheNewest(t *testing.T) {
	path := ledgerFile(t)
	const max = 4096
	for i := 0; i < 200; i++ {
		e := ledgerEntry{Actor: "trim", Action: "applied", To: i, Reason: strings.Repeat("x", 40)}
		if err := ledgerAppend(path, e, ledgerT0.Add(time.Duration(i)*time.Second), max); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() > max {
		t.Fatalf("ledger grew past its bound: size %d err %v (max %d)", st.Size(), err, max)
	}
	got, _ := ledgerTail(path, 1000)
	if len(got) == 0 || got[len(got)-1].To != 199 {
		t.Fatalf("the newest entry must survive rotation, got last=%+v", got)
	}
	// Only whole lines remain and they stay in order.
	for i := 1; i < len(got); i++ {
		if got[i].To != got[i-1].To+1 {
			t.Fatalf("rotation left a gap or reorder at %d: %d then %d", i, got[i-1].To, got[i].To)
		}
	}
}

// A HotSwap parent and candidate both record decisions into the same file.
// Concurrent appends that trigger rotation must neither fail on the shared
// temp file nor tear a line. Each writer takes its own file descriptor, so this
// exercises the inter-process flock, not just an in-process mutex.
func TestLedgerConcurrentAppendsAndRotationStayWholeAndBounded(t *testing.T) {
	path := ledgerFile(t)
	const max = 4096
	const writers, each = 8, 60
	errs := make(chan error, writers*each)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				e := ledgerEntry{Actor: "trim", Action: "applied", To: w*1000 + i, Reason: strings.Repeat("x", 40)}
				if err := ledgerAppend(path, e, ledgerT0, max); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ledgerAppend failed: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() > max {
		t.Fatalf("ledger grew past its bound: size %d err %v (max %d)", st.Size(), err, max)
	}
	b, _ := os.ReadFile(path)
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		var e ledgerEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Actor == "" {
			t.Fatalf("torn or corrupt ledger line %q", line)
		}
	}
}

func TestLedgerToleratesAMissingFileAndACorruptLine(t *testing.T) {
	path := ledgerFile(t)
	if got, err := ledgerTail(path, 5); err != nil || len(got) != 0 {
		t.Fatalf("missing file: %v %v", got, err)
	}
	if err := ledgerAppend(path, ledgerEntry{Actor: "a", Action: "x"}, ledgerT0, 1<<20); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()
	if err := ledgerAppend(path, ledgerEntry{Actor: "b", Action: "y"}, ledgerT0.Add(time.Second), 1<<20); err != nil {
		t.Fatal(err)
	}
	got, err := ledgerTail(path, 10)
	if err != nil || len(got) != 2 || got[0].Actor != "a" || got[1].Actor != "b" {
		t.Fatalf("a corrupt line must be skipped, got %+v err %v", got, err)
	}
}

// The OOM decision writes the ledger (a decision is only worth acting on if it
// can be audited), in shadow and on mode alike, and an unchanged state writes
// nothing more.
func TestOOMCapDecisionsAreLedgered(t *testing.T) {
	dir := oomCapTestEnv(t, "shadow")
	if lines := oomCapDecide(4127, "boot-A", 0, oomT0); len(lines) != 0 {
		t.Fatalf("a first clean start decides nothing, got %v", lines)
	}
	oomCapRecordStart(4127, "boot-A", 0, oomT0)
	if lines := oomCapDecide(4127, "boot-A", 1, oomT0.Add(time.Hour)); len(lines) != 1 {
		t.Fatalf("an OOM start must decide, got %v", lines)
	}

	got, err := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("want exactly one ledger entry, got %+v err %v", got, err)
	}
	e := got[0]
	if e.Actor != "oomcap" || e.Action != "reduce" || e.From != 0 || e.To != 3301 || e.Mode != "shadow" ||
		!strings.Contains(e.Reason, "peak running 4127") {
		t.Fatalf("ledger entry: %+v", e)
	}

	// The next start sees no new kill (the marker was re-recorded): the cap may
	// only relax after a clean window, and until then nothing is recorded.
	oomCapRecordStart(3301, "boot-A", 1, oomT0.Add(time.Hour))
	if lines := oomCapDecide(4127, "boot-A", 1, oomT0.Add(2*time.Hour)); len(lines) != 0 {
		t.Fatalf("no decision, no ledger line, got %v", lines)
	}
	again, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if len(again) != 1 {
		t.Fatalf("an unchanged cap must not re-ledger, got %d entries", len(again))
	}
}

// Turning the kill switch off forgets the standing cap; the audit must say so,
// with the mode that caused it.
func TestOOMCapKillSwitchOffLedgersTheClear(t *testing.T) {
	dir := oomCapTestEnv(t, "off")
	writeOOMCapState(t, oomCapState{Cap: 800})
	if lines := oomCapResetOnOff(ledgerT0); len(lines) != 1 {
		t.Fatalf("a standing cap must be cleared, got %v", lines)
	}
	got, err := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("want one clear entry, got %+v err %v", got, err)
	}
	e := got[0]
	if e.Actor != "oomcap" || e.Action != "clear" || e.From != 800 || e.To != 0 || e.Mode != "off" ||
		e.Reason != "kill switch off" {
		t.Fatalf("clear entry: %+v", e)
	}
	// The entry carries the clock the decision was taken on, not the instant
	// the write landed: a mocked decision time must survive to the file.
	if want := ledgerT0.UTC().Format(time.RFC3339); e.Time != want {
		t.Fatalf("entry time = %q, want the decision clock %q", e.Time, want)
	}

	// Nothing standing: nothing to record.
	oomCapResetOnOff(ledgerT0)
	again, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if len(again) != 1 {
		t.Fatalf("a second off switch must not re-ledger, got %d entries", len(again))
	}
}

// The launch selection is where the miner's trim cap binds, so the operator's
// cap must appear in the timeline as the decision that held proxies back - and
// only the start path may record it. A command reader recomputing the same
// selection must not append an action it did not take.
func TestLaunchTrimHoldIsLedgered(t *testing.T) {
	dir := oomCapTestEnv(t, "off")
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	m := oomCapStartupFixture(t, 5)
	launch, held, _, err := m.desired(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(launch) != 2 || len(held) != 3 {
		t.Fatalf("startup selection: launched %d, held %d, want 2/3", len(launch), len(held))
	}
	got, err := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("want one trim entry, got %+v err %v", got, err)
	}
	e := got[0]
	if e.Actor != "trim" || e.Action != "applied" || e.From != 5 || e.To != 2 || e.Mode != trimCapOperator ||
		!strings.Contains(e.Reason, "held 3") {
		t.Fatalf("trim entry: %+v", e)
	}

	// The same selection read by a command path records nothing.
	if _, _, _, err := m.desired(false); err != nil {
		t.Fatal(err)
	}
	again, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if len(again) != 1 {
		t.Fatalf("a command read must not ledger, got %d entries", len(again))
	}

	// The launch entry is this process's trim application; the first reload
	// re-reads the same cap and must not add a second entry for it.
	if _, _, _, err := m.desiredForReload(); err != nil {
		t.Fatal(err)
	}
	if again, _ := ledgerTail(filepath.Join(dir, ledgerFileName), 10); len(again) != 1 {
		t.Fatalf("a reload of the launch cap must not re-ledger, got %d entries", len(again))
	}
}

// ledgerEntries reads the ledger the current state directory holds, so a test
// can assert on the whole timeline rather than one entry.
func ledgerEntries(t *testing.T, dir string) []ledgerEntry {
	t.Helper()
	entries, err := ledgerTail(filepath.Join(dir, ledgerFileName), 50)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return entries
}

// A failed proxy_url.json read leaves every URL-sourced proxy out of the desired
// set, so the launch selection sees a partial pool. The cap still binds what is
// there, but recording that length as the baseline would understate a pool the
// operator configured (10 instead of 5,000) for the life of the file. The OOM
// decision and the start marker already refuse to record from a partial set;
// the trim application must too.
func TestLaunchTrimWithFailedURLReadRecordsNoBaseline(t *testing.T) {
	dir := oomCapTestEnv(t, "off")
	resetLiveTrimLedger()
	t.Cleanup(resetLiveTrimLedger)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	// A corrupt cache is exactly the failure a concurrent refresh can produce.
	if err := os.WriteFile(filepath.Join(dir, "proxy_url.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	m := oomCapStartupFixture(t, 5)
	launch, held, _, err := m.desired(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(launch) != 2 || len(held) != 3 {
		t.Fatalf("the cap still binds the proxies the read did see: launched %d, held %d, want 2/3", len(launch), len(held))
	}
	if got := ledgerEntries(t, dir); len(got) != 0 {
		t.Fatalf("a partial desired set must not be written as the trim baseline, got %+v", got)
	}
}

// A `proxy trim` that lands while the provider is running shrinks the pool in
// the reload path, and that is a capacity decision like the launch hold: exactly
// one entry, attributed to the live update rather than to the start, and none for
// a reload that only re-reads the standing cap.
func TestLiveTrimUpdateIsLedgeredOnce(t *testing.T) {
	dir := oomCapTestEnv(t, "off")
	resetLiveTrimLedger()
	t.Cleanup(resetLiveTrimLedger)
	m := oomCapStartupFixture(t, 5)
	// A running process has already persisted the source its start selected
	// (start() writes the state desired(true) reports), so the reload reads the
	// same proxy file the launch did.
	if err := writeProxyState(&ProxyState{Source: m.cfg.ProxyFile, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if launch, held, _, err := m.desiredForReload(); err != nil || len(launch) != 5 || len(held) != 0 {
		t.Fatalf("untrimmed reload: launched %d, held %d, err %v; want 5/0", len(launch), len(held), err)
	}
	if got := ledgerEntries(t, dir); len(got) != 0 {
		t.Fatalf("an untrimmed reload must not record, got %+v", got)
	}

	// The operator trims to 2 on the live box.
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	launch, held, _, err := m.desiredForReload()
	if err != nil {
		t.Fatal(err)
	}
	if len(launch) != 2 || len(held) != 3 {
		t.Fatalf("live trim selection: launched %d, held %d, want 2/3", len(launch), len(held))
	}
	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("a live trim must be recorded exactly once, got %+v", got)
	}
	e := got[0]
	if e.Actor != "trim" || e.Action != "applied" || e.From != 5 || e.To != 2 ||
		e.Mode != trimCapOperator+trimLiveModeSuffix || !strings.Contains(e.Reason, "held 3") {
		t.Fatalf("live trim entry: %+v", e)
	}

	// A periodic reload that re-applies the standing cap is not a new decision.
	if _, _, _, err := m.desiredForReload(); err != nil {
		t.Fatal(err)
	}
	if got := ledgerEntries(t, dir); len(got) != 1 {
		t.Fatalf("re-reading the same cap must not re-ledger, got %d entries", len(got))
	}

	// A command reader recomputing the binding selection still records nothing.
	if _, _, _, err := m.desired(false); err != nil {
		t.Fatal(err)
	}
	if got := ledgerEntries(t, dir); len(got) != 1 {
		t.Fatalf("a command read must not ledger, got %d entries", len(got))
	}

	// A tighter cap is a new decision and gets its own entry.
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.desiredForReload(); err != nil {
		t.Fatal(err)
	}
	got = ledgerEntries(t, dir)
	if len(got) != 2 || got[1].From != 5 || got[1].To != 1 || got[1].Mode != trimCapOperator+trimLiveModeSuffix {
		t.Fatalf("a tighter live cap must add one entry, got %+v", got)
	}
}

// A thrash restart is a capacity decision the operator finds out about only
// after the process is gone, so it must be in the timeline.
func TestThrashRestartIsLedgered(t *testing.T) {
	clock := swapThrashT0
	sensor := swapThrashStormSensor()
	var logs []string
	var exits []int
	watch, _ := swapThrashEscalationWatch(t, sensor, &clock, &logs, &exits)

	esc := watch.escalate(clock, swapThrashStormRates(), swapThrashStormReading(120*time.Second), "unit", 0.94, true)
	if esc.Action != "restart" {
		t.Fatalf("want a restart decision, got %+v", esc)
	}
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("want one restart entry, got %+v err %v", got, err)
	}
	e := got[0]
	if e.Actor != "thrash" || e.Action != "restart" || e.From != 500 || e.To != 300 || e.Mode != "on" ||
		!strings.Contains(e.Reason, "swap thrash") {
		t.Fatalf("restart entry: %+v", e)
	}
}

// A refusal to restart is a decision too: the operator has to be able to see
// that the box was thrashing and the watchdog held back.
func TestThrashRefusalIsLedgered(t *testing.T) {
	clock := swapThrashT0
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{
		swapThrashCalmReading(),
		swapThrashStormReading(30 * time.Second),
		swapThrashStormReading(60 * time.Second),
		swapThrashStormReading(120 * time.Second),
	}}
	var logs []string
	watch, _ := swapThrashTestWatch(t, sensor, &clock, &logs)
	// Self-heal off is the one refusal that never depends on the box: the
	// escalation gate denies the restart outright.
	watch.selfHealFn = func() bool { return false }

	watch.sampleIfDue(clock)
	clock = swapThrashT0.Add(30 * time.Second)
	watch.sampleIfDue(clock)
	clock = swapThrashT0.Add(60 * time.Second)
	watch.sampleIfDue(clock)
	clock = swapThrashT0.Add(120 * time.Second)
	watch.sampleIfDue(clock)

	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ledgerTail(filepath.Join(dir, ledgerFileName), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("want one alert entry, got %+v err %v", got, err)
	}
	e := got[0]
	if e.Actor != "thrash" || e.Action != "alert" || !strings.Contains(e.Reason, "self-heal is off") {
		t.Fatalf("alert entry: %+v", e)
	}
}

// The file the control socket serves reflects what was recorded: newest N, in
// order, as the JSON array the tool unmarshals.
func TestServedLedgerReflectsRecordedActions(t *testing.T) {
	withTempHome(t)
	for i := 0; i < 3; i++ {
		ledgerRecord(ledgerEntry{Actor: "oomcap", Action: "reduce", From: 0, To: 100 + i}, ledgerT0)
	}
	// A line without an actor is not a decision (and may be a torn write of a
	// future field): the reader drops it rather than serving it.
	ledgerRecord(ledgerEntry{Action: "orphan"}, ledgerT0)

	raw, err := minerLedgerTail(2)
	if err != nil {
		t.Fatal(err)
	}
	var got []ledgerEntry
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the served value must be the JSON entry list: %v\n%s", err, raw)
	}
	if len(got) != 2 || got[0].To != 101 || got[1].To != 102 {
		t.Fatalf("want the newest 2 in order (101,102), got %+v", got)
	}
	if strings.Contains(raw, "orphan") {
		t.Fatalf("a line without an actor must not be served: %s", raw)
	}

	// No ledger yet: an empty list, not an error.
	withTempHome(t)
	empty, err := minerLedgerTail(20)
	if err != nil || empty != "[]" {
		t.Fatalf("empty ledger must serve [], got %q err %v", empty, err)
	}
}
