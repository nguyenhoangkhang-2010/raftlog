package order

import (
	"testing"
	"time"
)

// The table in docs/kitchen says that a kitchen worker can neither start nor finish a ticket,
// and nobody can hand the order over, while a cancellation waits for the manager. Starting
// and handing over were checked; finishing was not, in the command or in Apply, until a test
// of the order service wrote the sequence and found that the kitchen could finish the work.

func TestNoStationFinishesItsWorkWhileACancellationWaits(t *testing.T) {
	w := newWorld(t)
	o := preparing(t, w) // drinks has started
	ev, err := o.RequestCancel(w.meta(ann), "waited too long")
	apply(t, o, ev, err)

	_, err = o.FinishTicket(w.meta(drinksK), "drinks")
	wantErr(t, "finishing while a cancellation waits", err, ErrCancelPending)

	// Starting the other station is refused for the same reason (already the rule).
	_, err = o.StartTicket(w.meta(hotK), "hot")
	wantErr(t, "starting while a cancellation waits", err, ErrCancelPending)

	// Once the manager rejects the request, the work goes on.
	ev, err = o.RejectCancel(w.meta(mgr), "the drink is already made")
	apply(t, o, ev, err)
	ev, err = o.FinishTicket(w.meta(drinksK), "drinks")
	apply(t, o, ev, err)
	if o.Tickets["drinks"].Status != TicketReady {
		t.Fatalf("drinks = %s after the request was rejected", o.Tickets["drinks"].Status)
	}
}

func TestApplyRefusesAFinishedTicketWhileACancellationWaits(t *testing.T) {
	w := newWorld(t)
	o := preparing(t, w)
	// The event that finishes the ticket is made before the request, as a service with a
	// stale view could do; it reaches Apply after the request was applied.
	finish, err := o.FinishTicket(w.meta(drinksK), "drinks")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := o.RequestCancel(w.meta(ann), "changed my mind")
	apply(t, o, ev, err)

	late := *finish
	late.Seq = o.Seq() + 1
	late.ID = "late-finish-1"
	late.At = late.At.Add(time.Minute)
	if err := o.Apply(late); err == nil {
		t.Fatal("a ticket was finished while a cancellation waited")
	}
	if o.Tickets["drinks"].Status != TicketStarted {
		t.Fatalf("the refused event changed the ticket to %s", o.Tickets["drinks"].Status)
	}
}

func TestApplyRefusesAStartedTicketWhileACancellationWaits(t *testing.T) {
	w := newWorld(t)
	o := paid(t, w)
	start, err := o.StartTicket(w.meta(drinksK), "drinks")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := o.RequestCancel(w.meta(ann), "changed my mind")
	apply(t, o, ev, err)

	late := *start
	late.Seq = o.Seq() + 1
	late.ID = "late-start-1"
	late.At = late.At.Add(time.Minute)
	if err := o.Apply(late); err == nil {
		t.Fatal("a ticket was started while a cancellation waited")
	}
	if o.Tickets["drinks"].Status != TicketPending {
		t.Fatalf("the refused event changed the ticket to %s", o.Tickets["drinks"].Status)
	}
}
