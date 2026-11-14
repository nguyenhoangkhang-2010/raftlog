package order

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// EventType says what happened.
type EventType string

// The events of an order's life.
const (
	EvPlaced          EventType = "order.placed"
	EvPaid            EventType = "order.paid"
	EvTicketStarted   EventType = "ticket.started"
	EvTicketReady     EventType = "ticket.ready"
	EvHandedOver      EventType = "order.handed_over"
	EvCancelRequested EventType = "cancel.requested"
	EvCancelRejected  EventType = "cancel.rejected"
	EvCancelled       EventType = "order.cancelled"
	EvRefunded        EventType = "order.refunded"
)

// MaxReasonRunes bounds a free-text reason.
const MaxReasonRunes = 200

// Event is one fact about an order. Exactly one of the payload fields is set, the
// one that belongs to Type.
type Event struct {
	// ID is unique. It is the ID of the command that caused the event, so the same
	// command repeated gives the same event, which is then ignored.
	ID    string `json:"id"`
	Order ID     `json:"order"`
	// Seq counts the events of this order from 1, without gaps.
	Seq int       `json:"seq"`
	At  time.Time `json:"at"`
	// By and Roles say who caused it, as they were at the time, for the audit trail.
	By    string      `json:"by"`
	Roles []rbac.Role `json:"roles,omitempty"`
	Type  EventType   `json:"type"`

	Placed    *PlacedData    `json:"placed,omitempty"`
	Paid      *PaidData      `json:"paid,omitempty"`
	Ticket    *TicketData    `json:"ticket,omitempty"`
	Cancel    *CancelData    `json:"cancel,omitempty"`
	Cancelled *CancelledData `json:"cancelled,omitempty"`
	Refund    *RefundData    `json:"refund,omitempty"`
}

// PlacedData is an order as it was taken: the lines and prices are frozen here.
type PlacedData struct {
	Number    int             `json:"number"`
	Channel   Channel         `json:"channel"`
	Customer  string          `json:"customer,omitempty"`
	Lines     []menu.Line     `json:"lines"`
	Discounts []promo.Applied `json:"discounts,omitempty"`
	Subtotal  money.Money     `json:"subtotal"`
	Discount  money.Money     `json:"discount"`
	Total     money.Money     `json:"total"`
	// Points are the points of the customer that the order uses, each worth Value. They are also one of the
	// Discounts, under the ID PointsDiscountID, so that the totals need no other rule.
	Points *PointsUse `json:"points,omitempty"`
}

// PointsDiscountID is the ID of the discount that points make. No promotion may use it.
const PointsDiscountID = promo.PointsID

// MaxPoints bounds the points of one order, of a payment and of a balance.
const MaxPoints int64 = 10_000_000

// PointsUse is points that an order uses, and what each was worth when it was placed: the order says so itself,
// so that what the venue sets later does not change what an old order meant.
type PointsUse struct {
	Used  int64       `json:"used"`
	Value money.Money `json:"value"`
}

// Amount is the money that the points take off.
func (p PointsUse) Amount() money.Money { return p.Value * money.Money(p.Used) }

// PaidData is a payment.
type PaidData struct {
	Method    string      `json:"method"`
	Amount    money.Money `json:"amount"`
	Tendered  money.Money `json:"tendered,omitempty"`
	Change    money.Money `json:"change,omitempty"`
	Reference string      `json:"reference,omitempty"`
	// PointsEarned is what the payment earned the customer of the order, decided by the service when it paid.
	PointsEarned int64 `json:"points_earned,omitempty"`
}

// TicketData names the station a ticket event is about.
type TicketData struct {
	Station menu.Station `json:"station"`
}

// CancelData carries the reason of a request or of its rejection.
type CancelData struct {
	Reason string `json:"reason"`
}

// CancelledData ends an order.
type CancelledData struct {
	Reason string `json:"reason"`
	// Refund is the money given back, 0 for an order that was never paid.
	Refund money.Money `json:"refund"`
	// Restock says that the ingredients go back to the stock.
	Restock     bool   `json:"restock"`
	RequestedBy string `json:"requested_by,omitempty"`
}

// RefundData is money given back after the order was completed.
type RefundData struct {
	Amount money.Money `json:"amount"`
	Reason string      `json:"reason"`
}

func validReason(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= MaxReasonRunes
}

func (c Channel) valid() bool { return c == ChannelApp || c == ChannelCounter }

