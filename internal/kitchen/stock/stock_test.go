package stock

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
)

func opening(t *testing.T) *Stock {
	t.Helper()
	s, err := New(map[Ingredient]Level{
		"coffee": {Qty: 100, LowAt: 40}, "milk": {Qty: 500}, "cup": {Qty: 3, LowAt: 5}, "bread": {Qty: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConsumeTakesWhatAnOrderNeeds(t *testing.T) {
	s := opening(t)
	if err := s.Apply(Movement{Kind: Consume, Needs: Recipe{"coffee": 20, "milk": 30, "cup": 1}, Ref: "o1"}); err != nil {
		t.Fatal(err)
	}
	if s.Qty("coffee") != 80 || s.Qty("milk") != 470 || s.Qty("cup") != 2 {
		t.Fatalf("stock = %v", s.Snapshot())
	}
}

func TestAShortOrderTakesNothingAndSaysEverythingThatIsShort(t *testing.T) {
	s := opening(t)
	before := s.Snapshot()
	err := s.Apply(Movement{Kind: Consume, Needs: Recipe{"coffee": 20, "cup": 9, "bread": 1}})
	var short *ShortageError
	if !errors.As(err, &short) {
		t.Fatalf("err = %v, want a ShortageError", err)
	}
	if len(short.Missing) != 2 || short.Missing[0] != (Shortage{"bread", 1, 0}) || short.Missing[1] != (Shortage{"cup", 9, 3}) {
		t.Fatalf("missing = %+v, want bread and cup, sorted", short.Missing)
	}
	if !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatalf("a refused consume changed the stock: %v -> %v", before, s.Snapshot())
	}
}

func TestReleaseGivesBackWhatWasConsumed(t *testing.T) {
	s := opening(t)
	before := s.Snapshot()
	needs := Recipe{"coffee": 20, "milk": 30}
	if err := s.Apply(Movement{Kind: Consume, Needs: needs}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(Movement{Kind: Release, Needs: needs}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatalf("stock %v, want %v after consume and release", s.Snapshot(), before)
	}
}

func TestReceivingAndWritingOff(t *testing.T) {
	s := opening(t)
	if err := s.Apply(Movement{Kind: Receive, Ingredient: "bread", Qty: 12}); err != nil {
		t.Fatal(err)
	}
	if s.Qty("bread") != 12 {
		t.Fatalf("bread = %d", s.Qty("bread"))
	}
	if err := s.Apply(Movement{Kind: WriteOff, Ingredient: "bread", Qty: 2, Reason: "dropped"}); err != nil || s.Qty("bread") != 10 {
		t.Fatalf("write-off: %v, bread %d", err, s.Qty("bread"))
	}
	// Writing off more than there is, or without a reason, is refused.
	if err := s.Apply(Movement{Kind: WriteOff, Ingredient: "bread", Qty: 11, Reason: "x"}); err == nil {
		t.Fatal("wrote off more than there was")
	}
	if err := s.Apply(Movement{Kind: WriteOff, Ingredient: "bread", Qty: 1, Reason: "  "}); !errors.Is(err, ErrReason) {
		t.Fatalf("without a reason: %v", err)
	}
	if s.Qty("bread") != 10 {
		t.Fatalf("a refused write-off changed the stock to %d", s.Qty("bread"))
	}
}

func TestAStocktakeSetsTheRealQuantity(t *testing.T) {
	s := opening(t)
	if err := s.Apply(Movement{Kind: Count, Ingredient: "milk", Qty: 430, Reason: "monthly count"}); err != nil || s.Qty("milk") != 430 {
		t.Fatalf("count: %v, milk %d", err, s.Qty("milk"))
	}
	if err := s.Apply(Movement{Kind: Count, Ingredient: "milk", Qty: 0, Reason: "empty"}); err != nil || s.Qty("milk") != 0 {
		t.Fatalf("a count of zero: %v", err)
	}
	if err := s.Apply(Movement{Kind: Count, Ingredient: "milk", Qty: -1, Reason: "x"}); !errors.Is(err, ErrNegative) {
		t.Fatalf("a negative count: %v", err)
	}
	if err := s.Apply(Movement{Kind: Count, Ingredient: "milk", Qty: 5}); !errors.Is(err, ErrReason) {
		t.Fatalf("a count without a reason: %v", err)
	}
}

func TestBadMovementsAreRefused(t *testing.T) {
	s := opening(t)
	before := s.Snapshot()
	bad := []Movement{
		{Kind: Receive, Ingredient: "milk", Qty: 0},
		{Kind: Receive, Ingredient: "milk", Qty: -5},
		{Kind: Receive, Ingredient: "", Qty: 5},
		{Kind: Consume, Needs: Recipe{"milk": 0}},
		{Kind: Consume, Needs: Recipe{"": 1}},
		{Kind: Release, Needs: Recipe{"milk": -1}},
		{Kind: "teleport"},
	}
	for _, m := range bad {
		if s.Apply(m) == nil {
			t.Errorf("accepted %+v", m)
		}
	}
	if !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatal("a refused movement changed the stock")
	}
	if _, err := New(map[Ingredient]Level{"x": {Qty: -1}}); err == nil {
		t.Error("an opening level below zero was accepted")
	}
}

func TestLowStockAlerts(t *testing.T) {
	s := opening(t)
	// cup 3 <= 5 is low; coffee 100 > 40 is not; milk has no alert.
	if got := s.Low(); !reflect.DeepEqual(got, []Ingredient{"cup"}) {
		t.Fatalf("Low = %v", got)
	}
	if err := s.Apply(Movement{Kind: Consume, Needs: Recipe{"coffee": 60}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Low(); !reflect.DeepEqual(got, []Ingredient{"coffee", "cup"}) { // coffee 40 <= 40
		t.Fatalf("Low = %v, want the threshold itself to count", got)
	}
	if err := s.SetLowAt("milk", 600); err != nil {
		t.Fatal(err)
	}
	if got := s.Low(); len(got) != 3 || got[1] != "cup" || got[2] != "milk" {
		t.Fatalf("Low = %v", got)
	}
	if s.SetLowAt("milk", -1) == nil {
		t.Fatal("a negative threshold was accepted")
	}
}

func TestHowManyCanBeMade(t *testing.T) {
	s := opening(t)
	r := Recipe{"coffee": 20, "milk": 30, "cup": 1}
	if got := s.MaxMakeable(r); got != 3 { // limited by the 3 cups
		t.Fatalf("MaxMakeable = %d, want 3", got)
	}
	if got := s.MaxMakeable(Recipe{"bread": 1}); got != 0 {
		t.Fatalf("no bread: %d", got)
	}
	if got := s.MaxMakeable(Recipe{}); got != -1 {
		t.Fatalf("an empty recipe has no limit, got %d", got)
	}
	if ok, short := s.CanMake(Recipe{"cup": 4}); ok || len(short) != 1 {
		t.Fatalf("CanMake = %v %v", ok, short)
	}
	if ok, short := s.CanMake(Recipe{"cup": 3}); !ok || short != nil {
		t.Fatalf("CanMake = %v %v", ok, short)
	}
}

func TestAnItemIsSoldOutWhenItsBaseRecipeCannotBeMade(t *testing.T) {
	m, err := menu.New([]menu.Item{
		{ID: "coffee", Name: "Coffee", Category: "drinks", Station: "drinks", Price: 25_000, Active: true, Recipe: Recipe{"coffee": 20, "cup": 1}},
		{ID: "banh-mi", Name: "Banh mi", Category: "food", Station: "hot", Price: 25_000, Active: true, Recipe: Recipe{"bread": 1}},
		{ID: "water", Name: "Water", Category: "drinks", Station: "drinks", Price: 5_000, Active: true}, // no recipe: never sold out
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := opening(t)
	out := s.SoldOut(m)
	if out("coffee") || !out("banh-mi") || out("water") || out("unknown") {
		t.Fatalf("sold out: coffee %v, banh-mi %v, water %v", out("coffee"), out("banh-mi"), out("water"))
	}
	if err := s.Apply(Movement{Kind: Receive, Ingredient: "bread", Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if out("banh-mi") {
		t.Fatal("the item is still sold out after bread arrived")
	}
}

// Whatever happens, no ingredient goes below zero, and the same movements give the
// same stock again.
func TestNoSequenceOfMovementsGoesBelowZeroAndReplayIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	ings := []Ingredient{"a", "b", "c", "d"}
	for trial := 0; trial < 300; trial++ {
		first, _ := New(map[Ingredient]Level{"a": {Qty: 50}, "b": {Qty: 20}, "c": {Qty: 0}, "d": {Qty: 7}})
		var accepted []Movement
		for step := 0; step < 200; step++ {
			var m Movement
			switch rng.Intn(5) {
			case 0:
				m = Movement{Kind: Receive, Ingredient: ings[rng.Intn(4)], Qty: int64(1 + rng.Intn(30))}
			case 1:
				m = Movement{Kind: WriteOff, Ingredient: ings[rng.Intn(4)], Qty: int64(1 + rng.Intn(15)), Reason: "r"}
			case 2:
				m = Movement{Kind: Count, Ingredient: ings[rng.Intn(4)], Qty: int64(rng.Intn(60)), Reason: "c"}
			case 3:
				needs := Recipe{}
				for i, n := 0, 1+rng.Intn(3); i < n; i++ {
					needs[ings[rng.Intn(4)]] = int64(1 + rng.Intn(12))
				}
				m = Movement{Kind: Consume, Needs: needs}
			default:
				m = Movement{Kind: Release, Needs: Recipe{ings[rng.Intn(4)]: int64(1 + rng.Intn(5))}}
			}
			if first.Apply(m) == nil {
				accepted = append(accepted, m)
			}
			for ing, q := range first.Snapshot() {
				if q < 0 {
					t.Fatalf("trial %d step %d: %s is %d after %+v", trial, step, ing, q, m)
				}
			}
		}
		second, _ := New(map[Ingredient]Level{"a": {Qty: 50}, "b": {Qty: 20}, "c": {Qty: 0}, "d": {Qty: 7}})
		for _, m := range accepted {
			if err := second.Apply(m); err != nil {
				t.Fatalf("trial %d: replaying an accepted movement failed: %v", trial, err)
			}
		}
		if !reflect.DeepEqual(first.Snapshot(), second.Snapshot()) {
			t.Fatalf("trial %d: replay gives %v, the original has %v", trial, second.Snapshot(), first.Snapshot())
		}
	}
}
