package orderservice

import (
	"errors"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// A point is earned by every 10.000đ paid, and takes 100đ off; at most half an order is paid with points.
var program = Loyalty{EarnPer: 10_000, Value: 100}

func newPointsRig(t *testing.T) *rig {
	t.Helper()
	r := newRigWith(t, program)
	r.stockAll()
	return r
}

func (r *rig) placeWith(a rbac.Actor, points int64, sels ...menu.Selection) (OrderView, error) {
	r.t.Helper()
	p, err := r.svc.Place(r.ctx, a, PlaceRequest{Command: r.cmd(), Selections: sels, Points: points})
	return p.Order, err
}

func (r *rig) mustPlaceWith(a rbac.Actor, points int64, sels ...menu.Selection) OrderView {
	r.t.Helper()
	v, err := r.placeWith(a, points, sels...)
	if err != nil {
		r.t.Fatalf("place with %d points: %v", points, err)
	}
	return v
}

// payBy pays an order by card, as the cashier does.
func (r *rig) payBy(id string) OrderView { return r.pay(id) }

func (r *rig) finish(id string) {
	r.t.Helper()
	for _, step := range []func() error{
		func() error { _, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(id), r.cmd(), "hot"); return err },
		func() error { _, err := r.svc.FinishTicket(r.ctx, hotCook, order.ID(id), r.cmd(), "hot"); return err },
		func() error { _, err := r.svc.HandOver(r.ctx, runner, order.ID(id), r.cmd()); return err },
	} {
		if err := step(); err != nil {
			r.t.Fatalf("finish %s: %v", id, err)
		}
	}
}

func (r *rig) points(c string) PointsView { return r.svc.PointsOf(c).PointsView }

func kinds(h []PointsEntry) []string {
	var out []string
	for _, e := range h {
		out = append(out, e.Kind)
	}
	return out
}

