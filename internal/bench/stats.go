// Package bench measures a raftlog cluster: how many messages per second it takes in
// and gives back, how long a request takes, and how long writes stop when the leader
// is killed.
//
// It reports what it measured and nothing else. Every figure comes from the clock of
// the process that ran the measurement, over a window that starts after a warm-up, and
// the result carries the settings that produced it, so that a number is never separated
// from the question it answers.
package bench

import (
	"sort"
	"time"
)

// Summary describes a set of durations.
type Summary struct {
	Count int           `json:"count"`
	Min   time.Duration `json:"min_ns"`
	Mean  time.Duration `json:"mean_ns"`
	P50   time.Duration `json:"p50_ns"`
	P95   time.Duration `json:"p95_ns"`
	P99   time.Duration `json:"p99_ns"`
	Max   time.Duration `json:"max_ns"`
}

// Summarize computes the summary of durs. The input is not changed. Percentiles use the
// nearest-rank method: the p-th percentile is the smallest value that at least p of the
// samples do not exceed.
func Summarize(durs []time.Duration) Summary {
	if len(durs) == 0 {
		return Summary{}
	}
	s := append([]time.Duration(nil), durs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var total time.Duration
	for _, d := range s {
		total += d
	}
	return Summary{
		Count: len(s),
		Min:   s[0],
		Mean:  total / time.Duration(len(s)),
		P50:   Percentile(s, 0.50),
		P95:   Percentile(s, 0.95),
		P99:   Percentile(s, 0.99),
		Max:   s[len(s)-1],
	}
}

// Percentile returns the nearest-rank q-quantile (0 < q <= 1) of an ascending slice, or
// zero for an empty one.
func Percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	// ceil(q*n) computed without floating-point surprises for exact products.
	rank := int(q * float64(len(sorted)))
	if float64(rank) < q*float64(len(sorted))-1e-9 {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Millis converts a duration to milliseconds with a fraction.
func Millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
