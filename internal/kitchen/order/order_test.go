package order

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// The people in these tests.
var (
	ann      = rbac.Actor{ID: "ann", Roles: []rbac.Role{rbac.Customer}}
	bob      = rbac.Actor{ID: "bob", Roles: []rbac.Role{rbac.Customer}}
	cashier  = rbac.Actor{ID: "cash1", Roles: []rbac.Role{rbac.Cashier}}
	drinksK  = rbac.Actor{ID: "k-drinks", Roles: []rbac.Role{rbac.Kitchen}, Station: "drinks"}
	drinksK2 = rbac.Actor{ID: "k-drinks-2", Roles: []rbac.Role{rbac.Kitchen}, Station: "drinks"}
	hotK     = rbac.Actor{ID: "k-hot", Roles: []rbac.Role{rbac.Kitchen}, Station: "hot"}
	coldK    = rbac.Actor{ID: "k-cold", Roles: []rbac.Role{rbac.Kitchen}, Station: "cold"}
	runner   = rbac.Actor{ID: "run1", Roles: []rbac.Role{rbac.Runner}}
	mgr      = rbac.Actor{ID: "mgr1", Roles: []rbac.Role{rbac.Manager}}
	mgr2     = rbac.Actor{ID: "mgr2", Roles: []rbac.Role{rbac.Manager}}
	admin    = rbac.Actor{ID: "adm", Roles: []rbac.Role{rbac.Admin}}
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// world numbers the commands and moves the clock, as a service would.
type world struct {
	t   *testing.T
	n   int
	now time.Time
}

func newWorld(t *testing.T) *world { return &world{t: t, now: t0} }

func (w *world) meta(a rbac.Actor) Meta {
	w.n++
	w.now = w.now.Add(time.Second)
	return Meta{Cmd: fmt.Sprintf("cmd-%d", w.n), Actor: a, At: w.now}
}

func drinkLine() menu.Line {
	return menu.Line{
		Kind: "item", Ref: "coffee", Name: "Milk coffee", Category: "drinks", Options: []string{"L"},
		Unit: 30_000, Quantity: 2, Total: 60_000, Items: []menu.ItemID{"coffee"},
		Pieces: []menu.Piece{{Station: "drinks", Name: "Milk coffee (L)"}},
		Needs:  menu.Recipe{"coffee": 20, "cup": 1},
	}
}

func foodLine() menu.Line {
	return menu.Line{
		Kind: "item", Ref: "banh-mi", Name: "Banh mi", Category: "food",
		Unit: 25_000, Quantity: 1, Total: 25_000, Items: []menu.ItemID{"banh-mi"},
		Pieces: []menu.Piece{{Station: "hot", Name: "Banh mi"}},
		Needs:  menu.Recipe{"bread": 1},
	}
}

func pricing(t *testing.T, lines []menu.Line, promos ...promo.Promotion) promo.Result {
	t.Helper()
	s, err := promo.NewSet(promos)
	if err != nil {
		t.Fatal(err)
	}
	return s.Evaluate(lines, promo.Context{At: t0})
}

// apply applies the event a command made, and fails the test if the command or
// the application was refused.
func apply(t *testing.T, o *Order, ev *Event, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("command refused: %v", err)
	}
	if ev == nil {
		t.Fatal("the command was taken for a repeat")
	}
	if err := o.Apply(*ev); err != nil {
		t.Fatalf("the event was refused: %v", err)
	}
}

// placed returns an order of two lines (85.000đ, stations drinks and hot) that
// ann ordered from the app.
func placed(t *testing.T, w *world) *Order {
	t.Helper()
	lines := []menu.Line{drinkLine(), foodLine()}
	ev, err := Place(w.meta(ann), PlaceInput{Order: "o1", Number: 7, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: pricing(t, lines)})
	if err != nil {
		t.Fatal(err)
	}
	o := &Order{}
	if err := o.Apply(ev); err != nil {
		t.Fatal(err)
	}
	return o
}

func paid(t *testing.T, w *world) *Order {
	t.Helper()
	o := placed(t, w)
	ev, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 100_000})
	apply(t, o, ev, err)
	return o
}

// preparing: paid, and the drinks station has started.
func preparing(t *testing.T, w *world) *Order {
	t.Helper()
	o := paid(t, w)
	ev, err := o.StartTicket(w.meta(drinksK), "drinks")
	apply(t, o, ev, err)
	return o
}

func ready(t *testing.T, w *world) *Order {
	t.Helper()
	o := paid(t, w)
	for _, k := range []rbac.Actor{drinksK, hotK} {
		ev, err := o.StartTicket(w.meta(k), menu.Station(k.Station))
		apply(t, o, ev, err)
		ev, err = o.FinishTicket(w.meta(k), menu.Station(k.Station))
		apply(t, o, ev, err)
	}
	return o
}

