package orderservice

import (
	"net/http/httptest"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/shifts"
)

type shiftRig struct {
	*accRig
	store *shifts.Store
}

func newShiftRig(t *testing.T, require bool) *shiftRig {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctxBackground, pool, accounts.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctxBackground, pool, shifts.Migrations(), pgmigrate.WithTable(shifts.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	acc, err := accounts.New(pool, accounts.Config{Hash: accounts.HashParams{Memory: 8, Iterations: 1, Parallelism: 1}})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	store := shifts.New(pool, r.clk.now) // the same clock as the service, which the tests set in the future
	r.stockAll()
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, WithAccounts(acc, AccountsOptions{}), WithShifts(store, require)))
	t.Cleanup(srv.Close)
	return &shiftRig{accRig: &accRig{rig: r, acc: acc, pool: pool, srv: srv}, store: store}
}

type shiftBody struct {
	Shift struct {
		ID           int64
		Till         string
		Cashier      string
		Status       string
		OpeningTotal int64  `json:"opening_total"`
		ClosingTotal *int64 `json:"closing_total"`
		Sales        *int64
		Refunds      *int64
		PayIn        *int64 `json:"pay_in"`
		PayOut       *int64 `json:"pay_out"`
		Expected     *int64
		Variance     *int64
		ClosedBy     string `json:"closed_by"`
		ReviewedBy   string `json:"reviewed_by"`
		Movements    []struct {
			Kind   string
			Amount int64
		}
	}
	Cash *struct {
		Sales   int64
		Refunds int64
		Entries []struct {
			Number int
			Kind   string
			Amount int64
		}
	}
	ExpectedSoFar *int64 `json:"expected_so_far"`
}

func (a *shiftRig) people() map[string]*browser {
	a.user("cash-1", "", rbac.Cashier)
	a.user("cash-2", "", rbac.Cashier)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cook-1", "drinks", rbac.Kitchen)
	a.user("store-1", "", rbac.Storekeeper)
	a.user("cus-1", "", rbac.Customer)
	out := map[string]*browser{}
	for _, id := range []string{"cash-1", "cash-2", "mgr-1", "cook-1", "store-1", "cus-1"} {
		out[id] = a.browser()
		out[id].signIn(id)
	}
	return out
}

func (a *shiftRig) payCashAs(b *browser, amountOfToasties int) int64 {
	a.t.Helper()
	o := a.place(customer, toastie(amountOfToasties))
	if code := b.status("POST", "/api/orders/"+o.ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "cash", "tendered": o.Total + 5000}); code != 200 {
		a.t.Fatalf("pay in cash: %d", code)
	}
	return o.Total
}

