package urnettools

import (
	"strings"
	"testing"
)

// status shows the run's identity provenance beside the pool counts, named as
// a per-run figure so it cannot be read as a lifetime total.
func TestRenderLiveBlockShowsClientIDProvenance(t *testing.T) {
	s := &NodeSnapshot{
		State:     "flowing",
		Proxies:   SnapshotProxies{Up: 9, Degraded: 1},
		ClientIDs: &SnapshotClientIDs{Reused: 7, Minted: 2},
	}
	out := renderLiveBlock(s, liveOpts{}, "")
	if !strings.Contains(out, "client ids  7 reused, 2 minted (this run)") {
		t.Fatalf("client id row missing or not marked per-run:\n%s", out)
	}
}

// A provider that predates the field, or one whose provide run has not
// started, renders no row at all rather than a pair of zeros.
func TestRenderLiveBlockOmitsUnreportedClientIDs(t *testing.T) {
	s := &NodeSnapshot{State: "flowing", Proxies: SnapshotProxies{Up: 9, Degraded: 1}}
	if out := renderLiveBlock(s, liveOpts{}, ""); strings.Contains(out, "client ids") {
		t.Fatalf("client id row rendered without a report:\n%s", out)
	}
}