func completed(t *testing.T, w *world) *Order {
	t.Helper()
	o := ready(t, w)
	ev, err := o.HandOver(w.meta(runner))
	apply(t, o, ev, err)
	return o
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

func TestAnOrderGoesFromPlacedToCompleted(t *testing.T) {
	w := newWorld(t)
	o := placed(t, w)
	if o.Status != Placed || o.Number != 7 || o.Total != 85_000 || o.Customer != "ann" || o.Channel != ChannelApp {
		t.Fatalf("placed: %+v", o)
	}
	if got := o.Stations(); len(got) != 2 || got[0] != "drinks" || got[1] != "hot" {
		t.Fatalf("stations = %v", got)
	}
	if o.StockHeld {
		t.Fatal("stock is taken at payment, not at placing")
	}

	ev, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 100_000})
	apply(t, o, ev, err)
	if o.Status != Paid || o.Payment == nil || o.Payment.Amount != 85_000 || o.Payment.Change != 15_000 || o.Payment.By != "cash1" || !o.StockHeld {
		t.Fatalf("paid: %+v %+v", o, o.Payment)
	}

	ev, err = o.StartTicket(w.meta(drinksK), "drinks")
	apply(t, o, ev, err)
	if o.Status != Preparing || o.Tickets["drinks"].Status != TicketStarted || o.Tickets["drinks"].StartedBy != "k-drinks" || o.Tickets["hot"].Status != TicketPending {
		t.Fatalf("preparing: %+v", o)
	}
	ev, err = o.FinishTicket(w.meta(drinksK2), "drinks") // another worker at the same station may finish it
	apply(t, o, ev, err)
	if o.Status != Preparing {
		t.Fatalf("one station is not enough for ready: %s", o.Status)
	}
	ev, err = o.StartTicket(w.meta(hotK), "hot")
	apply(t, o, ev, err)
	ev, err = o.FinishTicket(w.meta(hotK), "hot")
	apply(t, o, ev, err)
	if o.Status != Ready {
		t.Fatalf("every station is done: %s", o.Status)
	}

	ev, err = o.HandOver(w.meta(runner))
	apply(t, o, ev, err)
	if o.Status != Completed || o.CompletedBy != "run1" || !o.Done() || o.Seq() != 7 {
		t.Fatalf("completed: %+v seq %d", o, o.Seq())
	}
	// Needs: per-unit needs times quantity, over all lines.
	if n := o.Needs(); n["coffee"] != 40 || n["cup"] != 2 || n["bread"] != 1 {
		t.Fatalf("needs = %v", n)
	}
}

func TestWhoMayPlaceAnOrder(t *testing.T) {
	w := newWorld(t)
	lines := []menu.Line{drinkLine()}
	in := func(mut func(*PlaceInput)) PlaceInput {
		p := PlaceInput{Order: "o", Number: 1, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: pricing(t, lines)}
		mut(&p)
		return p
	}
	cases := []struct {
		name  string
		actor rbac.Actor
		in    PlaceInput
		want  error
	}{
		{"a customer for themselves", ann, in(func(*PlaceInput) {}), nil},
		{"a customer for someone else", bob, in(func(*PlaceInput) {}), ErrNotYours},
		{"a customer at the counter", ann, in(func(p *PlaceInput) { p.Channel = ChannelCounter }), ErrNotYours},
		{"the cashier at the counter, a walk-in", cashier, in(func(p *PlaceInput) { p.Channel, p.Customer = ChannelCounter, "" }), nil},
		{"the cashier for a known customer", cashier, in(func(p *PlaceInput) { p.Channel = ChannelCounter }), nil},
		{"the cashier posing as the app", cashier, in(func(*PlaceInput) {}), ErrInvalid},
		{"the kitchen", drinksK, in(func(*PlaceInput) {}), ErrNotAllowed},
		{"the administrator", admin, in(func(*PlaceInput) {}), ErrNotAllowed},
		{"no lines", ann, in(func(p *PlaceInput) { p.Lines, p.Pricing = nil, promo.Result{} }), ErrInvalid},
		{"no order ID", ann, in(func(p *PlaceInput) { p.Order = "" }), ErrInvalid},
		{"order number 0", ann, in(func(p *PlaceInput) { p.Number = 0 }), ErrInvalid},
		{"a total that does not add up", ann, in(func(p *PlaceInput) { p.Pricing.Total-- }), ErrInvalid},
		{"a discount larger than the order", ann, in(func(p *PlaceInput) {
			p.Pricing.Applied = []promo.Applied{{ID: "x", Amount: 999_999}}
			p.Pricing.Discount, p.Pricing.Total = 999_999, -939_999
		}), ErrInvalid},
		{"a line that does not multiply", ann, in(func(p *PlaceInput) {
			l := drinkLine()
			l.Total = 1
			p.Lines = []menu.Line{l}
		}), ErrInvalid},
		{"an app order with no customer", cashier, in(func(p *PlaceInput) { p.Channel, p.Customer = ChannelApp, "" }), ErrInvalid},
	}
	for _, c := range cases {
		_, err := Place(w.meta(c.actor), c.in)
		if c.want == nil && err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if c.want != nil {
			wantErr(t, c.name, err, c.want)
		}
	}
	// A command needs an ID and a time, and an actor who exists.
	if _, err := Place(Meta{Actor: ann, At: t0}, in(func(*PlaceInput) {})); !errors.Is(err, ErrInvalid) {
		t.Errorf("no command ID: %v", err)
	}
	if _, err := Place(Meta{Cmd: "c", Actor: ann}, in(func(*PlaceInput) {})); !errors.Is(err, ErrInvalid) {
		t.Errorf("no time: %v", err)
	}
	if _, err := Place(Meta{Cmd: "c", Actor: rbac.Actor{ID: "x", Roles: []rbac.Role{"wizard"}}, At: t0}, in(func(*PlaceInput) {})); err == nil {
		t.Error("an unknown role placed an order")
	}
}

