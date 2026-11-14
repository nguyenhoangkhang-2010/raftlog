// Package menu holds what is for sale and works out what a choice costs.
//
// A Menu is validated once, when it is built, so that pricing can rely on it. A
// customer's choice (a Selection) is checked against the menu and turned into a
// Line that carries everything about that choice as it was at that moment: the
// names, the price, the pieces the kitchen must make, the ingredients it uses. A
// Line is a snapshot. Editing the menu later never changes an order already taken.
package menu

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
)

// Identifiers.
type (
	ItemID     string
	ComboID    string
	GroupID    string
	OptionID   string
	Station    string // where the kitchen makes it, for example "drinks" or "hot"
	Ingredient string // a stock item, for example "coffee-beans"
)

// Recipe is how much of each ingredient one unit uses, in the ingredient's base
// unit (grams, millilitres, pieces). Amounts are whole numbers.
type Recipe map[Ingredient]int64

// Option is one choice inside a group, such as "less ice" or "extra pearls".
type Option struct {
	ID    OptionID
	Name  string
	Delta money.Money // added to the unit price; may be negative (a smaller size)
	// Recipe is what the option uses on top of the item's own recipe.
	Recipe Recipe
}

// Group is a set of options of which a customer picks between Min and Max.
// Min 0 makes the group optional, Min 1 and Max 1 a required single choice.
type Group struct {
	ID      GroupID
	Name    string
	Min     int
	Max     int
	Options []Option
}

// TimePrice replaces an item's price during a window of the day, for instance a
// happy hour. The window is [From, To) in minutes since midnight, in the venue's
// local time, on the listed weekdays (none listed: every day).
type TimePrice struct {
	Days  []time.Weekday
	From  int
	To    int
	Price money.Money
}

// Item is a thing on the menu.
type Item struct {
	ID         ItemID
	Name       string
	Category   string
	Station    Station
	Price      money.Money
	TimePrices []TimePrice // the first window that matches wins
	Groups     []Group
	Recipe     Recipe
	// Active is false for an item that is on the menu but not for sale.
	Active bool
}

// Slot is one choice inside a combo: exactly one of Items.
type Slot struct {
	Name  string
	Items []ItemID
}

// Combo is a fixed-price meal made of one item from each slot. The items'
// own prices are not charged; the options the customer picks on them are.
type Combo struct {
	ID     ComboID
	Name   string
	Price  money.Money
	Slots  []Slot
	Active bool
}

// Menu is a validated set of items and combos.
type Menu struct {
	items  map[ItemID]*Item
	combos map[ComboID]*Combo
	order  []ItemID // the order items were given in
}

// Errors returned for a choice that does not fit the menu. They are wrapped with
// the detail, so test with errors.Is.
var (
	ErrUnknownItem  = errors.New("menu: no such item")
	ErrUnknownCombo = errors.New("menu: no such combo")
	ErrInactive     = errors.New("menu: not for sale")
	ErrSoldOut      = errors.New("menu: sold out")
	ErrChoice       = errors.New("menu: the options do not fit the item")
	ErrQuantity     = errors.New("menu: quantity out of range")
	ErrNote         = errors.New("menu: invalid note")
	ErrPrice        = errors.New("menu: price out of range")
	ErrSelection    = errors.New("menu: a selection needs exactly one of item or combo")
	ErrTooManyLines = errors.New("menu: too many lines in one order")
)

// MaxNoteRunes bounds the free-text note on a line.
const MaxNoteRunes = 200

// New validates the items and combos and returns the menu.
func New(items []Item, combos []Combo) (*Menu, error) {
	m := &Menu{items: map[ItemID]*Item{}, combos: map[ComboID]*Combo{}}
	for i := range items {
		it := items[i]
		if err := validateItem(it); err != nil {
			return nil, fmt.Errorf("item %q: %w", it.ID, err)
		}
		if _, dup := m.items[it.ID]; dup {
			return nil, fmt.Errorf("item %q: duplicate ID", it.ID)
		}
		m.items[it.ID] = &it
		m.order = append(m.order, it.ID)
	}
	for i := range combos {
		c := combos[i]
		if err := m.validateCombo(c); err != nil {
			return nil, fmt.Errorf("combo %q: %w", c.ID, err)
		}
		if _, dup := m.combos[c.ID]; dup {
			return nil, fmt.Errorf("combo %q: duplicate ID", c.ID)
		}
		m.combos[c.ID] = &c
	}
	return m, nil
}

func validPrice(p money.Money) bool { return p >= 0 && p <= money.MaxPrice }

func validateRecipe(r Recipe) error {
	for ing, n := range r {
		if ing == "" || n <= 0 {
			return fmt.Errorf("recipe: %q needs a positive amount, got %d", ing, n)
		}
	}
	return nil
}

