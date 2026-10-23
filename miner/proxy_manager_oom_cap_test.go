package miner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// These tests pin the OOM-aware start cap ported from provider/oom_cap.go
// (proxy_manager_oom_cap.go): the pure decision logic, the persisted marker and
// state, the cgroup-aware counter, the effective-cap precedence the miner's
// trim surface now applies, and the oom_cap control key.

var oomT0 = time.Unix(1_800_000_000, 0)

// oomCapTestEnv isolates one OOM-cap test: a fresh state directory
// (URNETWORK_STATE_DIR through withTempHome), the given URNETWORK_OOM_CAP value,
// a fresh control state (oomCapMode reads it before the env fallback, and a
// previous test's persisted value would win otherwise) and the package-level
// state the cap keeps.
func oomCapTestEnv(t *testing.T, envMode string) string {
	t.Helper()
	dir := withTempHome(t)
	t.Setenv("URNETWORK_OOM_CAP", envMode)
	saved := globalControlState
	globalControlState = newControlState()
	t.Cleanup(func() { globalControlState = saved })
	trimUnreadableReset()
	t.Cleanup(trimUnreadableReset)
	resetOOMCapStartupForTest()
	t.Cleanup(resetOOMCapStartupForTest)
	setOOMCapRunningProxies(0)
	t.Cleanup(func() { setOOMCapRunningProxies(0) })
	drainDeferredCrit()
	t.Cleanup(func() { drainDeferredCrit() })
	return dir
}

// writeOOMCapState persists a standing automatic cap in the test's state
// directory.
func writeOOMCapState(t *testing.T, st oomCapState) {
	t.Helper()
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := oomWriteJSON(filepath.Join(dir, "oom_cap.json"), st); err != nil {
		t.Fatal(err)
	}
}

