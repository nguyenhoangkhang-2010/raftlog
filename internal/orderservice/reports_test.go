package orderservice

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
)

const topicName = "kitchen-test"

type reportRig struct {
	*rig
	store *reports.Store
	proj  *Projector
}

func newReportRig(t *testing.T) *reportRig {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(context.Background(), pool, reports.Migrations(), pgmigrate.WithTable(reports.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	store := reports.New(pool)
	return &reportRig{rig: r, store: store, proj: NewProjector(store, r.log, topicName, 7, nil)} // a small batch, so that batches are crossed
}

// follow reads the log until the projector has caught up with it.
func (r *reportRig) follow(proj *Projector) {
	r.t.Helper()
	for i := 0; i < 1000; i++ {
		n, err := proj.Step(r.ctx, 0)
		if err != nil {
			r.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	r.t.Fatal("the projector does not catch up")
}

var vn = reports.Period{From: "2026-11-19", To: "2026-11-21", Zone: "Asia/Ho_Chi_Minh"}

// a day of the shop: two cash orders, one by card, one cancelled with a refund, one refunded after it was
// handed over, one never paid, and some goods written off.
func (r *reportRig) aDay() (cashOrder, cardOrder, cancelled, refunded OrderView) {
	r.t.Helper()
	r.stockAll()
	if _, err := r.svc.ReceiveGoods(r.ctx, storekeeper, r.cmd(), ReceiptInput{Supplier: "sup-1", Document: "INV-1",
		Lines: []ReceiptLine{{Ingredient: "bread", Qty: 100, TotalCost: 120_000}}}); err != nil {
		r.t.Fatal(err)
	}
	cashOrder = r.payCash(cashier, r.place(customer, toastie(2)).ID) // 90,000 in cash
	cardOrder = r.pay(r.place(customer, coffee(1, "s")).ID)          // 35,000 by card
	cancelled = r.payCash(cashier2, r.place(customer, toastie(1)).ID)
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(cancelled.ID), r.cmd(), "changed my mind"); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(cancelled.ID), r.cmd(), money.Money(cancelled.Total)); err != nil {
		r.t.Fatal(err)
	}
	refunded = r.pay(r.place(customer, toastie(1)).ID)
	for _, step := range []func() error{
		func() error {
			_, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(refunded.ID), r.cmd(), "hot")
			return err
		},
		func() error {
			_, err := r.svc.FinishTicket(r.ctx, hotCook, order.ID(refunded.ID), r.cmd(), "hot")
			return err
		},
		func() error { _, err := r.svc.HandOver(r.ctx, cashier, order.ID(refunded.ID), r.cmd()); return err },
		func() error {
			_, err := r.svc.IssueRefund(r.ctx, manager, order.ID(refunded.ID), r.cmd(), 10_000, "a cold toastie")
			return err
		},
	} {
		if err := step(); err != nil {
			r.t.Fatal(err)
		}
	}
	r.place(customer, toastie(1)) // never paid
	if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: "write_off", Ingredient: "milk", Qty: 100, Cause: "spoiled", Reason: "left out"}); err != nil {
		r.t.Fatal(err)
	}
	return
}

