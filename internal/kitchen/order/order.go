// Package order is the life of one order, as a sequence of events.
//
// An order is never stored as a row that gets updated. It is the list of events
// that happened to it (placed, paid, a ticket started at a station, ...), and its
// state is what you get by applying them in order. That is what lets the orders be
// kept in a replicated log and rebuilt anywhere, and it is why the rules here are
// written as "which event may happen next, and who may cause it".
//
// Two functions of an Order matter. A command method (Pay, StartTicket, ...) checks
// who is asking and whether the order is in a state that allows it, and returns the
// event that should be recorded; it changes nothing. Apply takes an event, checks
// that it fits the order as it is, and changes the order. Live code calls a command
// and then Apply on the result; recovery calls Apply on events read from the log.
// Apply checks the structure again, so a log that was corrupted or written by
// something else cannot produce an impossible order.
//
// Commands carry an ID, and an event that has been applied is remembered by it.
// Repeating a command, which the log's at-least-once delivery will do, therefore
// changes nothing.
package order

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
)

// ID identifies an order.
type ID string

// Status is where an order is in its life.
type Status string

// The statuses. An order moves forward through them; Cancelled and Completed end it.
const (
	Placed    Status = "placed"    // taken, not paid
	Paid      Status = "paid"      // paid, waiting for the kitchen
	Preparing Status = "preparing" // at least one station has started
	Ready     Status = "ready"     // every station is done, waiting to be handed over
	Completed Status = "completed" // handed to the customer
	Cancelled Status = "cancelled"
)

// Channel is how the order came in.
type Channel string

// The channels.
const (
	ChannelApp     Channel = "app"     // the customer ordered from their own device
	ChannelCounter Channel = "counter" // staff took it at the counter
)

// Payment methods.
const (
	Cash     = "cash"
	Card     = "card"
	Transfer = "transfer"
)

// TicketStatus is the state of the work at one station.
type TicketStatus string

// The ticket statuses.
const (
	TicketPending TicketStatus = "pending"
	TicketStarted TicketStatus = "started"
	TicketReady   TicketStatus = "ready"
)

// Ticket is the work one station has to do for an order.
type Ticket struct {
	Station   menu.Station
	Status    TicketStatus
	StartedBy string
	StartedAt time.Time
	ReadyBy   string
	ReadyAt   time.Time
}

// Payment is how the order was paid.
type Payment struct {
	Method    string
	Amount    money.Money
	Tendered  money.Money // cash handed over
	Change    money.Money // cash given back
	Reference string      // card slip or transfer reference
	By        string
	At        time.Time
}

// CancelRequest is a request to cancel that waits for a manager.
type CancelRequest struct {
	By     string
	Reason string
	At     time.Time
}

// Errors.
var (
	ErrNotAllowed        = errors.New("order: this person may not do that")
	ErrWrongState        = errors.New("order: the order is not in a state that allows that")
	ErrNotYours          = errors.New("order: this is not your order")
	ErrWrongStation      = errors.New("order: this person does not work at that station")
	ErrNoSuchTicket      = errors.New("order: the order has nothing for that station")
	ErrCancelPending     = errors.New("order: a cancellation is waiting for a decision")
	ErrNoCancelRequest   = errors.New("order: there is no cancellation request to decide")
	ErrSelfApproval      = errors.New("order: nobody may approve their own request")
	ErrAmount            = errors.New("order: the amount is wrong")
	ErrReasonRequired    = errors.New("order: a reason is required")
	ErrRefundPolicy      = errors.New("order: a refund of this size is not allowed at this point")
	ErrInvalid           = errors.New("order: invalid input")
	ErrSequence          = errors.New("order: event out of sequence")
	ErrIllegalTransition = errors.New("order: the event does not fit the order as it is")
	ErrOtherOrder        = errors.New("order: the event belongs to another order")
)

// Order is the state of one order.
type Order struct {
	ID       ID
	Number   int
	Channel  Channel
	Customer string // the customer's ID; empty for a walk-in at the counter
	Status   Status

	Lines     []menu.Line
	Discounts []promo.Applied
	Subtotal  money.Money
	Discount  money.Money
	Total     money.Money

	PlacedAt time.Time
	Payment  *Payment
	Tickets  map[menu.Station]*Ticket
	Request  *CancelRequest // a cancellation waiting for a decision
	Refunded money.Money    // everything given back, by cancellation or refund

	CancelledBy  string
	CancelReason string
	CompletedBy  string
	CompletedAt  time.Time

	// StockHeld is true while the order holds ingredients taken from the stock at
	// payment: they are given back if it is cancelled before the kitchen starts.
	StockHeld bool

	seq  int
	seen map[string]bool
}

