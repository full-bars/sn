package miner

import (
	"testing"
	"time"
)

func resetShedForTest(t *testing.T) *h3OutageShedder {
	t.Helper()
	shedder := &h3OutageShedder{}
	resetH3OutageShed(shedder)
	t.Cleanup(func() { resetH3OutageShed(shedder) })
	return shedder
}

func TestH3OutageShed_ShedsAtOnceOnAnOutageReading(t *testing.T) {
	shedder := resetShedForTest(t)
	now := time.Unix(1_000_000, 0)
	if shed, changed := shedder.observe(now, false, true); shed || changed {
		t.Fatal("a healthy platform sheds nothing")
	}
	if shed, changed := shedder.observe(now.Add(time.Second), true, false); !shed || !changed {
		t.Fatal("an outage reading must shed on the reading that sees it")
	}
	if shed, changed := shedder.observe(now.Add(2*time.Second), true, false); !shed || changed {
		t.Fatal("staying in an outage is not a change")
	}
}

func TestH3OutageShed_ReleasesOnlyAfterRecoveryHoldsForTheWholeHold(t *testing.T) {
	shedder := resetShedForTest(t)
	now := time.Unix(1_000_000, 0)
	shedder.observe(now, true, false)

	if shed, changed := shedder.observe(now.Add(time.Second), false, true); !shed || changed {
		t.Fatal("one recovered reading must not restore H3")
	}
	if shed, _ := shedder.observe(now.Add(time.Second+h3OutageShedClearHold-time.Second), false, true); !shed {
		t.Fatal("the hold has not elapsed, H3 must stay shed")
	}
	if shed, changed := shedder.observe(now.Add(time.Second+h3OutageShedClearHold), false, true); shed || !changed {
		t.Fatal("recovered for the whole hold must restore H3")
	}
}

// A reading that is neither an outage nor a recovery (the platform answers a
// little, or not enough) is not grounds to give H3 back, and it restarts the hold.
func TestH3OutageShed_AnAmbiguousReadingKeepsTheShedAndRestartsTheHold(t *testing.T) {
	shedder := resetShedForTest(t)
	now := time.Unix(1_000_000, 0)
	shedder.observe(now, true, false)
	shedder.observe(now.Add(time.Second), false, true) // recovery starts
	shedder.observe(now.Add(h3OutageShedClearHold-time.Second), false, false)
	// the hold would have run out here had the ambiguous reading not restarted it
	if shed, _ := shedder.observe(now.Add(h3OutageShedClearHold+time.Second), false, true); !shed {
		t.Fatal("an ambiguous reading must restart the hold")
	}
	if shed, changed := shedder.observe(now.Add(2*h3OutageShedClearHold+time.Second), false, true); shed || !changed {
		t.Fatal("a full hold after the ambiguous reading must restore")
	}
}

func TestH3OutageShed_AFlapBackIntoAnOutageRestartsTheHold(t *testing.T) {
	shedder := resetShedForTest(t)
	now := time.Unix(1_000_000, 0)
	shedder.observe(now, true, false)
	shedder.observe(now.Add(time.Second), false, true)
	shedder.observe(now.Add(h3OutageShedClearHold-time.Second), true, false)
	shedder.observe(now.Add(h3OutageShedClearHold), false, true)
	if shed, _ := shedder.observe(now.Add(2*h3OutageShedClearHold-time.Second), false, true); !shed {
		t.Fatal("the hold counts from the last recovered reading after the flap")
	}
}

func TestH3OutageShed_NoProxyIsEligibleWhileShedding(t *testing.T) {
	shedder := resetShedForTest(t)
	key := testH3Proxies(1)[0].Key()
	all := h3Setting{Mode: h3ModeAll}
	if !h3ProxyEligible(all, key) {
		t.Fatal("setup: h3 all admits a proxy")
	}
	shedder.observe(time.Unix(1_000_000, 0), true, false)
	if h3ProxyEligible(all, key) {
		t.Fatal("a proxy kept H3 through an outage")
	}
	h3RankingValue.Store(&h3Ranking{cap: 1, keys: map[string]bool{key: true}})
	t.Cleanup(func() { h3RankingValue.Store(nil) })
	if h3ProxyEligible(h3Setting{Mode: h3ModeTop, Cap: 1}, key) {
		t.Fatal("a ranked member kept H3 through an outage")
	}
}

// The relaunch pass follows eligibility, so shedding is what restarts the
// identities that were launched with H3, and restoring is what gives it back.
func TestH3OutageShed_RelaunchListFollowsTheShed(t *testing.T) {
	shedder := resetShedForTest(t)
	t.Cleanup(func() {
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
	})
	h3LaunchMutex.Lock()
	h3LaunchEligible = map[string]bool{}
	h3LaunchMutex.Unlock()
	candidates := testH3Proxies(3)
	for _, c := range candidates {
		recordH3Launch(c.Key(), true)
	}
	all := h3Setting{Mode: h3ModeAll}
	if got := h3ProxiesToRelaunch(all); len(got) != 0 {
		t.Fatalf("launched as eligible and still eligible, got %v", got)
	}
	now := time.Unix(1_000_000, 0)
	shedder.observe(now, true, false)
	if got := h3ProxiesToRelaunch(all); len(got) != 3 {
		t.Fatalf("every H3 identity must be relaunched without it, got %v", got)
	}
	for _, c := range candidates {
		recordH3Launch(c.Key(), false)
	}
	shedder.observe(now.Add(time.Second), false, true)
	shedder.observe(now.Add(time.Second+h3OutageShedClearHold), false, true)
	if got := h3ProxiesToRelaunch(all); len(got) != 3 {
		t.Fatalf("restoring must relaunch them with H3, got %v", got)
	}
}
