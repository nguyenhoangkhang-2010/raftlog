package stock

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

var day0 = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

func days(n int) time.Time { return day0.Add(time.Duration(n) * 24 * time.Hour) }

func receive(t *testing.T, s *Stock, id string, ing Ingredient, qty int64, at, expires time.Time, cost int64) {
	t.Helper()
	if err := s.Apply(Movement{Kind: Receive, Ingredient: ing, Qty: qty, Ref: id, At: at, ExpiresAt: expires, TotalCost: cost, Supplier: "sup-1", Document: "INV-" + id}); err != nil {
		t.Fatalf("receive %s: %v", id, err)
	}
}

func lotQty(s *Stock, ing Ingredient) map[string]int64 {
	out := map[string]int64{}
	for _, l := range s.Lots(ing) {
		out[l.ID] = l.Qty
	}
	return out
}

func TestWhatIsTakenComesFirstFromTheLotThatExpiresFirst(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "late", "milk", 100, days(0), days(10), 0)
	receive(t, s, "soon", "milk", 100, days(1), days(3), 0) // arrived after, expires before
	receive(t, s, "none", "milk", 100, days(-5), time.Time{}, 0)
	res, err := s.Do(Movement{Kind: Consume, Needs: Recipe{"milk": 150}, Ref: "o1", At: days(2)})
	if err != nil {
		t.Fatal(err)
	}
	if got := lotQty(s, "milk"); !reflect.DeepEqual(got, map[string]int64{"late": 50, "none": 100}) {
		t.Fatalf("lots after taking 150: %v (the lot that expires first goes first, then the next, and a lot with no date last)", got)
	}
	if len(res.Takes) != 2 || res.Takes[0].Lot.ID != "soon" || res.Takes[0].Qty != 100 || res.Takes[1].Lot.ID != "late" || res.Takes[1].Qty != 50 {
		t.Fatalf("takes: %+v", res.Takes)
	}
	if s.Qty("milk") != 150 {
		t.Fatalf("the quantity is the sum of the lots: %d", s.Qty("milk"))
	}
}

func TestAnExpiredLotIsNotUsedToMakeAnOrderAndIsStillCounted(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "old", "cream", 50, days(-5), days(1), 0)
	receive(t, s, "fresh", "cream", 30, days(0), days(20), 0)
	at := days(2) // the old lot expired yesterday
	if s.Qty("cream") != 80 || s.Usable("cream", at) != 30 {
		t.Fatalf("qty %d usable %d, want 80 and 30", s.Qty("cream"), s.Usable("cream", at))
	}
	err := s.Apply(Movement{Kind: Consume, Needs: Recipe{"cream": 40}, At: at})
	var short *ShortageError
	if !errors.As(err, &short) || short.Missing[0].Have != 30 {
		t.Fatalf("40 of cream with 30 usable: %v", err)
	}
	if s.Qty("cream") != 80 {
		t.Fatal("a refused consume changed the stock")
	}
	if err := s.Apply(Movement{Kind: Consume, Needs: Recipe{"cream": 30}, At: at}); err != nil {
		t.Fatal(err)
	}
	if got := lotQty(s, "cream"); !reflect.DeepEqual(got, map[string]int64{"old": 50}) {
		t.Fatalf("the expired lot is what is left: %v", got)
	}
	// It still counts as stock until somebody writes it off; before it expired, it was used first.
	s2, _ := New(nil)
	receive(t, s2, "old", "cream", 50, days(-5), days(1), 0)
	receive(t, s2, "fresh", "cream", 30, days(0), days(20), 0)
	if err := s2.Apply(Movement{Kind: Consume, Needs: Recipe{"cream": 30}, At: days(0)}); err != nil {
		t.Fatal(err)
	}
	if got := lotQty(s2, "cream"); !reflect.DeepEqual(got, map[string]int64{"old": 20, "fresh": 30}) {
		t.Fatalf("before it expired the old lot goes first: %v", got)
	}
	// The sold-out test and the counts of what can be made follow the moment.
	if s.MaxMakeableAt(Recipe{"cream": 10}, days(2)) != 0 || s2.MaxMakeableAt(Recipe{"cream": 10}, days(0)) != 5 {
		t.Fatalf("makeable: %d, %d", s.MaxMakeableAt(Recipe{"cream": 10}, days(2)), s2.MaxMakeableAt(Recipe{"cream": 10}, days(0)))
	}
	if ok, _ := s2.CanMakeAt(Recipe{"cream": 50}, days(2)); ok {
		t.Fatal("after day 1 only the fresh lot (30) counts, 50 cannot be made")
	}
}

