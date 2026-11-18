package urnettools

import (
	"math"
	"time"
)

const (
	topMinuteBuckets  = 60
	topQuarterBuckets = 96
)

// topWindow stores bounded one-minute and fifteen-minute throughput averages.
type topWindow struct {
	minuteSum    [topMinuteBuckets]float64
	minuteCount  [topMinuteBuckets]int
	minuteIndex  [topMinuteBuckets]int64
	quarterSum   [topQuarterBuckets]float64
	quarterCount [topQuarterBuckets]int
	quarterIndex [topQuarterBuckets]int64
}

func (w *topWindow) reset() { *w = topWindow{} }

func (w *topWindow) push(rate float64, at time.Time) {
	mi := at.Unix() / 60
	i := int(mod(mi, int64(topMinuteBuckets)))
	if w.minuteIndex[i] != mi {
		w.minuteSum[i], w.minuteCount[i], w.minuteIndex[i] = 0, 0, mi
	}
	w.minuteSum[i] += rate
	w.minuteCount[i]++
	qi := at.Unix() / 900
	j := int(mod(qi, int64(topQuarterBuckets)))
	if w.quarterIndex[j] != qi {
		w.quarterSum[j], w.quarterCount[j], w.quarterIndex[j] = 0, 0, qi
	}
	w.quarterSum[j] += rate
	w.quarterCount[j]++
}

func mod(a, b int64) int64 {
	r := a % b
	if r < 0 {
		r += b
	}
	return r
}

func bucket(sum []float64, count []int, index []int64, want int64, buckets int) float64 {
	i := int(mod(want, int64(buckets)))
	if index[i] != want || count[i] == 0 {
		return math.NaN()
	}
	return sum[i] / float64(count[i])
}

func (w *topWindow) windowFor(h int, now time.Time) []float64 {
	switch h {
	case 1:
		out := make([]float64, topMinuteBuckets)
		cur := now.Unix() / 60
		for i := range out {
			out[i] = bucket(w.minuteSum[:], w.minuteCount[:], w.minuteIndex[:], cur-int64(len(out)-1-i), topMinuteBuckets)
		}
		return out
	case 24:
		out := make([]float64, topQuarterBuckets)
		cur := now.Unix() / 900
		for i := range out {
			out[i] = bucket(w.quarterSum[:], w.quarterCount[:], w.quarterIndex[:], cur-int64(len(out)-1-i), topQuarterBuckets)
		}
		return out
	}
	return nil
}

func windowTitle(h int) string {
	switch h {
	case 1:
		return "last 1 hour"
	case 24:
		return "last 24 hours"
	}
	return "last 10 minutes"
}