func writeMemoryEvents(t *testing.T, root, rel string, oomKill int) {
	t.Helper()
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "low 0\nhigh 0\nmax 0\noom 0\noom_kill " + strconv.Itoa(oomKill) + "\noom_group_kill 0\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The mode grammar is the one the control socket validates and urnet-tools
// sends: on/off/shadow plus the legacy boolean spellings, and the canonical
// name for the log is parseOOMCapMode's inverse.
func TestParseOOMCapModeAndModeName(t *testing.T) {
	cases := []struct {
		value string
		want  oomCapModeKind
		ok    bool
	}{
		{"", oomCapShadow, false},
		{"shadow", oomCapShadow, true},
		{"SHADOW", oomCapShadow, true},
		{" shadow ", oomCapShadow, true},
		{"off", oomCapOff, true},
		{"0", oomCapOff, true},
		{"false", oomCapOff, true},
		{"no", oomCapOff, true},
		{"on", oomCapOn, true},
		{"1", oomCapOn, true},
		{"true", oomCapOn, true},
		{"yes", oomCapOn, true},
		{"garbage", oomCapShadow, false},
		{"enforce", oomCapShadow, false},
	}
	for _, c := range cases {
		got, ok := parseOOMCapMode(c.value)
		if got != c.want || ok != c.ok {
			t.Errorf("parseOOMCapMode(%q) = %v,%v; want %v,%v", c.value, got, ok, c.want, c.ok)
		}
	}
	for mode, want := range map[oomCapModeKind]string{
		oomCapShadow: "shadow",
		oomCapOn:     "on",
		oomCapOff:    "off",
	} {
		if got := oomCapModeName(mode); got != want {
			t.Errorf("oomCapModeName(%v) = %q, want %q", mode, got, want)
		}
	}
}

// Precedence: ANY source saying off wins (a kill switch must always work, even
// against an env var that says on); otherwise ANY on wins; otherwise shadow.
func TestOOMCapModeCombinesEnvAndControl(t *testing.T) {
	cases := []struct {
		env, control string
		want         oomCapModeKind
	}{
		{"", "", oomCapShadow},
		{"on", "", oomCapOn},
		{"", "on", oomCapOn},
		{"on", "off", oomCapOff}, // control kill switch beats an env "on"
		{"off", "on", oomCapOff}, // env off beats a control "on"
		{"shadow", "on", oomCapOn},
		{"on", "shadow", oomCapOn},
		{"", "off", oomCapOff},
		{"shadow", "shadow", oomCapShadow},
	}
	for _, c := range cases {
		oomCapTestEnv(t, c.env)
		if c.control != "" {
			if err := globalControlState.set("oom_cap", c.control); err != nil {
				t.Fatal(err)
			}
		}
		if got := oomCapMode(); got != c.want {
			t.Errorf("env=%q control=%q: got %v, want %v", c.env, c.control, got, c.want)
		}
	}
}

func TestRecentReductionsKeepsTheWindow(t *testing.T) {
	old := oomT0.Add(-oomCapReductionWin - time.Minute).Unix()
	inside := oomT0.Add(-time.Hour).Unix()
	got := recentReductions([]int64{old, inside, oomT0.Unix()}, oomT0)
	if len(got) != 2 || got[0] != inside || got[1] != oomT0.Unix() {
		t.Fatalf("recentReductions = %v, want the two inside the %v window", got, oomCapReductionWin)
	}
	if out := recentReductions(nil, oomT0); len(out) != 0 {
		t.Fatalf("no history must stay empty, got %v", out)
	}
}

// The OOM decision matrix: reduce to 80% of what died, never below the floor,
// never grow a small pool, freeze after the per-window budget, and restart the
// clean-day clock on every OOM (including the ones that change nothing).
func TestOOMCapOnOOMDecisionMatrix(t *testing.T) {
	t.Run("eighty percent of what died, then step down from the cap", func(t *testing.T) {
		st, d := oomCapOnOOM(oomCapState{}, 4127, 4127, oomT0)
		if d.Action != "reduce" || d.To != 3301 || st.Cap != 3301 { // int(4127*0.8)
			t.Fatalf("decision %+v state %+v, want reduce to 3301", d, st)
		}
		st, d = oomCapOnOOM(st, 3301, 4127, oomT0.Add(time.Hour))
		if d.Action != "reduce" || d.To != 2640 {
			t.Fatalf("second OOM: %+v, want reduce to 2640", d)
		}
	})

	t.Run("never below the floor", func(t *testing.T) {
		// floor = max(50, desired/4) = 1000 for a 4000-proxy list
		st, d := oomCapOnOOM(oomCapState{}, 1100, 4000, oomT0)
		if d.To != 1000 || st.Cap != 1000 {
			t.Fatalf("got %+v, want the 1000 floor", d)
		}
		_, d = oomCapOnOOM(st, 1000, 4000, oomT0.Add(time.Hour))
		if d.Action != "none" {
			t.Fatalf("at the floor a further OOM must not reduce, got %+v", d)
		}
		// The at-floor OOM still restarts the clean-day clock: the box just
		// died, so a start more than 24h after the original reduction must not
		// relax.
		st, _ = oomCapOnOOM(oomCapState{Cap: 1000, SinceUnix: oomT0.Unix()}, 500, 4000, oomT0.Add(time.Hour))
		if st.SinceUnix != oomT0.Add(time.Hour).Unix() {
			t.Fatalf("an at-floor OOM must reset SinceUnix, got %d (want %d)", st.SinceUnix, oomT0.Add(time.Hour).Unix())
		}
		// Tiny list: the absolute floor of 50 applies.
		_, d = oomCapOnOOM(oomCapState{}, 60, 100, oomT0)
		if d.To != 50 {
			t.Fatalf("absolute floor: %+v, want 50", d)
		}
	})

	// On a small desired pool the floor can sit AT OR ABOVE what actually died:
	// desired=63 gives floor=50, and a 20-proxy death would otherwise get
	// "reduce 0 -> 50", a cap larger than the pool that just got OOM-killed.
	t.Run("no-op when the floor is not tighter than what died", func(t *testing.T) {
		st, d := oomCapOnOOM(oomCapState{}, 20, 63, oomT0)
		if d.Action != "none" || st.Cap != 0 {
			t.Fatalf("floor (50) >= proxiesAtDeath (20): got %+v state %+v, want a no-op", d, st)
		}
		if st.SinceUnix != oomT0.Unix() {
			t.Fatalf("a no-op OOM must still reset SinceUnix, got %d want %d", st.SinceUnix, oomT0.Unix())
		}
	})

	t.Run("freeze after three reductions in the window", func(t *testing.T) {
		st := oomCapState{}
		var d oomCapDecision
		proxies := 4000
		for i := 0; i < 3; i++ {
			st, d = oomCapOnOOM(st, proxies, 4000, oomT0.Add(time.Duration(i)*time.Hour))
			if d.Action != "reduce" {
				t.Fatalf("reduction %d: %+v", i, d)
			}
			proxies = st.Cap
		}
		st, d = oomCapOnOOM(st, proxies, 4000, oomT0.Add(4*time.Hour))
		if d.Action != "frozen" || st.Cap != d.From {
			t.Fatalf("fourth OOM inside the window must freeze, got %+v", d)
		}
		if st.SinceUnix != oomT0.Add(4*time.Hour).Unix() {
			t.Fatalf("a frozen OOM must reset SinceUnix, got %d (want %d)", st.SinceUnix, oomT0.Add(4*time.Hour).Unix())
		}
		// Outside the 24h window the budget is available again.
		_, d = oomCapOnOOM(st, proxies, 4000, oomT0.Add(30*time.Hour))
		if d.Action == "frozen" {
			t.Fatalf("after the window the budget must recover, got %+v", d)
		}
	})
}

// The clean-start matrix: nothing before a full clean window, +10% after it,
// a full further window before relaxing again, and a clear once the cap reaches
// the desired size.
func TestOOMCapOnCleanStartDecisionMatrix(t *testing.T) {
	st := oomCapState{Cap: 2000, SinceUnix: oomT0.Unix()}
	if _, d := oomCapOnCleanStart(st, 4000, oomT0.Add(23*time.Hour)); d.Action != "none" {
		t.Fatalf("before a clean day nothing changes, got %+v", d)
	}
	st, d := oomCapOnCleanStart(st, 4000, oomT0.Add(25*time.Hour))
	if d.Action != "relax" || d.To != 2200 {
		t.Fatalf("after a clean day: %+v, want relax to 2200", d)
	}
	if _, d := oomCapOnCleanStart(st, 4000, oomT0.Add(26*time.Hour)); d.Action != "none" {
		t.Fatalf("relax must wait a full window from the last change, got %+v", d)
	}
	st = oomCapState{Cap: 3800, SinceUnix: oomT0.Unix()}
	st, d = oomCapOnCleanStart(st, 4000, oomT0.Add(25*time.Hour))
	if d.Action != "clear" || st.Cap != 0 {
		t.Fatalf("reaching the desired size clears the cap, got %+v state %+v", d, st)
	}
	if _, d := oomCapOnCleanStart(oomCapState{}, 4000, oomT0); d.Action != "none" {
		t.Fatalf("no cap: nothing to relax, got %+v", d)
	}
}

// A stale marker is never blamed: the host-wide counter may have been bumped by
// ANY workload since. The heartbeat, not StartedUnix, is what ages it.
func TestOOMKilledSinceMarker(t *testing.T) {
	m := &oomMarker{BootID: "boot-A", OOMKills: 2}
	stale := &oomMarker{BootID: "boot-A", OOMKills: 2, StartedUnix: oomT0.Add(-(oomMarkerMaxAge + time.Hour)).Unix()}
	cases := []struct {
		name  string
		m     *oomMarker
		boot  string
		kills int64
		want  bool
	}{
		{"counter rose on the same boot", m, "boot-A", 3, true},
		{"counter unchanged", m, "boot-A", 2, false},
		{"a reboot resets the counter and is never an OOM", m, "boot-B", 9, false},
		{"no previous marker", nil, "boot-A", 3, false},
		{"unknown boot id", m, "", 3, false},
		{"unreadable counter", m, "boot-A", -1, false},
		{"a stale marker is never blamed even with a higher counter", stale, "boot-A", 9, false},
	}
	for _, c := range cases {
		if got := oomKilledSinceMarker(c.m, c.boot, c.kills, oomT0.Add(time.Hour)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// A fresh heartbeat overrides an old StartedUnix: a long-running process is
	// never blamed for a kill that happened while it was legitimately up.
	old := &oomMarker{BootID: "boot-A", OOMKills: 2, StartedUnix: oomT0.Add(-100 * time.Hour).Unix()}
	old.LastSeenUnix = oomT0.Add(-time.Hour).Unix()
	if !oomKilledSinceMarker(old, "boot-A", 3, oomT0) {
		t.Fatal("a heartbeat-refreshed marker must still be blamed")
	}
}

// The marker's peak only ever rises: a partial ramp after a restart never
// lowers the count the next decision measures.
func TestOOMMarkerWithPeakTransitions(t *testing.T) {
	m := oomMarker{BootID: "b", Proxies: 2000, StartedUnix: 5}
	if got, changed := oomMarkerWithPeak(m, 1500); changed || got.Proxies != 2000 {
		t.Fatalf("a lower reading must not shrink the peak: %+v changed=%v", got, changed)
	}
	if got, changed := oomMarkerWithPeak(m, 2000); changed || got.Proxies != 2000 {
		t.Fatalf("an equal reading is not a change: %+v changed=%v", got, changed)
	}
	got, changed := oomMarkerWithPeak(m, 3800)
	if !changed || got.Proxies != 3800 || got.BootID != "b" || got.StartedUnix != 5 {
		t.Fatalf("a higher reading raises only the peak: %+v changed=%v", got, changed)
	}
}

// The heartbeat refreshes at most once per oomHeartbeatInterval, and never for
// a marker that already has a recent one.
func TestOOMMarkerWithHeartbeatTransitions(t *testing.T) {
	m := oomMarker{BootID: "b", Proxies: 10}
	got, changed := oomMarkerWithHeartbeat(m, oomT0)
	if !changed || got.LastSeenUnix != oomT0.Unix() {
		t.Fatalf("a marker with no heartbeat must take one: %+v changed=%v", got, changed)
	}
	got, changed = oomMarkerWithHeartbeat(got, oomT0.Add(oomHeartbeatInterval-time.Minute))
	if changed || got.LastSeenUnix != oomT0.Unix() {
		t.Fatalf("a heartbeat inside the interval must not be rewritten: %+v changed=%v", got, changed)
	}
	got, changed = oomMarkerWithHeartbeat(got, oomT0.Add(oomHeartbeatInterval+time.Minute))
	if !changed || got.LastSeenUnix != oomT0.Add(oomHeartbeatInterval+time.Minute).Unix() {
		t.Fatalf("a heartbeat past the interval must refresh: %+v changed=%v", got, changed)
	}
}

// oomCapUpdatePeak raises the peak and refreshes the heartbeat, writes only on
// change, and keeps the heartbeat running with the kill switch off (the peak
// update is the only half that is skipped). With no marker it writes nothing:
// the next start, not this one, decides.
func TestOOMCapUpdatePeakTransitions(t *testing.T) {
	dir := oomCapTestEnv(t, "")
	markerPath := filepath.Join(dir, "run.marker")

	oomCapUpdatePeak(100, oomT0)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("no marker means nothing to update, stat err = %v", err)
	}

	oomCapRecordStart(2000, "boot-A", 0, oomT0)
	oomCapUpdatePeak(3800, oomT0)
	oomCapUpdatePeak(1000, oomT0.Add(time.Minute)) // lower: ignored, heartbeat inside the interval
	var m oomMarker
	if !oomReadJSON(markerPath, &m) {
		t.Fatal("run.marker must be readable")
	}
	if m.Proxies != 3800 {
		t.Fatalf("peak = %d, want the higher 3800 (a lower reading must not shrink it)", m.Proxies)
	}
	if m.LastSeenUnix != oomT0.Unix() {
		t.Fatalf("heartbeat = %d, want it inside the interval and unchanged (%d)", m.LastSeenUnix, oomT0.Unix())
	}

	// Past the interval the heartbeat alone is worth a write, with no peak move.
	oomCapUpdatePeak(1000, oomT0.Add(oomHeartbeatInterval+time.Minute))
	if !oomReadJSON(markerPath, &m) {
		t.Fatal("run.marker must still be readable")
	}
	if m.Proxies != 3800 {
		t.Fatalf("peak = %d, want 3800", m.Proxies)
	}
	if m.LastSeenUnix != oomT0.Add(oomHeartbeatInterval+time.Minute).Unix() {
		t.Fatalf("heartbeat = %d, want the refresh at %d", m.LastSeenUnix, oomT0.Add(oomHeartbeatInterval+time.Minute).Unix())
	}

	// With the switch off the heartbeat still runs and the peak is left alone.
	t.Setenv("URNETWORK_OOM_CAP", "off")
	later := oomT0.Add(4 * oomHeartbeatInterval)
	oomCapUpdatePeak(9999, later)
	if !oomReadJSON(markerPath, &m) {
		t.Fatal("run.marker must still be readable")
	}
	if m.Proxies != 3800 {
		t.Fatalf("the peak must NOT move while off; got %d", m.Proxies)
	}
	if m.LastSeenUnix != later.Unix() {
		t.Fatalf("the heartbeat must refresh while off; got %d want %d", m.LastSeenUnix, later.Unix())
	}
}

// The headline [oomcap] line must read as a sentence for every action, in both
// tenses, and a frozen decision must not read as a move between equal numbers.
func TestOOMCapActionPhrase(t *testing.T) {
	cases := []struct {
		d       oomCapDecision
		applied bool
		want    string
	}{
		{oomCapDecision{Action: "reduce", From: 0, To: 3301}, false, "would reduce the automatic start cap none -> 3301"},
		{oomCapDecision{Action: "reduce", From: 0, To: 3301}, true, "reduced the automatic start cap none -> 3301"},
		{oomCapDecision{Action: "relax", From: 3301, To: 3631}, false, "would relax the automatic start cap 3301 -> 3631"},
		{oomCapDecision{Action: "clear", From: 4300, To: 0}, true, "cleared the automatic start cap 4300 -> none"},
		{oomCapDecision{Action: "frozen", From: 100, To: 100}, false, "would hold the automatic start cap at 100 (reduction limit reached)"},
		{oomCapDecision{Action: "frozen", From: 100, To: 100}, true, "held the automatic start cap at 100 (reduction limit reached)"},
	}
	for _, c := range cases {
		if got := oomCapActionPhrase(c.d, c.applied); got != c.want {
			t.Errorf("oomCapActionPhrase(%+v, applied=%v) = %q, want %q", c.d, c.applied, got, c.want)
		}
	}
}

// A cap or OOM attribution that sits in a torn, hand-edited or unreadable file
// silently vanishes. A missing file is normal (first start) and stays silent;
// a file that exists but cannot be read is reported, and the rewrite makes the
// next start clean.
func TestOOMCapDecideWarnsWhenStateFilesAreUnreadable(t *testing.T) {
	dir := oomCapTestEnv(t, "")
	trimUnreadableReset()
	t.Cleanup(trimUnreadableReset)

	if lines := oomCapDecide(4127, "boot-A", 0, oomT0); lines != nil {
		t.Fatalf("missing state files are normal and must be silent, got %q", lines)
	}

	if err := os.WriteFile(filepath.Join(dir, "oom_cap.json"), []byte(`{"cap": 33`), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := oomCapDecide(4127, "boot-A", 0, oomT0.Add(time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "oom_cap.json exists but could not be read") {
		t.Fatalf("a torn oom_cap.json must be reported once, got %q", lines)
	}
	if lines := oomCapDecide(4127, "boot-A", 0, oomT0.Add(2*time.Hour)); lines != nil {
		t.Fatalf("after the rewrite the warning must not repeat, got %q", lines)
	}

	if err := os.WriteFile(filepath.Join(dir, "run.marker"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines = oomCapDecide(4127, "boot-A", 0, oomT0.Add(3*time.Hour))
	if len(lines) != 1 || !strings.Contains(lines[0], "run.marker exists but could not be read") {
		t.Fatalf("an unreadable run.marker must be reported, got %q", lines)
	}
}

// The cgroup counter must be oom_kill itself, never oom or oom_group_kill, and
// an unparseable or negative value must not count.
func TestParseCgroupOOMKills(t *testing.T) {
	if n, ok := parseCgroupOOMKills("low 0\nhigh 0\nmax 0\noom 2\noom_kill 3\noom_group_kill 1\n"); !ok || n != 3 {
		t.Fatalf("got %d ok=%v, want 3 (oom_kill, not oom or oom_group_kill)", n, ok)
	}
	for _, bad := range []string{"", "oom 1\n", "oom_kill x\n", "oom_kill -1\n"} {
		if _, ok := parseCgroupOOMKills(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

// The service's own cgroup is recreated on restart, so its counter resets. The
// counter to compare is the parent's, which persists and is hierarchical.
func TestReadCgroupOOMKillsUsesTheParentNotTheRecreatedOwnCgroup(t *testing.T) {
	root := t.TempDir()
	unit := "/user.slice/user-1000.slice/user@1000.service/app.slice/urnetwork.service"
	self := "0::" + unit + "\n"
	writeMemoryEvents(t, root, unit, 0)
	writeMemoryEvents(t, root, "/user.slice/user-1000.slice/user@1000.service/app.slice", 3)
	writeMemoryEvents(t, root, "/user.slice", 40)

	n, scope, ok := readCgroupOOMKills(root, self)
	if !ok || n != 3 || scope != "cgroup:/user.slice/user-1000.slice/user@1000.service/app.slice" {
		t.Fatalf("got %d %q ok=%v, want the app.slice counter 3", n, scope, ok)
	}

	// In a container the mount root IS the persistent file.
	container := t.TempDir()
	writeMemoryEvents(t, container, ".", 2)
	if n, scope, ok := readCgroupOOMKills(container, "0::/\n"); !ok || n != 2 || scope != "cgroup:/" {
		t.Fatalf("container root: got %d %q ok=%v, want 2 cgroup:/", n, scope, ok)
	}

	for name, self := range map[string]string{
		"no file on the path": "0::/system.slice/urnetwork.service\n",
		"cgroup v1 only":      "1:name=systemd:/x\n",
		"empty":               "",
	} {
		if _, _, ok := readCgroupOOMKills(t.TempDir(), self); ok {
			t.Fatalf("%s: must report not ok", name)
		}
	}
}

// The finding the cgroup epoch exists for: the host-wide counter blamed this
// provider for another workload's OOM kill. With a cgroup counter only kills
// inside this subtree count, and a marker from a different scope is never
// compared.
func TestOOMKillEpochIgnoresAnotherWorkloadsKillOnTheHost(t *testing.T) {
	root := t.TempDir()
	unit := "/user.slice/user-1000.slice/app.slice/urnetwork.service"
	self := "0::" + unit + "\n"
	parent := "/user.slice/user-1000.slice/app.slice"
	writeMemoryEvents(t, root, parent, 1)

	epochA, killsA := oomKillEpoch("boot-A", root, self, 10) // marker taken here
	marker := &oomMarker{BootID: epochA, OOMKills: killsA, StartedUnix: oomT0.Unix(), LastSeenUnix: oomT0.Unix()}

	// Some other container on the host is OOM-killed: only the host counter moves.
	epochB, killsB := oomKillEpoch("boot-A", root, self, 11)
	if oomKilledSinceMarker(marker, epochB, killsB, oomT0.Add(time.Hour)) {
		t.Fatal("another workload's OOM kill on the host must not be blamed on this provider")
	}
	// A kill inside this subtree does count, and the epoch carries the scope.
	writeMemoryEvents(t, root, parent, 2)
	epochC, killsC := oomKillEpoch("boot-A", root, self, 11)
	if !oomKilledSinceMarker(marker, epochC, killsC, oomT0.Add(time.Hour)) {
		t.Fatal("an OOM kill inside this provider's own cgroup subtree must be attributed")
	}
	if epochC != "boot-A|cgroup:"+parent {
		t.Fatalf("epoch = %q, want the scope label", epochC)
	}

	// Without a readable cgroup file the host-wide counter and the plain boot
	// id are used, and a marker under the old (host) epoch is not compared.
	if epoch, kills := oomKillEpoch("boot-A", t.TempDir(), "0::/nowhere\n", 7); epoch != "boot-A" || kills != 7 {
		t.Fatalf("fallback = %q,%d, want the plain boot id and the host counter", epoch, kills)
	}
	if e, k := oomKillEpoch("", t.TempDir(), "0::/x\n", 7); e != "" || k != 7 {
		t.Fatalf("an unknown boot id stays unknown, got %q,%d", e, k)
	}
	hostMarker := &oomMarker{BootID: "boot-A", OOMKills: 3, StartedUnix: oomT0.Unix(), LastSeenUnix: oomT0.Unix()}
	e, k := oomKillEpoch("boot-A", root, "0::/system.slice/urnetwork.service\n", 3)
	if oomKilledSinceMarker(hostMarker, e, k, oomT0.Add(time.Hour)) {
		t.Fatal("a host-scoped marker must not be compared with a cgroup-scoped reading")
	}
}

// The miner's effective cap is the relay's precedence over its own trim
// surface: the operator file and the automatic OOM cap, tightest positive wins,
// the automatic cap only in "on" mode, and the source names whichever bound.
func TestEffectiveTrimCapSourcePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		operator int
		auto     int
		wantCap  int
		wantSrc  string
	}{
		{"nothing set", "on", 0, 0, 0, ""},
		{"operator only", "on", 2000, 0, 2000, trimCapOperator},
		{"auto only, enforced", "on", 0, 1500, 1500, trimCapOOM},
		{"both, the tighter wins (auto)", "on", 2000, 1500, 1500, trimCapOOM},
		{"both, the tighter wins (operator)", "on", 1000, 1500, 1000, trimCapOperator},
		{"a tie goes to the operator source", "on", 1500, 1500, 1500, trimCapOperator},
		{"auto ignored in shadow", "shadow", 0, 1500, 0, ""},
		{"auto ignored in shadow, operator stands", "shadow", 2000, 1500, 2000, trimCapOperator},
		{"auto ignored when off", "off", 2000, 1500, 2000, trimCapOperator},
		{"auto ignored when off, alone", "off", 0, 1500, 0, ""},
		{"default mode is shadow", "", 0, 1500, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oomCapTestEnv(t, c.mode)
			if c.operator > 0 {
				if err := writeTrimTarget(c.operator); err != nil {
					t.Fatal(err)
				}
			}
			if c.auto > 0 {
				writeOOMCapState(t, oomCapState{Cap: c.auto})
			}
			cap, source, err := effectiveTrimCapSource()
			if err != nil {
				t.Fatalf("effectiveTrimCapSource: %v", err)
			}
			if cap != c.wantCap || source != c.wantSrc {
				t.Fatalf("effectiveTrimCapSource = %d,%q; want %d,%q", cap, source, c.wantCap, c.wantSrc)
			}
			// effectiveTrimCap is the same read without the source.
			if got, err := effectiveTrimCap(); err != nil || got != c.wantCap {
				t.Fatalf("effectiveTrimCap = %d,%v; want %d", got, err, c.wantCap)
			}
		})
	}
}

// An unreadable proxy_trim (permissions, a directory where the file belongs, a
// broken disk) must not silence the automatic cap: the operator cap reads as 0,
// says so once, and the OOM cap still applies. The events.log copy is queued
// (the read can run under the proxy manager's lock) and is queued only once.
func TestEffectiveTrimCapSourceKeepsTheAutomaticCapWhenOperatorFileIsUnreadable(t *testing.T) {
	dir := oomCapTestEnv(t, "on")
	writeOOMCapState(t, oomCapState{Cap: 400})
	if err := os.Mkdir(filepath.Join(dir, "proxy_trim"), 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		cap    int
		source string
		err    error
	)
	logged := captureTlog(t, func() { cap, source, err = effectiveTrimCapSource() })
	if err != nil {
		t.Fatalf("an unreadable operator file must not error: %v", err)
	}
	if cap != 400 || source != trimCapOOM {
		t.Fatalf("effectiveTrimCapSource = %d,%q; want the automatic 400", cap, source)
	}
	if !strings.Contains(logged, "cannot read proxy_trim") || !strings.Contains(logged, "the automatic cap still applies") {
		t.Fatalf("the unreadable file must be reported, got %q", logged)
	}
	if queued := drainDeferredCrit(); len(queued) != 1 || !strings.Contains(queued[0], "cannot read proxy_trim") {
		t.Fatalf("the events.log copy must be queued once, got %q", queued)
	}

	// A repeat of the same error says nothing at all, and the cap still holds.
	logged = captureTlog(t, func() { cap, source, err = effectiveTrimCapSource() })
	if err != nil || cap != 400 || source != trimCapOOM {
		t.Fatalf("second read = %d,%q,%v; want the automatic 400", cap, source, err)
	}
	if logged != "" {
		t.Fatalf("a repeat of the same read error must be silent, got %q", logged)
	}
	if queued := drainDeferredCrit(); len(queued) != 0 {
		t.Fatalf("a repeat must not queue a second events.log line, got %q", queued)
	}

	// With no automatic cap standing, an unreadable operator file is simply no
	// cap, still without an error for the callers.
	writeOOMCapState(t, oomCapState{})
	if cap, source, err := effectiveTrimCapSource(); err != nil || cap != 0 || source != "" {
		t.Fatalf("no cap standing: got %d,%q,%v", cap, source, err)
	}
}

// The snapshot's trim report carries the SOURCE the trim selection actually
// used, so an automatic shed is not attributed to the operator.
func TestNoteTrimSelectionReportsTheAutomaticSource(t *testing.T) {
	oomCapTestEnv(t, "on")
	writeOOMCapState(t, oomCapState{Cap: 500})
	if err := writeTrimTarget(2000); err != nil {
		t.Fatal(err)
	}
	noteTrimSelection(1200)
	desired, cap, source, ok := currentTrimSelection()
	if !ok || desired != 1200 || cap != 500 || source != trimCapOOM {
		t.Fatalf("got desired=%d cap=%d source=%q ok=%v, want 1200/500/%q", desired, cap, source, ok, trimCapOOM)
	}

	// A looser automatic cap leaves the operator as the binding source.
	writeOOMCapState(t, oomCapState{Cap: 2500})
	noteTrimSelection(1200)
	if _, cap, source, _ := currentTrimSelection(); cap != 1200 || source != trimCapOperator {
		t.Fatalf("got cap=%d source=%q, want the operator 1200", cap, source)
	}
}

// oomCapStartupFixture writes a proxy file of n entries and returns a manager
// whose desired set is exactly those n, in the state directory the test set up.
func oomCapStartupFixture(t *testing.T, n int) *proxyManager {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256) + ":1080:user:secret"
	}
	writeProxyFile(t, sourcePath, lines...)
	cfg := defaultProxyManagerConfig()
	cfg.ProxyFile = sourcePath
	return newProxyManager(cfg)
}

// oomCapPinEpoch fixes this process's OOM-kill epoch and reports how many times
// the underlying counter was read, so the tests do not depend on the host
// having had an OOM kill since boot.
func oomCapPinEpoch(t *testing.T, bootID string, kills int64) *int {
	t.Helper()
	reads := 0
	oomCapEpoch = sync.OnceValues(func() (string, int64) { reads++; return bootID, kills })
	t.Cleanup(resetOOMCapStartupForTest)
	return &reads
}

// The whole point of the port: a decision taken at startup applies to the same
// start's launch selection, so a box that was OOM-killed launches fewer
// proxies immediately instead of after another death. The decision runs once
// per process, so a later read never re-decides.
func TestOOMCapStartupDecisionAppliesBeforeTheTrimSelection(t *testing.T) {
	dir := oomCapTestEnv(t, "on")
	epochReads := oomCapPinEpoch(t, "boot-test", 5)
	// The previous start launched 100 and the counter rose since (4 of 5): an
	// OOM kill is claimed, and the decision is 80% of what died (floor 50 for a
	// 100-proxy desired pool).
	now := time.Now()
	if err := oomWriteJSON(filepath.Join(dir, "run.marker"), oomMarker{
		BootID:       "boot-test",
		OOMKills:     4,
		Proxies:      100,
		StartedUnix:  now.Add(-time.Hour).Unix(),
		LastSeenUnix: now.Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	m := oomCapStartupFixture(t, 100)

	var (
		launch []*connect.ProxySettings
		held   []*connect.ProxySettings
	)
	joined := captureImportantLog(t, func() {
		var err error
		launch, held, _, err = m.desired(true)
		if err != nil {
			t.Fatalf("desired: %v", err)
		}
	})
	if len(launch) != 80 || len(held) != 20 {
		t.Fatalf("launching %d and holding %d; want the 80 cap applied to this start (decision lines %q)",
			len(launch), len(held), joined)
	}
	if cap, _ := effectiveTrimCap(); cap != 80 {
		t.Fatalf("effectiveTrimCap = %d, want the decided 80", cap)
	}
	if !strings.Contains(joined, "[oomcap] applied: OOM kill since the last start") || !strings.Contains(joined, "none -> 80") {
		t.Fatalf("the applied decision must be logged, got %q", joined)
	}

	// The marker records what this start actually launched, so the next start
	// measures 80% of the pool that ran.
	var m2 oomMarker
	if !oomReadJSON(filepath.Join(dir, "run.marker"), &m2) {
		t.Fatal("run.marker must have been written by the start")
	}
	if m2.Proxies != 80 || m2.BootID != "boot-test" {
		t.Fatalf("marker = %+v, want 80 proxies under this epoch", m2)
	}
	if *epochReads != 1 {
		t.Fatalf("the epoch was read %d times; the decision and the marker must share one reading", *epochReads)
	}

	// Second read in the same process: no re-decision, and no further
	// oomcap lines (the cap it reads is the one already decided).
	again := captureImportantLog(t, func() {
		var err error
		launch, _, _, err = m.desired(true)
		if err != nil {
			t.Fatalf("desired: %v", err)
		}
	})
	if len(launch) != 80 {
		t.Fatalf("second read launched %d, want the same 80", len(launch))
	}
	if strings.Contains(again, "[oomcap]") {
		t.Fatalf("the startup decision must run once per process, got %q", again)
	}
	if *epochReads != 1 {
		t.Fatalf("the epoch was read %d times; a later read must not re-decide", *epochReads)
	}
}

// The persisted kill switch has to reach the startup decision even though the
// miner loads its control state after the proxy manager starts (run.go opens the
// control socket later). An operator who ran `urnet-tools set oom-cap on` while
// the provider was up expects the next start to be capped, and only the
// provider_state.json copy exists at that point.
func TestOOMCapStartupDecisionHonorsThePersistedControlValue(t *testing.T) {
	dir := oomCapTestEnv(t, "") // no env: the persisted value is the only source
	persisted := newControlState()
	if err := persisted.set("oom_cap", "on"); err != nil {
		t.Fatal(err)
	}
	if err := persisted.persist(); err != nil {
		t.Fatal(err)
	}
	if _, ok := globalControlState.get("oom_cap"); ok {
		t.Fatal("the live control state must be empty at startup")
	}
	oomCapPinEpoch(t, "boot-test", 5)
	now := time.Now()
	if err := oomWriteJSON(filepath.Join(dir, "run.marker"), oomMarker{
		BootID:       "boot-test",
		OOMKills:     4,
		Proxies:      100,
		StartedUnix:  now.Add(-time.Hour).Unix(),
		LastSeenUnix: now.Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	m := oomCapStartupFixture(t, 100)
	var (
		launch []*connect.ProxySettings
		held   []*connect.ProxySettings
	)
	joined := captureImportantLog(t, func() {
		var err error
		launch, held, _, err = m.desired(true)
		if err != nil {
			t.Fatalf("desired: %v", err)
		}
	})
	if got := oomCapModeName(oomCapMode()); got != "on" {
		t.Fatalf("the persisted mode must govern at startup, got %q", got)
	}
	if len(launch) != 80 || len(held) != 20 {
		t.Fatalf("launching %d and holding %d; want the persisted on applied to this start (decision lines %q)",
			len(launch), len(held), joined)
	}
	if cap, _ := effectiveTrimCap(); cap != 80 {
		t.Fatalf("effectiveTrimCap = %d, want the decided 80", cap)
	}
}

// The oom_cap kill switch is a control key: registered, persisted, applied
// live (the cap is read on every desired()/trim path) and cleared back to
// shadow.
func TestOOMCapControlKeyIsRegistered(t *testing.T) {
	if !controlKeys["oom_cap"] {
		t.Fatal("controlKeys has no oom_cap entry: the kill switch would never persist")
	}
	if !controlLiveEffectKeys["oom_cap"] {
		t.Fatal("controlLiveEffectKeys has no oom_cap entry: the switch would need a restart")
	}
	if controlUnsupportedKeys["oom_cap"] {
		t.Fatal("controlUnsupportedKeys still lists oom_cap: the switch applies live now")
	}
	if got := controlLiveDefaults["oom_cap"]; got != "shadow" {
		t.Fatalf("controlLiveDefaults[oom_cap] = %q, want shadow (clearing the key must return to the safe default)", got)
	}
	if controlNeedsRestart("oom_cap") {
		t.Fatal("oom_cap must not report needs_restart")
	}
}

func TestOOMCapControlValueIsValidated(t *testing.T) {
	oomCapTestEnv(t, "")
	for _, ok := range []string{"on", "off", "shadow", "ON", "Shadow"} {
		resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "oom_cap", Value: ok})
		if !resp.OK {
			t.Errorf("set oom_cap %q rejected: %s", ok, resp.Error)
		}
		if resp.NeedsRestart {
			t.Errorf("set oom_cap %q reports needs_restart; the cap is read live", ok)
		}
	}
	for _, bad := range []string{"", "maybe", "1", "enforce"} {
		resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "oom_cap", Value: bad})
		if resp.OK || !strings.Contains(resp.Error, "oom_cap") {
			t.Errorf("set oom_cap %q must be rejected with a clear error, got ok=%v err=%q", bad, resp.OK, resp.Error)
		}
	}
}

// The control-socket ack must report the mode that actually governs, not just
// the value the operator typed: URNETWORK_OOM_CAP=on always beats a persisted
// "shadow" or "off" (see oomCapMode's precedence), so an ack that echoed only
// the requested value silently misled an operator into thinking a
// control-socket "off" disabled enforcement when the env var kept it on.
func TestOOMCapControlAckReportsEffectiveMode(t *testing.T) {
	t.Run("no env override: ack matches the request", func(t *testing.T) {
		oomCapTestEnv(t, "")
		lines := captureImportantLog(t, func() {
			resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "oom_cap", Value: "shadow"})
			if !resp.OK {
				t.Fatalf("set rejected: %s", resp.Error)
			}
		})
		if !strings.Contains(lines, "mode set to shadow via control socket (the automatic") {
			t.Fatalf("expected a plain ack with no override note, got:\n%s", lines)
		}
		if strings.Contains(lines, "overrides it") {
			t.Fatalf("no env is set: must not claim an override, got:\n%s", lines)
		}
	})

	t.Run("env on overrides a control-socket shadow", func(t *testing.T) {
		oomCapTestEnv(t, "on")
		lines := captureImportantLog(t, func() {
			resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "oom_cap", Value: "shadow"})
			if !resp.OK {
				t.Fatalf("set rejected: %s", resp.Error)
			}
		})
		if !strings.Contains(lines, "mode set to shadow via control socket, but URNETWORK_OOM_CAP env overrides it: effective mode is on") {
			t.Fatalf("expected the ack to name the effective mode, got:\n%s", lines)
		}
	})
}

