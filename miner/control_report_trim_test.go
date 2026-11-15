package miner

import (
	"encoding/json"
	"testing"
)

// The snapshot's proxy block is the wire contract with urnet-tools
// (internal/urnettools/snapshot.go, SnapshotProxies). A capped miner has to
// carry the configured pool size, the cap in force and who set it, or
// `urnet-tools status` has no trim row and `proxy trim` can only read the
// operator file.

func TestSnapshotReportsTrimSelection(t *testing.T) {
	withTempHome(t)
	if err := writeTrimTarget(5); err != nil {
		t.Fatal(err)
	}
	noteTrimSelection(7)

	counts, note := snapshotProxyCounts()
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}
	if counts.Configured != 7 || counts.TrimCap != 5 || counts.TrimCapSource != trimCapOperator {
		t.Fatalf("snapshot trim = configured %d, cap %d, source %q; want 7/5/%q",
			counts.Configured, counts.TrimCap, counts.TrimCapSource, trimCapOperator)
	}
}

// A provider that runs its full pool reports the configured size and no cap,
// so the status row can say "no cap applied" instead of staying silent.
func TestSnapshotReportsConfiguredWithoutCap(t *testing.T) {
	withTempHome(t)
	noteTrimSelection(9)

	counts, _ := snapshotProxyCounts()
	if counts.Configured != 9 || counts.TrimCap != 0 {
		t.Fatalf("snapshot trim = configured %d, cap %d; want 9/0", counts.Configured, counts.TrimCap)
	}
}

// No decision yet: the trim fields stay at their zero values, which the wire
// contract reads as "not reported", never as "uncapped".
func TestSnapshotOmitsTrimBeforeFirstDecision(t *testing.T) {
	withTempHome(t)

	counts, _ := snapshotProxyCounts()
	if counts.Configured != 0 || counts.TrimCap != 0 || counts.TrimCapSource != "" {
		t.Fatalf("snapshot carried trim fields before the first decision: %+v", counts)
	}
}

// A cleared cap is still a report. Without the presence flag the reader cannot
// tell it from a provider build that predates trim reporting, so it falls back
// to the operator file and a first-start decision is reported as "this build
// does not report" instead of "no cap".
func TestSnapshotReportsClearedCap(t *testing.T) {
	withTempHome(t)
	noteTrimSelection(9)

	counts, _ := snapshotProxyCounts()
	if counts.Configured != 9 || counts.TrimCap != 0 {
		t.Fatalf("snapshot trim = configured %d, cap %d; want 9/0", counts.Configured, counts.TrimCap)
	}
	// The wire shape is what urnet-tools decodes, so pin the flag itself.
	raw, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["trim_reported"] != true {
		t.Fatalf("cleared cap carries no trim_reported flag: %s", raw)
	}
}

// A trimmed node that holds most of its configured pool back on purpose must not
// read as degraded. The state file lists every configured proxy, so the held
// ones have to stay out of the counts and out of the pool the state judges.
func TestSnapshotParkedProxiesAreNotDown(t *testing.T) {
	withTempHome(t)
	resetTrimSelection()
	t.Cleanup(resetTrimSelection)
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	var running []string
	for i := range 9 {
		key := "10.0.0." + string(rune('0'+i)) + ":1080"
		state.Proxies[key] = ProxyEntry{ID: i, Health: "up"}
		running = append(running, key)
	}
	// 30 held back: some went offline before the trim, some were never tried
	for i := range 30 {
		key := "10.0.1." + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ":1080"
		health := "recently_offline"
		if i%3 == 0 {
			health = "dead"
		}
		state.Proxies[key] = ProxyEntry{ID: 100 + i, Health: health}
	}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	if err := writeTrimTarget(9); err != nil {
		t.Fatal(err)
	}
	noteTrimSelection(39)
	controlRuntime.setRunningProxies(running)
	t.Cleanup(func() { controlRuntime.setRunningProxies(nil) })

	counts, note := snapshotProxyCounts()
	if note != "" {
		t.Fatal(note)
	}
	if counts.Up != 9 || counts.Degraded != 0 || counts.Dead != 0 || counts.Connecting != 0 {
		t.Fatalf("running pool = up %d degraded %d dead %d connecting %d, want 9/0/0/0", counts.Up, counts.Degraded, counts.Dead, counts.Connecting)
	}
	if counts.Parked != 30 {
		t.Fatalf("parked = %d, want 30", counts.Parked)
	}
	if counts.total() != 9 {
		t.Fatalf("pool size = %d, want the 9 running proxies", counts.total())
	}
	if counts.Configured != 39 || counts.TrimCap != 9 {
		t.Fatalf("trim = configured %d cap %d", counts.Configured, counts.TrimCap)
	}
	// the live numbers from the field: 2*(dead+degraded) > total would have read degraded
	payload, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) == "" {
		t.Fatal("no payload")
	}
}

// A genuinely unhealthy running pool still reports its own numbers.
func TestSnapshotUnhealthyRunningPoolStillCountsDown(t *testing.T) {
	withTempHome(t)
	resetTrimSelection()
	t.Cleanup(resetTrimSelection)
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	var running []string
	for i := range 10 {
		key := "10.0.2." + string(rune('0'+i)) + ":1080"
		health := "up"
		if i < 6 {
			health = "recently_offline"
		}
		state.Proxies[key] = ProxyEntry{ID: i, Health: health}
		running = append(running, key)
	}
	state.Proxies["10.0.3.1:1080"] = ProxyEntry{ID: 50, Health: "up"}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	if err := writeTrimTarget(10); err != nil {
		t.Fatal(err)
	}
	noteTrimSelection(11)
	controlRuntime.setRunningProxies(running)
	t.Cleanup(func() { controlRuntime.setRunningProxies(nil) })
	counts, _ := snapshotProxyCounts()
	if counts.Up != 4 || counts.Degraded != 6 || counts.Parked != 1 || counts.total() != 10 {
		t.Fatalf("counts = %+v, want 4 up, 6 degraded, 1 parked, pool of 10", counts)
	}
	if 2*(counts.Dead+counts.Degraded) <= counts.total() {
		t.Fatalf("a 6 of 10 down running pool must still read degraded: %+v", counts)
	}
}

// With no trim decision, or no cap below the configured size, nothing is parked.
func TestSnapshotNoTrimParksNothing(t *testing.T) {
	withTempHome(t)
	resetTrimSelection()
	t.Cleanup(resetTrimSelection)
	state := &ProxyState{Proxies: map[string]ProxyEntry{"10.0.4.1:1080": {ID: 1, Health: "recently_offline"}, "10.0.4.2:1080": {ID: 2, Health: "up"}}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	controlRuntime.setRunningProxies([]string{"10.0.4.2:1080"})
	t.Cleanup(func() { controlRuntime.setRunningProxies(nil) })
	counts, _ := snapshotProxyCounts()
	if counts.Parked != 0 || counts.Up != 1 || counts.Degraded != 1 {
		t.Fatalf("counts = %+v, want nothing parked without a trim", counts)
	}
}