func TestOnlyCashiersAndManagersWorkShiftsAndOnlyTheOwnerOfOneMovesMoneyInIt(t *testing.T) {
	a := newShiftRig(t, false)
	p := a.people()
	open := map[string]any{"till": "main", "counts": map[string]int{"100000": 5}}
	for id, want := range map[string]int{"cook-1": 403, "store-1": 403, "cus-1": 403} {
		if code := p[id].status("POST", "/api/shifts", open); code != want {
			t.Errorf("%s opening a shift: %d, want %d", id, code, want)
		}
		if code := p[id].status("GET", "/api/shifts", nil); code != 403 {
			t.Errorf("%s listing shifts: %d", id, code)
		}
		if code := p[id].status("GET", "/api/shifts/current", nil); code != 403 {
			t.Errorf("%s reading the current shift: %d", id, code)
		}
	}
	anon := a.browser()
	if code := anon.status("GET", "/api/shifts", nil); code != 401 {
		t.Errorf("without signing in: %d", code)
	}
	var sh shiftBody
	if code, _ := p["cash-1"].do("POST", "/api/shifts", open, &sh); code != 201 || sh.Shift.OpeningTotal != 500_000 || sh.Shift.Status != "open" || sh.Shift.Cashier != "cash-1" {
		t.Fatalf("a cashier opening a shift: %d %+v", code, sh)
	}
	base := "/api/shifts/" + itoa(sh.Shift.ID)
	mv := map[string]any{"kind": "out", "amount": 30000, "reason": "ice"}
	if code := p["cash-1"].status("POST", base+"/movements", mv); code != 200 {
		t.Fatalf("the cashier records money out: %d", code)
	}
	for _, id := range []string{"cash-2", "mgr-1"} {
		if code := p[id].status("POST", base+"/movements", mv); code != 403 {
			t.Errorf("%s recording money in the drawer of another: %d", id, code)
		}
	}
	// cash-2 does not see the shift of cash-1 at all, and a manager sees it
	if code := p["cash-2"].status("GET", base, nil); code != 403 {
		t.Errorf("another cashier reading the shift: %d", code)
	}
	if code := p["mgr-1"].status("GET", base, nil); code != 200 {
		t.Errorf("the manager reading the shift: %d", code)
	}
	// a manager may open a shift of their own
	if code := p["mgr-1"].status("POST", "/api/shifts", map[string]any{"till": "back", "counts": map[string]int{}}); code != 201 {
		t.Errorf("a manager opening a shift: %d", code)
	}
	var list struct{ Shifts []struct{ Cashier string } }
	p["cash-1"].do("GET", "/api/shifts", nil, &list)
	if len(list.Shifts) != 1 || list.Shifts[0].Cashier != "cash-1" {
		t.Fatalf("a cashier's list: %+v", list)
	}
	p["mgr-1"].do("GET", "/api/shifts", nil, &list)
	if len(list.Shifts) != 2 {
		t.Fatalf("the manager's list: %+v", list)
	}
	if code := p["mgr-1"].status("GET", "/api/shifts?status=sleeping", nil); code != 400 {
		t.Errorf("a status that is not one: %d", code)
	}
}

func TestACashierCountsBlindAndSeesTheFiguresOnlyWhenTheShiftIsClosed(t *testing.T) {
	a := newShiftRig(t, false)
	p := a.people()
	var sh shiftBody
	p["cash-1"].do("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{"100000": 7}}, &sh) // 700,000
	base := "/api/shifts/" + itoa(sh.Shift.ID)
	total := a.payCashAs(p["cash-1"], 1) + a.payCashAs(p["cash-1"], 2)
	// a card payment is not in the drawer
	o := a.place(customer, toastie(1))
	if code := p["cash-1"].status("POST", "/api/orders/"+o.ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "card", "reference": "slip"}); code != 200 {
		t.Fatalf("card: %d", code)
	}
	if code := p["cash-1"].status("POST", base+"/movements", map[string]any{"kind": "out", "amount": 50000, "reason": "ice"}); code != 200 {
		t.Fatalf("movement: %d", code)
	}
	var open shiftBody
	p["cash-1"].do("GET", base, nil, &open)
	if open.Cash != nil || open.ExpectedSoFar != nil || open.Shift.Expected != nil {
		t.Fatalf("a cashier sees what the drawer should hold before closing: %+v", open)
	}
	var seen shiftBody
	p["mgr-1"].do("GET", base, nil, &seen)
	want := 700_000 + total - 50_000
	if seen.ExpectedSoFar == nil || *seen.ExpectedSoFar != want || seen.Cash == nil || seen.Cash.Sales != total || len(seen.Cash.Entries) != 2 {
		t.Fatalf("a manager sees the figures of a shift that is open: %+v (want %d)", seen, want)
	}
	// the current shift is the same, without figures
	var cur struct {
		Shift         *struct{ ID int64 }
		Denominations []int64
		Required      bool
	}
	p["cash-1"].do("GET", "/api/shifts/current", nil, &cur)
	if cur.Shift == nil || cur.Shift.ID != sh.Shift.ID || len(cur.Denominations) != 11 || cur.Required {
		t.Fatalf("current: %+v", cur)
	}
	// 20,000 short: the count is 20,000 less than what the drawer should hold
	short := want - 20_000
	counts := map[string]int64{}
	rest := short
	for _, d := range []int64{500_000, 200_000, 100_000, 50_000, 20_000, 10_000, 5_000, 2_000, 1_000, 500, 200} {
		counts[itoa(d)], rest = rest/d, rest%d
	}
	if rest != 0 {
		t.Fatalf("the test cannot count %d in notes", short)
	}
	var closed shiftBody
	if code, _ := p["cash-1"].do("POST", base+"/close", map[string]any{"counts": counts, "note": "a customer said they paid 50,000"}, &closed); code != 200 {
		t.Fatalf("close: %d", code)
	}
	s := closed.Shift
	if s.Status != "closed" || *s.ClosingTotal != short || *s.Expected != want || *s.Variance != -20_000 || *s.Sales != total || *s.Refunds != 0 || *s.PayOut != 50_000 || *s.PayIn != 0 {
		t.Fatalf("closed: %+v", s)
	}
	if closed.Cash == nil || closed.Cash.Sales != total || len(closed.Cash.Entries) != 2 {
		t.Fatalf("after closing the cashier sees where it came from: %+v", closed.Cash)
	}
	// closed is closed
	if code := p["cash-1"].status("POST", base+"/close", map[string]any{"counts": counts}); code != 409 {
		t.Errorf("closing twice: %d", code)
	}
	if code := p["cash-1"].status("POST", base+"/movements", map[string]any{"kind": "in", "amount": 10, "reason": "x"}); code != 409 {
		t.Errorf("money in a closed shift: %d", code)
	}
}

