package miner

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

// TestSelectWorstRunningProxies pins the shed order: dead first, then confirmed
// F-tier (score 0.1 < 0.595), then ungraded (0.595), then C-tier (0.7), then
// A-tier (0.9).
func TestSelectWorstRunningProxies(t *testing.T) {
	state := map[string]ProxyEntry{
		"dead:1":   {Health: "dead"},
		"f:1":      {Health: "up", Score: 0.1, Graded: true}, // confirmed failing F-grade
		"a:1":      {Health: "up", Score: 0.9, Graded: true}, // best grade
		"c:1":      {Health: "up", Score: 0.7, Graded: true},
		"ungrad:1": {Health: "up"}, // never graded (probationary)
	}
	running := []string{"dead:1", "f:1", "a:1", "c:1", "ungrad:1"}
	// All idle (0 traffic) -> test reputation ranking
	traffic := map[string]uint64{}

	// Shed 1: dead first.
	if got := selectWorstRunningProxies(state, nil, traffic, running, 1); len(got) != 1 || got[0] != "dead:1" {
		t.Fatalf("shed 1 = %v, want [dead:1]", got)
	}
	// Shed 3: dead, then confirmed F-tier (score 0.1), then ungraded (0.595).
	got := selectWorstRunningProxies(state, nil, traffic, running, 3)
	want := []string{"dead:1", "f:1", "ungrad:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shed 3[%d] = %s, want %s (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestSelectWorstEarningProtection: an active earner (even with a low
// synthetic grade, e.g. D or F) must NEVER be shed before an idle proxy with
// 0 traffic.
func TestSelectWorstEarningProtection(t *testing.T) {
	state := map[string]ProxyEntry{
		"idle_a:1":   {Health: "up", Score: 0.95, Graded: true}, // A-tier but 0 bytes moved
		"idle_un:1":  {Health: "up"},                            // Ungraded, 0 bytes moved
		"earner_f:1": {Health: "up", Score: 0.1, Graded: true},  // F-tier synthetic grade BUT moving 50 MB
	}
	traffic := map[string]uint64{
		"idle_a:1":   0,
		"idle_un:1":  0,
		"earner_f:1": 50 * 1024 * 1024,
	}
	running := []string{"idle_a:1", "idle_un:1", "earner_f:1"}

	// Shed 2: both idle proxies must shed BEFORE the earning proxy, even though
	// idle_a:1 has an A-grade synthetic score.
	got := selectWorstRunningProxies(state, nil, traffic, running, 2)
	want := []string{"idle_un:1", "idle_a:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shed 2[%d] = %s, want %s (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestSelectWorstTrafficScale: among active earners, smaller earners shed before
// larger earners to preserve maximum operator revenue.
func TestSelectWorstTrafficScale(t *testing.T) {
	state := map[string]ProxyEntry{
		"small:1": {Health: "up", Score: 0.9, Graded: true},
		"huge:1":  {Health: "up", Score: 0.7, Graded: true},
	}
	traffic := map[string]uint64{
		"small:1": 1024,
		"huge:1":  500 * 1024 * 1024,
	}
	got := selectWorstRunningProxies(state, nil, traffic, []string{"huge:1", "small:1"}, 1)
	if got[0] != "small:1" {
		t.Fatalf("traffic scale shed = %v, want [small:1]", got)
	}
}

// TestReadWriteTrimTarget covers set / off / clear round-trip via the state
// directory override.
func TestReadWriteTrimTarget(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, "proxy_trim")

	if n, err := readTrimTarget(); err != nil || n != 0 {
		t.Fatalf("initial readTrimTarget = %d,%v; want 0", n, err)
	}
	if err := writeTrimTarget(500); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n, _ := readTrimTarget(); n != 500 {
		t.Fatalf("after set readTrimTarget = %d, want 500", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("target file missing: %v", err)
	}
	// off clears.
	if err := writeTrimTarget(0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("target file should be removed on clear, err=%v", err)
	}
}

// The status line's denominator is the number of proxies the provider will
// actually run.
func TestTrimmedConfiguredCount(t *testing.T) {
	withTempHome(t)

	if got := trimmedConfiguredCount(4127); got != 4127 {
		t.Fatalf("no cap: got %d, want 4127", got)
	}
	if err := writeTrimTarget(2000); err != nil {
		t.Fatal(err)
	}
	if got := trimmedConfiguredCount(4127); got != 2000 {
		t.Fatalf("cap 2000 over 4127 desired: got %d, want 2000", got)
	}
	if got := trimmedConfiguredCount(1500); got != 1500 {
		t.Fatalf("cap above desired must not inflate the count: got %d, want 1500", got)
	}
	if got := trimmedConfiguredCount(0); got != 0 {
		t.Fatalf("empty desired: got %d, want 0", got)
	}
	if err := writeTrimTarget(0); err != nil {
		t.Fatal(err)
	}
	if got := trimmedConfiguredCount(4127); got != 4127 {
		t.Fatalf("cleared cap: got %d, want 4127", got)
	}
}

// Operator trim commands must leave a receipt in the log: a line when a NEW
// cap is seen (received, and what it replaces) and again when it is cleared.
func TestNoteTrimCap(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)

	if prev, _, changed := noteTrimCap(0, ""); changed || prev != 0 {
		t.Fatalf("first observation of no-cap is not a change: prev=%d changed=%v", prev, changed)
	}
	if prev, _, changed := noteTrimCap(2000, trimCapOperator); !changed || prev != 0 {
		t.Fatalf("0 -> 2000: prev=%d changed=%v, want 0/true", prev, changed)
	}
	if _, _, changed := noteTrimCap(2000, trimCapOperator); changed {
		t.Fatalf("repeat of the same cap must not re-acknowledge")
	}
	if prev, _, changed := noteTrimCap(1500, trimCapOperator); !changed || prev != 2000 {
		t.Fatalf("2000 -> 1500: prev=%d changed=%v, want 2000/true", prev, changed)
	}
	if prev, prevSource, changed := noteTrimCap(0, ""); !changed || prev != 1500 || prevSource != trimCapOperator {
		t.Fatalf("1500 -> cleared: prev=%d prevSource=%q changed=%v, want 1500/operator/true", prev, prevSource, changed)
	}
}

// The cap must never read as "no cap" because a writer was mid-flight: a plain
// truncate-then-write shows readers an empty file. Concurrent writers and a
// reader must only ever see a whole number, and no temp file may be left behind.
func TestWriteTrimTargetIsAtomicUnderConcurrentReadWrite(t *testing.T) {
	home := withTempHome(t)
	if err := writeTrimTarget(100); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n, err := readTrimTarget(); err != nil || n < 100 {
				bad.Add(1)
			}
		}
	}()
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 200; i++ {
				if err := writeTrimTarget(100 + w*1000 + i); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(w)
	}
	writers.Wait()
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("a reader saw an empty, partial or failed read %d times while writers ran", bad.Load())
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// Garbage in the operator cap file still means no cap (never a false cap), but
// it is reported once instead of silently doing nothing.
func TestTrimGarbageIsReportedOnce(t *testing.T) {
	trimGarbageReset()
	t.Cleanup(trimGarbageReset)
	first := trimGarbageWarning("five hundred")
	if first == "" || !strings.Contains(first, "not a proxy count") || !strings.Contains(first, "five hundred") {
		t.Fatalf("first sighting must warn, got %q", first)
	}
	if again := trimGarbageWarning("five hundred"); again != "" {
		t.Fatalf("the same garbage must not warn again, got %q", again)
	}
	if other := trimGarbageWarning("-3"); other == "" {
		t.Fatal("different garbage must warn")
	}
	home := withTempHome(t)
	if err := os.WriteFile(filepath.Join(home, "proxy_trim"), []byte("lots"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := readTrimTarget(); err != nil || n != 0 {
		t.Fatalf("garbage must read as no cap, got %d,%v", n, err)
	}
}

// TestTrimPreviewText_ListsWorstAndReportsCounts: the preview must be computed
// with the same ranking a live trim uses.
func TestTrimPreviewText_ListsWorstAndReportsCounts(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"a:1": {Health: "up"},
		"b:1": {Health: "dead"},
		"c:1": {Health: "up"},
	}}
	got := trimPreviewText(state, nil, []string{"a:1", "b:1", "c:1"}, map[string]uint64{"a:1": 100}, 2)
	if !strings.Contains(got, "preview: 3 running; would shed 1 worst-graded to reach 2:") {
		t.Fatalf("header wrong:\n%s", got)
	}
	if !strings.Contains(got, "b:1") || strings.Contains(got, "a:1") {
		t.Fatalf("must shed the dead proxy and keep the earner:\n%s", got)
	}
	if none := trimPreviewText(state, nil, []string{"a:1"}, nil, 5); !strings.Contains(none, "running=1 <= 5, nothing to shed") {
		t.Fatalf("under-cap text wrong: %q", none)
	}
}

// TestStartupTrimSelection_HoldsWorstAndPreservesOrder: a fresh provider must
// never launch more than the cap, and the launch list must keep the desired
// order.
func TestStartupTrimSelection_HoldsWorstAndPreservesOrder(t *testing.T) {
	all := []*connect.ProxySettings{
		authSettings("a:1", "", ""),
		authSettings("b:1", "", ""),
		authSettings("c:1", "", ""),
	}
	state := map[string]ProxyEntry{
		"a:1": {Health: "up"},
		"b:1": {Health: "dead"},
		"c:1": {Health: "up"},
	}
	launch, held := startupTrimSelection(all, 2, state, nil, nil)
	if len(launch) != 2 || len(held) != 1 {
		t.Fatalf("launch=%d held=%d, want 2/1", len(launch), len(held))
	}
	if launch[0].Address != "a:1" || launch[1].Address != "c:1" {
		t.Fatalf("launch order not preserved: %v %v", launch[0].Address, launch[1].Address)
	}
	if held[0].Address != "b:1" {
		t.Fatalf("expected the dead proxy held, got %s", held[0].Address)
	}
	if launch, held := startupTrimSelection(all, 0, state, nil, nil); len(launch) != 3 || held != nil {
		t.Fatalf("cap 0 must launch everything, got %d/%v", len(launch), held)
	}
}

// TestUnreadableProxyTrimStillReports: readTrimTarget keeps its contract for the
// CLI paths that call it directly: an unreadable file (a directory at the path)
// is a real error.
func TestReadTrimTargetStillReportsUnreadableFile(t *testing.T) {
	home := withTempHome(t)
	trimPath := filepath.Join(home, "proxy_trim")
	if err := os.MkdirAll(trimPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrimTarget(); err == nil {
		t.Fatal("readTrimTarget must still report an unreadable file to its direct callers")
	}
}

// The unparseable-cap warning is written to the ramlog immediately, but its
// events.log copy is queued: readTrimTarget can run under the proxy manager's
// lock, and the startup path drains the queue after start() returns.
func TestTrimGarbageWarningIsQueuedForEventsLog(t *testing.T) {
	drainDeferredCrit()
	t.Cleanup(func() { drainDeferredCrit() })
	trimGarbageReset()
	t.Cleanup(trimGarbageReset)

	stateDir := withTempHome(t)

	if err := os.WriteFile(filepath.Join(stateDir, "proxy_trim"), []byte("five hundred"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := readTrimTarget(); err != nil || n != 0 {
		t.Fatalf("garbage must read as no cap, got %d,%v", n, err)
	}
	queued := drainDeferredCrit()
	if len(queued) != 1 || !strings.Contains(queued[0], "not a proxy count") {
		t.Fatalf("the warning must be queued for events.log, got %q", queued)
	}

	// A repeat sighting stays off the queue: that value already warned.
	if _, err := readTrimTarget(); err != nil {
		t.Fatal(err)
	}
	if queued := drainDeferredCrit(); len(queued) != 0 {
		t.Fatalf("a repeat sighting must not requeue, got %q", queued)
	}

	// The startup drain writes whatever is queued to events.log, which the
	// miner keeps in its state dir (the temp dir withTempHome pinned above).
	deferCritWrite("[proxy][trim] test queued line")
	writeDeferredCrit()
	b, err := os.ReadFile(filepath.Join(stateDir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "test queued line") {
		t.Fatalf("writeDeferredCrit must write events.log, got %q", string(b))
	}
}