func TestTheReportsCountWhatHappenedInTheLog(t *testing.T) {
	r := newReportRig(t)
	cashOrd, cardOrd, cancelled, refunded := r.aDay()
	r.follow(r.proj)

	days, err := r.store.SalesByDay(r.ctx, vn)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 3 || days[0].Orders != 0 || days[2].Orders != 0 {
		t.Fatalf("three days, the first and the last with nothing: %+v", days)
	}
	d := days[1]
	collected := cashOrd.Total + cardOrd.Total + cancelled.Total + refunded.Total
	given := cancelled.Total + 10_000
	if d.Date != "2026-11-20" || d.Orders != 4 || d.Total != collected || d.Subtotal != collected || d.Discount != 0 {
		t.Fatalf("the day: %+v (collected %d)", d, collected)
	}
	if d.Cash != cashOrd.Total+cancelled.Total || d.Card != cardOrd.Total+refunded.Total || d.Transfer != 0 {
		t.Fatalf("by method: cash %d card %d", d.Cash, d.Card)
	}
	if d.Refunds != given || d.Net != collected-given {
		t.Fatalf("refunds %d net %d, want %d and %d", d.Refunds, d.Net, given, collected-given)
	}
	if d.PrepSeconds <= 0 {
		t.Fatalf("the time to make an order: %d", d.PrepSeconds)
	}

	items, _ := r.store.BestSellers(r.ctx, vn, 10)
	// the cancelled order is not a sale of an item: toastie 2 + 1, coffee 1
	if len(items) != 2 || items[0].Ref != "toastie" || items[0].Quantity != 3 || items[0].Revenue != cashOrd.Total+refunded.Total || items[0].Orders != 2 || items[1].Ref != "coffee" || items[1].Quantity != 1 {
		t.Fatalf("best sellers: %+v", items)
	}
	if items[0].Name != "Toastie" || items[0].Kind != "item" {
		t.Fatalf("the item: %+v", items[0])
	}

	ings, _ := r.store.Ingredients(r.ctx, vn)
	byName := map[string]reports.IngredientRow{}
	for _, i := range ings {
		byName[i.Ingredient] = i
	}
	// bread: 4 for the first order, 2 + (-2) for the cancelled one, 2 for the refunded one
	if b := byName["bread"]; b.Used != 6 || b.Received != 100+100 || b.Cost != 120_000 {
		t.Fatalf("bread: %+v", b)
	}
	if c := byName["coffee-beans"]; c.Ingredient != "" {
		t.Fatalf("an ingredient the menu of the test does not use: %+v", c)
	}
	if b := byName["beans"]; b.Used != 20 {
		t.Fatalf("beans: %+v", b)
	}
	if m := byName["milk"]; m.Used != 150 || m.WrittenOff != 100 || m.Received != 5000 {
		t.Fatalf("milk: %+v", m)
	}
	if c := byName["cheese"]; c.Used != 80+40-40+40 {
		t.Fatalf("cheese: %+v", c)
	}

	refunds, _ := r.store.Refunds(r.ctx, vn)
	if len(refunds) != 2 || refunds[0].Kind != "refund" || refunds[0].Amount != 10_000 || refunds[0].Method != "card" || refunds[0].Reason != "a cold toastie" || refunds[0].Order != refunded.Number ||
		refunds[1].Kind != "cancel" || refunds[1].Amount != cancelled.Total || refunds[1].Method != "cash" || refunds[1].By != manager.ID {
		t.Fatalf("refunds: %+v", refunds)
	}

	hours, _ := r.store.Hours(r.ctx, vn)
	if len(hours) != 24 {
		t.Fatalf("hours: %d", len(hours))
	}
	var orders, total int64
	for _, h := range hours {
		orders += h.Orders
		total += h.Total
	}
	if orders != 4 || total != collected || hours[17].Orders != 4 { // 10:00 UTC is 17:00 in Ho Chi Minh City
		t.Fatalf("hours: %d orders, %d total, 17h: %+v", orders, total, hours[17])
	}

	cp, _ := r.store.Checkpoint(r.ctx, topicName)
	if cp.Next != uint64(r.log.Len()) || cp.AsOf == nil || cp.Skipped != 0 {
		t.Fatalf("checkpoint %+v, log has %d", cp, r.log.Len())
	}
}

func TestTheDayAnOrderBelongsToIsTheDayOfTheVenueNotOfUTC(t *testing.T) {
	r := newReportRig(t)
	r.stockAll()
	r.clk.t = time.Date(2026, 11, 20, 16, 50, 0, 0, time.UTC) // 23:50 in Ho Chi Minh City
	late := r.pay(r.place(customer, toastie(1)).ID)
	r.clk.t = time.Date(2026, 11, 20, 17, 10, 0, 0, time.UTC) // 00:10 the next day there
	early := r.pay(r.place(customer, toastie(2)).ID)
	r.follow(r.proj)
	days, _ := r.store.SalesByDay(r.ctx, vn)
	if days[1].Date != "2026-11-20" || days[1].Total != late.Total || days[2].Date != "2026-11-21" || days[2].Total != early.Total {
		t.Fatalf("days: %+v %+v %+v", days[0], days[1], days[2])
	}
	// the same orders by UTC are both on the 20th
	utc, _ := r.store.SalesByDay(r.ctx, reports.Period{From: "2026-11-20", To: "2026-11-20", Zone: "UTC"})
	if len(utc) != 1 || utc[0].Orders != 2 {
		t.Fatalf("by UTC: %+v", utc)
	}
	// the end of a period includes the whole of its last day
	one, _ := r.store.SalesByDay(r.ctx, reports.Period{From: "2026-11-21", To: "2026-11-21", Zone: "Asia/Ho_Chi_Minh"})
	if len(one) != 1 || one[0].Orders != 1 {
		t.Fatalf("the 21st: %+v", one)
	}
}