func TestADiscountIsPartOfThePlacedOrder(t *testing.T) {
	w := newWorld(t)
	lines := []menu.Line{drinkLine(), foodLine()}
	res := pricing(t, lines, promo.Promotion{ID: "ten", Name: "10%", Kind: promo.PercentOff, Percent: 1000})
	ev, err := Place(w.meta(cashier), PlaceInput{Order: "o2", Number: 1, Channel: ChannelCounter, Lines: lines, Pricing: res})
	if err != nil {
		t.Fatal(err)
	}
	o := &Order{}
	if err := o.Apply(ev); err != nil {
		t.Fatal(err)
	}
	// 85.000đ less 8.500đ.
	if o.Subtotal != 85_000 || o.Discount != 8_500 || o.Total != 76_500 || len(o.Discounts) != 1 || o.Customer != "" {
		t.Fatalf("order = %+v", o)
	}
	// Paying means paying the discounted total.
	pay, err := o.Pay(w.meta(cashier), PayInput{Method: Card, Reference: "slip-1"})
	apply(t, o, pay, err)
	if o.Payment.Amount != 76_500 {
		t.Fatalf("paid %v", o.Payment.Amount)
	}
}

func TestPaymentRules(t *testing.T) {
	w := newWorld(t)
	o := placed(t, w)
	try := func(a rbac.Actor, in PayInput) error { _, err := o.Pay(w.meta(a), in); return err }

	wantErr(t, "a customer paying their own order", try(ann, PayInput{Method: Cash, Tendered: 85_000}), ErrNotAllowed)
	wantErr(t, "the kitchen", try(drinksK, PayInput{Method: Cash, Tendered: 85_000}), ErrNotAllowed)
	wantErr(t, "the manager", try(mgr, PayInput{Method: Cash, Tendered: 85_000}), ErrNotAllowed)
	wantErr(t, "cash that is short", try(cashier, PayInput{Method: Cash, Tendered: 84_999}), ErrAmount)
	wantErr(t, "a card with no slip", try(cashier, PayInput{Method: Card}), ErrInvalid)
	wantErr(t, "a transfer with no reference", try(cashier, PayInput{Method: Transfer, Reference: "  "}), ErrInvalid)
	wantErr(t, "an unknown method", try(cashier, PayInput{Method: "bitcoin", Reference: "x"}), ErrInvalid)
	if o.Status != Placed {
		t.Fatal("a refused payment changed the order")
	}

	ev, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 85_000}) // exact: no change
	apply(t, o, ev, err)
	if o.Payment.Change != 0 {
		t.Fatalf("change = %v", o.Payment.Change)
	}
	wantErr(t, "paying twice", try(cashier, PayInput{Method: Cash, Tendered: 85_000}), ErrWrongState)

	// Card and transfer record the reference.
	o2 := placed(t, w)
	ev, err = o2.Pay(w.meta(cashier), PayInput{Method: Transfer, Reference: "FT26110312345"})
	apply(t, o2, ev, err)
	if o2.Payment.Method != Transfer || o2.Payment.Reference != "FT26110312345" || o2.Payment.Tendered != 0 {
		t.Fatalf("payment = %+v", o2.Payment)
	}
}

