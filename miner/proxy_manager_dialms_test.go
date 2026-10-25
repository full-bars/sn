package miner

import (
	"testing"
	"time"
)

// The median is what makes this number trustworthy on a shared path: one
// stalled host must not move it, and an empty pass must report nothing rather
// than a zero that reads like a real measurement.
func TestMedianDialMS(t *testing.T) {
	cases := []struct {
		name      string
		latencies []time.Duration
		want      int64
	}{
		{
			name:      "no answered targets reports nothing",
			latencies: nil,
			want:      0,
		},
		{
			name:      "single sample",
			latencies: []time.Duration{1200 * time.Millisecond},
			want:      1200,
		},
		{
			name: "unsorted samples take the middle value",
			latencies: []time.Duration{
				3100 * time.Millisecond,
				900 * time.Millisecond,
				1500 * time.Millisecond,
			},
			want: 1500,
		},
		{
			name: "one stalled host does not move the median",
			latencies: []time.Duration{
				200 * time.Millisecond,
				300 * time.Millisecond,
				400 * time.Millisecond,
				30000 * time.Millisecond,
			},
			// Upper-middle of the sorted set. A mean here would be ~7.7s.
			want: 400,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := medianDialMS(tc.latencies); got != tc.want {
				t.Fatalf("medianDialMS(%v) = %d, want %d", tc.latencies, got, tc.want)
			}
		})
	}
}

// medianDialMS must not reorder its input: callers pass a slice they still own.
func TestMedianDialMSDoesNotMutateInput(t *testing.T) {
	latencies := []time.Duration{
		3000 * time.Millisecond,
		1000 * time.Millisecond,
		2000 * time.Millisecond,
	}
	medianDialMS(latencies)
	if latencies[0] != 3000*time.Millisecond ||
		latencies[1] != 1000*time.Millisecond ||
		latencies[2] != 2000*time.Millisecond {
		t.Fatalf("input was reordered: %v", latencies)
	}
}