// Clearing the key drops the persisted value and reapplies the safe default,
// shadow, live: enforcement must stop without a restart.
func TestOOMCapControlClearRestoresShadow(t *testing.T) {
	oomCapTestEnv(t, "")
	writeOOMCapState(t, oomCapState{Cap: 700})
	if err := globalControlState.set("oom_cap", "on"); err != nil {
		t.Fatal(err)
	}
	if cap, _ := effectiveTrimCap(); cap != 700 {
		t.Fatalf("effectiveTrimCap = %d, want the enforced 700", cap)
	}

	lines := captureImportantLog(t, func() {
		resp := handleControlRequest(globalControlState, controlRequest{Cmd: "clear", Key: "oom_cap"})
		if !resp.OK {
			t.Fatalf("clear rejected: %s", resp.Error)
		}
		if resp.NeedsRestart {
			t.Fatal("clearing oom_cap reports needs_restart; the default is applied live")
		}
	})
	if _, found := globalControlState.get("oom_cap"); found {
		t.Fatal("the persisted value must be dropped by a clear")
	}
	if !strings.Contains(lines, "mode set to shadow via control socket") {
		t.Fatalf("the clear must reapply shadow live, got:\n%s", lines)
	}
	if cap, source, err := effectiveTrimCapSource(); err != nil || cap != 0 || source != "" {
		t.Fatalf("shadow must not enforce the standing cap: got %d,%q,%v", cap, source, err)
	}
}