func TestKitchenRules(t *testing.T) {
	w := newWorld(t)
	o := placed(t, w)
	start := func(a rbac.Actor, st menu.Station) error { _, err := o.StartTicket(w.meta(a), st); return err }
	finish := func(a rbac.Actor, st menu.Station) error { _, err := o.FinishTicket(w.meta(a), st); return err }

	// Nothing starts before payment.
	wantErr(t, "start before payment", start(drinksK, "drinks"), ErrWrongState)

	ev, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 100_000})
	apply(t, o, ev, err)

	wantErr(t, "a customer starting a ticket", start(ann, "drinks"), ErrNotAllowed)
	wantErr(t, "a manager starting a ticket", start(mgr, "drinks"), ErrNotAllowed)
	wantErr(t, "the hot station working on drinks", start(hotK, "drinks"), ErrWrongStation)
	wantErr(t, "a station the order does not need", start(coldK, "cold"), ErrNoSuchTicket)
	wantErr(t, "finish before start", finish(drinksK, "drinks"), ErrWrongState)

	ev, err = o.StartTicket(w.meta(drinksK), "drinks")
	apply(t, o, ev, err)
	wantErr(t, "starting a ticket twice", start(drinksK2, "drinks"), ErrWrongState)
	wantErr(t, "finishing at the wrong station", finish(hotK, "drinks"), ErrWrongStation)
	wantErr(t, "finishing a ticket that is still pending", finish(hotK, "hot"), ErrWrongState)

	ev, err = o.FinishTicket(w.meta(drinksK), "drinks")
	apply(t, o, ev, err)
	wantErr(t, "finishing a ticket twice", finish(drinksK, "drinks"), ErrWrongState)
	if o.Status != Preparing {
		t.Fatalf("status = %s, want preparing: the hot station has not started", o.Status)
	}
}

func TestAnOrderIsReadyOnlyWhenEveryStationIsDone(t *testing.T) {
	w := newWorld(t)
	o := paid(t, w)
	for _, k := range []rbac.Actor{drinksK, hotK} {
		ev, err := o.StartTicket(w.meta(k), menu.Station(k.Station))
		apply(t, o, ev, err)
	}
	ev, err := o.FinishTicket(w.meta(drinksK), "drinks")
	apply(t, o, ev, err)
	// The hot station has started but not finished: the order is not ready.
	if o.Status != Preparing {
		t.Fatalf("status = %s, want preparing while the hot station is still working", o.Status)
	}
	ev, err = o.FinishTicket(w.meta(hotK), "hot")
	apply(t, o, ev, err)
	if o.Status != Ready {
		t.Fatalf("status = %s, want ready", o.Status)
	}
}

func TestHandingOver(t *testing.T) {
	w := newWorld(t)
	o := preparing(t, w)
	hand := func(a rbac.Actor) error { _, err := o.HandOver(w.meta(a)); return err }

	wantErr(t, "hand over before it is ready", hand(runner), ErrWrongState)
	for _, k := range []rbac.Actor{drinksK, hotK} {
		st := menu.Station(k.Station)
		if o.Tickets[st].Status == TicketPending {
			ev, err := o.StartTicket(w.meta(k), st)
			apply(t, o, ev, err)
		}
		ev, err := o.FinishTicket(w.meta(k), st)
		apply(t, o, ev, err)
	}
	wantErr(t, "the customer collecting it themselves in the app", hand(ann), ErrNotAllowed)
	wantErr(t, "the kitchen", hand(drinksK), ErrNotAllowed)
	ev, err := o.HandOver(w.meta(cashier)) // the cashier may hand over too
	apply(t, o, ev, err)
	wantErr(t, "handing over twice", hand(runner), ErrWrongState)
}

func TestCancellingAnOrderThatIsNotPaid(t *testing.T) {
	w := newWorld(t)
	cancel := func(o *Order, a rbac.Actor, reason string) error {
		_, err := o.CancelUnpaid(w.meta(a), reason)
		return err
	}

	o := placed(t, w)
	wantErr(t, "someone else's order", cancel(o, bob, "changed my mind"), ErrNotYours)
	wantErr(t, "no reason", cancel(o, ann, "  "), ErrReasonRequired)
	wantErr(t, "the kitchen", cancel(o, drinksK, "x"), ErrNotAllowed)
	ev, err := o.CancelUnpaid(w.meta(ann), "changed my mind")
	apply(t, o, ev, err)
	if o.Status != Cancelled || o.Refunded != 0 || o.CancelledBy != "ann" || o.CancelReason != "changed my mind" || o.StockHeld {
		t.Fatalf("cancelled: %+v", o)
	}
	wantErr(t, "cancelling twice", cancel(o, ann, "again"), ErrWrongState)

	// A cashier may cancel any unpaid order.
	o = placed(t, w)
	ev, err = o.CancelUnpaid(w.meta(cashier), "customer left")
	apply(t, o, ev, err)
	if o.Status != Cancelled {
		t.Fatalf("status = %s", o.Status)
	}

	// Once paid, cancelling needs a request and a decision.
	o = paid(t, w)
	wantErr(t, "cancelling a paid order directly", cancel(o, ann, "x"), ErrWrongState)
}