func validateItem(it Item) error {
	switch {
	case it.ID == "" || strings.TrimSpace(it.Name) == "":
		return errors.New("an item needs an ID and a name")
	case it.Station == "":
		return errors.New("an item needs a station")
	case !validPrice(it.Price):
		return fmt.Errorf("price %d is outside 0..%d", it.Price, money.MaxPrice)
	}
	if err := validateRecipe(it.Recipe); err != nil {
		return err
	}
	for _, tp := range it.TimePrices {
		if tp.From < 0 || tp.To > 24*60 || tp.From >= tp.To {
			return fmt.Errorf("time price window [%d,%d) is not inside one day", tp.From, tp.To)
		}
		if !validPrice(tp.Price) {
			return fmt.Errorf("time price %d is outside 0..%d", tp.Price, money.MaxPrice)
		}
		for _, d := range tp.Days {
			if d < time.Sunday || d > time.Saturday {
				return fmt.Errorf("weekday %d does not exist", d)
			}
		}
	}
	groups := map[GroupID]bool{}
	for _, g := range it.Groups {
		if g.ID == "" || groups[g.ID] {
			return fmt.Errorf("group %q: missing or duplicate ID", g.ID)
		}
		groups[g.ID] = true
		if g.Min < 0 || g.Max < 1 || g.Min > g.Max || g.Max > len(g.Options) {
			return fmt.Errorf("group %q: needs 0 <= min <= max <= number of options, and max >= 1 (min %d, max %d, options %d)", g.ID, g.Min, g.Max, len(g.Options))
		}
		opts := map[OptionID]bool{}
		for _, o := range g.Options {
			if o.ID == "" || strings.TrimSpace(o.Name) == "" || opts[o.ID] {
				return fmt.Errorf("group %q: option %q is missing a name or has a duplicate ID", g.ID, o.ID)
			}
			opts[o.ID] = true
			if o.Delta < -money.MaxPrice || o.Delta > money.MaxPrice {
				return fmt.Errorf("group %q option %q: delta %d is out of range", g.ID, o.ID, o.Delta)
			}
			if err := validateRecipe(o.Recipe); err != nil {
				return fmt.Errorf("group %q option %q: %w", g.ID, o.ID, err)
			}
		}
	}
	return nil
}

func (m *Menu) validateCombo(c Combo) error {
	switch {
	case c.ID == "" || strings.TrimSpace(c.Name) == "":
		return errors.New("a combo needs an ID and a name")
	case !validPrice(c.Price):
		return fmt.Errorf("price %d is outside 0..%d", c.Price, money.MaxPrice)
	case len(c.Slots) == 0:
		return errors.New("a combo needs at least one slot")
	}
	for i, s := range c.Slots {
		if len(s.Items) == 0 {
			return fmt.Errorf("slot %d has no items to choose from", i)
		}
		seen := map[ItemID]bool{}
		for _, id := range s.Items {
			if _, ok := m.items[id]; !ok {
				return fmt.Errorf("slot %d refers to the unknown item %q", i, id)
			}
			if seen[id] {
				return fmt.Errorf("slot %d lists %q twice", i, id)
			}
			seen[id] = true
		}
	}
	return nil
}

// Item returns an item by ID.
func (m *Menu) Item(id ItemID) (Item, bool) {
	it, ok := m.items[id]
	if !ok {
		return Item{}, false
	}
	return *it, true
}

// Combo returns a combo by ID.
func (m *Menu) Combo(id ComboID) (Combo, bool) {
	c, ok := m.combos[id]
	if !ok {
		return Combo{}, false
	}
	return *c, true
}

// Items returns the items in the order they were given.
func (m *Menu) Items() []Item {
	out := make([]Item, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, *m.items[id])
	}
	return out
}

// Combos returns the combos, sorted by ID.
func (m *Menu) Combos() []Combo {
	out := make([]Combo, 0, len(m.combos))
	for _, c := range m.combos {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Categories returns the categories in use, sorted.
func (m *Menu) Categories() []string {
	seen := map[string]bool{}
	for _, it := range m.items {
		seen[it.Category] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// UnitPrice returns the price of an item at a moment: the first matching time
// window, or the normal price.
func (it Item) UnitPrice(at time.Time) money.Money {
	minute := at.Hour()*60 + at.Minute()
	for _, tp := range it.TimePrices {
		if minute < tp.From || minute >= tp.To {
			continue
		}
		if len(tp.Days) == 0 {
			return tp.Price
		}
		for _, d := range tp.Days {
			if d == at.Weekday() {
				return tp.Price
			}
		}
	}
	return it.Price
}
