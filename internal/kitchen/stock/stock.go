// Package stock keeps count of ingredients, in lots.
//
// Everything that changes the count is a Movement, applied through one method.
// The same movements applied in the same order give the same stock on every
// machine, which is what lets the stock be rebuilt from a log. The one invariant
// is that no ingredient is ever below zero: a movement that would do that is
// refused, and refused whole, leaving the stock as it was.
//
// Goods arrive in lots: a quantity of one ingredient that came together, with a price paid
// for it and perhaps a date after which it must not be used. What is taken goes first from the
// lot that expires first (a lot with no date after those that have one, and among equals the
// oldest), and a lot that has expired by the time of the movement is not used to make an order.
// The quantity of an ingredient is always the sum of its lots.
package stock

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
)

// Ingredient is a stock item.
type Ingredient = menu.Ingredient

// Recipe is a list of amounts per ingredient.
type Recipe = menu.Recipe

// Kind is the type of movement.
type Kind string

// The kinds of movement.
const (
	Receive  Kind = "receive"   // goods arrive, as a new lot
	Consume  Kind = "consume"   // an order takes what it needs
	Release  Kind = "release"   // an order that was cancelled gives it back
	WriteOff Kind = "write_off" // spoiled, spilled or lost
	Count    Kind = "count"     // a stocktake sets the real quantity
	Adjust   Kind = "adjust"    // a stocktake found a difference: Qty is the signed change
)

// The causes of a write-off, for telling where the waste comes from.
const (
	CauseSpoiled = "spoiled"
	CauseExpired = "expired"
	CauseSpilled = "spilled"
	CauseOther   = "other"
)

// Causes lists the causes of a write-off.
func Causes() []string { return []string{CauseSpoiled, CauseExpired, CauseSpilled, CauseOther} }

// ValidCause says whether c is a cause of a write-off; none at all counts as "other".
func ValidCause(c string) bool {
	if c == "" {
		return true
	}
	for _, k := range Causes() {
		if c == k {
			return true
		}
	}
	return false
}

// Lot is a quantity of one ingredient that arrived together.
type Lot struct {
	ID         string
	Ingredient Ingredient
	// Qty is what is left of the lot; Received is what came.
	Qty, Received int64
	ReceivedAt    time.Time
	// ExpiresAt is the moment after which the lot is not used; zero if it has no date.
	ExpiresAt time.Time
	// UnitCostMilli is what a unit cost, in thousandths of a đồng (a gram of coffee costs
	// a fraction of a đồng); 0 if the price is not known.
	UnitCostMilli int64
	Supplier      string
	Document      string
}

// Take is a part of a lot that was used, kept so that what is given back goes to the same
// lot with the same price. Lot.Qty and Lot.Received are not meaningful in a Take.
type Take struct {
	Ingredient Ingredient
	Qty        int64
	Lot        Lot
}

// Movement is one change of the stock.
type Movement struct {
	Kind Kind
	// Ingredient and Qty: for Receive and WriteOff the amount; for Count the real
	// quantity found; for Adjust the signed difference.
	Ingredient Ingredient
	Qty        int64
	// Needs: for Consume and Release, what an order uses.
	Needs Recipe
	// Ref says what caused it, for example an order ID. For Receive it is the ID of the new lot.
	Ref string
	// Reason is required for WriteOff, Count and Adjust.
	Reason string
	// At is when it happens. A Consume does not use a lot that has expired by then; zero means
	// that no lot counts as expired.
	At time.Time

	// For Receive: when the lot expires (zero for never), what was paid for all of it in whole
	// đồng (0 if not known), who it came from and the number of the document.
	ExpiresAt time.Time
	TotalCost int64
	Supplier  string
	Document  string

	// For WriteOff: take from this lot only, and the cause.
	FromLot string
	Cause   string
	// For Release: what the order took, to put it back where it came from.
	Takes []Take
}

// Result says what a movement did to the lots.
type Result struct {
	// Lot is the lot that Receive made.
	Lot string
	// Takes are the parts of lots that a Consume, WriteOff or negative Adjust used, in the order
	// they were used.
	Takes []Take
	// ValueMilli is the worth of what was taken, or put back or received, at the price of its lots
	// (thousandths of a đồng); 0 where the price was not known.
	ValueMilli int64
}

