package orderservice

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
)

// Projector follows the log of the service and writes what it finds to the tables of the reports.
// It reads the records in order from the checkpoint that the store keeps, turns each into facts, and
// writes a batch of them together with the new checkpoint in one transaction: a record is counted
// once, however often the projector is stopped and started, and a projector that is behind catches
// up from where it was.
//
// It trusts the log. A record that the service skipped because it did not fit (a payment that the stock
// could not cover, written by something else) is counted by the projector, which does not know the
// rules; the service counts such records and says so in /healthz.
type Projector struct {
	store  *reports.Store
	log    Log
	topic  string
	batch  int
	logger *slog.Logger
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

// Step reads one batch from the checkpoint, waiting up to wait for a record if there is none, and
// counts it. It returns how many records it read.
func (p *Projector) Step(ctx context.Context, wait time.Duration) (int, error) {
	cp, err := p.store.Checkpoint(ctx, p.topic)
	if err != nil {
		return 0, err
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
			at, ok, err := count(tx, e.Value)
			if err != nil {
				return fmt.Errorf("counting the record at offset %d: %w", e.Offset, err)
			}
			if !ok {
				tx.Skipped()
			}
			tx.Seen(at)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// count turns one record of the log into facts. ok is false for a record that could not be read.
// at is the time of the event, zero for a record that has none.
func count(tx *reports.Tx, value []byte) (at time.Time, ok bool, err error) {
	rec, derr := DecodeRecord(value)
	if derr != nil {
		return time.Time{}, false, nil // the service skips it too
	}
	switch {
	case rec.Barrier != "":
		return time.Time{}, true, nil
	case rec.Stock != nil:
		m := rec.Stock
		return m.At, true, tx.StockMoved(reports.StockMovement{ID: m.ID, At: m.At, Kind: string(m.Kind), Ingredient: m.Ingredient, Qty: m.Qty, Cause: m.Cause,
			Supplier: m.Supplier, Document: m.Document, TotalCost: m.TotalCost, By: m.By, Reason: m.Reason})
	}
	ev, derr := order.Decode(rec.Order)
	if derr != nil {
		return time.Time{}, false, nil
	}
	id := string(ev.Order)
	switch ev.Type {
	case order.EvPlaced:
		pl := ev.Placed
		po := reports.PlacedOrder{ID: id, Number: pl.Number, Channel: string(pl.Channel), Customer: pl.Customer, At: ev.At,
			Subtotal: int64(pl.Subtotal), Discount: int64(pl.Discount), Total: int64(pl.Total), Needs: map[string]int64{}}
		stations := map[string]bool{}
		for _, l := range pl.Lines {
			po.Lines = append(po.Lines, reports.Line{Kind: l.Kind, Ref: l.Ref, Name: l.Name, Category: l.Category, Quantity: l.Quantity, Unit: int64(l.Unit), Total: int64(l.Total)})
			for ing, n := range l.Needs {
				po.Needs[string(ing)] += n * int64(l.Quantity)
			}
			for _, pc := range l.Pieces {
				stations[string(pc.Station)] = true
			}
		}
		for st := range stations {
			po.Stations = append(po.Stations, st)
		}
		sort.Strings(po.Stations)
		return ev.At, true, tx.OrderPlaced(po)
	case order.EvPaid:
		return ev.At, true, tx.OrderPaid(id, ev.Seq, ev.At, ev.By, ev.Paid.Method)
	case order.EvTicketStarted:
		return ev.At, true, tx.OrderStarted(id, ev.Seq)
	case order.EvTicketReady:
		return ev.At, true, tx.OrderStationReady(id, ev.Seq, string(ev.Ticket.Station), ev.At)
	case order.EvHandedOver:
		return ev.At, true, tx.OrderHandedOver(id, ev.Seq, ev.At)
	case order.EvCancelRequested, order.EvCancelRejected:
		return ev.At, true, tx.OrderTouched(id, ev.Seq)
	case order.EvCancelled:
		c := ev.Cancelled
		return ev.At, true, tx.OrderCancelled(id, ev.Seq, ev.At, ev.By, c.Reason, int64(c.Refund), c.Restock)
	case order.EvRefunded:
		return ev.At, true, tx.OrderRefunded(id, ev.Seq, ev.At, ev.By, ev.Refund.Reason, int64(ev.Refund.Amount))
	}
	return ev.At, true, nil
}