func TestAProjectorThatIsStoppedAndStartedAgainCountsEachRecordOnce(t *testing.T) {
	r := newReportRig(t)
	r.aDay()
	// read some of the log, then start another projector, as after a restart
	if _, err := r.proj.Step(r.ctx, 0); err != nil {
		t.Fatal(err)
	}
	second := NewProjector(r.store, r.log, topicName, 4, nil)
	r.follow(second)
	r.follow(r.proj) // and the first, which has nothing more to do
	days, _ := r.store.SalesByDay(r.ctx, vn)
	cp, _ := r.store.Checkpoint(r.ctx, topicName)
	if days[1].Orders != 4 || cp.Next != uint64(r.log.Len()) {
		t.Fatalf("%+v, checkpoint %+v", days[1], cp)
	}
	// the same batch written twice: the second is refused and changes nothing
	err := r.store.Apply(r.ctx, topicName, 0, 5, func(_ *reports.Tx) error { return nil })
	if err != reports.ErrOutOfStep {
		t.Fatalf("an old batch: %v", err)
	}
	ings, _ := r.store.Ingredients(r.ctx, vn)
	r.follow(second)
	again, _ := r.store.Ingredients(r.ctx, vn)
	if !reflect.DeepEqual(ings, again) {
		t.Fatal("following a log that has nothing new changed the figures")
	}
}

func TestRebuildingFromTheStartGivesTheSameReportsAsFollowingAsItHappened(t *testing.T) {
	r := newReportRig(t)
	r.aDay()
	r.follow(r.proj)
	snapshot := func() map[string]any {
		d, _ := r.store.SalesByDay(r.ctx, vn)
		i, _ := r.store.BestSellers(r.ctx, vn, 50)
		g, _ := r.store.Ingredients(r.ctx, vn)
		f, _ := r.store.Refunds(r.ctx, vn)
		h, _ := r.store.Hours(r.ctx, vn)
		return map[string]any{"days": d, "items": i, "ings": g, "refunds": f, "hours": h}
	}
	before := snapshot()
	if err := r.store.Reset(r.ctx, topicName); err != nil {
		t.Fatal(err)
	}
	if cp, _ := r.store.Checkpoint(r.ctx, topicName); cp.Next != 0 {
		t.Fatalf("a reset leaves a checkpoint: %+v", cp)
	}
	if d, _ := r.store.SalesByDay(r.ctx, vn); d[1].Orders != 0 {
		t.Fatalf("a reset leaves facts: %+v", d[1])
	}
	r.follow(NewProjector(r.store, r.log, topicName, 50, nil)) // another batch size, the same log
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a rebuild gives other reports:\n%+v\n%+v", before, after)
	}
}

func TestARecordThatCannotBeReadIsPassedOverAndCounted(t *testing.T) {
	r := newReportRig(t)
	r.stockAll()
	r.payCash(cashier, r.place(customer, toastie(1)).ID)
	for _, junk := range []string{`not json`, `{"order":{"id":"x"}}`, `{}`} {
		if _, err := r.log.Append(r.ctx, []byte(junk)); err != nil {
			t.Fatal(err)
		}
	}
	r.pay(r.place(customer, toastie(1)).ID)
	r.follow(r.proj)
	days, _ := r.store.SalesByDay(r.ctx, vn)
	cp, _ := r.store.Checkpoint(r.ctx, topicName)
	if days[1].Orders != 2 || cp.Skipped != 3 || cp.Next != uint64(r.log.Len()) {
		t.Fatalf("%+v %+v", days[1], cp)
	}
}