// Shortage is one ingredient that there is not enough of.
type Shortage struct {
	Ingredient Ingredient
	Need       int64
	Have       int64
}

// ShortageError lists everything that was short, so that a screen can say it all
// at once.
type ShortageError struct {
	Missing []Shortage
}

func (e *ShortageError) Error() string {
	parts := make([]string, len(e.Missing))
	for i, s := range e.Missing {
		parts[i] = fmt.Sprintf("%s: need %d, have %d", s.Ingredient, s.Need, s.Have)
	}
	return "stock: not enough " + strings.Join(parts, "; ")
}

// Errors for a movement that cannot be made.
var (
	ErrQuantity = errors.New("stock: the quantity must be positive")
	ErrReason   = errors.New("stock: a reason is required")
	ErrUnknown  = errors.New("stock: unknown kind of movement")
	ErrNegative = errors.New("stock: a count cannot be negative")
	ErrCost     = errors.New("stock: what was paid cannot be negative")
	ErrExpiry   = errors.New("stock: the lot expires before it arrives")
	ErrCause    = errors.New("stock: not a cause of waste")
	ErrNoLot    = errors.New("stock: no such lot of that ingredient")
)

// Level is the state of one ingredient.
type Level struct {
	Qty int64
	// LowAt is the quantity at or below which the ingredient counts as running
	// low. 0 means no alert.
	LowAt int64
}

// Stock is the current count of every ingredient.
type Stock struct {
	levels map[Ingredient]Level
	lots   map[Ingredient][]*Lot
	seq    int // numbers the lots that have no ID of their own
}

// New returns a stock with the given opening levels, which must not be negative. What is
// there at the start is one lot per ingredient, with no date and no price.
func New(opening map[Ingredient]Level) (*Stock, error) {
	s := &Stock{levels: make(map[Ingredient]Level, len(opening)), lots: map[Ingredient][]*Lot{}}
	for ing, l := range opening {
		if ing == "" || l.Qty < 0 || l.LowAt < 0 {
			return nil, fmt.Errorf("stock: bad opening level for %q", ing)
		}
		s.levels[ing] = Level{LowAt: l.LowAt}
		if l.Qty > 0 {
			s.addLot(&Lot{ID: "opening-" + string(ing), Ingredient: ing, Qty: l.Qty, Received: l.Qty})
		}
	}
	return s, nil
}

// Qty returns how much of an ingredient there is, expired or not; 0 for one never seen.
func (s *Stock) Qty(ing Ingredient) int64 { return s.levels[ing].Qty }

// Usable returns how much of an ingredient can be used at a moment: what is not in a lot that
// has expired by then. A zero time counts nothing as expired.
func (s *Stock) Usable(ing Ingredient, at time.Time) int64 {
	var n int64
	for _, l := range s.lots[ing] {
		if usable(l, at) {
			n += l.Qty
		}
	}
	return n
}

func usable(l *Lot, at time.Time) bool {
	return at.IsZero() || l.ExpiresAt.IsZero() || at.Before(l.ExpiresAt)
}

// LowAt returns the alert threshold of an ingredient; 0 means none.
func (s *Stock) LowAt(ing Ingredient) int64 { return s.levels[ing].LowAt }

// SetLowAt sets the alert threshold of an ingredient.
func (s *Stock) SetLowAt(ing Ingredient, at int64) error {
	if at < 0 || ing == "" {
		return ErrQuantity
	}
	l := s.levels[ing]
	l.LowAt = at
	s.levels[ing] = l
	return nil
}

func (s *Stock) addLot(l *Lot) {
	s.lots[l.Ingredient] = append(s.lots[l.Ingredient], l)
	lv := s.levels[l.Ingredient]
	lv.Qty += l.Qty
	s.levels[l.Ingredient] = lv
}

// order returns the lots of an ingredient in the order they are used: the soonest to expire
// first, those with no date last, and among equals the oldest.
func (s *Stock) order(ing Ingredient) []*Lot {
	out := append([]*Lot(nil), s.lots[ing]...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ExpiresAt.IsZero() != b.ExpiresAt.IsZero() {
			return !a.ExpiresAt.IsZero()
		}
		if !a.ExpiresAt.Equal(b.ExpiresAt) {
			return a.ExpiresAt.Before(b.ExpiresAt)
		}
		if !a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ReceivedAt.Before(b.ReceivedAt)
		}
		return a.ID < b.ID
	})
	return out
}

