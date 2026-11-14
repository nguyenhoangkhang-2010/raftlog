package orderservice

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
)

// Projector follows the log of the service and writes what it finds to the tables of the reports.
// It reads the records in order from the checkpoint that the store keeps, turns each into facts, and
// writes a batch of them together with the new checkpoint in one transaction: a record is counted
// once, however often the projector is stopped and started, and a projector that is behind catches
// up from where it was.
//
// It applies every record to a State of its own, the one the service uses, so that it decides what
// the service decides: a record that the service skipped because it did not fit (a payment that the
// stock cannot cover) is skipped here too and is not counted, and what an order took from the stock is
// taken from the same lots, at the same prices. The price of that is a second copy of the state in
// memory, and a replay of the log from the start when the projector starts, which the service does too.
type Projector struct {
	store  *reports.Store
	log    Log
	topic  string
	batch  int
	logger *slog.Logger

	st *State
	// moved collects the movements of the stock that the record being applied made.
	moved []LedgerEntry
}

// NewProjector returns a projector of the log of a topic into the store. batch is how many records
// it reads at a time.
func NewProjector(store *reports.Store, l Log, topic string, batch int, logger *slog.Logger) *Projector {
	if batch <= 0 {
		batch = 500
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Projector{store: store, log: l, topic: topic, batch: batch, logger: logger}
}

// Run follows the log until the context ends. A failure (the log or the database did not answer) is
// logged and tried again a moment later.
func (p *Projector) Run(ctx context.Context) {
	pause := 500 * time.Millisecond
	for ctx.Err() == nil {
		n, err := p.Step(ctx, 2*time.Second)
		switch {
		case err != nil && ctx.Err() == nil:
			p.logger.Warn("the reports could not follow the log", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(pause):
			}
			if pause < 10*time.Second {
				pause *= 2
			}
		default:
			pause = 500 * time.Millisecond
			if n == 0 {
				select { // the log gave nothing and did not wait (a log that does not): do not spin
				case <-ctx.Done():
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
	}
}

// fresh starts the state again from nothing.
func (p *Projector) fresh() error {
	st, err := NewState(nil, nil, nil)
	if err != nil {
		return err
	}
	st.observe = func(e LedgerEntry) { p.moved = append(p.moved, e) }
	p.st = st
	return nil
}

// apply puts a record in the state and says what it did: the movements of the stock it made, and
// whether it was refused (the same rule that the service has).
func (p *Projector) apply(e Entry) (rec Record, moved []LedgerEntry, refused bool) {
	p.moved = p.moved[:0]
	rec, err := DecodeRecord(e.Value)
	if err != nil {
		p.st.skip(e.Offset)
		return Record{}, nil, true
	}
	ch, _ := p.st.ApplyRecord(e.Offset, rec)
	return rec, append([]LedgerEntry(nil), p.moved...), ch.Skipped != nil
}

// catchUp brings the state to the checkpoint without writing anything: what was counted is counted.
func (p *Projector) catchUp(ctx context.Context, to uint64) error {
	for p.st.Next() < to {
		entries, err := p.log.Read(ctx, p.st.Next(), p.batch, 0)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return fmt.Errorf("the checkpoint is at %d and the log ends at %d", to, p.st.Next())
		}
		for _, e := range entries {
			if e.Offset >= to {
				return nil
			}
			if e.Offset != p.st.Next() {
				return fmt.Errorf("the log gave offset %d where %d was next", e.Offset, p.st.Next())
			}
			p.apply(e)
		}
	}
	return nil
}

// Step reads one batch from the checkpoint, waiting up to wait for a record if there is none, and
// counts it. It returns how many records it read.
func (p *Projector) Step(ctx context.Context, wait time.Duration) (int, error) {
	cp, err := p.store.Checkpoint(ctx, p.topic)
	if err != nil {
		return 0, err
	}
	// the state must be exactly where the checkpoint is; if it is not (the database was reset, or a
	// batch failed after the state had moved), it is built again
	if p.st == nil || p.st.Next() != cp.Next {
		if err := p.fresh(); err != nil {
			return 0, err
		}
		if err := p.catchUp(ctx, cp.Next); err != nil {
			p.st = nil
			return 0, err
		}
	}
	entries, err := p.log.Read(ctx, cp.Next, p.batch, wait)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}
	next := cp.Next
	err = p.store.Apply(ctx, p.topic, cp.Next, entries[len(entries)-1].Offset+1, func(tx *reports.Tx) error {
		for _, e := range entries {
			if e.Offset != next {
				return fmt.Errorf("the log gave offset %d where %d was next", e.Offset, next)
			}
			next++
			rec, moved, refused := p.apply(e)
			if refused {
				tx.Skipped()
				continue
			}
			at, err := count(tx, rec, moved)
			if err != nil {
				return fmt.Errorf("counting the record at offset %d: %w", e.Offset, err)
			}
			tx.Seen(at)
		}
		return nil
	})
	if err != nil {
		p.st = nil // it has moved on and the database has not: start the state again
		return 0, err
	}
	return len(entries), nil
}

// count turns one record of the log, which the state accepted, into facts. at is the time of the event,
// zero for a record that has none.
func count(tx *reports.Tx, rec Record, moved []LedgerEntry) (at time.Time, err error) {
	switch {
	case rec.Barrier != "":
		return time.Time{}, nil
	case rec.Points != nil:
		return rec.Points.At, nil // points are not sales or goods: the reports do not count them
	case rec.Stock != nil:
		m := rec.Stock
		sm := reports.StockMovement{ID: m.ID, At: m.At, Kind: string(m.Kind), Ingredient: m.Ingredient, Qty: m.Qty, Cause: m.Cause,
			Supplier: m.Supplier, Document: m.Document, TotalCost: m.TotalCost, By: m.By, Reason: m.Reason}
		for _, e := range moved { // a movement of the stock touches one ingredient
			sm.Delta, sm.ValueMilli = e.Delta, e.ValueMilli
		}
		return m.At, tx.StockMoved(sm)
	}
	ev, derr := order.Decode(rec.Order)
	if derr != nil {
		return time.Time{}, nil // the state would have refused it
	}
	id := string(ev.Order)
	switch ev.Type {
	case order.EvPlaced:
		pl := ev.Placed
		po := reports.PlacedOrder{ID: id, Number: pl.Number, Channel: string(pl.Channel), Customer: pl.Customer, At: ev.At,
			Subtotal: int64(pl.Subtotal), Discount: int64(pl.Discount), Total: int64(pl.Total)}
		stations := map[string]bool{}
		for _, l := range pl.Lines {
			po.Lines = append(po.Lines, reports.Line{Kind: l.Kind, Ref: l.Ref, Name: l.Name, Category: l.Category, Quantity: l.Quantity, Unit: int64(l.Unit), Total: int64(l.Total)})
			for _, pc := range l.Pieces {
				stations[string(pc.Station)] = true
			}
		}
		for st := range stations {
			po.Stations = append(po.Stations, st)
		}
		sort.Strings(po.Stations)
		return ev.At, tx.OrderPlaced(po)
	case order.EvPaid:
		if err := tx.OrderPaid(id, ev.Seq, ev.At, ev.By, ev.Paid.Method); err != nil {
			return ev.At, err
		}
		return ev.At, usage(tx, id, ev.At, moved)
	case order.EvTicketStarted:
		return ev.At, tx.OrderStarted(id, ev.Seq)
	case order.EvTicketReady:
		return ev.At, tx.OrderStationReady(id, ev.Seq, string(ev.Ticket.Station), ev.At)
	case order.EvHandedOver:
		return ev.At, tx.OrderHandedOver(id, ev.Seq, ev.At)
	case order.EvCancelRequested, order.EvCancelRejected:
		return ev.At, tx.OrderTouched(id, ev.Seq)
	case order.EvCancelled:
		c := ev.Cancelled
		if err := tx.OrderCancelled(id, ev.Seq, ev.At, ev.By, c.Reason, int64(c.Refund)); err != nil {
			return ev.At, err
		}
		return ev.At, usage(tx, id, ev.At, moved)
	case order.EvRefunded:
		return ev.At, tx.OrderRefunded(id, ev.Seq, ev.At, ev.By, ev.Refund.Reason, int64(ev.Refund.Amount))
	}
	return ev.At, nil
}

// usage writes what the state says an order took from the stock or gave back, with its worth: the
// entries of its movement, one for each ingredient. A release is negative.
func usage(tx *reports.Tx, orderID string, at time.Time, moved []LedgerEntry) error {
	for _, e := range moved {
		if e.Kind != stock.Consume && e.Kind != stock.Release {
			continue
		}
		qty, value := -e.Delta, e.ValueMilli
		if qty < 0 {
			value = -value
		}
		if err := tx.Usage(orderID, at, e.Ingredient, qty, value); err != nil {
			return err
		}
	}
	return nil
}
