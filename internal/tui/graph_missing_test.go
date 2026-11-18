package tui

import (
	"math"
	"testing"
)

func TestGraphDistinguishesMissingFromZero(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		missing, _ := drawG(12, 2, Graph{Samples: []float64{math.NaN()}, Max: 1}, ascii)
		zero, _ := drawG(12, 2, Graph{Samples: []float64{0}, Max: 1}, ascii)
		if missing == zero {
			t.Fatalf("ascii=%v: missing and zero render identically: %q", ascii, zero)
		}
	}
}

func TestGraphBucketAveragesIgnoreMissing(t *testing.T) {
	got := plotColumns([]float64{math.NaN(), 10}, 1)
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("mixed bucket = %v", got)
	}
	_, vals := bucketColumns([]float64{math.NaN(), 10, math.NaN(), 20}, 2, 3, 4)
	if len(vals) != 2 || vals[0] != 10 || vals[1] != 20 {
		t.Fatalf("anchored buckets = %v", vals)
	}
	got = plotColumns([]float64{math.NaN(), math.NaN()}, 1)
	if !math.IsNaN(got[0]) {
		t.Fatalf("empty bucket = %v", got)
	}
}