func TestCashAfterTheShiftClosedIsNotInIt(t *testing.T) {
	a := newShiftRig(t, false)
	p := a.people()
	var sh shiftBody
	p["cash-1"].do("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{"10000": 10}}, &sh)
	before := a.payCashAs(p["cash-1"], 1)
	var closed shiftBody
	p["cash-1"].do("POST", "/api/shifts/"+itoa(sh.Shift.ID)+"/close", map[string]any{"counts": map[string]int{"10000": 10}}, &closed)
	if *closed.Shift.Sales != before || *closed.Shift.Variance != -before {
		t.Fatalf("the shift: %+v", closed.Shift)
	}
	a.payCashAs(p["cash-1"], 3) // another payment after: no shift of its own
	var again shiftBody
	p["cash-1"].do("GET", "/api/shifts/"+itoa(sh.Shift.ID), nil, &again)
	if again.Cash.Sales != before || len(again.Cash.Entries) != 1 {
		t.Fatalf("a closed shift has taken in cash that came after it: %+v", again.Cash)
	}
}

func TestAShiftThatWasLeftOpenIsClosedByAManagerAndReviewedOnce(t *testing.T) {
	a := newShiftRig(t, false)
	p := a.people()
	var sh shiftBody
	p["cash-1"].do("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{"50000": 4}}, &sh)
	base := "/api/shifts/" + itoa(sh.Shift.ID)
	if code := p["cash-2"].status("POST", base+"/close", map[string]any{"counts": map[string]int{}}); code != 403 {
		t.Fatalf("another cashier closing it: %d", code)
	}
	if code := p["cash-1"].status("POST", base+"/review", map[string]any{"note": "x"}); code != 403 {
		t.Fatalf("a cashier reviewing: %d", code)
	}
	if code := p["mgr-1"].status("POST", base+"/review", map[string]any{"note": "x"}); code != 409 {
		t.Fatalf("reviewing a shift that is still open: %d", code)
	}
	var closed shiftBody
	if code, _ := p["mgr-1"].do("POST", base+"/close", map[string]any{"counts": map[string]int{"50000": 4}, "note": "closed by the manager: the cashier went home"}, &closed); code != 200 {
		t.Fatalf("the manager closing it: %d", code)
	}
	if closed.Shift.ClosedBy != "mgr-1" || closed.Shift.Cashier != "cash-1" || *closed.Shift.Variance != 0 {
		t.Fatalf("closed: %+v", closed.Shift)
	}
	var rv shiftBody
	if code, _ := p["mgr-1"].do("POST", base+"/review", map[string]any{"note": "ok"}, &rv); code != 200 || rv.Shift.ReviewedBy != "mgr-1" {
		t.Fatalf("review: %d %+v", code, rv)
	}
	if code := p["mgr-1"].status("POST", base+"/review", map[string]any{"note": "again"}); code != 409 {
		t.Fatalf("reviewing twice: %d", code)
	}
	// the cashier can open a new shift now
	if code := p["cash-1"].status("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{}}); code != 201 {
		t.Fatalf("a new shift: %d", code)
	}
}

func TestCountsThatAreNotMoneyAreRefusedAndAShiftIsNotOpenedTwice(t *testing.T) {
	a := newShiftRig(t, false)
	p := a.people()
	for name, body := range map[string]map[string]any{
		"a note that does not exist": {"till": "main", "counts": map[string]int{"30000": 1}},
		"a negative count":           {"till": "main", "counts": map[string]int{"10000": -2}},
		"a drawer with no name":      {"till": "", "counts": map[string]int{}},
		"a drawer with a bad name":   {"till": "Main Till", "counts": map[string]int{}},
		"an unknown field":           {"till": "main", "counts": map[string]int{}, "total": 5},
	} {
		if code := p["cash-1"].status("POST", "/api/shifts", body); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code := p["cash-1"].status("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{}}); code != 201 {
		t.Fatalf("a good one: %d", code)
	}
	if code := p["cash-1"].status("POST", "/api/shifts", map[string]any{"till": "other", "counts": map[string]int{}}); code != 409 {
		t.Errorf("a second shift of the same cashier: %d", code)
	}
	if code := p["cash-2"].status("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{}}); code != 409 {
		t.Errorf("the same drawer in two shifts: %d", code)
	}
	if code := p["cash-1"].status("GET", "/api/shifts/abc", nil); code != 400 {
		t.Errorf("a shift that is not a number: %d", code)
	}
	if code := p["cash-1"].status("GET", "/api/shifts/9999", nil); code != 404 {
		t.Errorf("a shift that is not there: %d", code)
	}
}

func TestWhenAShiftIsRequiredCashNeedsOneAndCardDoesNot(t *testing.T) {
	a := newShiftRig(t, true)
	p := a.people()
	cash := func() int {
		o := a.place(customer, toastie(1))
		return p["cash-1"].status("POST", "/api/orders/"+o.ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "cash", "tendered": o.Total})
	}
	card := func() int {
		o := a.place(customer, toastie(1))
		return p["cash-1"].status("POST", "/api/orders/"+o.ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "card", "reference": "slip"})
	}
	var e errAnswer
	o := a.place(customer, toastie(1))
	if code, _ := p["cash-1"].do("POST", "/api/orders/"+o.ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "cash", "tendered": o.Total}, &e); code != 409 || e.Code != "no_shift" {
		t.Fatalf("cash without a shift: %d %+v", code, e)
	}
	if code := card(); code != 200 {
		t.Fatalf("card without a shift: %d", code)
	}
	var cur struct{ Required bool }
	p["cash-1"].do("GET", "/api/shifts/current", nil, &cur)
	if !cur.Required {
		t.Fatal("the screen is not told that a shift is required")
	}
	p["cash-1"].status("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{}})
	if code := cash(); code != 200 {
		t.Fatalf("cash with a shift: %d", code)
	}
	// another cashier's shift does not help this one
	if code, _ := p["cash-2"].do("POST", "/api/orders/"+a.place(customer, toastie(1)).ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "cash", "tendered": 100000}, &e); code != 409 {
		t.Fatalf("cash by a cashier with no shift of their own: %d", code)
	}
}

func TestWithoutTheRequirementCashCanBeTakenWithNoShift(t *testing.T) {
	a := newShiftRig(t, false)
	p := a.people()
	o := a.place(customer, toastie(1))
	if code := p["cash-1"].status("POST", "/api/orders/"+o.ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "cash", "tendered": o.Total}); code != 200 {
		t.Fatalf("cash with no shift and no requirement: %d", code)
	}
}