func TestGivingBackGoesToTheSameLotAtTheSamePrice(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "a", "beans", 100, days(0), days(30), 50) // 500 milli-dong each
	receive(t, s, "b", "beans", 100, days(1), days(60), 80)
	res, err := s.Do(Movement{Kind: Consume, Needs: Recipe{"beans": 130}, Ref: "o1", At: days(2)})
	if err != nil {
		t.Fatal(err)
	}
	if res.ValueMilli != 100*500+30*800 {
		t.Fatalf("what was taken is worth %d milli-dong, want %d", res.ValueMilli, 100*500+30*800)
	}
	// The lot "a" is used up and gone; giving back recreates it with its date and price.
	if _, ok := lotQty(s, "beans")["a"]; ok {
		t.Fatal("a lot that is used up is still listed")
	}
	giv, err := s.Do(Movement{Kind: Release, Needs: Recipe{"beans": 130}, Takes: res.Takes, Ref: "o1"})
	if err != nil {
		t.Fatal(err)
	}
	if giv.ValueMilli != res.ValueMilli {
		t.Fatalf("what was given back is worth %d, it was worth %d when taken", giv.ValueMilli, res.ValueMilli)
	}
	got := s.Lots("beans")
	if len(got) != 2 || got[0].ID != "a" || got[0].Qty != 100 || !got[0].ExpiresAt.Equal(days(30)) || got[0].UnitCostMilli != 500 || got[1].ID != "b" || got[1].Qty != 100 {
		t.Fatalf("lots after giving back: %+v", got)
	}
}

func TestThePriceOfALotIsWhatWasPaidForAllOfItDividedByWhatCame(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "a", "salt", 3, days(0), time.Time{}, 10) // 10 dong for 3: 3333 milli, rounded
	if got := s.Lots("salt")[0].UnitCostMilli; got != 3333 {
		t.Fatalf("unit cost %d, want 3333", got)
	}
	receive(t, s, "b", "salt", 1000, days(0), time.Time{}, 0) // no price known
	v, unpriced := s.ValueMilli("salt")
	if v != 3*3333 || unpriced != 1000 {
		t.Fatalf("value %d, unpriced %d", v, unpriced)
	}
}

func TestAReceiptThatMakesNoSenseIsRefused(t *testing.T) {
	s, _ := New(nil)
	if err := s.Apply(Movement{Kind: Receive, Ingredient: "x", Qty: 5, TotalCost: -1}); !errors.Is(err, ErrCost) {
		t.Fatalf("a negative price: %v", err)
	}
	if err := s.Apply(Movement{Kind: Receive, Ingredient: "x", Qty: 5, At: days(2), ExpiresAt: days(1)}); !errors.Is(err, ErrExpiry) {
		t.Fatalf("a lot that expires before it arrives: %v", err)
	}
	if err := s.Apply(Movement{Kind: Receive, Ingredient: "x", Qty: 5, At: days(2), ExpiresAt: days(2)}); !errors.Is(err, ErrExpiry) {
		t.Fatalf("a lot that expires the moment it arrives: %v", err)
	}
	if s.Qty("x") != 0 {
		t.Fatal("a refused receipt changed the stock")
	}
}