// A live `set oom-cap on` has to say what it will cost, not just that the value
// landed: the cap in force, its source and the shed it implies.
func TestOOMCapLiveApplyOnNamesTheCapAndTheShed(t *testing.T) {
	oomCapTestEnv(t, "on")
	writeOOMCapState(t, oomCapState{Cap: 500})
	// Two proxies running, so the shed count (running minus cap, floored at 0)
	// is 0 rather than a negative number.
	setOOMCapRunningProxies(2)

	lines := captureImportantLog(t, func() {
		if err := applyControlLiveSideEffect("oom_cap", "on"); err != nil {
			t.Fatalf("live apply: %v", err)
		}
	})
	if !strings.Contains(lines, "enforcing automatic cap 500 (oomcap)") {
		t.Fatalf("live apply on must name the cap in force and its source, got %q", lines)
	}
	if !strings.Contains(lines, "sheds about 0 of 2") {
		t.Fatalf("live apply on must report the shed count, got %q", lines)
	}

	// A tighter operator cap masks the automatic one in the effective source,
	// but the automatic cap still stands and must be named.
	if err := writeTrimTarget(300); err != nil {
		t.Fatal(err)
	}
	lines = captureImportantLog(t, func() {
		if err := applyControlLiveSideEffect("oom_cap", "on"); err != nil {
			t.Fatalf("live apply: %v", err)
		}
	})
	if strings.Contains(lines, "no automatic cap is standing") {
		t.Fatalf("a masked automatic cap must not read as none standing:\n%s", lines)
	}
	if !strings.Contains(lines, "automatic cap 500 (oomcap) is standing") ||
		!strings.Contains(lines, "operator trim cap is the effective limit") {
		t.Fatalf("the standing automatic cap and the operator limit must be named, got:\n%s", lines)
	}
}

