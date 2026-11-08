// Package promo works out discounts on an order.
//
// Promotions come in three kinds (a percentage off, a fixed amount off, buy some
// and get some free), apply to the whole order or to a category or certain items,
// and may need a code the customer types. The rules that make the result
// predictable are few, and the tests check them as properties:
//
//   - A discount never takes the total below zero, and each line never goes
//     below zero either, so a refund of a line can never exceed what it cost.
//   - The result does not depend on the order of the promotions or of the lines.
//   - Stackable promotions apply one after another by priority, each on what the
//     earlier ones left. An exclusive promotion applies alone; the customer gets
//     whichever is better, the best exclusive one or all the stackable ones.
//   - The discount is spread over the lines so that the lines add up exactly to
//     the order.
package promo

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
)

// ID names a promotion.
type ID string

// Kind is the type of discount.
type Kind string

// The kinds of promotion.
const (
	PercentOff Kind = "percent_off" // Percent basis points off the lines in scope
	FixedOff   Kind = "fixed_off"   // Amount off the lines in scope, at most what they cost
	BuyXGetY   Kind = "buy_x_get_y" // of every Buy+Free units in scope, the cheapest Free are free
)

// Scope limits a promotion to part of the order. Empty means the whole order.
type Scope struct {
	Categories []string
	Items      []menu.ItemID
}

func (s Scope) empty() bool { return len(s.Categories) == 0 && len(s.Items) == 0 }

func (s Scope) covers(l menu.Line) bool {
	if s.empty() {
		return true
	}
	for _, c := range s.Categories {
		if c == l.Category {
			return true
		}
	}
	for _, want := range s.Items {
		for _, have := range l.Items {
			if want == have {
				return true
			}
		}
	}
	return false
}

// Promotion is a discount offer.
type Promotion struct {
	ID    ID
	Name  string
	Kind  Kind
	Scope Scope
	// Code, when set, must be entered by the customer. Empty means automatic.
	Code string

	Percent int64       // PercentOff: basis points, 1..10000
	Amount  money.Money // FixedOff
	Buy     int         // BuyXGetY
	Free    int         // BuyXGetY

	// MinSubtotal is the least the lines in scope must cost, before any discount.
	MinSubtotal money.Money
	// MaxDiscount caps a PercentOff or FixedOff discount; 0 means no cap.
	MaxDiscount money.Money
	// ValidFrom and ValidTo bound the offer in time, [from, to); zero means no bound.
	ValidFrom time.Time
	ValidTo   time.Time

	// Exclusive promotions do not combine with anything else.
	Exclusive bool
	// Priority orders stackable promotions: lower goes first.
	Priority int
	// SingleUse lets each customer redeem the promotion once.
	SingleUse bool
}

// Validate checks that the promotion is well formed.
func (p Promotion) Validate() error {
	if p.ID == "" || strings.TrimSpace(p.Name) == "" {
		return errors.New("a promotion needs an ID and a name")
	}
	switch p.Kind {
	case PercentOff:
		if p.Percent < 1 || p.Percent > money.BasisPoints {
			return fmt.Errorf("percent must be 1..%d basis points, got %d", money.BasisPoints, p.Percent)
		}
	case FixedOff:
		if p.Amount < 1 || p.Amount > money.Bound {
			return fmt.Errorf("amount %d is out of range", p.Amount)
		}
	case BuyXGetY:
		if p.Buy < 1 || p.Free < 1 || p.Buy > money.MaxQuantity || p.Free > money.MaxQuantity {
			return fmt.Errorf("buy %d get %d is out of range", p.Buy, p.Free)
		}
		if p.MaxDiscount != 0 {
			return errors.New("a cap does not apply to buy-x-get-y")
		}
	default:
		return fmt.Errorf("unknown kind %q", p.Kind)
	}
	if p.MinSubtotal < 0 || p.MaxDiscount < 0 {
		return errors.New("minimum and maximum must not be negative")
	}
	if !p.ValidFrom.IsZero() && !p.ValidTo.IsZero() && !p.ValidFrom.Before(p.ValidTo) {
		return errors.New("the offer ends before it starts")
	}
	if p.Code != "" && p.Code != normalize(p.Code) {
		return fmt.Errorf("code %q must be upper case without spaces", p.Code)
	}
	return nil
}