func TestACancellationBeforeTheKitchenStartsRefundsEverythingAndRestocks(t *testing.T) {
	w := newWorld(t)
	o := paid(t, w)

	req := func(a rbac.Actor, reason string) error { _, err := o.RequestCancel(w.meta(a), reason); return err }
	wantErr(t, "someone else's order", req(bob, "x"), ErrNotYours)
	wantErr(t, "no reason", req(ann, ""), ErrReasonRequired)
	wantErr(t, "the kitchen", req(drinksK, "x"), ErrNotAllowed)

	ev, err := o.RequestCancel(w.meta(ann), "wrong order")
	apply(t, o, ev, err)
	if o.Request == nil || o.Request.By != "ann" || o.Status != Paid {
		t.Fatalf("request: %+v", o.Request)
	}
	wantErr(t, "a second request", req(ann, "again"), ErrCancelPending)

	// While the request waits, the kitchen may not start and nothing is handed over.
	if _, err := o.StartTicket(w.meta(drinksK), "drinks"); !errors.Is(err, ErrCancelPending) {
		t.Errorf("starting while a cancellation waits: %v", err)
	}

	approve := func(a rbac.Actor, refund money.Money) error { _, err := o.ApproveCancel(w.meta(a), refund); return err }
	wantErr(t, "the cashier approving", approve(cashier, 85_000), ErrNotAllowed)
	wantErr(t, "the customer approving their own", approve(ann, 85_000), ErrNotAllowed)
	wantErr(t, "a partial refund when nothing was made", approve(mgr, 50_000), ErrRefundPolicy)
	wantErr(t, "a refund above the total", approve(mgr, 85_001), ErrAmount)

	ev, err = o.ApproveCancel(w.meta(mgr), 85_000)
	apply(t, o, ev, err)
	if o.Status != Cancelled || o.Refunded != 85_000 || o.StockHeld || o.Request != nil || o.CancelledBy != "mgr1" || o.CancelReason != "wrong order" {
		t.Fatalf("cancelled: %+v", o)
	}
	if ev.Cancelled.RequestedBy != "ann" || !ev.Cancelled.Restock {
		t.Fatalf("event = %+v", ev.Cancelled)
	}
}

func TestACancellationAfterWorkStartedMayRefundLess(t *testing.T) {
	w := newWorld(t)
	o := preparing(t, w)
	ev, err := o.RequestCancel(w.meta(ann), "waited too long")
	apply(t, o, ev, err)

	ev, err = o.ApproveCancel(w.meta(mgr), 40_000) // the drinks were made
	apply(t, o, ev, err)
	if o.Status != Cancelled || o.Refunded != 40_000 {
		t.Fatalf("cancelled: %+v", o)
	}
	// The ingredients were used: they stay taken from the stock.
	if ev.Cancelled.Restock || !o.StockHeld {
		t.Fatalf("restock %v, stock held %v: food that was made does not go back on the shelf", ev.Cancelled.Restock, o.StockHeld)
	}

	// Even a zero refund is the manager's decision once food was made.
	o2 := preparing(t, w)
	ev, err = o2.RequestCancel(w.meta(ann), "x")
	apply(t, o2, ev, err)
	ev, err = o2.ApproveCancel(w.meta(mgr), 0)
	apply(t, o2, ev, err)
	if o2.Refunded != 0 {
		t.Fatalf("refunded %v", o2.Refunded)
	}
}

func TestNobodyApprovesTheirOwnRequest(t *testing.T) {
	w := newWorld(t)
	o := paid(t, w)
	// A manager may ask to cancel an order too (a customer walked away)...
	ev, err := o.RequestCancel(w.meta(mgr), "customer left")
	apply(t, o, ev, err)
	// ...but another person has to approve it.
	if _, err := o.ApproveCancel(w.meta(mgr), 85_000); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval: %v", err)
	}
	ev, err = o.ApproveCancel(w.meta(mgr2), 85_000)
	apply(t, o, ev, err)
	if o.Status != Cancelled {
		t.Fatalf("status = %s", o.Status)
	}
	// A person who is both cashier and manager is still one person.
	both := rbac.Actor{ID: "dual", Roles: []rbac.Role{rbac.Cashier, rbac.Manager}}
	o = paid(t, w)
	ev, err = o.RequestCancel(w.meta(both), "x")
	apply(t, o, ev, err)
	if _, err := o.ApproveCancel(w.meta(both), 85_000); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("a dual role approving their own request: %v", err)
	}
}