// take uses qty of an ingredient from its lots in order, skipping the lots that have expired by
// at (zero: none), or only from one lot. The caller has checked that there is enough.
func (s *Stock) take(ing Ingredient, qty int64, at time.Time, only string) ([]Take, int64) {
	var takes []Take
	var value int64
	left := qty
	for _, l := range s.order(ing) {
		if left == 0 {
			break
		}
		if (only != "" && l.ID != only) || !usable(l, at) {
			continue
		}
		n := l.Qty
		if n > left {
			n = left
		}
		t := Take{Ingredient: ing, Qty: n, Lot: *l}
		t.Lot.Qty, t.Lot.Received = 0, 0
		takes = append(takes, t)
		value += n * l.UnitCostMilli
		l.Qty -= n
		left -= n
	}
	// a lot that is used up is gone: what it was lives on in the takes and in the history
	kept := s.lots[ing][:0]
	for _, l := range s.lots[ing] {
		if l.Qty > 0 {
			kept = append(kept, l)
		}
	}
	s.lots[ing] = kept
	lv := s.levels[ing]
	lv.Qty -= qty
	s.levels[ing] = lv
	return takes, value
}

// give puts back what takes took: into the lot it came from if that is still there, else into a
// lot made again with the same date and price.
func (s *Stock) give(takes []Take) int64 {
	var value int64
	for _, t := range takes {
		var into *Lot
		for _, l := range s.lots[t.Ingredient] {
			if l.ID == t.Lot.ID {
				into = l
				break
			}
		}
		if into == nil {
			lot := t.Lot
			lot.Qty, lot.Received = 0, t.Qty
			into = &lot
			s.lots[t.Ingredient] = append(s.lots[t.Ingredient], into)
		}
		into.Qty += t.Qty
		lv := s.levels[t.Ingredient]
		lv.Qty += t.Qty
		s.levels[t.Ingredient] = lv
		value += t.Qty * t.Lot.UnitCostMilli
	}
	return value
}

// averageCostMilli is the average price of what is in stock, for a surplus that a count finds
// and that cost nothing that anyone wrote down; 0 if no price is known.
func (s *Stock) averageCostMilli(ing Ingredient) int64 {
	var qty, value int64
	for _, l := range s.lots[ing] {
		if l.UnitCostMilli > 0 {
			qty += l.Qty
			value += l.Qty * l.UnitCostMilli
		}
	}
	if qty == 0 {
		return 0
	}
	return (value + qty/2) / qty
}

// Apply makes one movement. It changes nothing if it returns an error.
func (s *Stock) Apply(m Movement) error {
	_, err := s.Do(m)
	return err
}