// Seq is the number of events applied so far.
func (o *Order) Seq() int { return o.seq }

// Stations returns the stations the order needs, sorted.
func (o *Order) Stations() []menu.Station {
	out := make([]menu.Station, 0, len(o.Tickets))
	for st := range o.Tickets {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Needs returns the ingredients the whole order uses.
func (o *Order) Needs() menu.Recipe {
	need := menu.Recipe{}
	for _, l := range o.Lines {
		for ing, n := range l.Needs {
			need[ing] += n * int64(l.Quantity)
		}
	}
	return need
}

func (o *Order) ticketsStarted() bool {
	for _, t := range o.Tickets {
		if t.Status != TicketPending {
			return true
		}
	}
	return false
}

func (o *Order) allTicketsReady() bool {
	for _, t := range o.Tickets {
		if t.Status != TicketReady {
			return false
		}
	}
	return len(o.Tickets) > 0
}

// Done reports whether nothing more can happen to the order except a refund.
func (o *Order) Done() bool { return o.Status == Completed || o.Status == Cancelled }

// Apply changes the order by one event. An event already applied (same ID) is
// ignored. An event that does not fit the order is refused and changes nothing.
func (o *Order) Apply(ev Event) error {
	if o.seen != nil && o.seen[ev.ID] {
		return nil
	}
	if err := ev.validate(); err != nil {
		return err
	}
	if o.seq == 0 {
		if ev.Type != EvPlaced {
			return fmt.Errorf("%w: the first event of an order is its placement, not %s", ErrIllegalTransition, ev.Type)
		}
	} else if ev.Order != o.ID {
		return fmt.Errorf("%w: %s is not %s", ErrOtherOrder, ev.Order, o.ID)
	}
	if ev.Seq != o.seq+1 {
		return fmt.Errorf("%w: got %d after %d", ErrSequence, ev.Seq, o.seq)
	}

	var err error
	switch ev.Type {
	case EvPlaced:
		err = o.applyPlaced(ev)
	case EvPaid:
		err = o.applyPaid(ev)
	case EvTicketStarted:
		err = o.applyTicketStarted(ev)
	case EvTicketReady:
		err = o.applyTicketReady(ev)
	case EvHandedOver:
		err = o.applyHandedOver(ev)
	case EvCancelRequested:
		err = o.applyCancelRequested(ev)
	case EvCancelRejected:
		err = o.applyCancelRejected(ev)
	case EvCancelled:
		err = o.applyCancelled(ev)
	case EvRefunded:
		err = o.applyRefunded(ev)
	}
	if err != nil {
		return err
	}
	o.seq = ev.Seq
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	o.seen[ev.ID] = true
	return nil
}

func illegal(ev Event, status Status) error {
	return fmt.Errorf("%w: %s while %s", ErrIllegalTransition, ev.Type, status)
}

func (o *Order) applyPlaced(ev Event) error {
	if o.seq != 0 {
		return illegal(ev, o.Status)
	}
	p := ev.Placed
	if err := p.check(); err != nil {
		return err
	}
	o.ID, o.Number, o.Channel, o.Customer = ev.Order, p.Number, p.Channel, p.Customer
	o.Status = Placed
	o.Lines = append([]menu.Line(nil), p.Lines...)
	o.Discounts = append([]promo.Applied(nil), p.Discounts...)
	o.Subtotal, o.Discount, o.Total = p.Subtotal, p.Discount, p.Total
	o.PlacedAt = ev.At
	o.Tickets = map[menu.Station]*Ticket{}
	for _, l := range o.Lines {
		for _, pc := range l.Pieces {
			if o.Tickets[pc.Station] == nil {
				o.Tickets[pc.Station] = &Ticket{Station: pc.Station, Status: TicketPending}
			}
		}
	}
	return nil
}

func (o *Order) applyPaid(ev Event) error {
	if o.Status != Placed {
		return illegal(ev, o.Status)
	}
	p := ev.Paid
	if p.Amount != o.Total {
		return fmt.Errorf("%w: paid %v for an order of %v", ErrAmount, p.Amount, o.Total)
	}
	o.Payment = &Payment{Method: p.Method, Amount: p.Amount, Tendered: p.Tendered, Change: p.Change, Reference: p.Reference, By: ev.By, At: ev.At}
	o.Status = Paid
	o.StockHeld = true
	return nil
}

func (o *Order) applyTicketStarted(ev Event) error {
	if o.Status != Paid && o.Status != Preparing {
		return illegal(ev, o.Status)
	}
	if o.Request != nil {
		return fmt.Errorf("%w: %s", ErrIllegalTransition, "a cancellation is waiting")
	}
	t := o.Tickets[ev.Ticket.Station]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrNoSuchTicket, ev.Ticket.Station)
	}
	if t.Status != TicketPending {
		return illegal(ev, o.Status)
	}
	t.Status, t.StartedBy, t.StartedAt = TicketStarted, ev.By, ev.At
	o.Status = Preparing
	return nil
}

