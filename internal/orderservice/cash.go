package orderservice

import (
	"sort"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
)

// CashEntry is a movement of cash in a drawer that follows from an order: money taken for it, or
// money given back. Card and transfer payments never touch a drawer and are not here.
type CashEntry struct {
	At time.Time `json:"at"`
	// Cashier is who took the cash. A refund of cash is counted against the cashier who took it
	// in the first place: the drawer that has the money is the one that gives it back.
	Cashier string `json:"cashier"`
	Order   string `json:"order"`
	Number  int    `json:"number"`
	// Kind is "sale" or "refund"; Amount is positive for both.
	Kind   string `json:"kind"`
	Amount int64  `json:"amount"`
}

// The kinds of CashEntry.
const (
	CashSale   = "sale"
	CashRefund = "refund"
)

// noteCash adds an entry to the cash of a cashier.
func (s *State) noteCash(e CashEntry) {
	s.cash[e.Cashier] = append(s.cash[e.Cashier], e)
}

// cashSale records the cash that a payment brought in.
func (s *State) cashSale(o *order.Order, ev order.Event) {
	if o.Payment == nil || o.Payment.Method != "cash" || o.Payment.Amount <= 0 {
		return
	}
	s.noteCash(CashEntry{At: ev.At, Cashier: o.Payment.By, Order: string(o.ID), Number: o.Number, Kind: CashSale, Amount: int64(o.Payment.Amount)})
}

// cashRefund records cash that was given back for an order that was paid in cash.
func (s *State) cashRefund(o *order.Order, ev order.Event, amount int64) {
	if o.Payment == nil || o.Payment.Method != "cash" || amount <= 0 {
		return
	}
	s.noteCash(CashEntry{At: ev.At, Cashier: o.Payment.By, Order: string(o.ID), Number: o.Number, Kind: CashRefund, Amount: amount})
}

// CashView is the cash that orders put into a drawer, and took out of it, in a period.
type CashView struct {
	Sales   int64       `json:"sales"`
	Refunds int64       `json:"refunds"`
	Entries []CashEntry `json:"entries"`
}

// Net is what the drawer holds because of orders: sales less refunds.
func (v CashView) Net() int64 { return v.Sales - v.Refunds }

// Cash returns the cash entries of a cashier from one moment up to, and not including, another,
// oldest first. The service applies the log in order, so an entry is here as soon as the payment is
// in the log; a payment that is being written at this moment may not be.
func (s *Service) Cash(cashier string, from, to time.Time) CashView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := CashView{Entries: []CashEntry{}}
	for _, e := range s.st.cash[cashier] {
		if e.At.Before(from) || !e.At.Before(to) {
			continue
		}
		v.Entries = append(v.Entries, e)
		if e.Kind == CashSale {
			v.Sales += e.Amount
		} else {
			v.Refunds += e.Amount
		}
	}
	sort.SliceStable(v.Entries, func(i, j int) bool { return v.Entries[i].At.Before(v.Entries[j].At) })
	return v
}
