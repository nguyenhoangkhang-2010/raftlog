package menu

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
)

// The venue in these tests: a small café. The items are invented for the tests;
// real menus come in through the import of the next branches.

var (
	tue1500 = time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC) // a Tuesday
	tue1600 = time.Date(2026, 11, 3, 16, 0, 0, 0, time.UTC)
	tue1359 = time.Date(2026, 11, 3, 13, 59, 0, 0, time.UTC)
	sat1500 = time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC) // a Saturday
)

func fixtureItems() []Item {
	size := Group{ID: "size", Name: "Size", Min: 1, Max: 1, Options: []Option{
		{ID: "m", Name: "M", Delta: 0},
		{ID: "l", Name: "L", Delta: 5_000, Recipe: Recipe{"milk": 20}},
	}}
	ice := Group{ID: "ice", Name: "Ice", Min: 0, Max: 1, Options: []Option{
		{ID: "less", Name: "less ice"}, {ID: "none", Name: "no ice"},
	}}
	topping := Group{ID: "top", Name: "Topping", Min: 0, Max: 2, Options: []Option{
		{ID: "pearl", Name: "pearls", Delta: 8_000, Recipe: Recipe{"pearl": 40}},
		{ID: "pudding", Name: "pudding", Delta: 7_000},
		{ID: "foam", Name: "foam", Delta: 5_000},
	}}
	return []Item{
		{ID: "coffee", Name: "Milk coffee", Category: "drinks", Station: "drinks", Price: 25_000, Active: true,
			Groups: []Group{size, ice, topping}, Recipe: Recipe{"coffee": 20, "milk": 30, "cup": 1}},
		{ID: "peach-tea", Name: "Peach tea", Category: "drinks", Station: "drinks", Price: 30_000, Active: true,
			TimePrices: []TimePrice{{Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, From: 14 * 60, To: 16 * 60, Price: 15_000}},
			Groups:     []Group{size}, Recipe: Recipe{"tea": 10, "cup": 1}},
		{ID: "banh-mi", Name: "Banh mi", Category: "food", Station: "hot", Price: 25_000, Active: true, Recipe: Recipe{"bread": 1}},
		{ID: "salad", Name: "Salad", Category: "food", Station: "cold", Price: 30_000, Active: true, Recipe: Recipe{"lettuce": 80}},
		{ID: "seasonal", Name: "Seasonal pie", Category: "food", Station: "hot", Price: 40_000, Active: false},
		{ID: "cheap", Name: "Cheap drink", Category: "drinks", Station: "drinks", Price: 5_000, Active: true,
			Groups: []Group{{ID: "mini", Name: "Mini", Min: 0, Max: 1, Options: []Option{{ID: "mini", Name: "mini", Delta: -8_000}}}}},
	}
}

func fixtureCombos() []Combo {
	return []Combo{{
		ID: "lunch", Name: "Lunch combo", Price: 49_000, Active: true,
		Slots: []Slot{
			{Name: "Main", Items: []ItemID{"banh-mi", "salad"}},
			{Name: "Drink", Items: []ItemID{"coffee", "peach-tea"}},
		},
	}}
}

