package order

import (
	"fmt"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// Meta is what every command carries.
type Meta struct {
	// Cmd identifies the command. Repeating a command with the same ID changes
	// nothing, which makes retries safe.
	Cmd   string
	Actor rbac.Actor
	At    time.Time
}

func (m Meta) check() error {
	if m.Cmd == "" {
		return fmt.Errorf("%w: a command needs an ID", ErrInvalid)
	}
	if m.At.IsZero() {
		return fmt.Errorf("%w: a command needs a time", ErrInvalid)
	}
	return m.Actor.Validate()
}

func (m Meta) event(o *Order, t EventType) Event {
	return Event{ID: m.Cmd, Order: o.ID, Seq: o.seq + 1, At: m.At, By: m.Actor.ID, Roles: append([]rbac.Role(nil), m.Actor.Roles...), Type: t}
}

// mayAct checks that the actor holds the permission and, for a customer, that the
// order is theirs. Staff who hold the permission may act on any order.
func (o *Order) mayAct(a rbac.Actor, p rbac.Permission) error {
	if !a.Can(p) {
		return fmt.Errorf("%w: needs %s", ErrNotAllowed, p)
	}
	for _, r := range a.Roles {
		if r != rbac.Customer && rbac.Allowed(r, p) {
			return nil
		}
	}
	if o.Customer != "" && o.Customer == a.ID {
		return nil
	}
	return ErrNotYours
}

// begin does what every command starts with. It returns done when the command was
// applied before, and the event then must not be made again.
func (o *Order) begin(m Meta, p rbac.Permission) (done bool, err error) {
	if err := m.check(); err != nil {
		return false, err
	}
	if o.seen[m.Cmd] {
		return true, nil
	}
	if o.seq == 0 {
		return false, fmt.Errorf("%w: the order has not been placed", ErrWrongState)
	}
	return false, o.mayAct(m.Actor, p)
}

func (o *Order) wrongState(what string) error {
	return fmt.Errorf("%w: cannot %s an order that is %s", ErrWrongState, what, o.Status)
}

// PlaceInput is an order as the customer or the cashier made it.
type PlaceInput struct {
	Order    ID
	Number   int // the order number shown to the customer, given by the service
	Channel  Channel
	Customer string
	Lines    []menu.Line
	Pricing  promo.Result // the outcome of the promotions for these lines
}

// Place makes the event that creates an order. The numbers in it are checked: the
// lines must add up to the subtotal and the discounts to the total.
func Place(m Meta, in PlaceInput) (Event, error) {
	if err := m.check(); err != nil {
		return Event{}, err
	}
	if in.Order == "" {
		return Event{}, fmt.Errorf("%w: an order needs an ID", ErrInvalid)
	}
	if !m.Actor.Can(rbac.PlaceOrder) {
		return Event{}, fmt.Errorf("%w: needs %s", ErrNotAllowed, rbac.PlaceOrder)
	}
	staff := false
	for _, r := range m.Actor.Roles {
		if r != rbac.Customer && rbac.Allowed(r, rbac.PlaceOrder) {
			staff = true
		}
	}
	switch {
	case staff && in.Channel != ChannelCounter:
		return Event{}, fmt.Errorf("%w: staff take orders at the counter", ErrInvalid)
	case !staff && (in.Channel != ChannelApp || in.Customer != m.Actor.ID):
		return Event{}, fmt.Errorf("%w: a customer orders for themselves", ErrNotYours)
	}

	data := &PlacedData{
		Number: in.Number, Channel: in.Channel, Customer: in.Customer,
		Lines: in.Lines, Discounts: in.Pricing.Applied,
		Subtotal: in.Pricing.Subtotal, Discount: in.Pricing.Discount, Total: in.Pricing.Total,
	}
	if err := data.check(); err != nil {
		return Event{}, err
	}
	return Event{ID: m.Cmd, Order: in.Order, Seq: 1, At: m.At, By: m.Actor.ID, Roles: append([]rbac.Role(nil), m.Actor.Roles...), Type: EvPlaced, Placed: data}, nil
}

// PayInput is how the customer pays. The amount is always the total of the order.
type PayInput struct {
	Method    string
	Tendered  money.Money // for cash: what was handed over
	Reference string      // for card and transfer
}

// Pay records the payment of the whole total. It returns nil, nil when the same
// command was applied before.
func (o *Order) Pay(m Meta, in PayInput) (*Event, error) {
	if done, err := o.begin(m, rbac.RecordPayment); done || err != nil {
		return nil, err
	}
	if o.Status != Placed {
		return nil, o.wrongState("take payment for")
	}
	p := &PaidData{Method: in.Method, Amount: o.Total}
	switch in.Method {
	case Cash:
		if in.Tendered < o.Total {
			return nil, fmt.Errorf("%w: %v handed over for %v", ErrAmount, in.Tendered, o.Total)
		}
		p.Tendered, p.Change = in.Tendered, in.Tendered-o.Total
	default:
		p.Reference = in.Reference
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	ev := m.event(o, EvPaid)
	ev.Paid = p
	return &ev, nil
}

// ticketCommand is what StartTicket and FinishTicket share.
func (o *Order) ticketCommand(m Meta, st menu.Station) error {
	if st == "" || m.Actor.Station != string(st) {
		return fmt.Errorf("%w: %q is at station %q", ErrWrongStation, m.Actor.ID, m.Actor.Station)
	}
	if o.Tickets[st] == nil {
		return fmt.Errorf("%w: %q", ErrNoSuchTicket, st)
	}
	return nil
}

// StartTicket says that the station has begun its part of the order.
func (o *Order) StartTicket(m Meta, st menu.Station) (*Event, error) {
	if done, err := o.begin(m, rbac.StartTicket); done || err != nil {
		return nil, err
	}
	if err := o.ticketCommand(m, st); err != nil {
		return nil, err
	}
	if o.Status != Paid && o.Status != Preparing {
		return nil, o.wrongState("start work on")
	}
	if o.Request != nil {
		return nil, ErrCancelPending
	}
	if o.Tickets[st].Status != TicketPending {
		return nil, fmt.Errorf("%w: the %s ticket is already %s", ErrWrongState, st, o.Tickets[st].Status)
	}
	ev := m.event(o, EvTicketStarted)
	ev.Ticket = &TicketData{Station: st}
	return &ev, nil
}

// FinishTicket says that the station has finished its part of the order.
func (o *Order) FinishTicket(m Meta, st menu.Station) (*Event, error) {
	if done, err := o.begin(m, rbac.FinishTicket); done || err != nil {
		return nil, err
	}
	if err := o.ticketCommand(m, st); err != nil {
		return nil, err
	}
	if o.Status != Preparing {
		return nil, o.wrongState("finish work on")
	}
	if o.Request != nil {
		return nil, ErrCancelPending
	}
	if o.Tickets[st].Status != TicketStarted {
		return nil, fmt.Errorf("%w: the %s ticket is %s, not started", ErrWrongState, st, o.Tickets[st].Status)
	}
	ev := m.event(o, EvTicketReady)
	ev.Ticket = &TicketData{Station: st}
	return &ev, nil
}

// HandOver gives the finished order to the customer.
func (o *Order) HandOver(m Meta) (*Event, error) {
	if done, err := o.begin(m, rbac.HandOver); done || err != nil {
		return nil, err
	}
	if o.Status != Ready {
		return nil, o.wrongState("hand over")
	}
	if o.Request != nil {
		return nil, ErrCancelPending
	}
	ev := m.event(o, EvHandedOver)
	return &ev, nil
}

// CancelUnpaid cancels an order that has not been paid. The customer may cancel
// their own; a cashier or manager may cancel any.
func (o *Order) CancelUnpaid(m Meta, reason string) (*Event, error) {
	if done, err := o.begin(m, rbac.CancelUnpaid); done || err != nil {
		return nil, err
	}
	if o.Status != Placed {
		return nil, o.wrongState("cancel without approval")
	}
	if !validReason(reason) {
		return nil, ErrReasonRequired
	}
	ev := m.event(o, EvCancelled)
	ev.Cancelled = &CancelledData{Reason: reason}
	return &ev, nil
}

// RequestCancel asks to cancel an order that is paid. A manager decides.
func (o *Order) RequestCancel(m Meta, reason string) (*Event, error) {
	if done, err := o.begin(m, rbac.RequestCancel); done || err != nil {
		return nil, err
	}
	switch o.Status {
	case Paid, Preparing, Ready:
	default:
		return nil, o.wrongState("ask to cancel")
	}
	if o.Request != nil {
		return nil, ErrCancelPending
	}
	if !validReason(reason) {
		return nil, ErrReasonRequired
	}
	ev := m.event(o, EvCancelRequested)
	ev.Cancel = &CancelData{Reason: reason}
	return &ev, nil
}

// ApproveCancel cancels the order on a request and sets what is given back. Until
// a station has started, the whole amount goes back; after that the manager may
// give back less, because food has been made. The manager who approves cannot be
// the person who asked.
func (o *Order) ApproveCancel(m Meta, refund money.Money) (*Event, error) {
	if done, err := o.begin(m, rbac.ApproveCancel); done || err != nil {
		return nil, err
	}
	if o.Request == nil {
		return nil, ErrNoCancelRequest
	}
	if o.Request.By == m.Actor.ID {
		return nil, ErrSelfApproval
	}
	if refund < 0 || refund > o.Total {
		return nil, fmt.Errorf("%w: refund %v of %v", ErrAmount, refund, o.Total)
	}
	started := o.ticketsStarted()
	if !started && refund != o.Total {
		return nil, fmt.Errorf("%w: nothing was made, so the whole %v goes back", ErrRefundPolicy, o.Total)
	}
	ev := m.event(o, EvCancelled)
	ev.Cancelled = &CancelledData{Reason: o.Request.Reason, Refund: refund, Restock: !started, RequestedBy: o.Request.By}
	return &ev, nil
}

// RejectCancel turns a request down; the order carries on.
func (o *Order) RejectCancel(m Meta, reason string) (*Event, error) {
	if done, err := o.begin(m, rbac.ApproveCancel); done || err != nil {
		return nil, err
	}
	if o.Request == nil {
		return nil, ErrNoCancelRequest
	}
	if !validReason(reason) {
		return nil, ErrReasonRequired
	}
	ev := m.event(o, EvCancelRejected)
	ev.Cancel = &CancelData{Reason: reason}
	return &ev, nil
}

// IssueRefund gives money back for an order that was completed, in full or in
// part, as often as needed until the total has been given back.
func (o *Order) IssueRefund(m Meta, amount money.Money, reason string) (*Event, error) {
	if done, err := o.begin(m, rbac.IssueRefund); done || err != nil {
		return nil, err
	}
	if o.Status != Completed {
		return nil, o.wrongState("refund")
	}
	if !validReason(reason) {
		return nil, ErrReasonRequired
	}
	if amount <= 0 || o.Refunded+amount > o.Total {
		return nil, fmt.Errorf("%w: %v with %v already given back of %v", ErrAmount, amount, o.Refunded, o.Total)
	}
	ev := m.event(o, EvRefunded)
	ev.Refund = &RefundData{Amount: amount, Reason: reason}
	return &ev, nil
}