func TestTheSameEventTwiceInTheLogIsCountedOnce(t *testing.T) {
	r := newReportRig(t)
	r.stockAll()
	o := r.payCash(cashier, r.place(customer, toastie(1)).ID)
	entries, err := r.log.Read(r.ctx, 0, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	// write every record about the order again, as a retry of a command can
	for _, e := range entries {
		if rec, err := DecodeRecord(e.Value); err == nil && len(rec.Order) > 0 {
			if _, err := r.log.Append(r.ctx, e.Value); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.follow(r.proj)
	days, _ := r.store.SalesByDay(r.ctx, vn)
	ings, _ := r.store.Ingredients(r.ctx, vn)
	byName := map[string]int64{}
	for _, i := range ings {
		byName[i.Ingredient] = i.Used
	}
	if days[1].Orders != 1 || days[1].Total != o.Total || byName["bread"] != 2 {
		t.Fatalf("%+v used %v", days[1], byName)
	}
}

func TestAPeriodThatIsNotOneIsRefusedBeforeTheDatabaseIsAsked(t *testing.T) {
	r := newReportRig(t)
	for name, p := range map[string]reports.Period{
		"a date that is not":      {From: "yesterday", To: "2026-11-20", Zone: "UTC"},
		"a day that is not there": {From: "2026-02-30", To: "2026-03-01", Zone: "UTC"},
		"ending before it starts": {From: "2026-11-20", To: "2026-11-19", Zone: "UTC"},
		"too long":                {From: "2024-01-01", To: "2026-01-01", Zone: "UTC"},
		"a zone that is not one":  {From: "2026-11-01", To: "2026-11-02", Zone: "Mars/Olympus"},
		"no zone":                 {From: "2026-11-01", To: "2026-11-02"},
		"a date with a time":      {From: "2026-11-01T00:00", To: "2026-11-02", Zone: "UTC"},
	} {
		var inv *reports.InvalidError
		if _, err := r.store.SalesByDay(r.ctx, p); err == nil || !asInvalid(err, &inv) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a year is the most
	if _, err := r.store.SalesByDay(r.ctx, reports.Period{From: "2026-01-01", To: "2026-12-31", Zone: "UTC"}); err != nil {
		t.Errorf("a whole year: %v", err)
	}
}

func asInvalid(err error, target **reports.InvalidError) bool {
	e, ok := err.(*reports.InvalidError)
	if ok {
		*target = e
	}
	return ok
}

func TestAnOrderThatIsNotReadyUntilEveryStationIsDoneIsAverageTimedFromThePaymentToTheLastOne(t *testing.T) {
	r := newReportRig(t)
	r.stockAll()
	o := r.pay(r.place(customer, toastie(1), coffee(1, "s")).ID) // a hot station and a drinks station
	for _, step := range []func() error{
		func() error { _, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(o.ID), r.cmd(), "hot"); return err },
		func() error { _, err := r.svc.FinishTicket(r.ctx, hotCook, order.ID(o.ID), r.cmd(), "hot"); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	r.follow(r.proj)
	if d, _ := r.store.SalesByDay(r.ctx, vn); d[1].PrepSeconds != 0 {
		t.Fatalf("an order with a station still to do is counted as ready: %+v", d[1])
	}
	r.clk.t = r.clk.t.Add(90 * time.Second)
	if _, err := r.svc.StartTicket(r.ctx, drinksCook, order.ID(o.ID), r.cmd(), "drinks"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.FinishTicket(r.ctx, drinksCook, order.ID(o.ID), r.cmd(), "drinks"); err != nil {
		t.Fatal(err)
	}
	r.follow(r.proj)
	d, _ := r.store.SalesByDay(r.ctx, vn)
	if d[1].PrepSeconds < 90 || d[1].PrepSeconds > 100 {
		t.Fatalf("the time to make the order: %d seconds", d[1].PrepSeconds)
	}
}