func (o *Order) applyTicketReady(ev Event) error {
	if o.Status != Preparing {
		return illegal(ev, o.Status)
	}
	t := o.Tickets[ev.Ticket.Station]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrNoSuchTicket, ev.Ticket.Station)
	}
	if t.Status != TicketStarted {
		return illegal(ev, o.Status)
	}
	t.Status, t.ReadyBy, t.ReadyAt = TicketReady, ev.By, ev.At
	if o.allTicketsReady() {
		o.Status = Ready
	}
	return nil
}

func (o *Order) applyHandedOver(ev Event) error {
	if o.Status != Ready {
		return illegal(ev, o.Status)
	}
	if o.Request != nil {
		return fmt.Errorf("%w: %s", ErrIllegalTransition, "a cancellation is waiting")
	}
	o.Status, o.CompletedBy, o.CompletedAt = Completed, ev.By, ev.At
	return nil
}

func (o *Order) applyCancelRequested(ev Event) error {
	switch o.Status {
	case Paid, Preparing, Ready:
	default:
		return illegal(ev, o.Status)
	}
	if o.Request != nil {
		return fmt.Errorf("%w: %s", ErrIllegalTransition, "a request is already waiting")
	}
	o.Request = &CancelRequest{By: ev.By, Reason: ev.Cancel.Reason, At: ev.At}
	return nil
}

func (o *Order) applyCancelRejected(ev Event) error {
	if o.Request == nil {
		return illegal(ev, o.Status)
	}
	o.Request = nil
	return nil
}

func (o *Order) applyCancelled(ev Event) error {
	c := ev.Cancelled
	switch o.Status {
	case Placed:
		// Never paid: nothing to give back and no stock was taken.
		if c.Refund != 0 || c.Restock {
			return fmt.Errorf("%w: an unpaid order has nothing to refund or restock", ErrIllegalTransition)
		}
	case Paid, Preparing, Ready:
		if o.Request == nil {
			return fmt.Errorf("%w: a paid order is cancelled only by an approved request", ErrIllegalTransition)
		}
		if c.Refund < 0 || c.Refund > o.Total {
			return fmt.Errorf("%w: refund %v of %v", ErrAmount, c.Refund, o.Total)
		}
		if c.Restock != !o.ticketsStarted() {
			return fmt.Errorf("%w: ingredients go back to the stock only if no station had started", ErrIllegalTransition)
		}
	default:
		return illegal(ev, o.Status)
	}
	o.Status = Cancelled
	o.Refunded = c.Refund
	o.CancelledBy, o.CancelReason = ev.By, c.Reason
	if c.Restock {
		o.StockHeld = false
	}
	o.Request = nil
	return nil
}

func (o *Order) applyRefunded(ev Event) error {
	if o.Status != Completed {
		return illegal(ev, o.Status)
	}
	r := ev.Refund
	if r.Amount <= 0 || o.Refunded+r.Amount > o.Total {
		return fmt.Errorf("%w: refund %v with %v already given back of %v", ErrAmount, r.Amount, o.Refunded, o.Total)
	}
	o.Refunded += r.Amount
	return nil
}