// Do makes one movement and says what it did to the lots. It changes nothing if it returns an
// error.
func (s *Stock) Do(m Movement) (Result, error) {
	switch m.Kind {
	case Receive:
		return s.receive(m)
	case WriteOff:
		if m.Qty <= 0 || m.Ingredient == "" {
			return Result{}, ErrQuantity
		}
		if strings.TrimSpace(m.Reason) == "" {
			return Result{}, ErrReason
		}
		if !ValidCause(m.Cause) {
			return Result{}, ErrCause
		}
		have := s.levels[m.Ingredient].Qty
		if m.FromLot != "" {
			have = 0
			found := false
			for _, l := range s.lots[m.Ingredient] {
				if l.ID == m.FromLot {
					have, found = l.Qty, true
				}
			}
			if !found {
				return Result{}, ErrNoLot
			}
		}
		if have < m.Qty {
			return Result{}, &ShortageError{Missing: []Shortage{{m.Ingredient, m.Qty, have}}}
		}
		takes, v := s.take(m.Ingredient, m.Qty, time.Time{}, m.FromLot)
		return Result{Takes: takes, ValueMilli: v}, nil
	case Count:
		if m.Ingredient == "" {
			return Result{}, ErrQuantity
		}
		if m.Qty < 0 {
			return Result{}, ErrNegative
		}
		if strings.TrimSpace(m.Reason) == "" {
			return Result{}, ErrReason
		}
		return s.adjust(m, m.Qty-s.levels[m.Ingredient].Qty)
	case Adjust:
		if m.Ingredient == "" || m.Qty == 0 {
			return Result{}, ErrQuantity
		}
		if strings.TrimSpace(m.Reason) == "" {
			return Result{}, ErrReason
		}
		return s.adjust(m, m.Qty)
	case Consume:
		if err := validNeeds(m.Needs); err != nil {
			return Result{}, err
		}
		if short := s.shortages(m.Needs, m.At); len(short) > 0 {
			return Result{}, &ShortageError{Missing: short}
		}
		var res Result
		for _, ing := range sortedNeeds(m.Needs) {
			takes, v := s.take(ing, m.Needs[ing], m.At, "")
			res.Takes = append(res.Takes, takes...)
			res.ValueMilli += v
		}
		return res, nil
	case Release:
		if err := validNeeds(m.Needs); err != nil {
			return Result{}, err
		}
		if len(m.Takes) > 0 {
			return Result{ValueMilli: s.give(m.Takes)}, nil
		}
		// nothing says where it came from: it goes in a lot of its own, with no date and no price
		for _, ing := range sortedNeeds(m.Needs) {
			id := fmt.Sprintf("restock-%s-%s", m.Ref, ing)
			if m.Ref == "" {
				s.seq++
				id = fmt.Sprintf("restock-%d", s.seq)
			}
			s.addLot(&Lot{ID: id, Ingredient: ing, Qty: m.Needs[ing], Received: m.Needs[ing], ReceivedAt: m.At})
		}
		return Result{}, nil
	default:
		return Result{}, fmt.Errorf("%w: %q", ErrUnknown, m.Kind)
	}
}

func (s *Stock) receive(m Movement) (Result, error) {
	if m.Qty <= 0 || m.Ingredient == "" {
		return Result{}, ErrQuantity
	}
	if m.TotalCost < 0 {
		return Result{}, ErrCost
	}
	if !m.ExpiresAt.IsZero() && !m.At.IsZero() && !m.ExpiresAt.After(m.At) {
		return Result{}, ErrExpiry
	}
	id := m.Ref
	if id == "" {
		s.seq++
		id = fmt.Sprintf("lot-%d", s.seq)
	}
	lot := &Lot{ID: id, Ingredient: m.Ingredient, Qty: m.Qty, Received: m.Qty, ReceivedAt: m.At, ExpiresAt: m.ExpiresAt,
		Supplier: m.Supplier, Document: m.Document}
	if m.TotalCost > 0 {
		lot.UnitCostMilli = (m.TotalCost*1000 + m.Qty/2) / m.Qty
	}
	s.addLot(lot)
	return Result{Lot: id, ValueMilli: m.Qty * lot.UnitCostMilli}, nil
}

// adjust changes the quantity by a signed amount: a surplus becomes a lot of its own, priced at the
// average of what is in stock; a loss is taken from the lots in order.
func (s *Stock) adjust(m Movement, delta int64) (Result, error) {
	switch {
	case delta == 0:
		return Result{}, nil
	case delta > 0:
		id := "found-" + m.Ref
		if m.Ref == "" {
			s.seq++
			id = fmt.Sprintf("found-%d", s.seq)
		}
		unit := s.averageCostMilli(m.Ingredient)
		s.addLot(&Lot{ID: id, Ingredient: m.Ingredient, Qty: delta, Received: delta, ReceivedAt: m.At, UnitCostMilli: unit})
		return Result{Lot: id, ValueMilli: delta * unit}, nil
	}
	if have := s.levels[m.Ingredient].Qty; have < -delta {
		return Result{}, &ShortageError{Missing: []Shortage{{m.Ingredient, -delta, have}}}
	}
	takes, v := s.take(m.Ingredient, -delta, time.Time{}, "")
	return Result{Takes: takes, ValueMilli: v}, nil
}

func validNeeds(r Recipe) error {
	for ing, n := range r {
		if ing == "" || n <= 0 {
			return ErrQuantity
		}
	}
	return nil
}