// A live `set oom-cap off` has to clear the standing cap immediately, not wait
// for the next start: a cap that is still on disk is one restart away from
// shedding the pool again.
func TestOOMCapLiveApplyOffClearsTheStandingCap(t *testing.T) {
	dir := oomCapTestEnv(t, "off")
	writeOOMCapState(t, oomCapState{Cap: 800, Reductions: []int64{1000, 2000}})

	lines := captureImportantLog(t, func() {
		if err := applyControlLiveSideEffect("oom_cap", "off"); err != nil {
			t.Fatalf("live apply: %v", err)
		}
	})
	if !strings.Contains(lines, "cleared the automatic start cap 800 -> none") {
		t.Fatalf("live apply off must say the cap was forgotten, got %q", lines)
	}
	var st oomCapState
	if !oomReadJSON(filepath.Join(dir, "oom_cap.json"), &st) || st.Cap != 0 {
		t.Fatalf("live apply off must clear the cap immediately, state is %+v", st)
	}
	if len(st.Reductions) != 2 {
		t.Fatalf("off must keep the reduction history, got %+v", st.Reductions)
	}
	if cap, source, err := effectiveTrimCapSource(); err != nil || cap != 0 || source != "" {
		t.Fatalf("after a live off, no automatic cap may bind: cap=%d source=%q err=%v", cap, source, err)
	}
}

