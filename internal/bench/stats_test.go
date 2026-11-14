package bench

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestPercentileUsesTheNearestRank(t *testing.T) {
	var s []time.Duration
	for i := 1; i <= 100; i++ {
		s = append(s, ms(i))
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{
		{0.50, ms(50)}, {0.95, ms(95)}, {0.99, ms(99)}, {1, ms(100)}, {0.001, ms(1)},
	} {
		if got := Percentile(s, c.q); got != c.want {
			t.Errorf("p%v of 1..100 ms = %v, want %v", c.q*100, got, c.want)
		}
	}
	// With ten samples the 95th percentile is the tenth, not the ninth.
	ten := s[:10]
	if got := Percentile(ten, 0.95); got != ms(10) {
		t.Errorf("p95 of 1..10 ms = %v, want 10ms", got)
	}
	if got := Percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile of nothing = %v, want 0", got)
	}
}

func TestSummarizeDoesNotChangeItsInputAndFindsTheExtremes(t *testing.T) {
	in := []time.Duration{ms(30), ms(10), ms(20)}
	got := Summarize(in)
	if in[0] != ms(30) || in[1] != ms(10) || in[2] != ms(20) {
		t.Fatalf("input was reordered: %v", in)
	}
	if got.Count != 3 || got.Min != ms(10) || got.Max != ms(30) || got.Mean != ms(20) || got.P50 != ms(20) {
		t.Fatalf("summary = %+v", got)
	}
	if (Summary{}) != Summarize(nil) {
		t.Fatalf("summary of nothing = %+v, want zero", Summarize(nil))
	}
}

func TestMillisKeepsTheFraction(t *testing.T) {
	if got := Millis(1500 * time.Microsecond); got != 1.5 {
		t.Fatalf("Millis(1.5ms) = %v", got)
	}
}
