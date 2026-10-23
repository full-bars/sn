package miner

import "testing"

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
