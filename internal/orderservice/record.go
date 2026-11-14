// Package orderservice is the kitchen system's service on raftlog: it takes commands
// (place an order, pay, start a ticket, receive goods, ...), decides them with the rules of
// internal/kitchen, and writes the resulting events to one raftlog topic. Its state is
// built from that topic and nothing else, so a restarted service, or a new one, rebuilds
// the same orders and the same stock from the log.
//
// Everything that happens goes into one partition, so the log gives one order of all the
// events. That order is what makes the stock decisions the same everywhere: the stock is
// taken when an order is paid, and which order was paid first is whichever comes first in
// the log.
package orderservice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// Record is what the service writes to the log: an event of an order, or a movement of the
// stock made by a person. Exactly one of the two is set.
//
// The stock that orders take and give back is not written: it follows from the order
// events (paid, cancelled), so every reader derives it alike.
type Record struct {
	// Order is an order.Event, exactly as order.Encode writes it.
	Order json.RawMessage `json:"order,omitempty"`
	Stock *StockMove      `json:"stock,omitempty"`
	// Points is a change of the points of a customer that a manager made.
	Points *PointsMove `json:"points,omitempty"`
	// Barrier marks a point of the log and changes nothing. The service writes one after it
	// starts, and after a write whose outcome it did not learn, and reads on until it sees its
	// own barrier: everything written before the barrier is then in its state. A read may be
	// answered by a node that is behind, so this is the only way to know that the state is
	// complete before deciding something on it.
	Barrier string `json:"barrier,omitempty"`
}

// kinds is how many of the four kinds the record has set; a valid record has exactly one.
func (r Record) kinds() int {
	n := 0
	if len(r.Order) != 0 {
		n++
	}
	if r.Stock != nil {
		n++
	}
	if r.Points != nil {
		n++
	}
	if r.Barrier != "" {
		n++
	}
	return n
}

// StockMove is a movement of the stock that a person made: goods received, something
// written off, a stocktake. ID is the ID of the command, which makes a repeat harmless.
type StockMove struct {
	ID         string      `json:"id"`
	By         string      `json:"by"`
	Roles      []rbac.Role `json:"roles,omitempty"`
	At         time.Time   `json:"at"`
	Kind       stock.Kind  `json:"kind"`
	Ingredient string      `json:"ingredient"`
	// Qty is the amount; for an adjustment it is signed.
	Qty    int64  `json:"qty"`
	Reason string `json:"reason,omitempty"`

	// For goods received: when the lot expires (not set if it has no date), what was paid for all
	// of it in whole đồng, who it came from (the ID of a supplier) and the number of the document.
	Expires   *time.Time `json:"expires,omitempty"`
	TotalCost int64      `json:"total_cost,omitempty"`
	Supplier  string     `json:"supplier,omitempty"`
	Document  string     `json:"document,omitempty"`
	// For a write-off: the lot it was taken from, if one was named, and the cause.
	Lot   string `json:"lot,omitempty"`
	Cause string `json:"cause,omitempty"`
}

// PointsMove is points that a manager gave to a customer, or took from them, with the reason: the one way to
// change a balance other than by an order (see ADR 23). ID is the ID of the command, which makes a repeat
// harmless. Points is signed and never zero.
type PointsMove struct {
	ID       string      `json:"id"`
	By       string      `json:"by"`
	Roles    []rbac.Role `json:"roles,omitempty"`
	At       time.Time   `json:"at"`
	Customer string      `json:"customer"`
	Points   int64       `json:"points"`
	Reason   string      `json:"reason"`
}

// Movement is the stock movement that this record stands for.
func (m StockMove) Movement() stock.Movement {
	mv := stock.Movement{Kind: m.Kind, Ingredient: stock.Ingredient(m.Ingredient), Qty: m.Qty, Reason: m.Reason, Ref: m.ID, At: m.At,
		TotalCost: m.TotalCost, Supplier: m.Supplier, Document: m.Document, FromLot: m.Lot, Cause: m.Cause}
	if m.Expires != nil {
		mv.ExpiresAt = *m.Expires
	}
	return mv
}

func validPointsReason(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= order.MaxReasonRunes
}

// ErrBadRecord is returned for a log entry that this version cannot read.
var ErrBadRecord = errors.New("orderservice: unreadable record")

// OrderRecord wraps an order event.
func OrderRecord(ev order.Event) (Record, error) {
	raw, err := order.Encode(ev)
	if err != nil {
		return Record{}, err
	}
	return Record{Order: raw}, nil
}

// Encode returns the JSON form of the record, for the log.
func (r Record) Encode() ([]byte, error) {
	if r.kinds() != 1 {
		return nil, fmt.Errorf("%w: a record holds exactly one of an order event, a stock movement, a points change and a barrier", ErrBadRecord)
	}
	return json.Marshal(r)
}

// DecodeRecord reads a record from the log. Unknown fields are an error, as they are for
// order events: a record this code does not understand must not be half understood.
func DecodeRecord(data []byte) (Record, error) {
	var r Record
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrBadRecord, err)
	}
	if dec.More() {
		return Record{}, fmt.Errorf("%w: trailing data", ErrBadRecord)
	}
	if r.kinds() != 1 {
		return Record{}, fmt.Errorf("%w: a record holds exactly one of an order event, a stock movement, a points change and a barrier", ErrBadRecord)
	}
	if m := r.Points; m != nil {
		switch {
		case m.ID == "" || m.By == "" || m.At.IsZero() || m.Customer == "":
			return Record{}, fmt.Errorf("%w: a change of points needs an ID, a person, a customer and a time", ErrBadRecord)
		case m.Points == 0 || m.Points > order.MaxPoints || m.Points < -order.MaxPoints:
			return Record{}, fmt.Errorf("%w: a change of %d points", ErrBadRecord, m.Points)
		case !validPointsReason(m.Reason):
			return Record{}, fmt.Errorf("%w: a change of points needs a reason of at most %d characters", ErrBadRecord, order.MaxReasonRunes)
		}
	}
	if r.Stock != nil {
		m := r.Stock
		switch {
		case m.ID == "" || m.By == "" || m.At.IsZero():
			return Record{}, fmt.Errorf("%w: a stock movement needs an ID, a person and a time", ErrBadRecord)
		case m.Kind != stock.Receive && m.Kind != stock.WriteOff && m.Kind != stock.Count && m.Kind != stock.Adjust:
			return Record{}, fmt.Errorf("%w: a person cannot make a %q movement", ErrBadRecord, m.Kind)
		}
	}
	return r, nil
}