func TestAWriteOffNamesItsCauseAndMayTakeFromOneLot(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "old", "milk", 40, days(-9), days(-1), 40) // expired, 1000 milli each
	receive(t, s, "new", "milk", 60, days(0), days(9), 60)
	res, err := s.Do(Movement{Kind: WriteOff, Ingredient: "milk", Qty: 40, FromLot: "old", Cause: CauseExpired, Reason: "past its date"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ValueMilli != 40*1000 || len(res.Takes) != 1 || res.Takes[0].Lot.ID != "old" {
		t.Fatalf("result %+v", res)
	}
	if got := lotQty(s, "milk"); !reflect.DeepEqual(got, map[string]int64{"new": 60}) {
		t.Fatalf("lots %v", got)
	}
	if err := s.Apply(Movement{Kind: WriteOff, Ingredient: "milk", Qty: 61, FromLot: "new", Reason: "x"}); err == nil {
		t.Fatal("more than the lot has")
	}
	if err := s.Apply(Movement{Kind: WriteOff, Ingredient: "milk", Qty: 1, FromLot: "ghost", Reason: "x"}); !errors.Is(err, ErrNoLot) {
		t.Fatalf("a lot that is not there: %v", err)
	}
	if err := s.Apply(Movement{Kind: WriteOff, Ingredient: "milk", Qty: 1, Cause: "theft?", Reason: "x"}); !errors.Is(err, ErrCause) {
		t.Fatalf("a cause that is not one: %v", err)
	}
	if err := s.Apply(Movement{Kind: WriteOff, Ingredient: "milk", Qty: 1, Reason: "x"}); err != nil {
		t.Fatalf("no cause is allowed: %v", err)
	}
}

func TestAnAdjustmentAddsAPricedLotOrTakesFromTheLotsInOrder(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "a", "eggs", 10, days(0), days(5), 10) // 1000 milli each
	if err := s.Apply(Movement{Kind: Adjust, Ingredient: "eggs", Qty: 4, Ref: "st1-1", Reason: "stocktake 1", At: days(1)}); err != nil {
		t.Fatal(err)
	}
	lots := lotQty(s, "eggs")
	if lots["a"] != 10 || lots["found-st1-1"] != 4 || s.Qty("eggs") != 14 {
		t.Fatalf("lots %v", lots)
	}
	for _, l := range s.Lots("eggs") {
		if l.ID == "found-st1-1" && l.UnitCostMilli != 1000 {
			t.Fatalf("a surplus is priced at the average of the stock, got %d", l.UnitCostMilli)
		}
	}
	res, err := s.Do(Movement{Kind: Adjust, Ingredient: "eggs", Qty: -12, Ref: "st1-2", Reason: "stocktake 1"})
	if err != nil || s.Qty("eggs") != 2 || res.ValueMilli != 12*1000 {
		t.Fatalf("a loss of 12: %v, qty %d, value %d", err, s.Qty("eggs"), res.ValueMilli)
	}
	if err := s.Apply(Movement{Kind: Adjust, Ingredient: "eggs", Qty: -3, Reason: "x"}); err == nil {
		t.Fatal("a loss larger than the stock")
	}
	if err := s.Apply(Movement{Kind: Adjust, Ingredient: "eggs", Qty: 0, Reason: "x"}); !errors.Is(err, ErrQuantity) {
		t.Fatalf("an adjustment of nothing: %v", err)
	}
	if err := s.Apply(Movement{Kind: Adjust, Ingredient: "eggs", Qty: 1}); !errors.Is(err, ErrReason) {
		t.Fatalf("an adjustment without a reason: %v", err)
	}
}

func TestACountMakesTheStockWhatWasFound(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "a", "rice", 100, days(0), days(50), 20)
	if err := s.Apply(Movement{Kind: Count, Ingredient: "rice", Qty: 70, Reason: "count", Ref: "c1"}); err != nil || s.Qty("rice") != 70 {
		t.Fatalf("a count below: %v, %d", err, s.Qty("rice"))
	}
	if err := s.Apply(Movement{Kind: Count, Ingredient: "rice", Qty: 90, Reason: "count", Ref: "c2"}); err != nil || s.Qty("rice") != 90 {
		t.Fatalf("a count above: %v, %d", err, s.Qty("rice"))
	}
	if err := s.Apply(Movement{Kind: Count, Ingredient: "rice", Qty: 0, Reason: "count", Ref: "c3"}); err != nil || s.Qty("rice") != 0 || len(s.Lots("rice")) != 0 {
		t.Fatalf("a count of zero: %v, %d, %d lots", err, s.Qty("rice"), len(s.Lots("rice")))
	}
}