func sortedNeeds(r Recipe) []Ingredient {
	out := make([]Ingredient, 0, len(r))
	for ing := range r {
		out = append(out, ing)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// shortages lists what is missing for the needs, sorted by ingredient. What is in a lot that has
// expired by at is not counted.
func (s *Stock) shortages(needs Recipe, at time.Time) []Shortage {
	var out []Shortage
	for ing, n := range needs {
		if have := s.Usable(ing, at); have < n {
			out = append(out, Shortage{ing, n, have})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ingredient < out[j].Ingredient })
	return out
}

// CanMake reports whether the needs can be met right now, and what is short.
func (s *Stock) CanMake(needs Recipe) (bool, []Shortage) { return s.CanMakeAt(needs, time.Time{}) }

// CanMakeAt is CanMake at a moment: an expired lot does not count.
func (s *Stock) CanMakeAt(needs Recipe, at time.Time) (bool, []Shortage) {
	short := s.shortages(needs, at)
	return len(short) == 0, short
}

// MaxMakeable returns how many units of a recipe the stock can make; for an empty
// recipe there is no limit and it returns -1.
func (s *Stock) MaxMakeable(r Recipe) int64 { return s.MaxMakeableAt(r, time.Time{}) }

// MaxMakeableAt is MaxMakeable at a moment: an expired lot does not count.
func (s *Stock) MaxMakeableAt(r Recipe, at time.Time) int64 {
	best := int64(-1)
	for ing, n := range r {
		if n <= 0 {
			continue
		}
		if k := s.Usable(ing, at) / n; best < 0 || k < best {
			best = k
		}
	}
	return best
}

// SoldOut returns a function, for menu pricing, that says whether an item cannot
// be made even once with the base recipe. An option that needs more of something
// is checked when the order takes its ingredients.
func (s *Stock) SoldOut(m *menu.Menu) func(menu.ItemID) bool { return s.SoldOutAt(m, time.Time{}) }

// SoldOutAt is SoldOut at a moment: an expired lot does not count.
func (s *Stock) SoldOutAt(m *menu.Menu, at time.Time) func(menu.ItemID) bool {
	return func(id menu.ItemID) bool {
		it, ok := m.Item(id)
		if !ok {
			return false
		}
		return s.MaxMakeableAt(it.Recipe, at) == 0
	}
}

// Low lists the ingredients at or below their alert threshold, sorted.
func (s *Stock) Low() []Ingredient {
	var out []Ingredient
	for ing, l := range s.levels {
		if l.LowAt > 0 && l.Qty <= l.LowAt {
			out = append(out, ing)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Lots returns the lots of an ingredient in the order they would be used (copies).
func (s *Stock) Lots(ing Ingredient) []Lot {
	var out []Lot
	for _, l := range s.order(ing) {
		out = append(out, *l)
	}
	return out
}

// ValueMilli is what the stock of an ingredient is worth at the price of its lots, in thousandths
// of a đồng; lots with no known price count for nothing, and Unpriced says how much of the
// quantity that is.
func (s *Stock) ValueMilli(ing Ingredient) (value, unpriced int64) {
	for _, l := range s.lots[ing] {
		if l.UnitCostMilli == 0 {
			unpriced += l.Qty
		}
		value += l.Qty * l.UnitCostMilli
	}
	return value, unpriced
}

// Clone returns an independent copy of the stock. A service uses it to try a movement
// and see whether it would be accepted before the movement is written to a log.
func (s *Stock) Clone() *Stock {
	c := &Stock{levels: make(map[Ingredient]Level, len(s.levels)), lots: make(map[Ingredient][]*Lot, len(s.lots)), seq: s.seq}
	for ing, l := range s.levels {
		c.levels[ing] = l
	}
	for ing, ls := range s.lots {
		cp := make([]*Lot, len(ls))
		for i, l := range ls {
			v := *l
			cp[i] = &v
		}
		c.lots[ing] = cp
	}
	return c
}

// Snapshot returns the quantity of every ingredient, for comparing two stocks.
func (s *Stock) Snapshot() map[Ingredient]int64 {
	out := make(map[Ingredient]int64, len(s.levels))
	for ing, l := range s.levels {
		out[ing] = l.Qty
	}
	return out
}
