package promo

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
)

var noon = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func line(category string, unit money.Money, qty int, items ...menu.ItemID) menu.Line {
	return menu.Line{
		Kind: "item", Ref: fmt.Sprint(category, unit), Name: category, Category: category,
		Unit: unit, Quantity: qty, Total: unit.Times(qty), Items: items,
	}
}

func set(t *testing.T, ps ...Promotion) *Set {
	t.Helper()
	s, err := NewSet(ps)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ctx(codes ...string) Context { return Context{At: noon, Codes: codes} }

func sum(ms []money.Money) money.Money {
	var t money.Money
	for _, m := range ms {
		t += m
	}
	return t
}

func TestAPercentageOffTheWholeOrder(t *testing.T) {
	s := set(t, Promotion{ID: "ten", Name: "10% off", Kind: PercentOff, Percent: 1000})
	r := s.Evaluate([]menu.Line{line("drinks", 25_000, 2), line("food", 50_000, 1)}, ctx())
	if r.Subtotal != 100_000 || r.Discount != 10_000 || r.Total != 90_000 {
		t.Fatalf("result = %+v", r)
	}
	if len(r.Applied) != 1 || r.Applied[0].ID != "ten" || r.Applied[0].Amount != 10_000 {
		t.Fatalf("applied = %+v", r.Applied)
	}
	// The discount is spread over the lines in proportion, and adds up.
	if r.PerLine[0] != 5_000 || r.PerLine[1] != 5_000 || sum(r.PerLine) != r.Discount {
		t.Fatalf("per line = %v", r.PerLine)
	}
}

func TestTheLinesAlwaysAddUpToTheDiscount(t *testing.T) {
	// 7% of three lines of 10.001đ: the shares round, and must still add up.
	s := set(t, Promotion{ID: "seven", Name: "7%", Kind: PercentOff, Percent: 700})
	r := s.Evaluate([]menu.Line{line("a", 10_001, 1), line("b", 10_001, 1), line("c", 10_001, 1)}, ctx())
	if sum(r.PerLine) != r.Discount || r.Discount != money.Money(30_003).Percent(700) {
		t.Fatalf("per line %v does not add up to %v", r.PerLine, r.Discount)
	}
}

func TestAFixedDiscountNeverTakesTheTotalBelowZero(t *testing.T) {
	s := set(t, Promotion{ID: "fifty", Name: "50k off", Kind: FixedOff, Amount: 50_000})
	r := s.Evaluate([]menu.Line{line("food", 30_000, 1)}, ctx())
	if r.Discount != 30_000 || r.Total != 0 || r.PerLine[0] != 30_000 {
		t.Fatalf("result = %+v: the discount must stop at what the order costs", r)
	}
}

func TestACapLimitsADiscount(t *testing.T) {
	s := set(t, Promotion{ID: "cap", Name: "20% up to 15k", Kind: PercentOff, Percent: 2000, MaxDiscount: 15_000})
	if r := s.Evaluate([]menu.Line{line("food", 200_000, 1)}, ctx()); r.Discount != 15_000 {
		t.Fatalf("discount = %v, want the 15.000đ cap", r.Discount)
	}
	if r := s.Evaluate([]menu.Line{line("food", 50_000, 1)}, ctx()); r.Discount != 10_000 {
		t.Fatalf("discount = %v, want 10.000đ: below the cap", r.Discount)
	}
}

func TestAMinimumSpendIsMeasuredBeforeAnyDiscount(t *testing.T) {
	s := set(t,
		Promotion{ID: "auto", Name: "auto 10%", Kind: PercentOff, Percent: 1000, MinSubtotal: 100_000},
		Promotion{ID: "code", Name: "code 5k", Kind: FixedOff, Amount: 5_000, MinSubtotal: 100_000, Code: "BIG"},
	)
	r := s.Evaluate([]menu.Line{line("food", 90_000, 1)}, ctx("big"))
	if r.Discount != 0 {
		t.Fatalf("discount = %v below the minimum", r.Discount)
	}
	// The offer that needs a code is explained; the automatic one is just silent.
	if len(r.Rejected) != 1 || !errors.Is(r.Rejected[0].Err, ErrMinimum) || r.Rejected[0].Code != "big" {
		t.Fatalf("rejected = %+v", r.Rejected)
	}
	// Exactly at the minimum counts. 100.000đ: 10% = 10.000đ, then 5.000đ off the rest.
	r = s.Evaluate([]menu.Line{line("food", 100_000, 1)}, ctx("BIG"))
	if r.Discount != 15_000 || r.Total != 85_000 {
		t.Fatalf("at the minimum: %+v", r)
	}
}

func TestAScopeLimitsWhatIsDiscounted(t *testing.T) {
	drinks := set(t, Promotion{ID: "d", Name: "20% off drinks", Kind: PercentOff, Percent: 2000, Scope: Scope{Categories: []string{"drinks"}}})
	r := drinks.Evaluate([]menu.Line{line("drinks", 25_000, 2), line("food", 40_000, 1)}, ctx())
	if r.Discount != 10_000 || r.PerLine[0] != 10_000 || r.PerLine[1] != 0 {
		t.Fatalf("result = %+v: only the drinks are discounted", r)
	}

	// A scope on an item reaches the items inside a combo, too.
	coffee := set(t, Promotion{ID: "c", Name: "coffee 10k off", Kind: FixedOff, Amount: 10_000, Scope: Scope{Items: []menu.ItemID{"coffee"}}})
	combo := menu.Line{Kind: "combo", Name: "combo", Category: "combo", Unit: 60_000, Quantity: 1, Total: 60_000, Items: []menu.ItemID{"banh-mi", "coffee"}}
	r = coffee.Evaluate([]menu.Line{combo, line("food", 30_000, 1, "banh-mi")}, ctx())
	if r.Discount != 10_000 || r.PerLine[0] != 10_000 || r.PerLine[1] != 0 {
		t.Fatalf("result = %+v: the combo contains coffee", r)
	}

	// A scope that matches nothing in the order does nothing, and no minimum is met.
	if r := drinks.Evaluate([]menu.Line{line("food", 40_000, 1)}, ctx()); r.Discount != 0 || len(r.Applied) != 0 {
		t.Fatalf("a promotion for drinks discounted a food-only order: %+v", r)
	}
}

func TestBuyTwoGetOneFreeTakesTheCheapest(t *testing.T) {
	s := set(t, Promotion{ID: "b2g1", Name: "buy 2 get 1", Kind: BuyXGetY, Buy: 2, Free: 1, Scope: Scope{Categories: []string{"drinks"}}})

	// Three drinks in two lines: the cheapest one is free.
	r := s.Evaluate([]menu.Line{line("drinks", 25_000, 2), line("drinks", 20_000, 1)}, ctx())
	if r.Discount != 20_000 || r.PerLine[1] != 20_000 || r.PerLine[0] != 0 {
		t.Fatalf("three drinks: %+v", r)
	}
	// Two are not enough; five give one free, six give two.
	if r := s.Evaluate([]menu.Line{line("drinks", 25_000, 2)}, ctx()); r.Discount != 0 {
		t.Fatalf("two drinks: %v", r.Discount)
	}
	if r := s.Evaluate([]menu.Line{line("drinks", 25_000, 5)}, ctx()); r.Discount != 25_000 {
		t.Fatalf("five drinks: %v", r.Discount)
	}
	if r := s.Evaluate([]menu.Line{line("drinks", 25_000, 3), line("drinks", 30_000, 3)}, ctx()); r.Discount != 50_000 {
		t.Fatalf("six drinks of two prices: %v, want the two cheapest free", r.Discount)
	}
	// Food does not count towards it.
	if r := s.Evaluate([]menu.Line{line("drinks", 25_000, 2), line("food", 10_000, 1)}, ctx()); r.Discount != 0 {
		t.Fatalf("a drink and food: %v", r.Discount)
	}
}

func TestStackablePromotionsApplyByPriorityOnWhatIsLeft(t *testing.T) {
	pct := Promotion{ID: "pct", Name: "10%", Kind: PercentOff, Percent: 1000, Priority: 1}
	fix := Promotion{ID: "fix", Name: "5k", Kind: FixedOff, Amount: 5_000, Priority: 2}
	lines := []menu.Line{line("food", 100_000, 1)}

	r := set(t, pct, fix).Evaluate(lines, ctx())
	if r.Total != 85_000 || len(r.Applied) != 2 || r.Applied[0].ID != "pct" {
		t.Fatalf("percentage first: %+v", r)
	}
	// With the priorities swapped the 5k comes off first, then 10% of 95k = 9.500.
	pct.Priority, fix.Priority = 2, 1
	r = set(t, pct, fix).Evaluate(lines, ctx())
	if r.Total != 85_500 || r.Applied[0].ID != "fix" {
		t.Fatalf("fixed first: %+v", r)
	}
}

func TestAnExclusiveOfferCompetesWithTheStackableOnes(t *testing.T) {
	a := Promotion{ID: "a", Name: "10%", Kind: PercentOff, Percent: 1000}
	b := Promotion{ID: "b", Name: "5k", Kind: FixedOff, Amount: 5_000, Priority: 1}
	big := Promotion{ID: "big", Name: "30% exclusive", Kind: PercentOff, Percent: 3000, Exclusive: true}
	small := Promotion{ID: "small", Name: "5% exclusive", Kind: PercentOff, Percent: 500, Exclusive: true}
	lines := []menu.Line{line("food", 100_000, 1)}

	// 30% beats 10% + 5k: it applies alone.
	r := set(t, a, b, big).Evaluate(lines, ctx())
	if r.Discount != 30_000 || len(r.Applied) != 1 || r.Applied[0].ID != "big" {
		t.Fatalf("big exclusive: %+v", r)
	}
	if !hasRejection(r, ErrNotCombinable, "a") || !hasRejection(r, ErrNotCombinable, "b") {
		t.Fatalf("the stackable offers that lost should be reported: %+v", r.Rejected)
	}
	// 5% loses to 10% + 5k: the stackable ones apply, the exclusive one is reported.
	r = set(t, a, b, small).Evaluate(lines, ctx())
	if r.Discount != 15_000 || len(r.Applied) != 2 || !hasRejection(r, ErrNotCombinable, "small") {
		t.Fatalf("small exclusive: %+v", r)
	}
	// Of two exclusive offers, the better one wins.
	r = set(t, small, big).Evaluate(lines, ctx())
	if r.Discount != 30_000 || r.Applied[0].ID != "big" {
		t.Fatalf("two exclusives: %+v", r)
	}
}

func TestWhenAnExclusiveOfferAndTheStackableOnesAreWorthTheSameTheStackableOnesApply(t *testing.T) {
	// 10% + 5.000đ off 100.000đ is 15.000đ, as is a 15% exclusive offer.
	s := set(t,
		Promotion{ID: "a", Name: "10%", Kind: PercentOff, Percent: 1000},
		Promotion{ID: "b", Name: "5k", Kind: FixedOff, Amount: 5_000, Priority: 1},
		Promotion{ID: "ex", Name: "15% exclusive", Kind: PercentOff, Percent: 1500, Exclusive: true},
	)
	r := s.Evaluate([]menu.Line{line("food", 100_000, 1)}, ctx())
	if r.Discount != 15_000 || len(r.Applied) != 2 || !hasRejection(r, ErrNotCombinable, "ex") {
		t.Fatalf("a tie: %+v", r)
	}
}

func hasRejection(r Result, want error, id ID) bool {
	for _, x := range r.Rejected {
		if x.ID == id && errors.Is(x.Err, want) {
			return true
		}
	}
	return false
}

func TestCodes(t *testing.T) {
	s := set(t,
		Promotion{ID: "lunch", Name: "lunch 10%", Kind: PercentOff, Percent: 1000, Code: "LUNCH10"},
		Promotion{ID: "once", Name: "welcome 20k", Kind: FixedOff, Amount: 20_000, Code: "WELCOME", SingleUse: true},
		Promotion{ID: "future", Name: "soon", Kind: FixedOff, Amount: 1_000, Code: "SOON", ValidFrom: noon.Add(time.Hour)},
		Promotion{ID: "past", Name: "over", Kind: FixedOff, Amount: 1_000, Code: "OLD", ValidTo: noon},
	)
	lines := []menu.Line{line("food", 100_000, 1)}

	// Any case, with spaces around.
	if r := s.Evaluate(lines, ctx("  lunch10 ")); r.Discount != 10_000 {
		t.Fatalf("lower-case code: %+v", r)
	}
	// Without the code nothing applies.
	if r := s.Evaluate(lines, ctx()); r.Discount != 0 {
		t.Fatalf("a code-only offer applied without its code: %+v", r)
	}
	// Reasons.
	r := s.Evaluate(lines, ctx("nope", "soon", "old"))
	want := map[string]error{"nope": ErrUnknownCode, "soon": ErrNotYet, "old": ErrExpired}
	if len(r.Rejected) != 3 {
		t.Fatalf("rejected = %+v", r.Rejected)
	}
	for _, x := range r.Rejected {
		if !errors.Is(x.Err, want[x.Code]) {
			t.Errorf("code %q: %v, want %v", x.Code, x.Err, want[x.Code])
		}
	}
	// Single use is per customer.
	used := map[string]bool{"ann": true}
	c := Context{At: noon, Codes: []string{"WELCOME"}, Customer: "ann", Redeemed: func(_ ID, who string) bool { return used[who] }}
	if r := s.Evaluate(lines, c); r.Discount != 0 || !hasRejection(r, ErrAlreadyRedeemed, "once") {
		t.Fatalf("ann used it already: %+v", r)
	}
	c.Customer = "bob"
	if r := s.Evaluate(lines, c); r.Discount != 20_000 {
		t.Fatalf("bob has not: %+v", r)
	}
	// A code that is valid but gives nothing is reported, not silently dropped.
	r = set(t, Promotion{ID: "d", Name: "drinks", Kind: PercentOff, Percent: 1000, Code: "DRINK", Scope: Scope{Categories: []string{"drinks"}}}).
		Evaluate(lines, ctx("DRINK"))
	if !hasRejection(r, ErrNothingToApply, "d") {
		t.Fatalf("a drinks code on a food order: %+v", r)
	}
}

func TestAnOfferIsValidFromTheStartAndUntilTheEnd(t *testing.T) {
	s := set(t, Promotion{ID: "w", Name: "window", Kind: FixedOff, Amount: 1_000, ValidFrom: noon, ValidTo: noon.Add(time.Hour)})
	lines := []menu.Line{line("food", 10_000, 1)}
	at := func(d time.Duration) money.Money {
		return s.Evaluate(lines, Context{At: noon.Add(d)}).Discount
	}
	if at(-time.Second) != 0 || at(0) != 1_000 || at(time.Hour-time.Second) != 1_000 || at(time.Hour) != 0 {
		t.Fatal("the window is [from, to)")
	}
}

func TestAnEmptyOrderGetsNothing(t *testing.T) {
	s := set(t, Promotion{ID: "x", Name: "x", Kind: PercentOff, Percent: 1000})
	if r := s.Evaluate(nil, ctx()); r.Discount != 0 || r.Total != 0 || len(r.PerLine) != 0 {
		t.Fatalf("result = %+v", r)
	}
}

func TestPromotionValidation(t *testing.T) {
	ok := func() Promotion { return Promotion{ID: "p", Name: "p", Kind: PercentOff, Percent: 1000} }
	cases := map[string]func(*Promotion){
		"no id":               func(p *Promotion) { p.ID = "" },
		"no name":             func(p *Promotion) { p.Name = " " },
		"unknown kind":        func(p *Promotion) { p.Kind = "free" },
		"zero percent":        func(p *Promotion) { p.Percent = 0 },
		"over 100 percent":    func(p *Promotion) { p.Percent = 10_001 },
		"fixed with no sum":   func(p *Promotion) { p.Kind, p.Amount = FixedOff, 0 },
		"buy with no free":    func(p *Promotion) { p.Kind, p.Buy, p.Free = BuyXGetY, 2, 0 },
		"a cap on buy-get":    func(p *Promotion) { p.Kind, p.Buy, p.Free, p.MaxDiscount = BuyXGetY, 2, 1, 5 },
		"negative minimum":    func(p *Promotion) { p.MinSubtotal = -1 },
		"ends before start":   func(p *Promotion) { p.ValidFrom, p.ValidTo = noon, noon.Add(-time.Hour) },
		"a code in lowercase": func(p *Promotion) { p.Code = "abc" },
	}
	for name, mutate := range cases {
		p := ok()
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ok().Validate(); err != nil {
		t.Fatalf("a good promotion: %v", err)
	}
	if _, err := NewSet([]Promotion{ok(), ok()}); err == nil {
		t.Error("a duplicate ID was accepted")
	}
	a, b := ok(), ok()
	a.Code, b.ID, b.Code = "SAME", "q", "SAME"
	if _, err := NewSet([]Promotion{a, b}); err == nil {
		t.Error("a duplicate code was accepted")
	}
}

// Random orders and random promotions: the guarantees hold for all of them.
func TestPropertiesHoldForRandomOrders(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	cats := []string{"drinks", "food", "dessert"}
	for trial := 0; trial < 1500; trial++ {
		var lines []menu.Line
		for i, n := 0, 1+rng.Intn(6); i < n; i++ {
			lines = append(lines, line(cats[rng.Intn(3)], money.Money(1+rng.Intn(120))*1_000+money.Money(rng.Intn(1000)), 1+rng.Intn(5)))
		}
		var promos []Promotion
		for i, n := 0, rng.Intn(5); i < n; i++ {
			p := Promotion{ID: ID(fmt.Sprint("p", i)), Name: "p", Priority: rng.Intn(3), Exclusive: rng.Intn(4) == 0}
			switch rng.Intn(3) {
			case 0:
				p.Kind, p.Percent = PercentOff, int64(1+rng.Intn(10_000))
				if rng.Intn(2) == 0 {
					p.MaxDiscount = money.Money(1 + rng.Intn(50_000))
				}
			case 1:
				p.Kind, p.Amount = FixedOff, money.Money(1+rng.Intn(300_000))
			default:
				p.Kind, p.Buy, p.Free = BuyXGetY, 1+rng.Intn(3), 1+rng.Intn(2)
			}
			if rng.Intn(2) == 0 {
				p.Scope = Scope{Categories: []string{cats[rng.Intn(3)]}}
			}
			if rng.Intn(3) == 0 {
				p.MinSubtotal = money.Money(rng.Intn(150_000))
			}
			promos = append(promos, p)
		}
		s, err := NewSet(promos)
		if err != nil {
			t.Fatal(err)
		}
		r := s.Evaluate(lines, ctx())

		if r.Discount < 0 || r.Discount > r.Subtotal || r.Total < 0 || r.Total != r.Subtotal-r.Discount {
			t.Fatalf("trial %d: subtotal %v discount %v total %v", trial, r.Subtotal, r.Discount, r.Total)
		}
		if sum(r.PerLine) != r.Discount {
			t.Fatalf("trial %d: the lines get %v but the order gets %v", trial, sum(r.PerLine), r.Discount)
		}
		var applied money.Money
		for _, a := range r.Applied {
			applied += a.Amount
		}
		if applied != r.Discount {
			t.Fatalf("trial %d: the promotions add up to %v, the discount is %v", trial, applied, r.Discount)
		}
		for i, d := range r.PerLine {
			if d < 0 || d > lines[i].Total {
				t.Fatalf("trial %d: line %d costs %v and gets %v off", trial, i, lines[i].Total, d)
			}
		}

		// The same order, shuffled: same discount and the same promotions.
		rng.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		rng.Shuffle(len(promos), func(i, j int) { promos[i], promos[j] = promos[j], promos[i] })
		s2, _ := NewSet(promos)
		r2 := s2.Evaluate(lines, ctx())
		if r2.Discount != r.Discount || r2.Total != r.Total || amounts(r2) != amounts(r) {
			t.Fatalf("trial %d: shuffling changed the result: %v vs %v (%s vs %s)", trial, r.Discount, r2.Discount, amounts(r), amounts(r2))
		}
	}
}

func amounts(r Result) string {
	var out []string
	for _, a := range r.Applied {
		out = append(out, fmt.Sprint(a.ID, "=", int64(a.Amount)))
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}
