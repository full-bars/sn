package miner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// providerClientIDGauge reads one sample out of the /metrics payload, so a
// test asserts the number an operator's panel would read rather than the
// tracker's internal tally.
func providerClientIDGauge(t *testing.T, name string) string {
	t.Helper()
	for _, line := range strings.Split(minerExtraMetrics(), "\n") {
		if rest, ok := strings.CutPrefix(line, name+" "); ok {
			return rest
		}
	}
	t.Fatalf("no %s sample in the scrape", name)
	return ""
}

func requireProviderClientIDGauges(t *testing.T, reused, minted string) {
	t.Helper()
	if got := providerClientIDGauge(t, "urnet_provider_client_ids_reused"); got != reused {
		t.Fatalf("urnet_provider_client_ids_reused = %s, want %s", got, reused)
	}
	if got := providerClientIDGauge(t, "urnet_provider_client_ids_minted"); got != minted {
		t.Fatalf("urnet_provider_client_ids_minted = %s, want %s", got, minted)
	}
}

// setTestProviderClientIDs publishes a tracker for one test and clears the
// process-wide handle afterwards, so no later test in this package reads this
// test's counts.
func setTestProviderClientIDs(t *testing.T, tracker *providerBringUpTracker) {
	t.Helper()
	setProviderBringUpTracker(tracker)
	t.Cleanup(func() {
		setProviderBringUpTracker(nil)
		if tracker != nil {
			tracker.shutdown()
		}
	})
}

// A scrape before the run starts must publish the series at zero: a panel that
// has never seen the family cannot tell "no run yet" from a missing scrape.
func TestProviderClientIDGaugesZeroBeforeRun(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)
	setTestProviderClientIDs(t, nil)

	requireProviderClientIDGauges(t, "0", "0")

	// The status payload has no provenance to report either, and must omit the
	// block rather than publish two zeros that read as "every slot minted".
	snap, err := buildMinerSnapshot()
	if err != nil {
		t.Fatalf("buildMinerSnapshot: %v", err)
	}
	if snap.ClientIDs != nil {
		t.Fatalf("snapshot reports client ids before any run: %+v", *snap.ClientIDs)
	}
}

// The gauges follow the running set: a slot that authenticates joins one of
// them, and a slot that leaves running drops out of both.
func TestProviderClientIDGaugesFollowRunningSlots(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	tracker := newProviderBringUpTracker(3)
	setTestProviderClientIDs(t, tracker)

	// The heartbeat is never started, so the tracker only moves on the auth
	// outcomes below and nothing here waits on a ticker.
	tracker.authReused(0, connect.NewId(), "direct")
	tracker.authNew(1, connect.NewId(), "proxy:10.0.0.1:1080")
	requireProviderClientIDGauges(t, "1", "1")

	// Slot 1 minted its id and then failed its worker. It is no longer
	// running, so the minted gauge drops to 0 while the reused slot stays.
	tracker.workerFailed(1)
	requireProviderClientIDGauges(t, "1", "0")
}

// The status payload carries both numbers beside the pool counts, and the JSON
// `urnet-tools status` reads is what carries them.
func TestProviderClientIDStatusFieldsCarryCounts(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	resetControlRuntimeForTest()
	t.Cleanup(resetControlRuntimeForTest)
	tracker := newProviderBringUpTracker(2)
	setTestProviderClientIDs(t, tracker)

	tracker.authReused(0, connect.NewId(), "direct")
	tracker.authNew(1, connect.NewId(), "proxy:10.0.0.1:1080")

	snap, err := buildMinerSnapshot()
	if err != nil {
		t.Fatalf("buildMinerSnapshot: %v", err)
	}
	if snap.ClientIDs == nil {
		t.Fatal("snapshot has no client_ids block")
	}
	if snap.ClientIDs.Reused != 1 || snap.ClientIDs.Minted != 1 {
		t.Fatalf("client_ids = %+v, want 1 reused and 1 minted", *snap.ClientIDs)
	}

	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if !strings.Contains(string(body), `"client_ids":{"reused":1,"minted":1}`) {
		t.Fatalf("client_ids missing from the wire payload: %s", string(body))
	}
}

// A slot that never resolved its identity is in neither count, whether it
// failed its sign-in or was still pending when the scrape ran.
func TestProviderClientIDCountsSkipUnresolvedSlots(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	tracker := newProviderBringUpTracker(4)
	setTestProviderClientIDs(t, tracker)

	tracker.authReused(0, connect.NewId(), "direct")
	tracker.authNew(1, connect.NewId(), "proxy:10.0.0.1:1080")
	tracker.authFailed(2, "proxy:10.0.0.2:1080", errors.New("synthetic refused"), "in 15m")
	// Slot 3 never reported an outcome at all.

	requireProviderClientIDGauges(t, "1", "1")
	// The run-wide tally the heartbeat prints agrees here, and either way the
	// two unresolved slots are in neither reading.
	tracker.mu.Lock()
	reused, minted := tracker.identityCounts()
	tracker.mu.Unlock()
	if reused != 1 || minted != 1 {
		t.Fatalf("identityCounts = (%d reused, %d minted), want (1, 1)", reused, minted)
	}
}
