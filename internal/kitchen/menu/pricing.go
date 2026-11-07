package menu

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
)

// Choice maps a group to the options picked in it.
type Choice map[GroupID][]OptionID

// Pick is the item chosen for one slot of a combo, with its own options.
type Pick struct {
	Slot    int
	Item    ItemID
	Options Choice
}

// Selection is what a customer put in the cart: one item, or one combo, a
// quantity, and an optional note for the kitchen.
type Selection struct {
	Item     ItemID
	Combo    ComboID
	Options  Choice // for an item
	Picks    []Pick // for a combo: one per slot
	Quantity int
	Note     string
}

// Piece is one thing the kitchen has to make, and where.
type Piece struct {
	Station Station
	Name    string // including the chosen options, for the ticket
}

// Line is a priced selection, frozen as it was when the order was taken.
type Line struct {
	Kind     string // "item" or "combo"
	Ref      string // the item or combo ID
	Name     string
	Category string // "combo" for a combo
	Options  []string
	Unit     money.Money
	Quantity int
	Total    money.Money
	Note     string
	// Items lists the item IDs the line contains: the item itself, or the items
	// picked in a combo. Promotions that apply to certain items use it.
	Items []ItemID
	// Pieces is what the kitchen makes for ONE unit; the ticket repeats it
	// Quantity times.
	Pieces []Piece
	// Needs is the ingredients ONE unit uses.
	Needs Recipe
}

// PriceOptions are what pricing needs to know about the moment.
type PriceOptions struct {
	// At is the time of the order, in the venue's local time zone: time windows
	// are read off its clock.
	At time.Time
	// SoldOut reports an item that cannot be made right now (out of stock).
	// Optional: nil means nothing is sold out.
	SoldOut func(ItemID) bool
}

// LineError says which line of a cart was refused and why.
type LineError struct {
	Index int
	Err   error
}

func (e LineError) Error() string { return fmt.Sprintf("line %d: %v", e.Index+1, e.Err) }
func (e LineError) Unwrap() error { return e.Err }

// PriceCart prices every selection. It returns all the lines when they are all
// valid; otherwise it returns no lines and one LineError for every selection that
// was refused, so that a screen can mark each of them.
func (m *Menu) PriceCart(sels []Selection, opts PriceOptions) ([]Line, []LineError) {
	if len(sels) == 0 {
		return nil, []LineError{{Index: 0, Err: fmt.Errorf("%w: the cart is empty", ErrSelection)}}
	}
	if len(sels) > money.MaxLines {
		return nil, []LineError{{Index: money.MaxLines, Err: fmt.Errorf("%w: at most %d", ErrTooManyLines, money.MaxLines)}}
	}
	var lines []Line
	var errs []LineError
	for i, s := range sels {
		l, err := m.PriceLine(s, opts)
		if err != nil {
			errs = append(errs, LineError{Index: i, Err: err})
			continue
		}
		lines = append(lines, l)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return lines, nil
}

// PriceLine checks one selection against the menu and prices it.
func (m *Menu) PriceLine(s Selection, opts PriceOptions) (Line, error) {
	if s.Quantity < 1 || s.Quantity > money.MaxQuantity {
		return Line{}, fmt.Errorf("%w: %d (allowed 1..%d)", ErrQuantity, s.Quantity, money.MaxQuantity)
	}
	if utf8.RuneCountInString(s.Note) > MaxNoteRunes || !utf8.ValidString(s.Note) || strings.ContainsFunc(s.Note, func(r rune) bool { return r < 0x20 && r != '\n' || r == 0x7f }) {
		return Line{}, fmt.Errorf("%w: at most %d characters of text", ErrNote, MaxNoteRunes)
	}
	if (s.Item == "") == (s.Combo == "") {
		return Line{}, ErrSelection
	}

	var l Line
	var err error
	if s.Item != "" {
		l, err = m.priceItem(s, opts)
	} else {
		l, err = m.priceCombo(s, opts)
	}
	if err != nil {
		return Line{}, err
	}
	if l.Unit < 0 || l.Unit > money.MaxPrice {
		return Line{}, fmt.Errorf("%w: %v for %q", ErrPrice, l.Unit, l.Name)
	}
	l.Quantity = s.Quantity
	l.Total = l.Unit.Times(s.Quantity)
	l.Note = strings.TrimSpace(s.Note)
	return l, nil
}

// available checks that an item can be sold now.
func (m *Menu) available(id ItemID, opts PriceOptions) (*Item, error) {
	it, ok := m.items[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownItem, id)
	}
	if !it.Active {
		return nil, fmt.Errorf("%w: %s", ErrInactive, it.Name)
	}
	if opts.SoldOut != nil && opts.SoldOut(id) {
		return nil, fmt.Errorf("%w: %s", ErrSoldOut, it.Name)
	}
	return it, nil
}