func TestARejectedRequestLetsTheOrderCarryOn(t *testing.T) {
	w := newWorld(t)
	o := preparing(t, w)
	if _, err := o.RejectCancel(w.meta(mgr), "x"); !errors.Is(err, ErrNoCancelRequest) {
		t.Fatalf("rejecting nothing: %v", err)
	}
	if _, err := o.ApproveCancel(w.meta(mgr), 0); !errors.Is(err, ErrNoCancelRequest) {
		t.Fatalf("approving nothing: %v", err)
	}
	ev, err := o.RequestCancel(w.meta(ann), "changed my mind")
	apply(t, o, ev, err)
	if _, err := o.RejectCancel(w.meta(mgr), ""); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("rejecting without a reason: %v", err)
	}
	if _, err := o.RejectCancel(w.meta(cashier), "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("the cashier rejecting: %v", err)
	}
	ev, err = o.RejectCancel(w.meta(mgr), "already made")
	apply(t, o, ev, err)
	if o.Request != nil || o.Status != Preparing {
		t.Fatalf("after the rejection: %+v", o)
	}
	// The kitchen can go on, and the customer may ask again.
	ev, err = o.StartTicket(w.meta(hotK), "hot")
	apply(t, o, ev, err)
	if _, err := o.RequestCancel(w.meta(ann), "second try"); err != nil {
		t.Fatalf("asking again: %v", err)
	}
}

func TestACancellationRequestWaitsOutTheHandOver(t *testing.T) {
	w := newWorld(t)
	o := ready(t, w)
	ev, err := o.RequestCancel(w.meta(ann), "I am leaving")
	apply(t, o, ev, err)
	if _, err := o.HandOver(w.meta(runner)); !errors.Is(err, ErrCancelPending) {
		t.Fatalf("hand over while a cancellation waits: %v", err)
	}
	// A completed or cancelled order cannot be cancelled again.
	done := completed(t, w)
	if _, err := done.RequestCancel(w.meta(ann), "late"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("asking to cancel a completed order: %v", err)
	}
	if _, err := placed(t, w).RequestCancel(w.meta(ann), "unpaid"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("asking to cancel an unpaid order: it can simply be cancelled: %v", err)
	}
}

func TestRefundingACompletedOrder(t *testing.T) {
	w := newWorld(t)
	o := completed(t, w)
	refund := func(a rbac.Actor, amount money.Money, reason string) error {
		_, err := o.IssueRefund(w.meta(a), amount, reason)
		return err
	}
	wantErr(t, "the cashier", refund(cashier, 10_000, "cold"), ErrNotAllowed)
	wantErr(t, "the customer", refund(ann, 10_000, "cold"), ErrNotAllowed)
	wantErr(t, "no reason", refund(mgr, 10_000, ""), ErrReasonRequired)
	wantErr(t, "nothing", refund(mgr, 0, "x"), ErrAmount)
	wantErr(t, "more than the total", refund(mgr, 85_001, "x"), ErrAmount)

	ev, err := o.IssueRefund(w.meta(mgr), 30_000, "coffee was cold")
	apply(t, o, ev, err)
	if o.Refunded != 30_000 || o.Status != Completed {
		t.Fatalf("after the first refund: %+v", o)
	}
	wantErr(t, "refunding more than what is left", refund(mgr, 55_001, "x"), ErrAmount)
	ev, err = o.IssueRefund(w.meta(mgr2), 55_000, "the rest")
	apply(t, o, ev, err)
	if o.Refunded != 85_000 {
		t.Fatalf("refunded %v", o.Refunded)
	}
	wantErr(t, "refunding it all twice", refund(mgr, 1, "x"), ErrAmount)

	// Only a completed order is refunded this way.
	wantErr(t, "refunding a paid order", func() error { _, err := paid(t, w).IssueRefund(w.meta(mgr), 1, "x"); return err }(), ErrWrongState)
}

func TestTheAdministratorCannotTouchOrders(t *testing.T) {
	w := newWorld(t)
	o := paid(t, w)
	if _, err := o.RequestCancel(w.meta(admin), "x"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("request: %v", err)
	}
	if _, err := o.ApproveCancel(w.meta(admin), 1); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("approve: %v", err)
	}
	if _, err := o.IssueRefund(w.meta(admin), 1, "x"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("refund: %v", err)
	}
	if _, err := o.Pay(w.meta(admin), PayInput{Method: Cash, Tendered: 1}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("pay: %v", err)
	}
}

func TestACommandRepeatedChangesNothing(t *testing.T) {
	w := newWorld(t)
	o := placed(t, w)
	m := w.meta(cashier)
	ev, err := o.Pay(m, PayInput{Method: Cash, Tendered: 100_000})
	apply(t, o, ev, err)
	before, _ := json.Marshal(o)

	// The same command arrives again (the log delivers at least once).
	again, err := o.Pay(m, PayInput{Method: Cash, Tendered: 100_000})
	if again != nil || err != nil {
		t.Fatalf("a repeat = %v, %v; want nothing to do and no error", again, err)
	}
	// And so does the event itself.
	if err := o.Apply(*ev); err != nil {
		t.Fatalf("an event applied twice: %v", err)
	}
	after, _ := json.Marshal(o)
	if string(before) != string(after) || o.Seq() != 2 {
		t.Fatal("a repeat changed the order")
	}
	// A repeat is recognised before anything else is checked, so a customer who
	// resends a command that worked is not told it was forbidden.
	m2 := w.meta(drinksK)
	ev, err = o.StartTicket(m2, "drinks")
	apply(t, o, ev, err)
	if again, err := o.StartTicket(m2, "drinks"); again != nil || err != nil {
		t.Fatalf("a repeated start = %v, %v", again, err)
	}
}

