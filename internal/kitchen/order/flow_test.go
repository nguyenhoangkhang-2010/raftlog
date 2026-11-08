package order

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// A small shop, to test the packages working together: the menu prices a cart, the
// promotions discount it, the order records it, the stock follows the order.

func shopMenu(t *testing.T) *menu.Menu {
	t.Helper()
	m, err := menu.New([]menu.Item{
		{ID: "coffee", Name: "Milk coffee", Category: "drinks", Station: "drinks", Price: 25_000, Active: true,
			Recipe: menu.Recipe{"coffee": 20, "cup": 1},
			Groups: []menu.Group{{ID: "size", Name: "Size", Min: 1, Max: 1, Options: []menu.Option{
				{ID: "m", Name: "M"}, {ID: "l", Name: "L", Delta: 5_000, Recipe: menu.Recipe{"coffee": 10}},
			}}}},
		{ID: "banh-mi", Name: "Banh mi", Category: "food", Station: "hot", Price: 25_000, Active: true, Recipe: menu.Recipe{"bread": 1}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func shopStock(t *testing.T) *stock.Stock {
	t.Helper()
	s, err := stock.New(map[stock.Ingredient]stock.Level{"coffee": {Qty: 100}, "cup": {Qty: 10}, "bread": {Qty: 2}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// takeOrder prices the cart and places the order, as the service will.
func takeOrder(t *testing.T, w *world, m *menu.Menu, s *stock.Stock, id ID, sels []menu.Selection, promos ...promo.Promotion) *Order {
	t.Helper()
	lines, errs := m.PriceCart(sels, menu.PriceOptions{At: t0, SoldOut: s.SoldOut(m)})
	if errs != nil {
		t.Fatalf("pricing: %v", errs)
	}
	set, err := promo.NewSet(promos)
	if err != nil {
		t.Fatal(err)
	}
	res := set.Evaluate(lines, promo.Context{At: t0})
	ev, err := Place(w.meta(ann), PlaceInput{Order: id, Number: 1, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: res})
	if err != nil {
		t.Fatal(err)
	}
	o := &Order{}
	if err := o.Apply(ev); err != nil {
		t.Fatal(err)
	}
	return o
}

// pay records the payment and takes the ingredients, which is what the service does
// when it sees the order become paid.
func pay(t *testing.T, w *world, o *Order, s *stock.Stock) {
	t.Helper()
	ev, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: o.Total})
	apply(t, o, ev, err)
	if err := s.Apply(stock.Movement{Kind: stock.Consume, Needs: o.Needs(), Ref: string(o.ID)}); err != nil {
		t.Fatalf("the paid order could not take its ingredients: %v", err)
	}
}

func TestAnOrderFlowsFromTheMenuThroughPromotionsToTheStock(t *testing.T) {
	w := newWorld(t)
	m, s := shopMenu(t), shopStock(t)
	cart := []menu.Selection{
		{Item: "coffee", Quantity: 2, Options: menu.Choice{"size": {"l"}}}, // 30.000 each, 30 coffee each
		{Item: "banh-mi", Quantity: 1},
	}
	ten := promo.Promotion{ID: "ten", Name: "10% off", Kind: promo.PercentOff, Percent: 1000}
	o := takeOrder(t, w, m, s, "o1", cart, ten)
	// (2 x 30.000 + 25.000) = 85.000, less 10%.
	if o.Subtotal != 85_000 || o.Discount != 8_500 || o.Total != 76_500 {
		t.Fatalf("order totals: %v %v %v", o.Subtotal, o.Discount, o.Total)
	}

	pay(t, w, o, s)
	// Coffee 20 + 10 for L, twice = 60; two cups; one bread.
	if s.Qty("coffee") != 40 || s.Qty("cup") != 8 || s.Qty("bread") != 1 {
		t.Fatalf("stock after payment = %v", s.Snapshot())
	}
}

func TestCancellingBeforeTheKitchenStartsPutsTheIngredientsBack(t *testing.T) {
	w := newWorld(t)
	m, s := shopMenu(t), shopStock(t)
	before := s.Snapshot()
	o := takeOrder(t, w, m, s, "o1", []menu.Selection{{Item: "banh-mi", Quantity: 2}})
	pay(t, w, o, s)
	if s.Qty("bread") != 0 {
		t.Fatalf("bread = %d", s.Qty("bread"))
	}
	ev, err := o.RequestCancel(w.meta(ann), "wrong order")
	apply(t, o, ev, err)
	ev, err = o.ApproveCancel(w.meta(mgr), o.Total)
	apply(t, o, ev, err)
	if ev.Cancelled.Restock {
		if err := s.Apply(stock.Movement{Kind: stock.Release, Needs: o.Needs(), Ref: string(o.ID)}); err != nil {
			t.Fatal(err)
		}
	}
	for ing, want := range before {
		if s.Qty(ing) != want {
			t.Fatalf("%s: %d, want %d: the stock did not return to where it was", ing, s.Qty(ing), want)
		}
	}
}

func TestCancellingAfterTheKitchenStartedKeepsTheIngredientsTaken(t *testing.T) {
	w := newWorld(t)
	m, s := shopMenu(t), shopStock(t)
	o := takeOrder(t, w, m, s, "o1", []menu.Selection{{Item: "banh-mi", Quantity: 1}})
	pay(t, w, o, s)
	ev, err := o.StartTicket(w.meta(hotK), "hot")
	apply(t, o, ev, err)
	ev, err = o.RequestCancel(w.meta(ann), "waited too long")
	apply(t, o, ev, err)
	ev, err = o.ApproveCancel(w.meta(mgr), o.Total)
	apply(t, o, ev, err)
	if ev.Cancelled.Restock || !o.StockHeld {
		t.Fatal("the bread was already being used: it cannot go back")
	}
	if s.Qty("bread") != 1 {
		t.Fatalf("bread = %d", s.Qty("bread"))
	}
}

func TestItemsRunOutAsOrdersTakeTheirIngredients(t *testing.T) {
	w := newWorld(t)
	m, s := shopMenu(t), shopStock(t) // 2 breads
	order := func(id ID, qty int) *Order {
		return takeOrder(t, w, m, s, id, []menu.Selection{{Item: "banh-mi", Quantity: qty}})
	}
	pay(t, w, order("o1", 1), s)
	pay(t, w, order("o2", 1), s)
	// The bread is gone: the next customer cannot even put it in the cart.
	_, errs := m.PriceCart([]menu.Selection{{Item: "banh-mi", Quantity: 1}}, menu.PriceOptions{At: t0, SoldOut: s.SoldOut(m)})
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want the banh mi to be sold out", errs)
	}
	// And the stock refuses to give more than it has, whatever the menu said.
	if err := s.Apply(stock.Movement{Kind: stock.Consume, Needs: menu.Recipe{"bread": 1}}); err == nil {
		t.Fatal("the stock went below zero")
	}
}

// --- a random walk over the whole life of an order --------------------------------

var allActors = []rbac.Actor{ann, bob, cashier, drinksK, drinksK2, hotK, coldK, runner, mgr, mgr2, admin,
	{ID: "dual", Roles: []rbac.Role{rbac.Cashier, rbac.Manager}}}

func randomLines(rng *rand.Rand) []menu.Line {
	pool := []menu.Line{drinkLine(), foodLine(), {
		Kind: "item", Ref: "salad", Name: "Salad", Category: "food", Unit: 30_000, Quantity: 1, Total: 30_000, Items: []menu.ItemID{"salad"},
		Pieces: []menu.Piece{{Station: "cold", Name: "Salad"}}, Needs: menu.Recipe{"lettuce": 80},
	}}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return append([]menu.Line(nil), pool[:1+rng.Intn(3)]...)
}

// guided picks, for the state the order is in, a sensible command by the right
// person, so that the random walk gets deep into an order's life instead of being
// refused at the first step. The commands are still allowed to be refused.
func guided(o *Order, rng *rand.Rand) (rbac.Actor, func(Meta) (*Event, error)) {
	workers := map[menu.Station]rbac.Actor{"drinks": drinksK, "hot": hotK, "cold": coldK}
	switch o.Status {
	case Placed:
		if rng.Intn(8) == 0 {
			return ann, func(m Meta) (*Event, error) { return o.CancelUnpaid(m, "changed my mind") }
		}
		return cashier, func(m Meta) (*Event, error) {
			return o.Pay(m, PayInput{Method: Cash, Tendered: o.Total + money.Money(rng.Intn(20_000))})
		}
	case Paid, Preparing, Ready:
		if o.Request != nil {
			if rng.Intn(3) == 0 {
				return mgr, func(m Meta) (*Event, error) { return o.RejectCancel(m, "carry on") }
			}
			refund := o.Total
			if o.ticketsStarted() {
				refund = money.Money(rng.Int63n(int64(o.Total) + 1))
			}
			return mgr2, func(m Meta) (*Event, error) { return o.ApproveCancel(m, refund) }
		}
		if rng.Intn(10) == 0 {
			return ann, func(m Meta) (*Event, error) { return o.RequestCancel(m, "waited too long") }
		}
		if o.Status == Ready {
			return runner, func(m Meta) (*Event, error) { return o.HandOver(m) }
		}
		for _, st := range o.Stations() {
			tk := o.Tickets[st]
			switch tk.Status {
			case TicketPending:
				return workers[st], func(m Meta) (*Event, error) { return o.StartTicket(m, st) }
			case TicketStarted:
				return workers[st], func(m Meta) (*Event, error) { return o.FinishTicket(m, st) }
			}
		}
	case Completed:
		if left := o.Total - o.Refunded; left > 0 {
			amount := money.Money(1 + rng.Int63n(int64(left)))
			return mgr, func(m Meta) (*Event, error) { return o.IssueRefund(m, amount, "complaint") }
		}
	}
	return ann, func(Meta) (*Event, error) { return nil, nil }
}

func checkInvariants(t *testing.T, trial int, o *Order, events []Event) {
	t.Helper()
	fail := func(format string, a ...any) {
		t.Helper()
		t.Fatalf("trial %d, after %d events (status %s): %s", trial, len(events), o.Status, fmt.Sprintf(format, a...))
	}
	if o.Seq() != len(events) {
		fail("seq %d for %d events", o.Seq(), len(events))
	}
	if o.Refunded < 0 || o.Refunded > o.Total {
		fail("refunded %v of %v", o.Refunded, o.Total)
	}
	if o.Refunded > 0 && o.Status != Cancelled && o.Status != Completed {
		fail("money was given back on a %s order", o.Status)
	}
	if o.Subtotal-o.Discount != o.Total || o.Total < 0 {
		fail("the numbers do not add up: %v %v %v", o.Subtotal, o.Discount, o.Total)
	}
	paid := o.Payment != nil
	switch o.Status {
	case Placed:
		if paid || o.StockHeld {
			fail("an unpaid order has a payment or holds stock")
		}
	case Preparing, Ready, Completed:
		if !paid {
			fail("an order in service without a payment")
		}
	case Cancelled:
		if o.Request != nil {
			fail("a cancelled order still has a request")
		}
	}
	if o.StockHeld && !paid {
		fail("stock held for an unpaid order")
	}
	started, ready := 0, 0
	for st, tk := range o.Tickets {
		if tk.Status != TicketPending && !paid {
			fail("station %s has begun on an unpaid order", st)
		}
		if tk.Status == TicketReady && tk.StartedBy == "" {
			fail("station %s finished what it never started", st)
		}
		if tk.Status != TicketPending {
			started++
		}
		if tk.Status == TicketReady {
			ready++
		}
	}
	if (o.Status == Ready || o.Status == Completed) && ready != len(o.Tickets) {
		fail("%s with %d of %d tickets ready", o.Status, ready, len(o.Tickets))
	}
	if o.Status == Preparing && started == 0 {
		fail("preparing with nothing started")
	}
	if o.Status == Paid && started != 0 {
		fail("paid, but a station has started")
	}
}

func TestRandomLivesOfOrdersKeepEveryInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	accepted := map[EventType]int{}
	for trial := 0; trial < 600; trial++ {
		w := newWorld(t)
		lines := randomLines(rng)
		var promos []promo.Promotion
		if rng.Intn(2) == 0 {
			promos = append(promos, promo.Promotion{ID: "p", Name: "p", Kind: promo.PercentOff, Percent: int64(1 + rng.Intn(5000))})
		}
		ev, err := Place(w.meta(ann), PlaceInput{Order: "o", Number: 1, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: pricing(t, lines, promos...)})
		if err != nil {
			t.Fatal(err)
		}
		o := &Order{}
		if err := o.Apply(ev); err != nil {
			t.Fatal(err)
		}
		events := []Event{ev}
		var lastMeta Meta
		var lastCmd func(Meta) (*Event, error)

		for step := 0; step < 60; step++ {
			a := allActors[rng.Intn(len(allActors))]
			stations := []menu.Station{"drinks", "hot", "cold"}
			st := stations[rng.Intn(3)]
			// Kitchen workers usually go to their own station, sometimes not.
			if a.Station != "" && rng.Intn(5) != 0 {
				st = menu.Station(a.Station)
			}
			var cmd func(Meta) (*Event, error)
			if rng.Intn(2) == 0 {
				a, cmd = guided(o, rng)
				m := w.meta(a)
				got, err := cmd(m)
				if err != nil || got == nil {
					continue
				}
				if err := o.Apply(*got); err != nil {
					t.Fatalf("trial %d: the command made %s by %s, and applying it failed: %v", trial, got.Type, a.ID, err)
				}
				accepted[got.Type]++
				events = append(events, *got)
				lastMeta, lastCmd = m, cmd
				checkInvariants(t, trial, o, events)
				continue
			}
			switch rng.Intn(11) {
			case 0, 1:
				cmd = func(m Meta) (*Event, error) {
					return o.Pay(m, PayInput{Method: []string{Cash, Card, Transfer, "x"}[rng.Intn(4)], Tendered: money.Money(rng.Intn(200_000)), Reference: "r"})
				}
			case 2, 3:
				cmd = func(m Meta) (*Event, error) { return o.StartTicket(m, st) }
			case 4, 5:
				cmd = func(m Meta) (*Event, error) { return o.FinishTicket(m, st) }
			case 6:
				cmd = func(m Meta) (*Event, error) { return o.HandOver(m) }
			case 7:
				if rng.Intn(2) == 0 {
					cmd = func(m Meta) (*Event, error) { return o.CancelUnpaid(m, "reason") }
				} else {
					cmd = func(m Meta) (*Event, error) { return o.RequestCancel(m, "reason") }
				}
			case 8:
				refund := money.Money(rng.Int63n(int64(o.Total) + 10_000))
				cmd = func(m Meta) (*Event, error) { return o.ApproveCancel(m, refund) }
			case 9:
				if rng.Intn(2) == 0 {
					cmd = func(m Meta) (*Event, error) { return o.RejectCancel(m, "no") }
				} else {
					amount := money.Money(1 + rng.Int63n(int64(o.Total)+1))
					cmd = func(m Meta) (*Event, error) { return o.IssueRefund(m, amount, "reason") }
				}
			default:
				// The same command again: it must change nothing.
				if lastCmd != nil {
					before, _ := json.Marshal(o)
					if again, err := lastCmd(lastMeta); again != nil || err != nil {
						t.Fatalf("trial %d: a repeated command gave %v, %v", trial, again, err)
					}
					after, _ := json.Marshal(o)
					if string(before) != string(after) {
						t.Fatalf("trial %d: a repeated command changed the order", trial)
					}
				}
				continue
			}

			m := w.meta(a)
			got, err := cmd(m)
			if err != nil || got == nil {
				continue // refused, which is allowed: nothing happened
			}
			// What a command accepts, Apply must accept: the two layers agree.
			if err := o.Apply(*got); err != nil {
				t.Fatalf("trial %d: the command made %s by %s, and applying it failed: %v", trial, got.Type, a.ID, err)
			}
			accepted[got.Type]++
			events = append(events, *got)
			lastMeta, lastCmd = m, cmd
			checkInvariants(t, trial, o, events)
		}

		// The log replays to the same order, and delivering everything twice changes nothing.
		rebuilt, err := Replay(events)
		if err != nil {
			t.Fatalf("trial %d: replay: %v", trial, err)
		}
		a, _ := json.Marshal(o)
		b, _ := json.Marshal(rebuilt)
		if string(a) != string(b) {
			t.Fatalf("trial %d: replay differs:\n%s\n%s", trial, a, b)
		}
		for _, ev := range events {
			if err := o.Apply(ev); err != nil {
				t.Fatalf("trial %d: a duplicate delivery failed: %v", trial, err)
			}
		}
		c, _ := json.Marshal(o)
		if string(a) != string(c) {
			t.Fatalf("trial %d: duplicate deliveries changed the order", trial)
		}
	}

	// The walk must actually get places, or it proves little.
	for _, ty := range []EventType{EvPaid, EvTicketStarted, EvTicketReady, EvHandedOver, EvCancelRequested, EvCancelRejected, EvCancelled, EvRefunded} {
		if accepted[ty] < 20 {
			t.Errorf("only %d %s events happened in the random walks: the test does not reach that part", accepted[ty], ty)
		}
	}
}