// choose checks a choice against the groups of an item and returns the options
// picked, in menu order, so that the same choice always gives the same line.
func choose(it *Item, c Choice) ([]Option, error) {
	for gid := range c {
		known := false
		for _, g := range it.Groups {
			if g.ID == gid {
				known = true
			}
		}
		if !known {
			return nil, fmt.Errorf("%w: %s has no group %q", ErrChoice, it.Name, gid)
		}
	}
	var picked []Option
	for _, g := range it.Groups {
		ids := c[g.ID]
		if len(ids) < g.Min || len(ids) > g.Max {
			return nil, fmt.Errorf("%w: %s / %s needs %s, got %d", ErrChoice, it.Name, g.Name, rangeText(g.Min, g.Max), len(ids))
		}
		seen := map[OptionID]bool{}
		for _, id := range ids {
			if seen[id] {
				return nil, fmt.Errorf("%w: %s / %s lists %q twice", ErrChoice, it.Name, g.Name, id)
			}
			seen[id] = true
		}
		for _, o := range g.Options { // menu order, not request order
			if seen[o.ID] {
				picked = append(picked, o)
				delete(seen, o.ID)
			}
		}
		if len(seen) > 0 {
			rest := make([]string, 0, len(seen))
			for id := range seen {
				rest = append(rest, string(id))
			}
			sort.Strings(rest)
			return nil, fmt.Errorf("%w: %s / %s has no option %s", ErrChoice, it.Name, g.Name, strings.Join(rest, ", "))
		}
	}
	return picked, nil
}

func rangeText(lo, hi int) string {
	if lo == hi {
		return fmt.Sprintf("exactly %d", lo)
	}
	return fmt.Sprintf("%d to %d", lo, hi)
}

func addRecipe(into Recipe, from Recipe) {
	for ing, n := range from {
		into[ing] += n
	}
}

func describe(name string, picked []Option) (string, []string) {
	names := make([]string, len(picked))
	for i, o := range picked {
		names[i] = o.Name
	}
	if len(names) == 0 {
		return name, nil
	}
	return name + " (" + strings.Join(names, ", ") + ")", names
}

func (m *Menu) priceItem(s Selection, opts PriceOptions) (Line, error) {
	it, err := m.available(s.Item, opts)
	if err != nil {
		return Line{}, err
	}
	picked, err := choose(it, s.Options)
	if err != nil {
		return Line{}, err
	}
	unit := it.UnitPrice(opts.At)
	needs := Recipe{}
	addRecipe(needs, it.Recipe)
	for _, o := range picked {
		unit += o.Delta
		addRecipe(needs, o.Recipe)
	}
	full, names := describe(it.Name, picked)
	return Line{
		Kind: "item", Ref: string(it.ID), Name: it.Name, Category: it.Category, Options: names,
		Unit: unit, Items: []ItemID{it.ID}, Needs: needs,
		Pieces: []Piece{{Station: it.Station, Name: full}},
	}, nil
}

func (m *Menu) priceCombo(s Selection, opts PriceOptions) (Line, error) {
	c, ok := m.combos[s.Combo]
	if !ok {
		return Line{}, fmt.Errorf("%w: %q", ErrUnknownCombo, s.Combo)
	}
	if !c.Active {
		return Line{}, fmt.Errorf("%w: %s", ErrInactive, c.Name)
	}
	if len(s.Picks) != len(c.Slots) {
		return Line{}, fmt.Errorf("%w: %s has %d slots, got %d picks", ErrChoice, c.Name, len(c.Slots), len(s.Picks))
	}
	bySlot := make([]*Pick, len(c.Slots))
	for i := range s.Picks {
		p := &s.Picks[i]
		if p.Slot < 0 || p.Slot >= len(c.Slots) || bySlot[p.Slot] != nil {
			return Line{}, fmt.Errorf("%w: %s: slot %d is out of range or picked twice", ErrChoice, c.Name, p.Slot)
		}
		bySlot[p.Slot] = p
	}

	unit := c.Price
	needs := Recipe{}
	var optionNames []string
	var itemIDs []ItemID
	var pieces []Piece
	for i, slot := range c.Slots {
		p := bySlot[i]
		allowed := false
		for _, id := range slot.Items {
			if id == p.Item {
				allowed = true
			}
		}
		if !allowed {
			return Line{}, fmt.Errorf("%w: %s / %s does not offer %q", ErrChoice, c.Name, slot.Name, p.Item)
		}
		it, err := m.available(p.Item, opts)
		if err != nil {
			return Line{}, err
		}
		picked, err := choose(it, p.Options)
		if err != nil {
			return Line{}, err
		}
		addRecipe(needs, it.Recipe)
		for _, o := range picked {
			unit += o.Delta
			addRecipe(needs, o.Recipe)
			optionNames = append(optionNames, it.Name+": "+o.Name)
		}
		full, _ := describe(it.Name, picked)
		pieces = append(pieces, Piece{Station: it.Station, Name: full})
		itemIDs = append(itemIDs, it.ID)
	}
	return Line{
		Kind: "combo", Ref: string(c.ID), Name: c.Name, Category: "combo", Options: optionNames,
		Unit: unit, Items: itemIDs, Needs: needs, Pieces: pieces,
	}, nil
}
