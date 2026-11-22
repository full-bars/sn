package miner

import (
	"fmt"
	"testing"
	"time"
)

func TestMeasurementSweepOrder(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	candidates := []measurementCandidate{
		{key: "later", udpAt: now.Add(-20 * time.Minute), udpInterval: 10 * time.Minute},
		{key: "first", udpAt: now.Add(-time.Hour), udpInterval: 10 * time.Minute},
		{key: "never"},
		{key: "v6", udpAt: now.Add(-time.Minute), udpInterval: time.Hour, ipv6Due: now.Add(-time.Hour)},
		{key: "cancelled", attemptedAt: now.Add(-time.Minute)},
	}
	got := selectMeasurementSweep(now, candidates, 5)
	want := []string{"never", "v6", "first", "later", "cancelled"}
	for i, key := range want {
		if got[i].key != key {
			t.Fatalf("position %d = %s, want %s", i, got[i].key, key)
		}
	}
}

func TestMeasurementSweepClockAndTies(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	candidates := []measurementCandidate{{key: "future", udpAt: now.Add(time.Hour), udpInterval: time.Minute}}
	for i := 99; i >= 0; i-- {
		candidates = append(candidates, measurementCandidate{key: fmt.Sprintf("%03d", i)})
	}
	got := selectMeasurementSweep(now, candidates, 100)
	if len(got) != 100 {
		t.Fatalf("got %d candidates", len(got))
	}
	for i, candidate := range got {
		if candidate.key != fmt.Sprintf("%03d", i) {
			t.Fatalf("position %d = %s", i, candidate.key)
		}
	}
}