func TestACloneOfAStockWithLotsIsIndependent(t *testing.T) {
	s, _ := New(nil)
	receive(t, s, "a", "oil", 100, days(0), days(9), 5)
	c := s.Clone()
	if err := c.Apply(Movement{Kind: Consume, Needs: Recipe{"oil": 60}, At: days(1)}); err != nil {
		t.Fatal(err)
	}
	if s.Lots("oil")[0].Qty != 100 || c.Lots("oil")[0].Qty != 40 {
		t.Fatalf("a clone shares its lots: %d and %d", s.Lots("oil")[0].Qty, c.Lots("oil")[0].Qty)
	}
	if err := s.Apply(Movement{Kind: Receive, Ingredient: "oil", Qty: 7}); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(Movement{Kind: Receive, Ingredient: "oil", Qty: 7}); err != nil {
		t.Fatal(err)
	}
	// a lot without an ID of its own is numbered, and the clone carries on from the same number
	ids := func(st *Stock) []string {
		var out []string
		for _, l := range st.Lots("oil") {
			out = append(out, l.ID)
		}
		return out
	}
	if !reflect.DeepEqual(ids(s)[len(ids(s))-1:], ids(c)[len(ids(c))-1:]) {
		t.Fatalf("lot numbers diverged: %v and %v", ids(s), ids(c))
	}
}

// The quantity is always the sum of the lots, nothing goes below zero, and the same movements
// applied again give the same lots, whatever mix of dates and prices came in.
func TestLotsStayConsistentAndReplayIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	ings := []Ingredient{"a", "b", "c"}
	for trial := 0; trial < 200; trial++ {
		mk := func() *Stock {
			s, _ := New(map[Ingredient]Level{"a": {Qty: 30}, "b": {Qty: 0}})
			return s
		}
		first := mk()
		var accepted []Movement
		var pending [][]Take // takes of what orders consumed, for giving back
		for step := 0; step < 150; step++ {
			at := days(step / 10)
			ing := ings[rng.Intn(3)]
			var m Movement
			switch rng.Intn(7) {
			case 0, 1:
				var exp time.Time
				if rng.Intn(3) > 0 {
					exp = at.Add(time.Duration(1+rng.Intn(40)) * 24 * time.Hour)
				}
				m = Movement{Kind: Receive, Ingredient: ing, Qty: int64(1 + rng.Intn(30)), Ref: "r" + time.Duration(step).String(), At: at, ExpiresAt: exp, TotalCost: int64(rng.Intn(5000))}
			case 2:
				m = Movement{Kind: WriteOff, Ingredient: ing, Qty: int64(1 + rng.Intn(10)), Reason: "r", Cause: Causes()[rng.Intn(4)]}
			case 3:
				m = Movement{Kind: Adjust, Ingredient: ing, Qty: int64(rng.Intn(21) - 10), Reason: "a", Ref: "a" + time.Duration(step).String(), At: at}
			case 4:
				m = Movement{Kind: Count, Ingredient: ing, Qty: int64(rng.Intn(50)), Reason: "c", Ref: "c" + time.Duration(step).String(), At: at}
			case 5:
				m = Movement{Kind: Consume, Needs: Recipe{ing: int64(1 + rng.Intn(15))}, At: at, Ref: "o"}
			default:
				if len(pending) > 0 {
					takes := pending[len(pending)-1]
					pending = pending[:len(pending)-1]
					needs := Recipe{}
					for _, tk := range takes {
						needs[tk.Ingredient] += tk.Qty
					}
					m = Movement{Kind: Release, Needs: needs, Takes: takes, At: at, Ref: "o"}
				} else {
					continue
				}
			}
			res, err := first.Do(m)
			if err != nil {
				continue
			}
			accepted = append(accepted, m)
			if m.Kind == Consume {
				pending = append(pending, res.Takes)
			}
			for _, i := range ings {
				var sum int64
				for _, l := range first.Lots(i) {
					if l.Qty <= 0 {
						t.Fatalf("trial %d step %d: a lot of %s with %d left is kept", trial, step, i, l.Qty)
					}
					sum += l.Qty
				}
				if sum != first.Qty(i) || first.Qty(i) < 0 {
					t.Fatalf("trial %d step %d: %s is %d, its lots add up to %d, after %+v", trial, step, i, first.Qty(i), sum, m)
				}
			}
		}
		second := mk()
		for _, m := range accepted {
			if _, err := second.Do(m); err != nil {
				t.Fatalf("trial %d: replaying an accepted movement failed: %v", trial, err)
			}
		}
		for _, i := range ings {
			if !reflect.DeepEqual(first.Lots(i), second.Lots(i)) {
				t.Fatalf("trial %d: replay gives other lots of %s:\n%+v\n%+v", trial, i, second.Lots(i), first.Lots(i))
			}
		}
	}
}