func normalize(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// Set is a validated list of promotions.
type Set struct {
	promos []Promotion
}

// NewSet validates the promotions. IDs and codes must be unique.
func NewSet(promos []Promotion) (*Set, error) {
	ids := map[ID]bool{}
	codes := map[string]bool{}
	for _, p := range promos {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("promotion %q: %w", p.ID, err)
		}
		if ids[p.ID] {
			return nil, fmt.Errorf("promotion %q: duplicate ID", p.ID)
		}
		ids[p.ID] = true
		if p.Code != "" {
			if codes[p.Code] {
				return nil, fmt.Errorf("promotion %q: duplicate code %q", p.ID, p.Code)
			}
			codes[p.Code] = true
		}
	}
	return &Set{promos: append([]Promotion(nil), promos...)}, nil
}

// Reasons a code was not used.
var (
	ErrUnknownCode     = errors.New("promo: no such code")
	ErrNotYet          = errors.New("promo: the offer has not started")
	ErrExpired         = errors.New("promo: the offer has ended")
	ErrMinimum         = errors.New("promo: the order is below the minimum for this offer")
	ErrAlreadyRedeemed = errors.New("promo: this customer has used the offer already")
	ErrNothingToApply  = errors.New("promo: the offer applies to nothing in this order")
	ErrNotCombinable   = errors.New("promo: a better offer was applied instead")
)

// Context is what the evaluation needs to know besides the lines.
type Context struct {
	At       time.Time
	Codes    []string // codes the customer entered, in any case
	Customer string
	// Redeemed reports whether the customer has used a single-use promotion.
	// Optional: nil means nobody has.
	Redeemed func(id ID, customer string) bool
}

// Applied is one promotion that took effect.
type Applied struct {
	ID     ID
	Name   string
	Amount money.Money // the discount, positive
}

// Rejection says why an entered code, or an offer, was not applied.
type Rejection struct {
	Code string
	ID   ID
	Err  error
}

// Result is the outcome.
type Result struct {
	Subtotal money.Money
	Applied  []Applied
	Discount money.Money
	Total    money.Money
	// PerLine is the discount given to each line, in the order the lines came in.
	// They add up to Discount, and none exceeds its line.
	PerLine  []money.Money
	Rejected []Rejection
}

// Evaluate works out the discounts for the lines.
func (s *Set) Evaluate(lines []menu.Line, ctx Context) Result {
	res := Result{PerLine: make([]money.Money, len(lines))}
	for _, l := range lines {
		res.Subtotal += l.Total
	}
	res.Total = res.Subtotal

	entered := map[string]string{} // normalized -> as typed
	for _, c := range ctx.Codes {
		if n := normalize(c); n != "" {
			entered[n] = c
		}
	}
	used := map[string]bool{}

	var cands []Promotion
	for _, p := range s.promos {
		if p.Code != "" {
			if _, ok := entered[p.Code]; !ok {
				continue
			}
			used[p.Code] = true
		}
		if err := s.usable(p, lines, ctx); err != nil {
			if p.Code != "" {
				res.Rejected = append(res.Rejected, Rejection{Code: entered[p.Code], ID: p.ID, Err: err})
			}
			continue
		}
		cands = append(cands, p)
	}
	for n, typed := range entered {
		if !used[n] {
			res.Rejected = append(res.Rejected, Rejection{Code: typed, Err: ErrUnknownCode})
		}
	}
	sort.Slice(res.Rejected, func(i, j int) bool { return res.Rejected[i].Code < res.Rejected[j].Code })

	// Candidates in a fixed order, whatever order the promotions were given in.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Priority != cands[j].Priority {
			return cands[i].Priority < cands[j].Priority
		}
		return cands[i].ID < cands[j].ID
	})

	// Option A: every stackable promotion, in order.
	var stackable, exclusive []Promotion
	for _, p := range cands {
		if p.Exclusive {
			exclusive = append(exclusive, p)
		} else {
			stackable = append(stackable, p)
		}
	}
	stackRes := apply(lines, stackable)

	// Option B: the best single exclusive promotion.
	var best *outcome
	for _, p := range exclusive {
		o := apply(lines, []Promotion{p})
		if o.discount > 0 && (best == nil || o.discount > best.discount) {
			cp := o
			best = &cp
		}
	}

	chosen := stackRes
	if best != nil && best.discount > stackRes.discount {
		chosen = *best
		// The stackable promotions lost to the exclusive one: say so.
		for _, p := range stackable {
			res.Rejected = append(res.Rejected, Rejection{Code: p.Code, ID: p.ID, Err: ErrNotCombinable})
		}
	} else if best != nil {
		for _, p := range exclusive {
			res.Rejected = append(res.Rejected, Rejection{Code: p.Code, ID: p.ID, Err: ErrNotCombinable})
		}
	}
	for _, a := range chosen.applied {
		if a.Amount == 0 && a.hadCode {
			res.Rejected = append(res.Rejected, Rejection{Code: a.code, ID: a.ID, Err: ErrNothingToApply})
		}
	}
	for _, a := range chosen.applied {
		if a.Amount > 0 {
			res.Applied = append(res.Applied, Applied{ID: a.ID, Name: a.Name, Amount: a.Amount})
		}
	}
	res.Discount = chosen.discount
	res.Total = res.Subtotal - res.Discount
	copy(res.PerLine, chosen.perLine)
	return res
}