// check validates an order as it was taken, so that a corrupt event cannot create
// an order whose numbers do not add up.
func (p *PlacedData) check() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
	}
	if p.Number < 1 {
		return bad("the order number must be positive")
	}
	if !p.Channel.valid() {
		return bad("unknown channel %q", p.Channel)
	}
	if p.Channel == ChannelApp && p.Customer == "" {
		return bad("an order from the app belongs to a customer")
	}
	if len(p.Lines) < 1 || len(p.Lines) > money.MaxLines {
		return bad("an order has 1 to %d lines, got %d", money.MaxLines, len(p.Lines))
	}
	var sub money.Money
	for i, l := range p.Lines {
		if l.Quantity < 1 || l.Quantity > money.MaxQuantity {
			return bad("line %d: quantity %d", i+1, l.Quantity)
		}
		if l.Unit < 0 || l.Unit > money.MaxPrice || l.Total != l.Unit.Times(l.Quantity) {
			return bad("line %d: %v x %d is not %v", i+1, l.Unit, l.Quantity, l.Total)
		}
		if len(l.Pieces) == 0 {
			return bad("line %d: nothing for the kitchen to make", i+1)
		}
		for _, pc := range l.Pieces {
			if pc.Station == "" {
				return bad("line %d: a piece without a station", i+1)
			}
		}
		sub += l.Total
	}
	var disc money.Money
	pointsDiscounts := 0
	for _, d := range p.Discounts {
		if d.Amount <= 0 {
			return bad("discount %q is not positive", d.ID)
		}
		if d.ID == PointsDiscountID {
			pointsDiscounts++
			if p.Points == nil || d.Amount != p.Points.Amount() {
				return bad("the discount of points is not what the points are worth")
			}
		}
		disc += d.Amount
	}
	if p.Points != nil {
		switch {
		case p.Points.Used < 1 || p.Points.Used > MaxPoints || p.Points.Value < 1 || p.Points.Value > money.MaxPrice:
			return bad("points used %d at %v each", p.Points.Used, p.Points.Value)
		case p.Channel != ChannelApp:
			return bad("points are used on an order from the app")
		case pointsDiscounts != 1:
			return bad("an order that uses points has exactly one discount of points")
		}
	}
	if p.Subtotal != sub || p.Discount != disc || disc > sub || p.Total != sub-disc {
		return bad("subtotal %v, discount %v, total %v do not add up from the lines", p.Subtotal, p.Discount, p.Total)
	}
	return nil
}

// validate checks the event on its own, without the order it applies to.
func (ev Event) validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
	}
	if ev.ID == "" || ev.Order == "" || ev.By == "" || ev.Seq < 1 {
		return bad("an event needs an ID, an order, an author and a sequence number")
	}
	payloads := 0
	for _, set := range []bool{ev.Placed != nil, ev.Paid != nil, ev.Ticket != nil, ev.Cancel != nil, ev.Cancelled != nil, ev.Refund != nil} {
		if set {
			payloads++
		}
	}
	if payloads > 1 {
		return bad("an event carries one payload, got %d", payloads)
	}
	need := func(ok bool) error {
		if !ok || payloads != 1 {
			return bad("%s needs exactly its own payload", ev.Type)
		}
		return nil
	}
	switch ev.Type {
	case EvPlaced:
		return need(ev.Placed != nil)
	case EvPaid:
		if err := need(ev.Paid != nil); err != nil {
			return err
		}
		return ev.Paid.check()
	case EvTicketStarted, EvTicketReady:
		if err := need(ev.Ticket != nil); err != nil {
			return err
		}
		if ev.Ticket.Station == "" {
			return bad("a ticket event needs a station")
		}
	case EvHandedOver:
		if payloads != 0 {
			return bad("handing over carries no payload")
		}
	case EvCancelRequested, EvCancelRejected:
		if err := need(ev.Cancel != nil); err != nil {
			return err
		}
		if !validReason(ev.Cancel.Reason) {
			return fmt.Errorf("%w: %v", ErrReasonRequired, ev.Type)
		}
	case EvCancelled:
		if err := need(ev.Cancelled != nil); err != nil {
			return err
		}
		if !validReason(ev.Cancelled.Reason) {
			return fmt.Errorf("%w: %v", ErrReasonRequired, ev.Type)
		}
		if ev.Cancelled.Refund < 0 {
			return bad("a negative refund")
		}
	case EvRefunded:
		if err := need(ev.Refund != nil); err != nil {
			return err
		}
		if !validReason(ev.Refund.Reason) {
			return fmt.Errorf("%w: %v", ErrReasonRequired, ev.Type)
		}
	default:
		return bad("unknown event type %q", ev.Type)
	}
	return nil
}

func (p *PaidData) check() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
	}
	if p.Amount < 0 || p.Tendered < 0 || p.Change < 0 {
		return bad("negative money in a payment")
	}
	if p.PointsEarned < 0 || p.PointsEarned > MaxPoints {
		return bad("points earned %d", p.PointsEarned)
	}
	switch p.Method {
	case Cash:
		if p.Tendered < p.Amount || p.Change != p.Tendered-p.Amount {
			return bad("cash: %v handed over for %v with %v change", p.Tendered, p.Amount, p.Change)
		}
	case Card, Transfer:
		if strings.TrimSpace(p.Reference) == "" {
			return bad("a %s payment needs a reference", p.Method)
		}
		if p.Tendered != 0 || p.Change != 0 {
			return bad("a %s payment has no change", p.Method)
		}
	default:
		return bad("unknown payment method %q", p.Method)
	}
	return nil
}

// Encode returns the JSON form of an event, for the log.
func Encode(ev Event) ([]byte, error) {
	if err := ev.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(ev)
}

// Decode reads an event from the log. Unknown fields are an error: an event this
// code does not understand must not be half understood.
func Decode(data []byte) (Event, error) {
	var ev Event
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if dec.More() {
		return Event{}, fmt.Errorf("%w: trailing data after the event", ErrInvalid)
	}
	if err := ev.validate(); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// Replay rebuilds an order from its events, in order. It stops at the first event
// that does not fit and says which one.
func Replay(events []Event) (*Order, error) {
	if len(events) == 0 {
		return nil, errors.New("order: no events to replay")
	}
	o := &Order{}
	for i, ev := range events {
		if err := o.Apply(ev); err != nil {
			return nil, fmt.Errorf("event %d (%s): %w", i+1, ev.Type, err)
		}
	}
	return o, nil
}
