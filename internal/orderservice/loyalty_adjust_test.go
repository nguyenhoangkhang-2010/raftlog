package orderservice

import (
	"errors"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

func (r *rig) adjust(a rbac.Actor, c string, n int64, why string) (PointsAnswer, error) {
	r.t.Helper()
	return r.svc.AdjustPoints(r.ctx, a, r.cmd(), c, n, why)
}

func (r *rig) mustAdjust(c string, n int64, why string) PointsAnswer {
	r.t.Helper()
	v, err := r.adjust(manager, c, n, why)
	if err != nil {
		r.t.Fatalf("adjust %d: %v", n, err)
	}
	return v
}

// every balance is what was earned, less what was spent, plus what managers changed
func balanced(t *testing.T, p PointsView) {
	t.Helper()
	if p.Balance != p.Earned-p.Spent+p.Adjusted {
		t.Errorf("balance %d is not earned %d - spent %d + adjusted %d", p.Balance, p.Earned, p.Spent, p.Adjusted)
	}
}

func TestAManagerGivesAndTakesPointsAndTheReasonStaysWithTheChange(t *testing.T) {
	r := newPointsRig(t)
	ans := r.mustAdjust("cus-1", 50, "  late order, apologised  ")
	p := ans.PointsView
	if p.Balance != 50 || p.Available != 50 || p.Adjusted != 50 || p.Earned != 0 || p.Spent != 0 || len(p.History) != 1 {
		t.Fatalf("after giving 50: %+v", p)
	}
	e := p.History[0]
	if e.Kind != PointsAdjusted || e.Points != 50 || e.By != "mgr-1" || e.Reason != "late order, apologised" || e.Order != "" || e.Number != 0 {
		t.Fatalf("the entry: %+v", e)
	}
	r.mustAdjust("cus-1", -20, "given twice by mistake")
	p = r.points("cus-1")
	if p.Balance != 30 || p.Adjusted != 30 || len(p.History) != 2 || p.History[1].Points != -20 {
		t.Fatalf("after taking 20: %+v", p)
	}
	balanced(t, p)

	// points given by a manager are used like any others, and the balance still adds up
	o := r.mustPlaceWith(customer, 10, toastie(2)) // 90.000đ, may use up to 450
	r.payBy(o.ID)                                  // spends 10, earns 8
	p = r.points("cus-1")
	if p.Balance != 28 || p.Spent != 10 || p.Earned != 8 || p.Adjusted != 30 {
		t.Fatalf("after an order: %+v", p)
	}
	balanced(t, p)
	// the amounts of someone else are not touched
	if other := r.points("cus-2"); other.Balance != 0 || len(other.History) != 0 {
		t.Fatalf("somebody else's: %+v", other)
	}
}

func TestPointsThatAreSetAsideCannotBeTakenOff(t *testing.T) {
	r := newPointsRig(t)
	r.mustAdjust("cus-1", 40, "welcome")
	r.mustPlaceWith(customer, 25, toastie(2)) // held, not paid
	_, err := r.adjust(manager, "cus-1", -16, "mistake")
	var short *PointsShortError
	if !errors.As(err, &short) || short.Available != 15 {
		t.Fatalf("taking more than is available: %v", err)
	}
	if p := r.points("cus-1"); p.Balance != 40 || p.Held != 25 || len(p.History) != 1 {
		t.Fatalf("nothing was written: %+v", p)
	}
	if _, err := r.adjust(manager, "cus-1", -15, "mistake"); err != nil {
		t.Fatalf("taking what is available: %v", err)
	}
	// a customer who has nothing can be given points, and cannot have any taken off
	if _, err := r.adjust(manager, "cus-9", -1, "no"); !errors.As(err, &short) || short.Available != 0 {
		t.Fatalf("a customer with nothing: %v", err)
	}
}

func TestWhoMayChangePointsAndWhat(t *testing.T) {
	r := newPointsRig(t)
	for name, who := range map[string]rbac.Actor{"customer": customer, "cashier": cashier, "kitchen": hotCook, "runner": runner, "storekeeper": storekeeper} {
		if _, err := r.adjust(who, "cus-2", 5, "why"); !errors.Is(err, order.ErrNotAllowed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.adjust(manager, "mgr-1", 5, "myself"); !errors.Is(err, order.ErrNotAllowed) {
		t.Errorf("a manager's own points: %v", err)
	}
	for name, c := range map[string]struct {
		cust string
		n    int64
		why  string
		want error
	}{
		"zero":          {"cus-1", 0, "why", order.ErrInvalid},
		"too many":      {"cus-1", MaxAdjust + 1, "why", order.ErrInvalid},
		"too many less": {"cus-1", -MaxAdjust - 1, "why", order.ErrInvalid},
		"no reason":     {"cus-1", 5, "  ", order.ErrReasonRequired},
		"long reason":   {"cus-1", 5, string(make([]rune, order.MaxReasonRunes+1)), order.ErrReasonRequired},
		"nobody":        {"", 5, "why", order.ErrInvalid},
	} {
		if _, err := r.adjust(manager, c.cust, c.n, c.why); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := r.svc.AdjustPoints(r.ctx, manager, "bad id", "cus-1", 5, "why"); !errors.Is(err, ErrBadCommandID) {
		t.Errorf("a bad command ID: %v", err)
	}
	if _, err := r.adjust(manager, "CUS-1 ", MaxAdjust, "the most"); err != nil {
		t.Errorf("the most, and the login in any case: %v", err)
	}
	if p := r.points("cus-1"); p.Balance != MaxAdjust || len(p.History) != 1 {
		t.Errorf("only the good change was written: %+v", p)
	}
	// without a program there is nothing to change
	off := newRigWith(t, Loyalty{})
	if _, err := off.adjust(manager, "cus-1", 5, "why"); !errors.Is(err, ErrNoPoints) {
		t.Errorf("no program: %v", err)
	}
}

func TestAChangeOfPointsSentTwiceIsMadeOnce(t *testing.T) {
	r := newPointsRig(t)
	cmd := r.cmd()
	for i := 0; i < 3; i++ {
		ans, err := r.svc.AdjustPoints(r.ctx, manager, cmd, "cus-1", 7, "once")
		if err != nil || ans.Balance != 7 {
			t.Fatalf("send %d: %+v %v", i+1, ans, err)
		}
	}
	if p := r.points("cus-1"); p.Balance != 7 || len(p.History) != 1 {
		t.Fatalf("%+v", p)
	}
	// even by a service that reads the log again
	again := r.open(r.log)
	if ans, err := again.AdjustPoints(r.ctx, manager, cmd, "cus-1", 7, "once"); err != nil || ans.Balance != 7 {
		t.Fatalf("after a restart: %+v %v", ans, err)
	}
}

func TestChangesOfPointsSurviveARestartAndAreTheSameForEveryReader(t *testing.T) {
	r := newPointsRig(t)
	r.mustAdjust("cus-1", 100, "welcome")
	r.payBy(r.place(customer, toastie(2)).ID) // earns 9
	r.mustAdjust("cus-1", -30, "correction")
	before := r.points("cus-1")
	r.loyalty = Loyalty{EarnPer: 1_000, Value: 1_000, MaxSharePercent: 90} // the settings of the day do not matter
	after := r.open(r.log).PointsOf("cus-1").PointsView
	if after.Balance != 79 || after.Adjusted != 70 || after.Balance != before.Balance || len(after.History) != 3 {
		t.Fatalf("before %+v, after %+v", before, after)
	}
	balanced(t, after)
	for i, e := range after.History {
		if e.By != before.History[i].By || e.Reason != before.History[i].Reason {
			t.Errorf("entry %d: %+v", i, e)
		}
	}
}

func TestAChangeThatTheLogRefusesIsRefusedByEveryReader(t *testing.T) {
	r := newPointsRig(t)
	r.mustAdjust("cus-1", 10, "welcome")
	// somebody writes straight to the log a change that takes more than the customer has
	forged := PointsMove{ID: "forged-change-1", By: "mgr-1", At: r.clk.now(), Customer: "cus-1", Points: -500, Reason: "forged"}
	data, err := Record{Points: &forged}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.log.Append(r.ctx, data); err != nil {
		t.Fatal(err)
	}
	for name, svc := range map[string]*Service{"the first": r.svc, "a new one": r.open(r.log)} {
		if err := svc.barrier(r.ctx); err != nil {
			t.Fatal(err)
		}
		if p := svc.PointsOf("cus-1"); p.Balance != 10 || len(p.History) != 1 {
			t.Errorf("%s: %+v", name, p.PointsView)
		}
		if svc.Health().Skipped != 1 {
			t.Errorf("%s: skipped %d", name, svc.Health().Skipped)
		}
	}
}

func TestTheRecordOfAChangeIsChecked(t *testing.T) {
	good := PointsMove{ID: "c-1", By: "mgr-1", At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), Customer: "cus-1", Points: 5, Reason: "why"}
	if _, err := (Record{Points: &good}).Encode(); err != nil {
		t.Fatal(err)
	}
	roundTrip := func(m PointsMove) error {
		data, err := Record{Points: &m}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		_, err = DecodeRecord(data)
		return err
	}
	if err := roundTrip(good); err != nil {
		t.Fatalf("a good change: %v", err)
	}
	for name, mut := range map[string]func(*PointsMove){
		"no ID":       func(m *PointsMove) { m.ID = "" },
		"no author":   func(m *PointsMove) { m.By = "" },
		"no time":     func(m *PointsMove) { m.At = time.Time{} },
		"no customer": func(m *PointsMove) { m.Customer = "" },
		"zero":        func(m *PointsMove) { m.Points = 0 },
		"too many":    func(m *PointsMove) { m.Points = order.MaxPoints + 1 },
		"no reason":   func(m *PointsMove) { m.Reason = " " },
	} {
		m := good
		mut(&m)
		if err := roundTrip(m); !errors.Is(err, ErrBadRecord) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a record is one thing
	if _, err := (Record{Points: &good, Barrier: "b"}).Encode(); err == nil {
		t.Error("a change and a barrier in one record")
	}
}

func TestPointsOfAnOrderAndPointsOfAChangeAreKeptApart(t *testing.T) {
	r := newPointsRig(t)
	r.mustAdjust("cus-1", 3, "welcome")
	paid := r.payBy(r.place(customer, toastie(1)).ID) // 45.000đ earns 4
	p := r.points("cus-1")
	if got := kinds(p.History); len(got) != 2 || got[0] != PointsAdjusted || got[1] != PointsEarned {
		t.Fatalf("kinds %v", got)
	}
	if p.History[1].Number != paid.Number || p.History[1].By != "" || p.History[1].Reason != "" {
		t.Fatalf("an order's entry: %+v", p.History[1])
	}
}
