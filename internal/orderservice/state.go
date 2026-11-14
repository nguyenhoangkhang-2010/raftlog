package orderservice

import (
	"fmt"
	"sort"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// Change says what applying a record did. The zero value means nothing changed: the
// record was one that had been applied before, or one that did not fit and was skipped.
type Change struct {
	Offset uint64
	// Order is set for an order event.
	Order  order.ID
	Number int
	Type   order.EventType
	Status order.Status
	// Customer owns the order (empty for a walk-in).
	Customer string
	// MenuChanged is true when the menu, the promotions or a photo changed: a screen that shows the
	// menu reads it again.
	MenuChanged bool
	// StockChanged is true when the quantity of an ingredient changed.
	StockChanged bool
	// Skipped is set when the record was refused: why.
	Skipped error
}

// Applied reports whether the record changed anything.
func (c Change) Applied() bool { return c.Order != "" || c.StockChanged || c.MenuChanged }

type redeemKey struct {
	promo    promo.ID
	customer string
}

// State is everything the service knows, built by applying the records of the log in
// order. It holds no clock and does no I/O, so the same log gives the same State anywhere.
//
// A record that does not fit (an event for an order that does not exist, a payment the
// order cannot take, a movement the stock refuses) is skipped and counted. The rule is the
// same on every reader, so they stay in step, and the orders stay consistent; it is
// reported, never silent.
type State struct {
	menu   *menu.Menu
	promos *promo.Set

	stock     *stock.Stock
	orders    map[order.ID]*order.Order
	events    map[order.ID][]order.Event
	moveSeen  map[string]bool
	redeemed  map[redeemKey]bool
	maxNumber int

	// takes are the parts of lots that each paid order used, so that a cancelled order gives them
	// back to the same lots; an order's entry goes when it is handed over or cancelled.
	takes map[order.ID][]stock.Take
	// ledger is the history of each ingredient (see ledger.go).
	ledger        map[stock.Ingredient][]LedgerEntry
	// observe, if set, is told of every movement of the stock as it is applied.
	observe func(LedgerEntry)
	// cash is, for each cashier, the cash that orders put into their drawer and took out of it.
	cash map[string][]CashEntry
	ledgerDropped map[stock.Ingredient]bool

	next    uint64 // the offset of the next record to apply
	skipped int
}

// NewState returns the state before any record: no orders, an empty stock. lowAt gives the
// quantity at or below which an ingredient counts as running low.
func NewState(m *menu.Menu, p *promo.Set, lowAt map[stock.Ingredient]int64) (*State, error) {
	st, err := stock.New(nil)
	if err != nil {
		return nil, err
	}
	for ing, at := range lowAt {
		if err := st.SetLowAt(ing, at); err != nil {
			return nil, err
		}
	}
	return &State{
		menu: m, promos: p, stock: st,
		orders:   map[order.ID]*order.Order{},
		events:   map[order.ID][]order.Event{},
		moveSeen: map[string]bool{},
		redeemed: map[redeemKey]bool{},
		takes:    map[order.ID][]stock.Take{},
		cash:     map[string][]CashEntry{},
		ledger:   map[stock.Ingredient][]LedgerEntry{}, ledgerDropped: map[stock.Ingredient]bool{},
	}, nil
}

// Next is the offset of the next record the state expects.
func (s *State) Next() uint64 { return s.next }

// Skipped is how many records were refused so far.
func (s *State) Skipped() int { return s.skipped }

// Order returns an order, or nil.
func (s *State) Order(id order.ID) *order.Order { return s.orders[id] }

// Events returns the events of an order, oldest first.
func (s *State) Events(id order.ID) []order.Event { return s.events[id] }

// Redeemed says whether the customer has used the promotion.
func (s *State) Redeemed(id promo.ID, customer string) bool {
	return customer != "" && s.redeemed[redeemKey{id, customer}]
}

// NextNumber is the number the next order will get.
func (s *State) NextNumber() int { return s.maxNumber + 1 }

// Orders returns every order, newest first.
func (s *State) Orders() []*order.Order {
	out := make([]*order.Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out
}

// ApplyRecord applies the record that is at the given offset. Offsets must come in order,
// without a gap; anything else is a bug of the caller and an error.
func (s *State) ApplyRecord(offset uint64, rec Record) (Change, error) {
	if offset != s.next {
		return Change{}, fmt.Errorf("orderservice: record at offset %d, expected %d", offset, s.next)
	}
	s.next++
	ch := Change{Offset: offset}
	var err error
	switch {
	case rec.Barrier != "":
		return ch, nil // a mark in the log; it changes nothing and is not a record that was skipped
	case rec.Stock != nil:
		ch, err = s.applyMove(ch, *rec.Stock)
	default:
		ch, err = s.applyEvent(ch, rec)
	}
	if err != nil {
		s.skipped++
		return Change{Offset: offset, Skipped: err}, nil
	}
	return ch, nil
}

func (s *State) applyMove(ch Change, m StockMove) (Change, error) {
	if s.moveSeen[m.ID] {
		return Change{Offset: ch.Offset}, nil // a repeat of a command that was applied
	}
	before := map[stock.Ingredient]int64{stock.Ingredient(m.Ingredient): s.stock.Qty(stock.Ingredient(m.Ingredient))}
	res, err := s.stock.Do(m.Movement())
	if err != nil {
		return ch, fmt.Errorf("stock movement %s: %w", m.ID, err)
	}
	s.moveSeen[m.ID] = true
	ch.StockChanged = true
	s.noteResult(ch.Offset, m.At, m.Kind, m.ID, m.By, m.Reason, m.Cause, before, res, LedgerEntry{Lot: res.Lot + m.Lot, Supplier: m.Supplier, Document: m.Document})
	return ch, nil
}

func (s *State) applyEvent(ch Change, rec Record) (Change, error) {
	ev, err := order.Decode(rec.Order)
	if err != nil {
		return ch, err
	}
	o := s.orders[ev.Order]
	fresh := o == nil
	if fresh {
		if ev.Type != order.EvPlaced {
			return ch, fmt.Errorf("event %s for the unknown order %s", ev.ID, ev.Order)
		}
		o = &order.Order{}
	}
	before := o.Seq()
	hadStock := o.StockHeld
	if err := o.Apply(ev); err != nil {
		return ch, err
	}
	if o.Seq() == before {
		return Change{Offset: ch.Offset}, nil // the same event again
	}

	// What the event does beyond the order itself.
	switch ev.Type {
	case order.EvPlaced:
		s.orders[ev.Order] = o
		if o.Number > s.maxNumber {
			s.maxNumber = o.Number
		}
		for _, d := range o.Discounts {
			if o.Customer != "" {
				s.redeemed[redeemKey{d.ID, o.Customer}] = true
			}
		}
	case order.EvPaid:
		needs := o.Needs()
		before := s.beforeOf(needs)
		res, err := s.stock.Do(stock.Movement{Kind: stock.Consume, Needs: needs, Ref: string(o.ID), At: ev.At})
		if err == nil {
			s.takes[o.ID] = res.Takes
			s.noteResult(ch.Offset, ev.At, stock.Consume, string(o.ID), ev.By, "", "", before, res, LedgerEntry{})
		}
		if err != nil {
			// The order took the payment but the stock cannot cover it. The service checks
			// before it writes the payment, so this is a log that was written by something
			// else; the event is undone by not applying it to anything else.
			return ch, rollbackPaid(s, ev, err)
		}
		ch.StockChanged = true
		s.cashSale(o, ev)
	case order.EvRefunded:
		s.cashRefund(o, ev, int64(ev.Refund.Amount))
	case order.EvCancelled:
		if ev.Cancelled.Restock && hadStock {
			needs := o.Needs()
			before := s.beforeOf(needs)
			takes := s.takes[o.ID]
			res, err := s.stock.Do(stock.Movement{Kind: stock.Release, Needs: needs, Ref: string(o.ID), At: ev.At, Takes: takes})
			if err != nil {
				return ch, fmt.Errorf("giving back the stock of %s: %w", o.ID, err)
			}
			res.Takes = takes
			s.noteResult(ch.Offset, ev.At, stock.Release, string(o.ID), ev.By, "", "", before, res, LedgerEntry{})
			ch.StockChanged = true
		}
		delete(s.takes, o.ID)
		if ev.Cancelled != nil {
			s.cashRefund(o, ev, int64(ev.Cancelled.Refund))
		}
	case order.EvHandedOver:
		delete(s.takes, o.ID)
	}
	s.events[ev.Order] = append(s.events[ev.Order], ev)
	ch.Order, ch.Number, ch.Type, ch.Status, ch.Customer = o.ID, o.Number, ev.Type, o.Status, o.Customer
	return ch, nil
}

// rollbackPaid reports a payment that the stock could not cover. The order has already
// been changed by Apply, so the order is rebuilt without the event.
func rollbackPaid(s *State, ev order.Event, cause error) error {
	events := s.events[ev.Order]
	o, err := order.Replay(events)
	if err != nil {
		return fmt.Errorf("rebuilding %s after a refused payment: %w", ev.Order, err)
	}
	s.orders[ev.Order] = o
	return fmt.Errorf("payment %s: the stock cannot cover the order: %w", ev.ID, cause)
}

// skip moves past a record that could not even be read.
func (s *State) skip(offset uint64) {
	if offset == s.next {
		s.next++
		s.skipped++
	}
}

// StockLevels returns the quantity of every ingredient the state has seen, by name.
func (s *State) StockLevels() map[string]int64 {
	out := map[string]int64{}
	for ing, q := range s.stock.Snapshot() {
		out[string(ing)] = q
	}
	return out
}

// setCatalog replaces the menu and the promotions, and the thresholds of the stock that go with
// them. It checks everything before it changes anything, so that a refusal leaves the state as it was.
func (s *State) setCatalog(m *menu.Menu, p *promo.Set, lowAt map[stock.Ingredient]int64) error {
	for ing, at := range lowAt {
		if at < 0 {
			return fmt.Errorf("orderservice: the threshold of %q is negative", ing)
		}
	}
	for ing := range s.stock.Snapshot() {
		if err := s.stock.SetLowAt(ing, lowAt[ing]); err != nil { // zero when the new menu gives none
			return err
		}
	}
	for ing, at := range lowAt {
		if err := s.stock.SetLowAt(ing, at); err != nil {
			return err
		}
	}
	s.menu, s.promos = m, p
	return nil
}

// beforeOf reads the quantity of each ingredient of a recipe, to see afterwards what changed.
func (s *State) beforeOf(r stock.Recipe) map[stock.Ingredient]int64 {
	out := make(map[stock.Ingredient]int64, len(r))
	for ing := range r {
		out[ing] = s.stock.Qty(ing)
	}
	return out
}
