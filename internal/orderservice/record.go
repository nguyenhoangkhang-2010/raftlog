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
	"time"

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
	Qty        int64       `json:"qty"`
	Reason     string      `json:"reason,omitempty"`
}

// Movement is the stock movement that this record stands for.
func (m StockMove) Movement() stock.Movement {
	return stock.Movement{Kind: m.Kind, Ingredient: stock.Ingredient(m.Ingredient), Qty: m.Qty, Reason: m.Reason, Ref: m.ID}
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
	if (len(r.Order) == 0) == (r.Stock == nil) {
		return nil, fmt.Errorf("%w: a record holds exactly one of an order event and a stock movement", ErrBadRecord)
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
	if (len(r.Order) == 0) == (r.Stock == nil) {
		return Record{}, fmt.Errorf("%w: a record holds exactly one of an order event and a stock movement", ErrBadRecord)
	}
	if r.Stock != nil {
		m := r.Stock
		switch {
		case m.ID == "" || m.By == "" || m.At.IsZero():
			return Record{}, fmt.Errorf("%w: a stock movement needs an ID, a person and a time", ErrBadRecord)
		case m.Kind != stock.Receive && m.Kind != stock.WriteOff && m.Kind != stock.Count:
			return Record{}, fmt.Errorf("%w: a person cannot make a %q movement", ErrBadRecord, m.Kind)
		}
	}
	return r, nil
}
