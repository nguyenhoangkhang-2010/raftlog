package orderservice

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// bareRig is a rig whose stock has nothing in it, so that the lots in a test are the ones it makes.
func lotsOf(t *testing.T, r *rig, a rbac.Actor, ing string) []LotView {
	t.Helper()
	w, err := r.svc.Warehouse(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range w.Rows {
		if row.Ingredient == ing {
			return row.Lots
		}
	}
	t.Fatalf("no row for %s", ing)
	return nil
}

func (r *rig) deliver(a rbac.Actor, supplier, doc string, lines ...ReceiptLine) {
	r.t.Helper()
	if _, err := r.svc.ReceiveGoods(r.ctx, a, r.cmd(), ReceiptInput{Supplier: supplier, Document: doc, Lines: lines}); err != nil {
		r.t.Fatalf("delivery %s: %v", doc, err)
	}
}

func qtyByLot(lots []LotView) map[string]int64 {
	out := map[string]int64{}
	for _, l := range lots {
		out[l.Document+"/"+l.ID[strings.LastIndex(l.ID, "-")+1:]] = l.Qty
	}
	return out
}

func TestADeliveryMakesALotForEachLineWithItsDatePriceSupplierAndDocument(t *testing.T) {
	r := newRig(t)
	r.deliver(storekeeper, "sup-1", "INV-1",
		ReceiptLine{Ingredient: "bread", Qty: 50, TotalCost: 100_000, Expires: "2026-11-25"},
		ReceiptLine{Ingredient: "cheese", Qty: 2000, TotalCost: 300_000})
	bread := lotsOf(t, r, storekeeper, "bread")
	if len(bread) != 1 || bread[0].Qty != 50 || bread[0].Supplier != "sup-1" || bread[0].Document != "INV-1" || bread[0].UnitCostMilli != 2_000_000 {
		t.Fatalf("bread lots: %+v", bread)
	}
	// the last day is a day it may be used: the lot stops being usable at the start of the next day
	if bread[0].ExpiresAt == nil || !bread[0].ExpiresAt.Equal(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expires at %v, want the start of 26 November", bread[0].ExpiresAt)
	}
	if cheese := lotsOf(t, r, storekeeper, "cheese"); cheese[0].ExpiresAt != nil || cheese[0].UnitCostMilli != 150_000 {
		t.Fatalf("a lot with no date: %+v", cheese)
	}
	// Bad deliveries are refused whole.
	before := r.log.Len()
	bad := map[string]ReceiptInput{
		"no lines":            {},
		"an unknown item":     {Lines: []ReceiptLine{{Ingredient: "bread", Qty: 1}, {Ingredient: "unobtainium", Qty: 1}}},
		"a quantity of zero":  {Lines: []ReceiptLine{{Ingredient: "bread", Qty: 1}, {Ingredient: "bread", Qty: 0}}},
		"a bad date":          {Lines: []ReceiptLine{{Ingredient: "bread", Qty: 1, Expires: "25/11/2026"}}},
		"a date that is gone": {Lines: []ReceiptLine{{Ingredient: "bread", Qty: 1, Expires: "2026-11-10"}}},
		"a negative price":    {Lines: []ReceiptLine{{Ingredient: "bread", Qty: 1, TotalCost: -5}}},
	}
	for name, in := range bad {
		if _, err := r.svc.ReceiveGoods(r.ctx, storekeeper, r.cmd(), in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if r.log.Len() != before {
		t.Fatalf("a refused delivery wrote %d records", r.log.Len()-before)
	}
	// Only someone who may receive goods can.
	for _, a := range []rbac.Actor{customer, cashier, drinksCook} {
		if _, err := r.svc.ReceiveGoods(r.ctx, a, r.cmd(), ReceiptInput{Lines: []ReceiptLine{{Ingredient: "bread", Qty: 1}}}); err == nil {
			t.Errorf("%v received goods", a.Roles)
		}
	}
	if _, err := r.svc.ReceiveGoods(r.ctx, manager, r.cmd(), ReceiptInput{Lines: []ReceiptLine{{Ingredient: "bread", Qty: 1}}}); err == nil {
		t.Error("a manager received goods: that is the storekeeper's work")
	}
}

func TestADeliveryThatWasCutShortIsFinishedByRepeatingItAndNothingIsCountedTwice(t *testing.T) {
	r := newRig(t)
	cmd := r.cmd()
	in := ReceiptInput{Supplier: "sup-1", Document: "INV-9", Lines: []ReceiptLine{
		{Ingredient: "bread", Qty: 10}, {Ingredient: "cheese", Qty: 100}, {Ingredient: "milk", Qty: 1000}}}
	// the log goes away after the first line was written (the first append is the barrier of Open)
	fl := &failingLog{MemoryLog: r.log, failAt: 3}
	svc, err := Open(r.ctx, fl, Options{Menu: testMenu(t), Promos: testPromos(t), Location: time.UTC, Now: r.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReceiveGoods(r.ctx, storekeeper, cmd, in); err == nil {
		t.Fatal("the cut was not reported")
	}
	got, _ := svc.Warehouse(storekeeper)
	written := 0
	for _, row := range got.Rows {
		if row.Qty > 0 {
			written++
		}
	}
	if written == 0 || written == 3 {
		t.Fatalf("%d lines were written, the test needs a delivery that is cut in the middle", written)
	}
	if _, err := svc.ReceiveGoods(r.ctx, storekeeper, cmd, in); err != nil {
		t.Fatalf("repeating: %v", err)
	}
	if _, err := svc.ReceiveGoods(r.ctx, storekeeper, cmd, in); err != nil { // and again, for good measure
		t.Fatalf("repeating again: %v", err)
	}
	for ing, want := range map[string]int64{"bread": 10, "cheese": 100, "milk": 1000} {
		if q, _ := svc.StockQty(ing); q != want {
			t.Errorf("%s is %d, want %d", ing, q, want)
		}
	}
	rec, _ := svc.Receipts(storekeeper, 10)
	if len(rec) != 1 || len(rec[0].Lines) != 3 || rec[0].Document != "INV-9" {
		t.Fatalf("the delivery as the history shows it: %+v", rec)
	}
}

// failingLog stops storing after n appends, as a log does that is lost for a moment.
type failingLog struct {
	*MemoryLog
	n      int
	failAt int
	down   bool
}

func (f *failingLog) Append(ctx context.Context, v []byte) (uint64, error) {
	f.n++
	if f.n == f.failAt {
		f.down = true
	}
	if f.down {
		f.down = false
		f.failAt = -1
		return 0, errors.New("no leader")
	}
	return f.MemoryLog.Append(ctx, v)
}

func TestAnOrderTakesFromTheLotThatExpiresFirstAndACancelGivesItBack(t *testing.T) {
	r := newRig(t)
	r.deliver(storekeeper, "sup-1", "A", ReceiptLine{Ingredient: "bread", Qty: 10, Expires: "2026-11-22", TotalCost: 10})
	r.deliver(storekeeper, "sup-1", "B", ReceiptLine{Ingredient: "bread", Qty: 10, Expires: "2026-11-29", TotalCost: 20})
	r.deliver(storekeeper, "sup-1", "C", ReceiptLine{Ingredient: "cheese", Qty: 2000})
	o := r.place(customer, toastie(3)) // 6 bread
	o = r.pay(o.ID)
	if got := qtyByLot(lotsOf(t, r, storekeeper, "bread")); !reflect.DeepEqual(got, map[string]int64{"A/01": 4, "B/01": 10}) {
		t.Fatalf("lots after paying: %v: the lot that expires first goes first", got)
	}
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(o.ID), r.cmd(), "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(o.ID), r.cmd(), money.Money(o.Total)); err != nil {
		t.Fatal(err)
	}
	if got := qtyByLot(lotsOf(t, r, storekeeper, "bread")); !reflect.DeepEqual(got, map[string]int64{"A/01": 10, "B/01": 10}) {
		t.Fatalf("lots after the cancel: %v, want them as they were", got)
	}
	// the history says what happened, newest first
	led, err := r.svc.Ledger(storekeeper, "bread", 10)
	if err != nil || len(led.Entries) != 4 {
		t.Fatalf("ledger: %v %+v", err, led)
	}
	kinds := []stock.Kind{led.Entries[0].Kind, led.Entries[1].Kind, led.Entries[2].Kind, led.Entries[3].Kind}
	if !reflect.DeepEqual(kinds, []stock.Kind{stock.Release, stock.Consume, stock.Receive, stock.Receive}) {
		t.Fatalf("kinds %v", kinds)
	}
	if e := led.Entries[1]; e.Delta != -6 || e.After != 14 || e.Ref != o.ID || e.By != cashier.ID {
		t.Fatalf("what the payment took: %+v", e)
	}
	// 6 bread from lot A at 1000 milli-dong each
	if led.Entries[1].ValueMilli != 6*1000 || led.Entries[0].ValueMilli != 6*1000 {
		t.Fatalf("values: %d took, %d gave back", led.Entries[1].ValueMilli, led.Entries[0].ValueMilli)
	}
}

func TestAnExpiredLotCannotMakeAnOrderAndItsItemIsSoldOut(t *testing.T) {
	r := newRig(t)
	r.deliver(storekeeper, "", "", ReceiptLine{Ingredient: "bread", Qty: 10, Expires: "2026-11-20"}, ReceiptLine{Ingredient: "cheese", Qty: 500})
	o := r.place(customer, toastie(1)) // placed on the 20th, while it can still be made
	r.clk.t = time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	var out *OutOfStockError
	if _, err := r.svc.Pay(r.ctx, cashier, order.ID(o.ID), r.cmd(), order.PayInput{Method: order.Card, Reference: "s"}); !errors.As(err, &out) {
		t.Fatalf("paying with only an expired lot: %v", err)
	}
	for _, it := range r.svc.Menu().Items {
		if it.ID == "toastie" && !it.SoldOut {
			t.Fatal("an item that only an expired lot could make is for sale")
		}
	}
	if _, err := r.svc.Place(r.ctx, customer, PlaceRequest{Command: r.cmd(), Selections: []menu.Selection{toastie(1)}}); err == nil {
		t.Fatal("an order was taken for an item that is sold out")
	}
	// The expired lot is still stock until somebody writes it off, and the view says so.
	w, _ := r.svc.Warehouse(storekeeper)
	for _, row := range w.Rows {
		if row.Ingredient == "bread" && (row.Qty != 10 || row.Usable != 0 || row.Gone != 10 || row.Lots[0].State != "expired") {
			t.Fatalf("bread: %+v", row)
		}
	}
	lot := lotsOf(t, r, storekeeper, "bread")[0].ID
	if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: stock.WriteOff, Ingredient: "bread", Qty: 10, Lot: lot, Cause: stock.CauseExpired, Reason: "past its date"}); err != nil {
		t.Fatal(err)
	}
	if q, _ := r.svc.StockQty("bread"); q != 0 {
		t.Fatalf("bread after the write-off: %d", q)
	}
}

func TestALotThatExpiresSoonIsFlaggedWithinTheWarningTime(t *testing.T) {
	r := newRig(t)
	r.deliver(storekeeper, "", "", ReceiptLine{Ingredient: "bread", Qty: 5, Expires: "2026-11-21"}, ReceiptLine{Ingredient: "milk", Qty: 5, Expires: "2026-12-30"})
	w, _ := r.svc.Warehouse(storekeeper)
	state := map[string]string{}
	for _, row := range w.Rows {
		for _, l := range row.Lots {
			state[row.Ingredient] = l.State
		}
	}
	// the 21st ends at the start of the 22nd: about 38 hours from 10:00 on the 20th, inside 72
	if state["bread"] != "soon" || state["milk"] != "" || w.WarningHours != 72 {
		t.Fatalf("states %v, warning %d h", state, w.WarningHours)
	}
}

func TestTheServiceRebuiltFromTheLogHasTheSameLotsAndTheSameHistory(t *testing.T) {
	r := newRig(t)
	r.deliver(storekeeper, "sup-1", "A", ReceiptLine{Ingredient: "bread", Qty: 10, Expires: "2026-11-22", TotalCost: 10}, ReceiptLine{Ingredient: "cheese", Qty: 900, TotalCost: 90})
	r.deliver(storekeeper, "sup-2", "B", ReceiptLine{Ingredient: "bread", Qty: 10, Expires: "2026-11-29", TotalCost: 20})
	a := r.pay(r.place(customer, toastie(2)).ID)
	r.pay(r.place(customer, toastie(1)).ID)
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(a.ID), r.cmd(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(a.ID), r.cmd(), money.Money(a.Total)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: stock.WriteOff, Ingredient: "cheese", Qty: 30, Cause: stock.CauseSpilled, Reason: "dropped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: stock.Adjust, Ingredient: "bread", Qty: -2, Reason: "stocktake 3"}); err != nil {
		t.Fatal(err)
	}
	again := r.open(r.log)
	w1, _ := r.svc.Warehouse(storekeeper)
	w2, _ := again.Warehouse(storekeeper)
	if !reflect.DeepEqual(w1, w2) {
		t.Fatalf("a service rebuilt from the log has other lots:\n%+v\n%+v", w1, w2)
	}
	for _, ing := range []string{"bread", "cheese"} {
		l1, _ := r.svc.Ledger(storekeeper, ing, 100)
		l2, _ := again.Ledger(storekeeper, ing, 100)
		if !reflect.DeepEqual(l1, l2) {
			t.Fatalf("the history of %s differs after a rebuild:\n%+v\n%+v", ing, l1, l2)
		}
	}
}

func TestWhoMayLookAtThePricesAndTheMovements(t *testing.T) {
	r := newRig(t)
	r.deliver(storekeeper, "sup-1", "A", ReceiptLine{Ingredient: "bread", Qty: 10, TotalCost: 50_000})
	for _, a := range []rbac.Actor{customer, cashier, runner} {
		if _, err := r.svc.Warehouse(a); err == nil {
			t.Errorf("%v looked at the warehouse", a.Roles)
		}
		if _, err := r.svc.Ledger(a, "bread", 10); err == nil {
			t.Errorf("%v read the history", a.Roles)
		}
	}
	// the kitchen sees the quantities and the lots, not what they cost
	w, err := r.svc.Warehouse(drinksCook)
	if err != nil || w.Costs {
		t.Fatalf("kitchen: %v costs %v", err, w.Costs)
	}
	for _, row := range w.Rows {
		if row.ValueMilli != nil || row.Unpriced != 0 {
			t.Fatalf("the kitchen sees a value: %+v", row)
		}
		for _, l := range row.Lots {
			if l.UnitCostMilli != 0 {
				t.Fatalf("the kitchen sees a price: %+v", l)
			}
		}
	}
	led, _ := r.svc.Ledger(drinksCook, "bread", 10)
	for _, e := range led.Entries {
		if e.ValueMilli != 0 {
			t.Fatalf("the kitchen sees the value of a movement: %+v", e)
		}
	}
	// the storekeeper and the manager see them
	for _, a := range []rbac.Actor{storekeeper, manager} {
		w, _ := r.svc.Warehouse(a)
		for _, row := range w.Rows {
			if row.Ingredient == "bread" && (row.ValueMilli == nil || *row.ValueMilli != 50_000_000) {
				t.Fatalf("%v: bread is worth %v", a.Roles, row.ValueMilli)
			}
		}
	}
}

func TestWasteIsAddedUpByIngredientAndCause(t *testing.T) {
	r := newRig(t)
	r.deliver(storekeeper, "", "", ReceiptLine{Ingredient: "milk", Qty: 1000, TotalCost: 30}, ReceiptLine{Ingredient: "cheese", Qty: 1000, TotalCost: 50})
	move := func(in StockInput) {
		t.Helper()
		if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), in); err != nil {
			t.Fatal(err)
		}
	}
	move(StockInput{Kind: stock.WriteOff, Ingredient: "milk", Qty: 100, Cause: stock.CauseSpoiled, Reason: "left out"})
	move(StockInput{Kind: stock.WriteOff, Ingredient: "milk", Qty: 50, Cause: stock.CauseSpoiled, Reason: "left out"})
	move(StockInput{Kind: stock.WriteOff, Ingredient: "milk", Qty: 20, Reason: "no cause given"})
	move(StockInput{Kind: stock.Adjust, Ingredient: "cheese", Qty: -40, Reason: "stocktake 1"})
	move(StockInput{Kind: stock.Adjust, Ingredient: "cheese", Qty: 25, Reason: "stocktake 1"}) // a surplus is not waste
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	w, err := r.svc.Waste(storekeeper, from, from.AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []WasteRow{
		{"cheese", CauseStocktake, 40, 40 * 50},
		{"milk", stock.CauseOther, 20, 20 * 30},
		{"milk", stock.CauseSpoiled, 150, 150 * 30},
	}
	if !reflect.DeepEqual(w.Rows, want) {
		t.Fatalf("waste:\n got %+v\nwant %+v", w.Rows, want)
	}
	// what falls outside the period is left out, and nothing is shown to a person who may not see costs
	none, _ := r.svc.Waste(storekeeper, from.AddDate(0, 2, 0), from.AddDate(0, 3, 0))
	if len(none.Rows) != 0 {
		t.Fatalf("waste of another month: %+v", none.Rows)
	}
	k, _ := r.svc.Waste(drinksCook, from, from.AddDate(0, 1, 0))
	for _, row := range k.Rows {
		if row.ValueMilli != 0 {
			t.Fatalf("the kitchen sees a value: %+v", row)
		}
	}
}

func TestTheHistoryOfAnIngredientIsBoundedAndSaysSo(t *testing.T) {
	r := newRig(t)
	for i := 0; i < ledgerCap+5; i++ {
		if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: stock.Receive, Ingredient: "bread", Qty: 1}); err != nil {
			t.Fatal(err)
		}
	}
	led, _ := r.svc.Ledger(storekeeper, "bread", 500)
	if len(led.Entries) != 500 || !led.Truncated {
		t.Fatalf("%d entries, truncated %v", len(led.Entries), led.Truncated)
	}
	if q, _ := r.svc.StockQty("bread"); q != int64(ledgerCap+5) {
		t.Fatalf("the quantity is not the history: %d", q)
	}
	if led.Entries[0].After != int64(ledgerCap+5) {
		t.Fatalf("the newest entry: %+v", led.Entries[0])
	}
}

func TestACommandCannotCarryWhatBelongsToAnotherKind(t *testing.T) {
	r := newRig(t)
	bad := []StockInput{
		{Kind: stock.WriteOff, Ingredient: "bread", Qty: 1, Reason: "x", Supplier: "sup-1"},
		{Kind: stock.WriteOff, Ingredient: "bread", Qty: 1, Reason: "x", Expires: "2026-12-01"},
		{Kind: stock.Receive, Ingredient: "bread", Qty: 1, Cause: stock.CauseSpoiled},
		{Kind: stock.Receive, Ingredient: "bread", Qty: 1, Lot: "x"},
		{Kind: stock.Adjust, Ingredient: "bread", Qty: 1, Reason: "x", TotalCost: 5},
		{Kind: stock.Consume, Ingredient: "bread", Qty: 1},
	}
	for _, in := range bad {
		if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	// An adjustment is the storekeeper's or the manager's; nobody else's.
	if _, err := r.svc.MoveStockWith(r.ctx, drinksCook, r.cmd(), StockInput{Kind: stock.Adjust, Ingredient: "bread", Qty: 1, Reason: "x"}); err == nil {
		t.Error("a cook adjusted the stock")
	}
}
