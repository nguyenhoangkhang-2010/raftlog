package money

import (
	"math"
	"math/rand"
	"testing"
)

func TestPercentRoundsToTheNearestDongHalvesAwayFromZero(t *testing.T) {
	cases := []struct {
		m    Money
		bp   int64
		want Money
	}{
		{100_000, 1000, 10_000}, // 10%
		{45_000, 1500, 6_750},   // 15%
		{10, 500, 1},            // 0.5 rounds up
		{10, 499, 0},            // 0.499 rounds down
		{25, 1000, 3},           // 2.5 rounds up
		{-25, 1000, -3},         // and a discount rounds the same way
		{99, 0, 0},
		{99, 10000, 99},
		{0, 5000, 0},
		{12_345, 3333, 4_115}, // 4114.6...
	}
	for _, c := range cases {
		if got := c.m.Percent(c.bp); got != c.want {
			t.Errorf("%v.Percent(%d) = %v, want %v", int64(c.m), c.bp, int64(got), int64(c.want))
		}
	}
}

func TestStringFormatsLikeVietnamesePrices(t *testing.T) {
	cases := map[Money]string{
		0: "0đ", 5: "5đ", 999: "999đ", 1_000: "1.000đ", 45_000: "45.000đ",
		1_234_567: "1.234.567đ", -45_000: "-45.000đ", 100_000_000: "100.000.000đ",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("Money(%d).String() = %q, want %q", int64(m), got, want)
		}
	}
}

func TestTheBoundsKeepTotalsFarFromOverflow(t *testing.T) {
	if Bound <= 0 || Bound > Money(math.MaxInt64/1000) {
		t.Fatalf("Bound = %d leaves too little room below int64", int64(Bound))
	}
	// Percent multiplies by up to 10000: the largest possible total must survive it.
	if got := Bound.Percent(10_000); got != Bound {
		t.Fatalf("100%% of the bound = %d, want the bound", int64(got))
	}
}

func TestSplitAddsUpExactly(t *testing.T) {
	got, err := Split(100, []Money{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	// 33.33 each: the extra đồng goes to the first of the equal remainders.
	if got[0] != 34 || got[1] != 33 || got[2] != 33 {
		t.Fatalf("Split(100, 1:1:1) = %v, want 34, 33, 33", got)
	}

	got, err = Split(10_000, []Money{30_000, 70_000})
	if err != nil || got[0] != 3_000 || got[1] != 7_000 {
		t.Fatalf("Split proportional = %v, %v", got, err)
	}
	// A line of weight 0 gets nothing.
	got, _ = Split(500, []Money{0, 100, 100})
	if got[0] != 0 || got[1]+got[2] != 500 {
		t.Fatalf("Split with a zero weight = %v", got)
	}
	if got, err := Split(0, []Money{1, 2}); err != nil || got[0] != 0 || got[1] != 0 {
		t.Fatalf("Split of nothing = %v, %v", got, err)
	}
	if got, err := Split(0, nil); err != nil || len(got) != 0 {
		t.Fatalf("Split(0, nil) = %v, %v", got, err)
	}
}

func TestSplitRefusesWhatCannotBeShared(t *testing.T) {
	if _, err := Split(-1, []Money{1}); err != ErrNegativeShare {
		t.Errorf("negative total: %v", err)
	}
	if _, err := Split(1, []Money{-1, 5}); err != ErrNegativeShare {
		t.Errorf("negative weight: %v", err)
	}
	if _, err := Split(1, nil); err != ErrNoWeights {
		t.Errorf("a share of nobody: %v", err)
	}
	if _, err := Split(1, []Money{0, 0}); err != ErrNoWeights {
		t.Errorf("all-zero weights: %v", err)
	}
}

// Whatever the weights, the shares add up to the total, each is within its
// weight, and each differs from the exact proportion by less than one đồng.
func TestSplitPropertiesHoldForRandomInput(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 2000; trial++ {
		n := 1 + rng.Intn(12)
		weights := make([]Money, n)
		var sum Money
		for i := range weights {
			weights[i] = Money(rng.Int63n(int64(MaxPrice) * 10))
			if rng.Intn(5) == 0 {
				weights[i] = 0
			}
			sum += weights[i]
		}
		if sum == 0 {
			continue
		}
		total := Money(rng.Int63n(int64(sum) + 1))
		shares, err := Split(total, weights)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		var got Money
		for i, s := range shares {
			if s < 0 || s > weights[i] {
				t.Fatalf("trial %d: share %d is %d for weight %d", trial, i, int64(s), int64(weights[i]))
			}
			got += s
			// |share - total*w/sum| < 1, checked in integers: |share*sum - total*w| < sum.
			diff := float64(s)*float64(sum) - float64(total)*float64(weights[i])
			if math.Abs(diff) >= float64(sum)*1.000001 {
				t.Fatalf("trial %d: share %d (%d) is not within one đồng of its proportion", trial, i, int64(s))
			}
		}
		if got != total {
			t.Fatalf("trial %d: shares add up to %d, want %d", trial, int64(got), int64(total))
		}
	}
}