func TestTheProgramIsChecked(t *testing.T) {
	for name, l := range map[string]Loyalty{
		"negative earn":      {EarnPer: -1, Value: 100},
		"negative value":     {EarnPer: 1000, Value: -1},
		"earn without value": {EarnPer: 1000},
		"value without earn": {Value: 100},
		"share over 100":     {EarnPer: 1000, Value: 100, MaxSharePercent: 101},
		"negative share":     {EarnPer: 1000, Value: 100, MaxSharePercent: -1},
	} {
		if err := l.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (Loyalty{}).Validate(); err != nil {
		t.Errorf("no program is allowed: %v", err)
	}
	if (Loyalty{}).On() || !program.On() {
		t.Error("On")
	}
	if n := program.Earned(25_000); n != 2 {
		t.Errorf("25.000đ earned %d points, want 2 (whole points only)", n)
	}
	if n := program.Earned(9_999); n != 0 {
		t.Errorf("9.999đ earned %d", n)
	}
	if n := (Loyalty{}).Earned(1_000_000); n != 0 {
		t.Errorf("no program earned %d", n)
	}
	// half of 90.000đ is 45.000đ, which is 450 points of 100đ; the customer has fewer, or more
	if n := program.MaxUsable(90_000, 1000); n != 450 {
		t.Errorf("the most an order of 90.000đ may use: %d, want 450", n)
	}
	if n := program.MaxUsable(90_000, 30); n != 30 {
		t.Errorf("a customer with 30 points: %d", n)
	}
	if n := (Loyalty{EarnPer: 1000, Value: 100, MaxSharePercent: 100}).MaxUsable(90_000, 1000); n != 900 {
		t.Errorf("the whole order may be paid with points when the venue says so: %d", n)
	}
	if n := program.MaxUsable(0, 10); n != 0 {
		t.Errorf("an order of nothing: %d", n)
	}
}

func TestAPaymentEarnsPointsForTheCustomerOfTheOrder(t *testing.T) {
	r := newPointsRig(t)
	o := r.place(customer, toastie(2)) // 90.000đ
	if p := r.points("cus-1"); p.Balance != 0 || p.Earned != 0 {
		t.Fatalf("before paying: %+v", p)
	}
	paid := r.payBy(o.ID)
	if paid.PointsEarned != 9 {
		t.Fatalf("the order says it earned %d", paid.PointsEarned)
	}
	p := r.points("cus-1")
	if p.Balance != 9 || p.Available != 9 || p.Held != 0 || p.Earned != 9 || p.Spent != 0 || len(p.History) != 1 || p.History[0].Kind != PointsEarned || p.History[0].Points != 9 || p.History[0].Number != o.Number {
		t.Fatalf("points: %+v", p)
	}
	if other := r.points("cus-2"); other.Balance != 0 || len(other.History) != 0 {
		t.Fatalf("somebody else's: %+v", other)
	}
	// an order of the counter with no customer earns nothing, and with a customer it earns for them
	walkIn, err := r.svc.Place(r.ctx, cashier, PlaceRequest{Command: r.cmd(), Selections: []menu.Selection{toastie(2)}})
	if err != nil {
		t.Fatal(err)
	}
	if v := r.payBy(walkIn.Order.ID); v.PointsEarned != 0 {
		t.Fatalf("a walk-in earned %d", v.PointsEarned)
	}
	named, err := r.svc.Place(r.ctx, cashier, PlaceRequest{Command: r.cmd(), Selections: []menu.Selection{toastie(1)}, Customer: "cus-2"}) // 45.000đ
	if err != nil {
		t.Fatal(err)
	}
	r.payBy(named.Order.ID)
	if other := r.points("cus-2"); other.Balance != 4 {
		t.Fatalf("a customer named at the counter: %+v", other)
	}
}

func TestPointsTakeMoneyOffAndAreHeldUntilThePaymentSpendsThem(t *testing.T) {
	r := newPointsRig(t)
	r.payBy(r.place(customer, toastie(2)).ID) // earns 9
	o := r.mustPlaceWith(customer, 5, toastie(2))
	if o.Total != 89_500 || o.Discount != 500 || o.Points == nil || o.Points.Used != 5 || o.Points.Value != 100 {
		t.Fatalf("the order: %+v", o)
	}
	if len(o.Discounts) != 1 || o.Discounts[0].ID != "points" || o.Discounts[0].Amount != 500 {
		t.Fatalf("discounts: %+v", o.Discounts)
	}
	// held, not spent: the balance is the same, what is available is less
	p := r.points("cus-1")
	if p.Balance != 9 || p.Held != 5 || p.Available != 4 || p.Spent != 0 {
		t.Fatalf("while held: %+v", p)
	}
	// the points that are held cannot be used again
	_, err := r.placeWith(customer, 5, toastie(2))
	var limit *PointsLimitError
	if !errors.As(err, &limit) || limit.Max != 4 {
		t.Fatalf("a second order with the same points: %v", err)
	}
	if _, err := r.placeWith(customer, 4, toastie(2)); err != nil {
		t.Fatalf("with what is left: %v", err)
	}
	// paying spends them and earns again from what was paid (89.500đ: 8 points)
	paid := r.payBy(o.ID)
	if paid.PointsEarned != 8 || paid.Payment.Amount != 89_500 {
		t.Fatalf("paid: %+v", paid)
	}
	p = r.points("cus-1")
	if p.Balance != 12 || p.Held != 4 || p.Available != 8 || p.Earned != 17 || p.Spent != 5 {
		t.Fatalf("after the payment: %+v", p)
	}
	if got := kinds(p.History); len(got) != 3 || got[0] != PointsEarned || got[1] != PointsRedeemed || got[2] != PointsEarned {
		t.Fatalf("history: %v", got)
	}
}

func TestWhoMayUsePointsAndHowManyOfThem(t *testing.T) {
	r := newPointsRig(t)
	r.payBy(r.place(customer, toastie(2)).ID) // 9 points
	// the cashier does not spend a customer's points
	_, err := r.svc.Place(r.ctx, cashier, PlaceRequest{Command: r.cmd(), Selections: []menu.Selection{toastie(2)}, Customer: "cus-1", Points: 5})
	if !errors.Is(err, ErrNoPoints) {
		t.Fatalf("points at the counter: %v", err)
	}
	// nor a negative number, nor more than there are
	if _, err := r.placeWith(customer, -3, toastie(2)); !errors.Is(err, ErrNoPoints) {
		t.Fatalf("negative: %v", err)
	}
	var limit *PointsLimitError
	if _, err := r.placeWith(customer, 10, toastie(2)); !errors.As(err, &limit) || limit.Max != 9 {
		t.Fatalf("more than there are: %v", err)
	}
	// a customer with none cannot use any
	if _, err := r.placeWith(otherCust, 1, toastie(2)); !errors.As(err, &limit) || limit.Max != 0 {
		t.Fatalf("a customer with none: %v", err)
	}
	// and, when the customer has plenty, not more than half of the order: a customer with 900 points (90.000đ at
	// 100đ a point earned 1 point per 100đ) orders one toastie of 45.000đ, half of which is 22.500đ, 225 points
	rich := newRigWith(t, Loyalty{EarnPer: 100, Value: 100})
	rich.stockAll()
	rich.payBy(rich.place(customer, toastie(2)).ID)
	if _, err := rich.placeWith(customer, 226, toastie(1)); !errors.As(err, &limit) || limit.Max != 225 {
		t.Fatalf("limited by half of the order: %v", err)
	}
	v, err := rich.placeWith(customer, 225, toastie(1))
	if err != nil || v.Total != 22_500 {
		t.Fatalf("exactly half: %+v %v", v, err)
	}
}

func TestWithoutAProgramPointsAreNotEarnedOrUsed(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	o := r.place(customer, toastie(2))
	if v := r.pay(o.ID); v.PointsEarned != 0 {
		t.Fatalf("earned %d with no program", v.PointsEarned)
	}
	if _, err := r.placeWith(customer, 1, toastie(1)); !errors.Is(err, ErrNoPoints) {
		t.Fatalf("points with no program: %v", err)
	}
	if ans := r.svc.PointsOf("cus-1"); ans.Enabled || ans.EarnPer != 0 || ans.Value != 0 {
		t.Fatalf("answer: %+v", ans)
	}
	if ans := newPointsRig(t).svc.PointsOf("cus-1"); !ans.Enabled || ans.EarnPer != 10_000 || ans.Value != 100 || ans.MaxShare != 50 {
		t.Fatalf("the program: %+v", ans)
	}
}

func TestAnOrderThatIsNotPaidLetsGoOfItsPoints(t *testing.T) {
	r := newPointsRig(t)
	r.payBy(r.place(customer, toastie(2)).ID) // 9 points
	o := r.mustPlaceWith(customer, 9, toastie(2))
	if p := r.points("cus-1"); p.Available != 0 || p.Held != 9 {
		t.Fatalf("held: %+v", p)
	}
	if _, err := r.svc.CancelUnpaid(r.ctx, customer, order.ID(o.ID), r.cmd(), "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if p := r.points("cus-1"); p.Available != 9 || p.Held != 0 || p.Balance != 9 || len(p.History) != 1 {
		t.Fatalf("after cancelling: %+v", p)
	}
	// and the same points can be used again
	if _, err := r.placeWith(customer, 9, toastie(2)); err != nil {
		t.Fatal(err)
	}
}

func TestCancellingAPaidOrderGivesBackWhatItUsedAndTakesBackWhatItEarned(t *testing.T) {
	r := newPointsRig(t)
	r.payBy(r.place(customer, toastie(2)).ID) // 9 points
	o := r.mustPlaceWith(customer, 5, toastie(2))
	r.payBy(o.ID) // balance 9 - 5 + 8 = 12
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(o.ID), r.cmd(), "wrong order"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(o.ID), r.cmd(), money.Money(89_500)); err != nil {
		t.Fatal(err)
	}
	p := r.points("cus-1")
	// 5 points back, 8 taken back: where the customer began
	if p.Balance != 9 || p.Held != 0 || p.Earned != 9 || p.Spent != 0 {
		t.Fatalf("after the cancellation: %+v", p)
	}
	if got := kinds(p.History); len(got) != 5 || got[3] != PointsReturned || got[4] != PointsClawedBack {
		t.Fatalf("history: %v", got)
	}
}