// captureImportantLog collects the lines importantLogf would write.
func captureImportantLog(t *testing.T, fn func()) string {
	t.Helper()
	old := importantLogHook
	var lines []string
	importantLogHook = func(line string) { lines = append(lines, line) }
	defer func() { importantLogHook = old }()
	fn()
	return strings.Join(lines, "\n")
}

// Clearing the control key must release the mode. oomCapMode falls back to the
// mode read at startup whenever the live state has no value, so dropping only
// the live key left the old value in force and the clear did nothing.
func TestClearOOMCapControlReleasesThePersistedMode(t *testing.T) {
	resetOOMCapStartupForTest()
	t.Cleanup(resetOOMCapStartupForTest)
	t.Setenv("URNETWORK_OOM_CAP", "")

	oomCapPersistedMode.Store("on")
	if got := oomCapModeName(oomCapMode()); got != "on" {
		t.Fatalf("with only the persisted mode set, mode = %q, want on", got)
	}

	if err := applyControlLiveDefault("oom_cap"); err != nil {
		t.Fatalf("applyControlLiveDefault(oom_cap): %v", err)
	}
	if got := oomCapModeName(oomCapMode()); got != "shadow" {
		t.Fatalf("after clear, mode = %q, want the safe default shadow", got)
	}
}
