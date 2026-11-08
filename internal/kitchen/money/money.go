// Package money is the currency of the kitchen system: Vietnamese đồng, as whole
// numbers. There are no fractions of a đồng, and no floating point anywhere near a
// price, so a total is the same on every machine and every replay.
//
// Overflow is excluded by bounds instead of checked at every addition: a price is
// at most MaxPrice, a line holds at most MaxQuantity of it, an order has at most
// MaxLines lines, so no total can come near the limit of an int64. The packages
// that accept prices enforce the first bound; Bound documents the product.
package money

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// Money is an amount in đồng. It may be negative (a discount, a refund).
type Money int64

// MaxPrice is the largest price of one unit: 100 million đồng.
const MaxPrice Money = 100_000_000

// Bounds on how much one line and one order may hold.
const (
	MaxQuantity = 99
	MaxLines    = 100
)

// Bound is the largest total an order can reach: MaxPrice * MaxQuantity * MaxLines.
// It keeps every total far from int64 overflow.
const Bound Money = MaxPrice * MaxQuantity * MaxLines

// ErrNegativeShare is returned by Split for a negative total or weight.
var ErrNegativeShare = errors.New("money: cannot split a negative amount")

// ErrNoWeights is returned by Split when there is something to share and nobody
// to share it with: no weights, or weights that are all zero.
var ErrNoWeights = errors.New("money: nothing to split the amount in proportion to")

// BasisPoints is a percentage in hundredths of a percent: 10000 is 100%.
const BasisPoints = 10000

// Percent returns bp basis points of m, rounded to the nearest đồng, halves away
// from zero. Percent(1050) is 10.5%.
func (m Money) Percent(bp int64) Money {
	if m < 0 {
		return -(-m).Percent(bp)
	}
	return Money((int64(m)*bp + BasisPoints/2) / BasisPoints)
}

// Times returns m * n.
func (m Money) Times(n int) Money { return m * Money(n) }

// Min returns the smaller of two amounts.
func Min(a, b Money) Money {
	if a < b {
		return a
	}
	return b
}

// Max returns the larger of two amounts.
func Max(a, b Money) Money {
	if a > b {
		return a
	}
	return b
}

// String formats m the Vietnamese way: dots between thousands and a trailing
// "đ", for example 45.000đ.
func (m Money) String() string {
	sign := ""
	n := int64(m)
	if n < 0 {
		sign, n = "-", -n
	}
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	return fmt.Sprintf("%s%sđ", sign, b.String())
}

// Split shares total among the weights in proportion to them. The shares add up
// to exactly total: what rounding takes from one is given to another, the largest
// remainders first, and ties go to the earlier weight. This is how an order-level
// discount is spread over the lines, so that the lines add up to the order.
//
// Neither the total nor a weight may be negative. When total does not exceed the
// sum of the weights, as with a discount over the lines it applies to, no share is
// larger than its weight.
func Split(total Money, weights []Money) ([]Money, error) {
	if total < 0 {
		return nil, ErrNegativeShare
	}
	var sum uint64
	for _, w := range weights {
		if w < 0 {
			return nil, ErrNegativeShare
		}
		sum += uint64(w)
	}
	out := make([]Money, len(weights))
	if total == 0 {
		return out, nil
	}
	if sum == 0 {
		return nil, ErrNoWeights
	}

	type rem struct {
		i int
		r uint64
	}
	rems := make([]rem, len(weights))
	var given uint64
	for i, w := range weights {
		hi, lo := bits.Mul64(uint64(total), uint64(w)) // total*w can exceed 64 bits
		q, r := bits.Div64(hi, lo, sum)                // the quotient is at most total, so it fits
		out[i] = Money(q)
		rems[i] = rem{i, r}
		given += q
	}
	// Hand out what is left, one đồng each, to the largest remainders.
	left := uint64(total) - given
	for ; left > 0; left-- {
		best := -1
		for j, r := range rems {
			if best < 0 || r.r > rems[best].r {
				best = j
			}
		}
		out[rems[best].i]++
		rems[best].r = 0 // used up; the next largest goes next (ties: earlier index wins)
	}
	return out, nil
}
