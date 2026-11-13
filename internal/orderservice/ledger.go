package orderservice

import (
	"sort"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// ledgerCap is how many movements of one ingredient the service keeps to show. The log has them
// all; the state is rebuilt from the log at every start, so this only bounds memory: an ingredient
// that is used all day has its oldest movements dropped, and the screens say when that happened.
const ledgerCap = 1000

// LedgerEntry is one change of the quantity of an ingredient, as it was applied.
type LedgerEntry struct {
	Offset     uint64     `json:"offset"`
	At         time.Time  `json:"at"`
	Kind       stock.Kind `json:"kind"`
	Ingredient string     `json:"ingredient"`
	// Delta is how much the quantity changed (negative for what leaves); After is what was left.
	Delta int64 `json:"delta"`
	After int64 `json:"after"`
	// Ref is the order for what an order took or gave back, and the ID of the movement otherwise.
	Ref      string `json:"ref"`
	By       string `json:"by"`
	Reason   string `json:"reason,omitempty"`
	Cause    string `json:"cause,omitempty"`
	Lot      string `json:"lot,omitempty"`
	Supplier string `json:"supplier,omitempty"`
	Document string `json:"document,omitempty"`
	// ValueMilli is the worth of the change at the price of the lots it touched, in thousandths of
	// a đồng; 0 where no price was known.
	ValueMilli int64 `json:"value_milli,omitempty"`
}

// note adds an entry to the history of an ingredient.
func (s *State) note(e LedgerEntry) {
	ing := stock.Ingredient(e.Ingredient)
	list := append(s.ledger[ing], e)
	if len(list) > ledgerCap {
		list = append([]LedgerEntry(nil), list[len(list)-ledgerCap:]...)
		s.ledgerDropped[ing] = true
	}
	s.ledger[ing] = list
}

// Ledger returns the movements of an ingredient, newest first, at most limit of them; and whether
// older ones were dropped to bound memory.
func (s *State) Ledger(ing string, limit int) ([]LedgerEntry, bool) {
	list := s.ledger[stock.Ingredient(ing)]
	if limit <= 0 || limit > len(list) {
		limit = len(list)
	}
	out := make([]LedgerEntry, 0, limit)
	for i := len(list) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, list[i])
	}
	return out, s.ledgerDropped[stock.Ingredient(ing)]
}

// ledgerIngredients lists the ingredients that have a history, sorted.
func (s *State) ledgerIngredients() []string {
	out := make([]string, 0, len(s.ledger))
	for ing := range s.ledger {
		out = append(out, string(ing))
	}
	sort.Strings(out)
	return out
}

// noteResult records what a movement did, one entry for each ingredient it touched.
func (s *State) noteResult(offset uint64, at time.Time, kind stock.Kind, ref, by, reason, cause string, before map[stock.Ingredient]int64, res stock.Result, extra LedgerEntry) {
	touched := map[stock.Ingredient]bool{}
	for ing := range before {
		touched[ing] = true
	}
	names := make([]stock.Ingredient, 0, len(touched))
	for ing := range touched {
		names = append(names, ing)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, ing := range names {
		after := s.stock.Qty(ing)
		delta := after - before[ing]
		if delta == 0 {
			continue
		}
		e := extra
		e.Offset, e.At, e.Kind, e.Ingredient, e.Delta, e.After = offset, at, kind, string(ing), delta, after
		e.Ref, e.By, e.Reason, e.Cause = ref, by, reason, cause
		for _, t := range res.Takes {
			if t.Ingredient == ing && e.Lot == "" {
				e.Lot = t.Lot.ID
			}
		}
		if len(names) == 1 {
			e.ValueMilli = res.ValueMilli
		} else {
			e.ValueMilli = 0
			for _, t := range res.Takes {
				if t.Ingredient == ing {
					e.ValueMilli += t.Qty * t.Lot.UnitCostMilli
				}
			}
		}
		s.note(e)
	}
}