// usable checks time, minimum and single use; it does not look at the code.
func (s *Set) usable(p Promotion, lines []menu.Line, ctx Context) error {
	if !p.ValidFrom.IsZero() && ctx.At.Before(p.ValidFrom) {
		return ErrNotYet
	}
	if !p.ValidTo.IsZero() && !ctx.At.Before(p.ValidTo) {
		return ErrExpired
	}
	if p.SingleUse && ctx.Redeemed != nil && ctx.Redeemed(p.ID, ctx.Customer) {
		return ErrAlreadyRedeemed
	}
	var inScope money.Money
	for _, l := range lines {
		if p.Scope.covers(l) {
			inScope += l.Total
		}
	}
	if inScope == 0 && len(lines) > 0 && !p.Scope.empty() {
		return ErrNothingToApply
	}
	if inScope < p.MinSubtotal {
		return ErrMinimum
	}
	return nil
}

type appliedRow struct {
	Applied
	hadCode bool
	code    string
}

type outcome struct {
	applied  []appliedRow
	discount money.Money
	perLine  []money.Money
}

// apply runs promotions one after another over the lines.
func apply(lines []menu.Line, promos []Promotion) outcome {
	rem := make([]money.Money, len(lines)) // what each line still costs
	for i, l := range lines {
		rem[i] = l.Total
	}
	out := outcome{perLine: make([]money.Money, len(lines))}
	for _, p := range promos {
		alloc := discountFor(p, lines, rem)
		var amount money.Money
		for i, a := range alloc {
			rem[i] -= a
			out.perLine[i] += a
			amount += a
		}
		out.discount += amount
		out.applied = append(out.applied, appliedRow{
			Applied: Applied{ID: p.ID, Name: p.Name, Amount: amount},
			hadCode: p.Code != "", code: p.Code,
		})
	}
	return out
}

// discountFor returns how much of the discount of p falls on each line, given what
// each line still costs. No share exceeds what its line still costs.
func discountFor(p Promotion, lines []menu.Line, rem []money.Money) []money.Money {
	alloc := make([]money.Money, len(lines))
	var inScope []int
	var base money.Money
	for i, l := range lines {
		if p.Scope.covers(l) {
			inScope = append(inScope, i)
			base += rem[i]
		}
	}
	if len(inScope) == 0 || base == 0 {
		return alloc
	}

	var amount money.Money
	switch p.Kind {
	case PercentOff:
		amount = base.Percent(p.Percent)
	case FixedOff:
		amount = p.Amount
	case BuyXGetY:
		return buyXGetY(p, lines, rem, inScope)
	}
	if p.MaxDiscount > 0 {
		amount = money.Min(amount, p.MaxDiscount)
	}
	amount = money.Min(amount, base)

	weights := make([]money.Money, len(inScope))
	for k, i := range inScope {
		weights[k] = rem[i]
	}
	shares, err := money.Split(amount, weights)
	if err != nil { // cannot happen: amount <= base and base > 0
		return alloc
	}
	for k, i := range inScope {
		alloc[i] = shares[k]
	}
	return alloc
}

// buyXGetY makes free the cheapest units in scope: of every Buy+Free units, Free.
func buyXGetY(p Promotion, lines []menu.Line, rem []money.Money, inScope []int) []money.Money {
	alloc := make([]money.Money, len(lines))
	type unit struct {
		line  int
		price money.Money
	}
	var units []unit
	for _, i := range inScope {
		for q := 0; q < lines[i].Quantity; q++ {
			units = append(units, unit{i, lines[i].Unit})
		}
	}
	free := len(units) / (p.Buy + p.Free) * p.Free
	if free == 0 {
		return alloc
	}
	sort.SliceStable(units, func(a, b int) bool { return units[a].price < units[b].price })
	for _, u := range units[:free] {
		give := money.Min(u.price, rem[u.line]-alloc[u.line])
		alloc[u.line] += give
	}
	return alloc
}