func TestApplyRefusesEventsThatDoNotFit(t *testing.T) {
	w := newWorld(t)
	o := paid(t, w)
	mk := func(mut func(*Event)) Event {
		ev := Event{ID: "x1", Order: "o1", Seq: o.Seq() + 1, At: t0, By: "k-hot", Type: EvTicketStarted, Ticket: &TicketData{Station: "hot"}}
		mut(&ev)
		return ev
	}
	cases := map[string]struct {
		ev   Event
		want error
	}{
		"a gap in the sequence":  {mk(func(e *Event) { e.Seq++ }), ErrSequence},
		"an old sequence number": {mk(func(e *Event) { e.Seq = 1 }), ErrSequence},
		"another order":          {mk(func(e *Event) { e.Order = "o2" }), ErrOtherOrder},
		"placing it again":       {mk(func(e *Event) { e.Type, e.Ticket = EvPlaced, nil; e.Placed = &PlacedData{} }), ErrIllegalTransition},
		"paying it again": {mk(func(e *Event) {
			e.Type, e.Ticket = EvPaid, nil
			e.Paid = &PaidData{Method: Cash, Amount: 85_000, Tendered: 85_000}
		}), ErrIllegalTransition},
		"handing over too early":     {mk(func(e *Event) { e.Type, e.Ticket = EvHandedOver, nil }), ErrIllegalTransition},
		"finishing before starting":  {mk(func(e *Event) { e.Type = EvTicketReady }), ErrIllegalTransition},
		"a station it does not have": {mk(func(e *Event) { e.Ticket.Station = "cold" }), ErrNoSuchTicket},
		"cancelling without a request": {mk(func(e *Event) {
			e.Type, e.Ticket = EvCancelled, nil
			e.Cancelled = &CancelledData{Reason: "x", Refund: 85_000, Restock: true}
		}), ErrIllegalTransition},
		"an unknown type":         {mk(func(e *Event) { e.Type = "order.teleported" }), ErrInvalid},
		"two payloads":            {mk(func(e *Event) { e.Cancel = &CancelData{Reason: "x"} }), ErrInvalid},
		"no payload":              {mk(func(e *Event) { e.Ticket = nil }), ErrInvalid},
		"no author":               {mk(func(e *Event) { e.By = "" }), ErrInvalid},
		"no ID":                   {mk(func(e *Event) { e.ID = "" }), ErrInvalid},
		"a refund before the end": {mk(func(e *Event) { e.Type, e.Ticket = EvRefunded, nil; e.Refund = &RefundData{Amount: 1, Reason: "x"} }), ErrIllegalTransition},
	}
	before, _ := json.Marshal(o)
	for name, c := range cases {
		wantErr(t, name, o.Apply(c.ev), c.want)
	}
	after, _ := json.Marshal(o)
	if string(before) != string(after) {
		t.Fatal("a refused event changed the order")
	}

	// An order cannot start with anything but its placement.
	fresh := &Order{}
	wantErr(t, "starting with a payment", fresh.Apply(Event{ID: "p", Order: "o9", Seq: 1, At: t0, By: "c", Type: EvPaid, Paid: &PaidData{Method: Cash}}), ErrIllegalTransition)
}

func TestAPaymentEventForTheWrongAmountIsRefused(t *testing.T) {
	w := newWorld(t)
	o := placed(t, w)
	ev := Event{ID: "p", Order: "o1", Seq: 2, At: t0, By: "cash1", Type: EvPaid, Paid: &PaidData{Method: Cash, Amount: 50_000, Tendered: 50_000}}
	wantErr(t, "wrong amount", o.Apply(ev), ErrAmount)
	ev.Paid = &PaidData{Method: Cash, Amount: 85_000, Tendered: 90_000, Change: 1}
	wantErr(t, "wrong change", o.Apply(ev), ErrInvalid)
	if o.Status != Placed {
		t.Fatal("a refused payment changed the order")
	}
}

