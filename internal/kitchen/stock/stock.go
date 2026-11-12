// Package stock keeps count of ingredients.
//
// Everything that changes the count is a Movement, applied through one method.
// The same movements applied in the same order give the same stock on every
// machine, which is what lets the stock be rebuilt from a log. The one invariant
// is that no ingredient is ever below zero: a movement that would do that is
// refused, and refused whole, leaving the stock as it was.
package stock

import (
	"errors"
	"fmt"
	"sort"
	"strings"

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
	Receive  Kind = "receive"   // goods arrive
	Consume  Kind = "consume"   // an order takes what it needs
	Release  Kind = "release"   // an order that was cancelled gives it back
	WriteOff Kind = "write_off" // spoiled, spilled or lost
	Count    Kind = "count"     // a stocktake sets the real quantity
)

// Movement is one change of the stock.
type Movement struct {
	Kind Kind
	// Ingredient and Qty: for Receive and WriteOff the amount; for Count the real
	// quantity found.
	Ingredient Ingredient
	Qty        int64
	// Needs: for Consume and Release, what an order uses.
	Needs Recipe
	// Ref says what caused it, for example an order ID.
	Ref string
	// Reason is required for WriteOff and Count.
	Reason string
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
}

// New returns a stock with the given opening levels, which must not be negative.
func New(opening map[Ingredient]Level) (*Stock, error) {
	s := &Stock{levels: make(map[Ingredient]Level, len(opening))}
	for ing, l := range opening {
		if ing == "" || l.Qty < 0 || l.LowAt < 0 {
			return nil, fmt.Errorf("stock: bad opening level for %q", ing)
		}
		s.levels[ing] = l
	}
	return s, nil
}

// Qty returns how much of an ingredient there is; 0 for one never seen.
func (s *Stock) Qty(ing Ingredient) int64 { return s.levels[ing].Qty }

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

// Apply makes one movement. It changes nothing if it returns an error.
func (s *Stock) Apply(m Movement) error {
	switch m.Kind {
	case Receive:
		if m.Qty <= 0 || m.Ingredient == "" {
			return ErrQuantity
		}
		l := s.levels[m.Ingredient]
		l.Qty += m.Qty
		s.levels[m.Ingredient] = l
	case WriteOff:
		if m.Qty <= 0 || m.Ingredient == "" {
			return ErrQuantity
		}
		if strings.TrimSpace(m.Reason) == "" {
			return ErrReason
		}
		if have := s.levels[m.Ingredient].Qty; have < m.Qty {
			return &ShortageError{Missing: []Shortage{{m.Ingredient, m.Qty, have}}}
		}
		l := s.levels[m.Ingredient]
		l.Qty -= m.Qty
		s.levels[m.Ingredient] = l
	case Count:
		if m.Ingredient == "" {
			return ErrQuantity
		}
		if m.Qty < 0 {
			return ErrNegative
		}
		if strings.TrimSpace(m.Reason) == "" {
			return ErrReason
		}
		l := s.levels[m.Ingredient]
		l.Qty = m.Qty
		s.levels[m.Ingredient] = l
	case Consume:
		if err := validNeeds(m.Needs); err != nil {
			return err
		}
		if short := s.shortages(m.Needs); len(short) > 0 {
			return &ShortageError{Missing: short}
		}
		for ing, n := range m.Needs {
			l := s.levels[ing]
			l.Qty -= n
			s.levels[ing] = l
		}
	case Release:
		if err := validNeeds(m.Needs); err != nil {
			return err
		}
		for ing, n := range m.Needs {
			l := s.levels[ing]
			l.Qty += n
			s.levels[ing] = l
		}
	default:
		return fmt.Errorf("%w: %q", ErrUnknown, m.Kind)
	}
	return nil
}

func validNeeds(r Recipe) error {
	for ing, n := range r {
		if ing == "" || n <= 0 {
			return ErrQuantity
		}
	}
	return nil
}

// shortages lists what is missing for the needs, sorted by ingredient.
func (s *Stock) shortages(needs Recipe) []Shortage {
	var out []Shortage
	for ing, n := range needs {
		if have := s.levels[ing].Qty; have < n {
			out = append(out, Shortage{ing, n, have})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ingredient < out[j].Ingredient })
	return out
}

// CanMake reports whether the needs can be met right now, and what is short.
func (s *Stock) CanMake(needs Recipe) (bool, []Shortage) {
	short := s.shortages(needs)
	return len(short) == 0, short
}

// MaxMakeable returns how many units of a recipe the stock can make; for an empty
// recipe there is no limit and it returns -1.
func (s *Stock) MaxMakeable(r Recipe) int64 {
	best := int64(-1)
	for ing, n := range r {
		if n <= 0 {
			continue
		}
		if k := s.levels[ing].Qty / n; best < 0 || k < best {
			best = k
		}
	}
	return best
}

// SoldOut returns a function, for menu pricing, that says whether an item cannot
// be made even once with the base recipe. An option that needs more of something
// is checked when the order takes its ingredients.
func (s *Stock) SoldOut(m *menu.Menu) func(menu.ItemID) bool {
	return func(id menu.ItemID) bool {
		it, ok := m.Item(id)
		if !ok {
			return false
		}
		return s.MaxMakeable(it.Recipe) == 0
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

// Clone returns an independent copy of the stock. A service uses it to try a movement
// and see whether it would be accepted before the movement is written to a log.
func (s *Stock) Clone() *Stock {
	c := &Stock{levels: make(map[Ingredient]Level, len(s.levels))}
	for ing, l := range s.levels {
		c.levels[ing] = l
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