func TestARefundAfterTheHandOverTakesBackTheShareThatItEarnedAndGivesNothingBack(t *testing.T) {
	r := newPointsRig(t)
	r.payBy(r.place(customer, toastie(2)).ID) // 9 points
	o := r.mustPlaceWith(customer, 5, toastie(2))
	r.payBy(o.ID) // earned 8, spent 5: balance 12
	r.finish(o.ID)
	// half of the money back: half of 8 points taken back
	if _, err := r.svc.IssueRefund(r.ctx, manager, order.ID(o.ID), r.cmd(), money.Money(44_750), "cold"); err != nil {
		t.Fatal(err)
	}
	if p := r.points("cus-1"); p.Balance != 8 || p.Spent != 5 {
		t.Fatalf("after half: %+v", p)
	}
	// the rest: the other half, so that all 8 are taken back and no more
	if _, err := r.svc.IssueRefund(r.ctx, manager, order.ID(o.ID), r.cmd(), money.Money(44_750), "still cold"); err != nil {
		t.Fatal(err)
	}
	p := r.points("cus-1")
	if p.Balance != 4 || p.Earned != 9 || p.Spent != 5 {
		t.Fatalf("after all of it: %+v (9 - 5 + 8 - 8)", p)
	}
}

func TestAnOrderRefusedByTheLogIsRefusedByEveryReader(t *testing.T) {
	r := newPointsRig(t)
	// somebody writes straight to the log an order that uses points that the customer does not have
	lines, errs := r.svc.st.menu.PriceCart([]menu.Selection{toastie(2)}, menu.PriceOptions{At: r.clk.now()})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	ev, err := order.Place(order.Meta{Cmd: "forged-points-1", Actor: customer, At: r.clk.now()}, order.PlaceInput{
		Order: "ord-forged-points-1", Number: 50, Channel: order.ChannelApp, Customer: "cus-1", Lines: lines,
		Pricing: r.svc.st.promos.Evaluate(lines, promo.Context{At: r.clk.now()}),
		Points:  &order.PointsUse{Used: 500, Value: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := OrderRecord(ev)
	if err != nil {
		t.Fatal(err)
	}
	data, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.log.Append(r.ctx, data); err != nil {
		t.Fatal(err)
	}
	// a service that reads the log skips it, and so does one that is opened on it later
	again := r.open(r.log)
	for name, svc := range map[string]*Service{"the first": r.svc, "a new one": again} {
		if err := svc.barrier(r.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Order(customer, "ord-forged-points-1"); err == nil {
			t.Errorf("%s: the order was kept", name)
		}
		if svc.Health().Skipped != 1 {
			t.Errorf("%s: skipped %d", name, svc.Health().Skipped)
		}
	}
}

func TestPointsSurviveARestartAndDoNotDependOnTheSettingsOfTheDay(t *testing.T) {
	r := newPointsRig(t)
	r.payBy(r.place(customer, toastie(2)).ID)
	o := r.mustPlaceWith(customer, 5, toastie(2))
	r.payBy(o.ID)
	before := r.points("cus-1")
	// the venue changes the program; what the log says does not change
	r.loyalty = Loyalty{EarnPer: 1_000, Value: 1_000, MaxSharePercent: 90}
	again := r.open(r.log)
	after := again.PointsOf("cus-1").PointsView
	if after.Balance != before.Balance || after.Earned != before.Earned || after.Spent != before.Spent || len(after.History) != len(before.History) {
		t.Fatalf("before %+v, after %+v", before, after)
	}
	view, err := again.Order(customer, order.ID(o.ID))
	if err != nil || view.Points == nil || view.Points.Value != 100 || view.Discount != 500 || view.PointsEarned != 8 {
		t.Fatalf("the order keeps what its points were worth: %+v %v", view, err)
	}
}

func TestTheHistoryOfPointsIsBounded(t *testing.T) {
	var a pointsAccount
	for i := 0; i < maxPointsHistory+25; i++ {
		a.note(PointsEntry{Kind: PointsEarned, Points: 1, Number: i})
	}
	if len(a.history) != maxPointsHistory || a.dropped != 25 || a.history[0].Number != 25 {
		t.Fatalf("history %d, dropped %d, first %d", len(a.history), a.dropped, a.history[0].Number)
	}
}

func TestShareOfPoints(t *testing.T) {
	for _, c := range []struct {
		n              int64
		refunded, paid money.Money
		want           int64
	}{
		{8, 0, 100, 0}, {8, 100, 100, 8}, {8, 150, 100, 8}, {8, 50, 100, 4}, {8, 33, 100, 2}, {0, 50, 100, 0}, {8, 50, 0, 0},
	} {
		if got := share(c.n, c.refunded, c.paid); got != c.want {
			t.Errorf("share(%d, %v, %v) = %d, want %d", c.n, c.refunded, c.paid, got, c.want)
		}
	}
}
