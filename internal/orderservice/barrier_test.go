package orderservice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// These tests are about a state that is behind the log. Reads of a raftlog cluster may be
// answered by a node that has not caught up, so a service that rebuilds its state from the log,
// or that lost the answer to a write, cannot know from a read that it has seen everything. A
// chaos test on a real cluster found two orders with the same number after a restart: the
// service had read the log from a node that was behind and decided on what it saw.

func TestAServiceThatStartsOnANodeThatIsBehindDoesNotGiveANumberTwice(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	for i := 0; i < 5; i++ {
		r.place(customer, toastie(1))
	}
	stored := r.log.Len() // the five orders are the last records

	// The first reads of the new service are answered by a node that lacks the last three orders.
	r.log.HideAfter, r.log.HideReads = stored-3, 2
	again := r.open(r.log)
	p, err := again.Place(r.ctx, customer, PlaceRequest{Command: "after-the-restart-1", Selections: []menu.Selection{toastie(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Order.Number != 6 {
		t.Fatalf("the first order after the restart got number %d, want 6: the service decided on a state without the last three orders", p.Order.Number)
	}
	if got := again.Orders(manager, OrderFilter{}); len(got) != 6 {
		t.Fatalf("%d orders after the restart, want 6", len(got))
	}
}

func TestACommandDecidedOnAStateThatWasBehindIsNotAcknowledged(t *testing.T) {
	r := newRig(t)
	// Beans for exactly one coffee. Two orders are placed, and a second writer pays one of them
	// without the first service having heard about it.
	r.receive("beans", 20)
	r.receive("milk", 150)
	a := r.place(customer, coffee(1, "s"))
	b := r.place(otherCust, coffee(1, "s"))

	other := r.open(r.log) // a second service on the same log, as a configuration mistake would make
	if _, err := other.Pay(r.ctx, cashier, order.ID(a.ID), r.cmd(), order.PayInput{Method: order.Card, Reference: "x"}); err != nil {
		t.Fatal(err)
	}

	// r.svc has not seen that payment, so it believes the beans are there. Its payment is written,
	// and when it is applied, in the order of the log, the beans are gone.
	n := r.log.Len()
	_, err := r.svc.Pay(r.ctx, cashier, order.ID(b.ID), r.cmd(), order.PayInput{Method: order.Card, Reference: "y"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a payment that the log could not take was reported as %v, want ErrConflict: the customer would have been told it was paid", err)
	}
	if r.log.Len() != n+1 {
		t.Fatalf("log grew by %d records, want 1 (the refused payment stays in the log, skipped)", r.log.Len()-n)
	}
	v, _ := r.svc.Order(cashier, order.ID(b.ID))
	if v.Status != "placed" {
		t.Fatalf("the order is %s after a payment that did not fit", v.Status)
	}
	if got := r.qty("beans"); got != 0 {
		t.Fatalf("beans = %d, want 0 (taken once, by the first order)", got)
	}
	if h := r.svc.Health(); h.Skipped != 1 {
		t.Fatalf("health %+v, want one skipped record", h)
	}
	// The same service now knows the truth, and says so to the next payment too.
	_, err = r.svc.Pay(r.ctx, cashier, order.ID(b.ID), r.cmd(), order.PayInput{Method: order.Card, Reference: "z"})
	var oos *OutOfStockError
	if !errors.As(err, &oos) {
		t.Fatalf("the retry: %v, want an out-of-stock refusal", err)
	}
}

func TestABarrierIsARecordThatChangesNothing(t *testing.T) {
	rec := Record{Barrier: "b-1-1"}
	data, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeRecord(data)
	if err != nil || back.Barrier != "b-1-1" {
		t.Fatalf("decoding a barrier: %v %+v", err, back)
	}
	if _, err := (Record{Barrier: "x", Stock: &StockMove{}}).Encode(); err == nil {
		t.Fatal("a record with two kinds was encoded")
	}
	if _, err := (Record{}).Encode(); err == nil {
		t.Fatal("an empty record was encoded")
	}
	if _, err := DecodeRecord([]byte(`{"barrier":"b","order":{"id":"x"}}`)); err == nil {
		t.Fatal("a record with two kinds was decoded")
	}

	r := newRig(t)
	st, err := NewState(testMenu(t), testPromos(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := st.ApplyRecord(0, rec)
	if err != nil || ch.Applied() || ch.Skipped != nil || st.Next() != 1 || st.Skipped() != 0 {
		t.Fatalf("applying a barrier: %v %+v next %d skipped %d", err, ch, st.Next(), st.Skipped())
	}
	_ = r
}

func TestAServiceThatLostTheAnswerOfAWriteMakesItsStateCompleteByItself(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, toastie(1))
	r.log.LoseAnswerNext = errors.New("connection reset")
	if _, err := r.svc.Pay(r.ctx, cashier, order.ID(a.ID), r.cmd(), order.PayInput{Method: order.Card, Reference: "slip"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if !r.svc.Health().Stale {
		t.Fatal("the service does not know that it may be behind")
	}
	// A reader asks for the order, and nothing else is being done: the state is old until the
	// service looks at the log again.
	if v, _ := r.svc.Order(cashier, order.ID(a.ID)); v.Status != "placed" {
		t.Fatalf("the order is %s before the service looked at the log", v.Status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.svc.Reconcile(ctx, 10*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for r.svc.Health().Stale && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.svc.Health().Stale {
		t.Fatal("the service is still behind after 5 seconds")
	}
	if v, _ := r.svc.Order(cashier, order.ID(a.ID)); v.Status != "paid" {
		t.Fatalf("the order is %s after the service made its state complete, want paid: the write had been stored", v.Status)
	}
}

func TestMakingTheStateCompleteWaitsForANodeThatIsBehind(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, toastie(1))
	stored := r.log.Len()
	r.log.LoseAnswerNext = errors.New("timeout")
	_, _ = r.svc.Pay(r.ctx, cashier, order.ID(a.ID), r.cmd(), order.PayInput{Method: order.Card, Reference: "slip"})
	// For the next several reads the node shows nothing past the order: the payment and the
	// barrier are hidden until it catches up.
	r.log.HideAfter, r.log.HideReads = stored, 5
	if err := func() error {
		r.svc.mu.Lock()
		defer r.svc.mu.Unlock()
		return r.svc.ensureFresh(r.ctx)
	}(); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.svc.Order(cashier, order.ID(a.ID)); v.Status != "paid" {
		t.Fatalf("the order is %s: the state was declared complete while the node was still behind", v.Status)
	}
	if r.log.HideReads != 0 {
		t.Fatalf("%d hidden reads left: the service did not wait", r.log.HideReads)
	}
	_ = stock.Receive
}