func fixture(t *testing.T) *Menu {
	t.Helper()
	m, err := New(fixtureItems(), fixtureCombos())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func opts(at time.Time) PriceOptions { return PriceOptions{At: at} }

func TestAnItemIsPricedWithItsOptions(t *testing.T) {
	m := fixture(t)
	l, err := m.PriceLine(Selection{
		Item: "coffee", Quantity: 2,
		Options: Choice{"size": {"l"}, "ice": {"less"}, "top": {"pearl", "foam"}},
	}, opts(tue1359))
	if err != nil {
		t.Fatal(err)
	}
	// 25.000 + 5.000 (L) + 8.000 (pearls) + 5.000 (foam), twice.
	if l.Unit != 43_000 || l.Quantity != 2 || l.Total != 86_000 {
		t.Fatalf("price = %v x%d = %v, want 43.000đ x2 = 86.000đ", l.Unit, l.Quantity, l.Total)
	}
	if l.Kind != "item" || l.Ref != "coffee" || l.Category != "drinks" || len(l.Items) != 1 {
		t.Fatalf("line = %+v", l)
	}
	if got := strings.Join(l.Options, "|"); got != "L|less ice|pearls|foam" {
		t.Fatalf("options = %q (menu order, not request order)", got)
	}
	if len(l.Pieces) != 1 || l.Pieces[0].Station != "drinks" || l.Pieces[0].Name != "Milk coffee (L, less ice, pearls, foam)" {
		t.Fatalf("pieces = %+v", l.Pieces)
	}
	// The ingredients of ONE unit: the item's, plus what the options use.
	want := Recipe{"coffee": 20, "milk": 50, "cup": 1, "pearl": 40}
	if len(l.Needs) != len(want) {
		t.Fatalf("needs = %v, want %v", l.Needs, want)
	}
	for ing, n := range want {
		if l.Needs[ing] != n {
			t.Fatalf("needs = %v, want %v", l.Needs, want)
		}
	}
}

func TestTheOrderOfOptionsInTheRequestDoesNotChangeTheLine(t *testing.T) {
	m := fixture(t)
	a, _ := m.PriceLine(Selection{Item: "coffee", Quantity: 1, Options: Choice{"size": {"m"}, "top": {"pearl", "foam"}}}, opts(tue1359))
	b, _ := m.PriceLine(Selection{Item: "coffee", Quantity: 1, Options: Choice{"top": {"foam", "pearl"}, "size": {"m"}}}, opts(tue1359))
	if strings.Join(a.Options, ",") != strings.Join(b.Options, ",") || a.Unit != b.Unit || a.Pieces[0].Name != b.Pieces[0].Name {
		t.Fatalf("the same choice made two lines:\n%+v\n%+v", a, b)
	}
}

func TestAnItemWithoutRequiredOptionsIsRefused(t *testing.T) {
	m := fixture(t)
	cases := map[string]Choice{
		"required group missing": {},
		"too many in a group":    {"size": {"m", "l"}},
		"too many toppings":      {"size": {"m"}, "top": {"pearl", "pudding", "foam"}},
		"unknown option":         {"size": {"xl"}},
		"unknown group":          {"size": {"m"}, "sugar": {"none"}},
		"same option twice":      {"size": {"m"}, "top": {"pearl", "pearl"}},
	}
	for name, c := range cases {
		if _, err := m.PriceLine(Selection{Item: "coffee", Quantity: 1, Options: c}, opts(tue1359)); !errors.Is(err, ErrChoice) {
			t.Errorf("%s: err = %v, want ErrChoice", name, err)
		}
	}
}

func TestHappyHourPricesFollowTheClock(t *testing.T) {
	m := fixture(t)
	choice := Choice{"size": {"m"}}
	price := func(at time.Time) money.Money {
		l, err := m.PriceLine(Selection{Item: "peach-tea", Quantity: 1, Options: choice}, opts(at))
		if err != nil {
			t.Fatal(err)
		}
		return l.Unit
	}
	cases := []struct {
		name string
		at   time.Time
		want money.Money
	}{
		{"before the window", tue1359, 30_000},
		{"at the start of the window", tue1500.Add(-time.Hour), 15_000},
		{"inside", tue1500, 15_000},
		{"the end is not inside", tue1600, 30_000},
		{"a Saturday", sat1500, 30_000},
	}
	for _, c := range cases {
		if got := price(c.at); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestItemsThatCannotBeSold(t *testing.T) {
	m := fixture(t)
	sel := func(id ItemID) Selection { return Selection{Item: id, Quantity: 1} }
	if _, err := m.PriceLine(sel("nothing"), opts(tue1500)); !errors.Is(err, ErrUnknownItem) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := m.PriceLine(sel("seasonal"), opts(tue1500)); !errors.Is(err, ErrInactive) {
		t.Errorf("inactive: %v", err)
	}
	o := PriceOptions{At: tue1500, SoldOut: func(id ItemID) bool { return id == "salad" }}
	if _, err := m.PriceLine(sel("salad"), o); !errors.Is(err, ErrSoldOut) {
		t.Errorf("sold out: %v", err)
	}
	if _, err := m.PriceLine(sel("banh-mi"), o); err != nil {
		t.Errorf("something else is still for sale: %v", err)
	}
}

func TestQuantityNoteAndKindOfASelection(t *testing.T) {
	m := fixture(t)
	for _, q := range []int{0, -1, money.MaxQuantity + 1} {
		if _, err := m.PriceLine(Selection{Item: "banh-mi", Quantity: q}, opts(tue1500)); !errors.Is(err, ErrQuantity) {
			t.Errorf("quantity %d: %v", q, err)
		}
	}
	if _, err := m.PriceLine(Selection{Item: "banh-mi", Quantity: money.MaxQuantity}, opts(tue1500)); err != nil {
		t.Errorf("the largest quantity: %v", err)
	}
	if _, err := m.PriceLine(Selection{Item: "banh-mi", Quantity: 1, Note: strings.Repeat("a", MaxNoteRunes+1)}, opts(tue1500)); !errors.Is(err, ErrNote) {
		t.Errorf("long note: %v", err)
	}
	if _, err := m.PriceLine(Selection{Item: "banh-mi", Quantity: 1, Note: "bell\x07"}, opts(tue1500)); !errors.Is(err, ErrNote) {
		t.Errorf("control character in a note: %v", err)
	}
	l, err := m.PriceLine(Selection{Item: "banh-mi", Quantity: 1, Note: "  no chili\nplease "}, opts(tue1500))
	if err != nil || l.Note != "no chili\nplease" {
		t.Errorf("a note with a line break: %q, %v", l.Note, err)
	}
	if _, err := m.PriceLine(Selection{Quantity: 1}, opts(tue1500)); !errors.Is(err, ErrSelection) {
		t.Errorf("neither item nor combo: %v", err)
	}
	if _, err := m.PriceLine(Selection{Item: "banh-mi", Combo: "lunch", Quantity: 1}, opts(tue1500)); !errors.Is(err, ErrSelection) {
		t.Errorf("both item and combo: %v", err)
	}
}

func TestAPriceCannotGoNegative(t *testing.T) {
	m := fixture(t)
	// A 5.000đ drink with a -8.000đ option would cost -3.000đ.
	_, err := m.PriceLine(Selection{Item: "cheap", Quantity: 1, Options: Choice{"mini": {"mini"}}}, opts(tue1500))
	if !errors.Is(err, ErrPrice) {
		t.Fatalf("err = %v, want ErrPrice", err)
	}
}

func TestAComboChargesItsPriceAndTheOptionsOnly(t *testing.T) {
	m := fixture(t)
	l, err := m.PriceLine(Selection{
		Combo: "lunch", Quantity: 3,
		Picks: []Pick{
			{Slot: 1, Item: "coffee", Options: Choice{"size": {"l"}, "top": {"pudding"}}},
			{Slot: 0, Item: "banh-mi"},
		},
	}, opts(tue1500))
	if err != nil {
		t.Fatal(err)
	}
	// 49.000 combo + 5.000 (L) + 7.000 (pudding); the items' own prices are not charged.
	if l.Unit != 61_000 || l.Total != 183_000 || l.Kind != "combo" || l.Category != "combo" {
		t.Fatalf("line = %+v", l)
	}
	if len(l.Items) != 2 || l.Items[0] != "banh-mi" || l.Items[1] != "coffee" {
		t.Fatalf("items = %v, want them in slot order whatever order the picks came in", l.Items)
	}
	if len(l.Pieces) != 2 || l.Pieces[0].Station != "hot" || l.Pieces[1].Station != "drinks" {
		t.Fatalf("pieces = %+v: each part goes to its own station", l.Pieces)
	}
	if l.Needs["bread"] != 1 || l.Needs["coffee"] != 20 || l.Needs["milk"] != 50 {
		t.Fatalf("needs = %v", l.Needs)
	}
}

func TestAComboRefusesWrongPicks(t *testing.T) {
	m := fixture(t)
	good := func() []Pick {
		return []Pick{{Slot: 0, Item: "banh-mi"}, {Slot: 1, Item: "peach-tea", Options: Choice{"size": {"m"}}}}
	}
	if _, err := m.PriceLine(Selection{Combo: "lunch", Quantity: 1, Picks: good()}, opts(tue1359)); err != nil {
		t.Fatalf("the good picks: %v", err)
	}
	cases := map[string][]Pick{
		"a slot missing":      good()[:1],
		"a slot twice":        {{Slot: 0, Item: "banh-mi"}, {Slot: 0, Item: "salad"}},
		"a slot out of range": {{Slot: 0, Item: "banh-mi"}, {Slot: 2, Item: "coffee"}},
		"an item not offered": {{Slot: 0, Item: "coffee", Options: Choice{"size": {"m"}}}, {Slot: 1, Item: "peach-tea", Options: Choice{"size": {"m"}}}},
		"a missing option":    {{Slot: 0, Item: "banh-mi"}, {Slot: 1, Item: "coffee"}},
	}
	for name, picks := range cases {
		if _, err := m.PriceLine(Selection{Combo: "lunch", Quantity: 1, Picks: picks}, opts(tue1359)); !errors.Is(err, ErrChoice) {
			t.Errorf("%s: err = %v, want ErrChoice", name, err)
		}
	}
	if _, err := m.PriceLine(Selection{Combo: "nothing", Quantity: 1}, opts(tue1359)); !errors.Is(err, ErrUnknownCombo) {
		t.Errorf("unknown combo: %v", err)
	}
	// An item of the combo that is sold out makes the combo unavailable too.
	o := PriceOptions{At: tue1359, SoldOut: func(id ItemID) bool { return id == "banh-mi" }}
	if _, err := m.PriceLine(Selection{Combo: "lunch", Quantity: 1, Picks: good()}, o); !errors.Is(err, ErrSoldOut) {
		t.Errorf("a sold-out part: %v", err)
	}
}

func TestACartReportsEveryBadLineAndNoLinesAtAll(t *testing.T) {
	m := fixture(t)
	lines, errs := m.PriceCart([]Selection{
		{Item: "banh-mi", Quantity: 1},
		{Item: "seasonal", Quantity: 1},
		{Item: "coffee", Quantity: 1}, // the required size is missing
		{Item: "salad", Quantity: 2},
	}, opts(tue1500))
	if lines != nil || len(errs) != 2 {
		t.Fatalf("lines %v, errors %v; want no lines and two errors", lines, errs)
	}
	if errs[0].Index != 1 || !errors.Is(errs[0], ErrInactive) || errs[1].Index != 2 || !errors.Is(errs[1], ErrChoice) {
		t.Fatalf("errors = %v", errs)
	}

	lines, errs = m.PriceCart([]Selection{{Item: "banh-mi", Quantity: 1}, {Item: "salad", Quantity: 2}}, opts(tue1500))
	if errs != nil || len(lines) != 2 || lines[1].Total != 60_000 {
		t.Fatalf("a good cart: %v %v", lines, errs)
	}
	if _, errs := m.PriceCart(nil, opts(tue1500)); len(errs) != 1 {
		t.Errorf("an empty cart: %v", errs)
	}
	many := make([]Selection, money.MaxLines+1)
	for i := range many {
		many[i] = Selection{Item: "banh-mi", Quantity: 1}
	}
	if _, errs := m.PriceCart(many, opts(tue1500)); len(errs) != 1 || !errors.Is(errs[0], ErrTooManyLines) {
		t.Errorf("too many lines: %v", errs)
	}
}

func TestALineIsASnapshotOfTheMenu(t *testing.T) {
	m := fixture(t)
	l, err := m.PriceLine(Selection{Item: "banh-mi", Quantity: 1}, opts(tue1500))
	if err != nil {
		t.Fatal(err)
	}
	l.Needs["bread"] = 999 // scribbling on the line...
	again, _ := m.PriceLine(Selection{Item: "banh-mi", Quantity: 1}, opts(tue1500))
	if again.Needs["bread"] != 1 {
		t.Fatal("...changed the recipe in the menu")
	}
	it, _ := m.Item("banh-mi")
	if it.Recipe["bread"] != 1 {
		t.Fatal("the menu's recipe was changed through a line")
	}
}

func TestMenuValidation(t *testing.T) {
	base := func() []Item { return fixtureItems() }
	bad := map[string]func([]Item, []Combo) ([]Item, []Combo){
		"duplicate item": func(i []Item, c []Combo) ([]Item, []Combo) { return append(i, i[0]), c },
		"no station":     func(i []Item, c []Combo) ([]Item, []Combo) { i[0].Station = ""; return i, c },
		"negative price": func(i []Item, c []Combo) ([]Item, []Combo) { i[0].Price = -1; return i, c },
		"price too high": func(i []Item, c []Combo) ([]Item, []Combo) { i[0].Price = money.MaxPrice + 1; return i, c },
		"no name":        func(i []Item, c []Combo) ([]Item, []Combo) { i[0].Name = "  "; return i, c },
		"bad recipe":     func(i []Item, c []Combo) ([]Item, []Combo) { i[0].Recipe = Recipe{"x": 0}; return i, c },
		"min above max": func(i []Item, c []Combo) ([]Item, []Combo) {
			i[0].Groups = []Group{{ID: "g", Name: "g", Min: 2, Max: 1, Options: []Option{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}}}}
			return i, c
		},
		"max above the options": func(i []Item, c []Combo) ([]Item, []Combo) {
			i[0].Groups = []Group{{ID: "g", Name: "g", Min: 0, Max: 3, Options: []Option{{ID: "a", Name: "a"}}}}
			return i, c
		},
		"duplicate option": func(i []Item, c []Combo) ([]Item, []Combo) {
			i[0].Groups = []Group{{ID: "g", Name: "g", Min: 0, Max: 1, Options: []Option{{ID: "a", Name: "a"}, {ID: "a", Name: "b"}}}}
			return i, c
		},
		"a window that is backwards": func(i []Item, c []Combo) ([]Item, []Combo) {
			i[1].TimePrices = []TimePrice{{From: 20 * 60, To: 2 * 60, Price: 1}}
			return i, c
		},
		"a window past midnight": func(i []Item, c []Combo) ([]Item, []Combo) {
			i[1].TimePrices = []TimePrice{{From: 0, To: 24*60 + 1, Price: 1}}
			return i, c
		},
		"a combo with no slots": func(i []Item, c []Combo) ([]Item, []Combo) { c[0].Slots = nil; return i, c },
		"a combo slot with an unknown item": func(i []Item, c []Combo) ([]Item, []Combo) {
			c[0].Slots[0].Items = []ItemID{"ghost"}
			return i, c
		},
		"a combo slot with an item twice": func(i []Item, c []Combo) ([]Item, []Combo) {
			c[0].Slots[0].Items = []ItemID{"salad", "salad"}
			return i, c
		},
		"duplicate combo": func(i []Item, c []Combo) ([]Item, []Combo) { return i, append(c, c[0]) },
	}
	for name, mutate := range bad {
		items, combos := mutate(base(), fixtureCombos())
		if _, err := New(items, combos); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestListingTheMenu(t *testing.T) {
	m := fixture(t)
	items := m.Items()
	if len(items) != len(fixtureItems()) || items[0].ID != "coffee" || items[1].ID != "peach-tea" {
		t.Fatalf("Items = %v: they must come back in the order given", items)
	}
	if got := strings.Join(m.Categories(), ","); got != "drinks,food" {
		t.Fatalf("Categories = %q", got)
	}
	if _, ok := m.Combo("lunch"); !ok {
		t.Fatal("the combo is missing")
	}
	if _, ok := m.Combo("none"); ok {
		t.Fatal("a combo that does not exist was found")
	}
}