func TestAnOrderIsRebuiltFromItsEvents(t *testing.T) {
	w := newWorld(t)
	var events []Event
	live := &Order{}
	step := func(ev *Event, err error) {
		t.Helper()
		apply(t, live, ev, err)
		events = append(events, *ev)
	}
	lines := []menu.Line{drinkLine(), foodLine()}
	placedEv, err := Place(w.meta(ann), PlaceInput{Order: "o1", Number: 3, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: pricing(t, lines)})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Apply(placedEv); err != nil {
		t.Fatal(err)
	}
	events = append(events, placedEv)

	step(live.Pay(w.meta(cashier), PayInput{Method: Card, Reference: "slip"}))
	step(live.StartTicket(w.meta(drinksK), "drinks"))
	step(live.RequestCancel(w.meta(ann), "too slow"))
	step(live.RejectCancel(w.meta(mgr), "almost done"))
	step(live.FinishTicket(w.meta(drinksK), "drinks"))
	step(live.StartTicket(w.meta(hotK), "hot"))
	step(live.FinishTicket(w.meta(hotK), "hot"))
	step(live.HandOver(w.meta(runner)))
	step(live.IssueRefund(w.meta(mgr), 5_000, "napkins missing"))

	rebuilt, err := Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(live)
	b, _ := json.Marshal(rebuilt)
	if string(a) != string(b) || rebuilt.Seq() != live.Seq() || rebuilt.Seq() != len(events) {
		t.Fatalf("the rebuilt order differs:\n%s\n%s", a, b)
	}

	// Through the JSON the log carries, too.
	var decoded []Event
	for _, ev := range events {
		data, err := Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("decoding %s: %v", data, err)
		}
		decoded = append(decoded, got)
	}
	again, err := Replay(decoded)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := json.Marshal(again)
	if string(a) != string(c) {
		t.Fatalf("an order rebuilt from JSON differs:\n%s\n%s", a, c)
	}
}

func TestReplayStopsAtTheFirstBadEvent(t *testing.T) {
	w := newWorld(t)
	o := paid(t, w)
	_ = o
	if _, err := Replay(nil); err == nil {
		t.Error("replayed nothing")
	}
	lines := []menu.Line{drinkLine()}
	placedEv, _ := Place(w.meta(ann), PlaceInput{Order: "o1", Number: 1, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: pricing(t, lines)})
	bad := Event{ID: "b", Order: "o1", Seq: 3, At: t0, By: "x", Type: EvHandedOver}
	_, err := Replay([]Event{placedEv, bad})
	if !errors.Is(err, ErrSequence) {
		t.Fatalf("err = %v", err)
	}
	if got := err.Error(); len(got) < 8 || got[:8] != "event 2 " {
		t.Fatalf("the error should say which event: %q", got)
	}
}

func TestDecodeRefusesWhatItDoesNotUnderstand(t *testing.T) {
	good, err := Encode(Event{ID: "e", Order: "o", Seq: 2, At: t0, By: "k", Type: EvTicketStarted, Ticket: &TicketData{Station: "hot"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(good); err != nil {
		t.Fatalf("a good event: %v", err)
	}
	for name, data := range map[string]string{
		"not json":            `{`,
		"an unknown field":    `{"id":"e","order":"o","seq":2,"at":"2026-11-03T12:00:00Z","by":"k","type":"ticket.started","ticket":{"station":"hot"},"surprise":1}`,
		"trailing data":       string(good) + `{}`,
		"no payload":          `{"id":"e","order":"o","seq":2,"at":"2026-11-03T12:00:00Z","by":"k","type":"ticket.started"}`,
		"the wrong payload":   `{"id":"e","order":"o","seq":2,"at":"2026-11-03T12:00:00Z","by":"k","type":"ticket.started","refund":{"amount":1,"reason":"x"}}`,
		"a future event type": `{"id":"e","order":"o","seq":2,"at":"2026-11-03T12:00:00Z","by":"k","type":"order.exploded"}`,
	} {
		if _, err := Decode([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Encode(Event{ID: "e"}); err == nil {
		t.Error("an invalid event was encoded")
	}
}

func TestTheLedgerOfAnOrderAddsUp(t *testing.T) {
	// Whatever the discount, the lines, the discount and the total agree.
	w := newWorld(t)
	lines := []menu.Line{drinkLine(), foodLine()}
	for _, pct := range []int64{1, 333, 1000, 5000, 9999, 10000} {
		res := pricing(t, lines, promo.Promotion{ID: "p", Name: "p", Kind: promo.PercentOff, Percent: pct})
		ev, err := Place(w.meta(cashier), PlaceInput{Order: "o", Number: 1, Channel: ChannelCounter, Lines: lines, Pricing: res})
		if err != nil {
			t.Fatalf("%d bp: %v", pct, err)
		}
		o := &Order{}
		if err := o.Apply(ev); err != nil {
			t.Fatal(err)
		}
		if o.Subtotal-o.Discount != o.Total || o.Total < 0 {
			t.Fatalf("%d bp: subtotal %v discount %v total %v", pct, o.Subtotal, o.Discount, o.Total)
		}
	}
}
