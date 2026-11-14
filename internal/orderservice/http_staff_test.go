package orderservice

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/expenses"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/shifts"
	staffing "github.com/nguyenhoangkhang-2010/raftlog/internal/staff"
)

// wallClock is a clock that a test sets; the clock of the rig moves on at every look, which a time clock
// must not.
type wallClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *wallClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *wallClock) set(day string, hh, mm int) {
	d, _ := time.Parse("2006-01-02", day)
	c.mu.Lock()
	c.t = time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, time.UTC)
	c.mu.Unlock()
}

type staffRig struct {
	*accRig
	store *staffing.Store
	clk   *wallClock
	shift *shifts.Store
}

func newStaffRig(t *testing.T, requireClockIn bool) *staffRig {
	t.Helper()
	pool := pgtest.Pool(t)
	ctx := ctxBackground
	if _, err := pgmigrate.Apply(ctx, pool, accounts.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctx, pool, shifts.Migrations(), pgmigrate.WithTable(shifts.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctx, pool, staffing.Migrations(), pgmigrate.WithTable(staffing.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctx, pool, reports.Migrations(), pgmigrate.WithTable(reports.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctx, pool, expenses.Migrations(), pgmigrate.WithTable(expenses.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	acc, err := accounts.New(pool, accounts.Config{Hash: accounts.HashParams{Memory: 8, Iterations: 1, Parallelism: 1}})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	clk := &wallClock{}
	clk.set("2026-11-20", 10, 0)
	store := staffing.New(pool, time.UTC, clk.now)
	sh := shifts.New(pool, r.clk.now)
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, WithAccounts(acc, AccountsOptions{}), WithShifts(sh, false), WithStaff(store, requireClockIn),
		WithReports(reports.New(pool), topicName, "UTC"), WithExpenses(expenses.New(pool, nil))))
	t.Cleanup(srv.Close)
	return &staffRig{accRig: &accRig{rig: r, acc: acc, pool: pool, srv: srv}, store: store, clk: clk, shift: sh}
}

func (a *staffRig) people() map[string]*browser {
	a.user("cash-1", "", rbac.Cashier)
	a.user("cash-2", "", rbac.Cashier)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cook-1", "drinks", rbac.Kitchen)
	a.user("store-1", "", rbac.Storekeeper)
	a.user("cus-1", "", rbac.Customer)
	a.user("adm-1", "", rbac.Admin)
	out := map[string]*browser{}
	for _, id := range []string{"cash-1", "cash-2", "mgr-1", "cook-1", "store-1", "cus-1", "adm-1"} {
		out[id] = a.browser()
		out[id].signIn(id)
	}
	return out
}

type staffEntry struct {
	ID        int64      `json:"id"`
	User      string     `json:"user"`
	Name      string     `json:"name"`
	ClockIn   time.Time  `json:"clock_in"`
	ClockOut  *time.Time `json:"clock_out"`
	Minutes   int64      `json:"minutes"`
	RotaID    *int64     `json:"rota_id"`
	Manual    bool       `json:"manual"`
	Corrected int        `json:"corrected"`
}

type staffShift struct {
	ID      int64  `json:"id"`
	User    string `json:"user"`
	Name    string `json:"name"`
	Day     string `json:"day"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Station string `json:"station"`
	Status  string `json:"status"`
}

func (a *staffRig) plan(mgr *browser, user, day, start, end string) staffShift {
	a.t.Helper()
	var out struct{ Shift staffShift }
	if code, _ := mgr.do("POST", "/api/rota", map[string]any{"user": user, "day": day, "start": start, "end": end, "station": "line"}, &out); code != 201 {
		a.t.Fatalf("plan a shift for %s: %d", user, code)
	}
	return out.Shift
}

func TestOnlyStaffClockInAndOnlyAManagerRunsTheSchedule(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	for _, id := range []string{"cus-1", "adm-1"} {
		for _, call := range [][2]string{{"GET", "/api/staff/me"}, {"POST", "/api/clock/in"}, {"POST", "/api/clock/out"}, {"GET", "/api/rota"}, {"GET", "/api/timesheet"}, {"GET", "/api/staff/hours"}} {
			if code := p[id].status(call[0], call[1], nil); code != 403 {
				t.Errorf("%s %s %s: %d, want 403", id, call[0], call[1], code)
			}
		}
	}
	anon := a.browser()
	for _, path := range []string{"/api/staff/me", "/api/rota", "/api/timesheet", "/api/staff/hours", "/api/staff/rates"} {
		if code := anon.status("GET", path, nil); code != 401 {
			t.Errorf("without signing in %s: %d", path, code)
		}
	}
	shift := map[string]any{"user": "cook-1", "day": "2026-11-20", "start": "09:00", "end": "17:00"}
	entry := map[string]any{"user": "cook-1", "day": "2026-11-20", "in": "08:00", "out": "09:00", "reason": "forgot"}
	for _, id := range []string{"cash-1", "cook-1", "store-1"} {
		for _, call := range []struct {
			method, path string
			body         any
		}{
			{"POST", "/api/rota", shift},
			{"PUT", "/api/rota/1", shift},
			{"POST", "/api/rota/1/cancel", map[string]any{"reason": "x"}},
			{"POST", "/api/rota/copy", map[string]any{"from": "2026-11-16", "to": "2026-11-22", "days": 7}},
			{"POST", "/api/timesheet", entry},
			{"PUT", "/api/timesheet/1", map[string]any{"day": "2026-11-20", "in": "08:00", "out": "09:00", "reason": "x"}},
			{"GET", "/api/timesheet/1/history", nil},
			{"GET", "/api/staff/rates", nil},
			{"POST", "/api/staff/rates", map[string]any{"user": "cook-1", "from": "2026-11-01", "rate": 30000}},
		} {
			if code := p[id].status(call.method, call.path, call.body); code != 403 {
				t.Errorf("%s %s %s: %d, want 403", id, call.method, call.path, code)
			}
		}
	}
	// the manager may do all of it
	if code := p["mgr-1"].status("POST", "/api/rota", shift); code != 201 {
		t.Errorf("manager plans: %d", code)
	}
	if code := p["mgr-1"].status("GET", "/api/staff/rates", nil); code != 200 {
		t.Errorf("manager reads rates: %d", code)
	}
}

func TestAManagerPlansShiftsOnlyForPeopleWhoWorkThem(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	if _, err := a.acc.SetDisabled(ctxBackground, "test", "cash-2", true, accounts.Client{}); err != nil {
		t.Fatal(err)
	}
	for user, want := range map[string]int{"cook-1": 201, "nobody": 400, "cus-1": 400, "adm-1": 400, "cash-2": 400, "Cook 1": 400} {
		body := map[string]any{"user": user, "day": "2026-11-20", "start": "09:00", "end": "13:00"}
		if code := p["mgr-1"].status("POST", "/api/rota", body); code != want {
			t.Errorf("a shift for %s: %d, want %d", user, code, want)
		}
	}
	var list struct {
		Shifts []staffShift
		People []Person
	}
	if code, _ := p["mgr-1"].do("GET", "/api/rota?from=2026-11-20&to=2026-11-20", nil, &list); code != 200 || len(list.Shifts) != 1 || list.Shifts[0].Name != "Person cook-1" {
		t.Fatalf("rota: %d %+v", code, list)
	}
	ids := map[string]bool{}
	for _, x := range list.People {
		ids[x.ID] = true
	}
	if !ids["cook-1"] || !ids["cash-1"] || !ids["mgr-1"] || ids["cus-1"] || ids["adm-1"] || ids["cash-2"] {
		t.Fatalf("the people a manager can pick: %v", ids)
	}
	// a shift that overlaps another is a conflict, and a shift that is not well formed is a bad request
	if code := p["mgr-1"].status("POST", "/api/rota", map[string]any{"user": "cook-1", "day": "2026-11-20", "start": "12:00", "end": "15:00"}); code != 409 {
		t.Errorf("overlap: %d", code)
	}
	if code := p["mgr-1"].status("POST", "/api/rota", map[string]any{"user": "cook-1", "day": "20/11", "start": "12:00", "end": "15:00"}); code != 400 {
		t.Errorf("bad day: %d", code)
	}
	if code := p["mgr-1"].status("POST", "/api/rota", map[string]any{"user": "cook-1", "day": "2026-11-21", "start": "12:00", "end": "15:00", "surprise": 1}); code != 400 {
		t.Errorf("an unknown field: %d", code)
	}
}

func TestAPersonSeesOnlyTheirOwnShiftsAndEntries(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	a.plan(p["mgr-1"], "cook-1", "2026-11-20", "09:00", "17:00")
	a.plan(p["mgr-1"], "cash-1", "2026-11-20", "09:00", "17:00")
	var mine struct {
		Shifts []staffShift
	}
	if code, _ := p["cook-1"].do("GET", "/api/rota?from=2026-11-20&to=2026-11-20&user=cash-1", nil, &mine); code != 200 || len(mine.Shifts) != 1 || mine.Shifts[0].User != "cook-1" {
		t.Fatalf("a cook reads the rota: %d %+v (asking for somebody else's does not help)", code, mine)
	}
	// both clock in; each sees only their own entries
	for _, id := range []string{"cook-1", "cash-1"} {
		if code := p[id].status("POST", "/api/clock/in", nil); code != 201 {
			t.Fatalf("%s clocks in: %d", id, code)
		}
	}
	var ts struct {
		Entries []staffEntry
		OnDuty  []staffEntry `json:"on_duty"`
	}
	if code, _ := p["cook-1"].do("GET", "/api/timesheet?user=cash-1", nil, &ts); code != 200 || len(ts.Entries) != 1 || ts.Entries[0].User != "cook-1" || len(ts.OnDuty) != 0 {
		t.Fatalf("the time sheet of a cook: %d %+v", code, ts)
	}
	if code, _ := p["mgr-1"].do("GET", "/api/timesheet", nil, &ts); code != 200 || len(ts.Entries) != 2 || len(ts.OnDuty) != 2 || ts.Entries[0].Name == "" {
		t.Fatalf("the time sheet of the manager: %d %+v", code, ts)
	}
}

func TestClockingInAndOutMatchesTheShiftAndTellsWhyItCannot(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	sh := a.plan(p["mgr-1"], "cook-1", "2026-11-20", "09:00", "17:00")
	var me struct {
		Open   *staffEntry
		Shifts []staffShift
		Day    string
	}
	if code, _ := p["cook-1"].do("GET", "/api/staff/me", nil, &me); code != 200 || me.Open != nil || len(me.Shifts) != 1 || me.Shifts[0].ID != sh.ID || me.Day != "2026-11-20" {
		t.Fatalf("before: %d %+v", code, me)
	}
	var in struct{ Entry staffEntry }
	if code, _ := p["cook-1"].do("POST", "/api/clock/in", nil, &in); code != 201 || in.Entry.RotaID == nil || *in.Entry.RotaID != sh.ID || in.Entry.ClockOut != nil {
		t.Fatalf("clock in: %d %+v", code, in)
	}
	var bad errorBody
	if code, _ := p["cook-1"].do("POST", "/api/clock/in", nil, &bad); code != 409 || bad.Code != "clocked_in" {
		t.Fatalf("twice: %d %+v", code, bad)
	}
	if code, _ := p["cook-1"].do("GET", "/api/staff/me", nil, &me); code != 200 || me.Open == nil || me.Open.ID != in.Entry.ID {
		t.Fatalf("while in: %d %+v", code, me)
	}
	a.clk.set("2026-11-20", 13, 30)
	var out struct{ Entry staffEntry }
	if code, _ := p["cook-1"].do("POST", "/api/clock/out", nil, &out); code != 200 || out.Entry.ClockOut == nil || out.Entry.Minutes != 210 {
		t.Fatalf("clock out: %d %+v", code, out)
	}
	if code, _ := p["cook-1"].do("POST", "/api/clock/out", nil, &bad); code != 409 || bad.Code != "not_clocked_in" {
		t.Fatalf("out twice: %d %+v", code, bad)
	}
}

func TestADrawerThatIsOpenHoldsTheCashierAtTheDoor(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	if code := p["cash-1"].status("POST", "/api/clock/in", nil); code != 201 {
		t.Fatalf("clock in: %d", code)
	}
	var sh shiftBody
	if code, _ := p["cash-1"].do("POST", "/api/shifts", map[string]any{"till": "main", "counts": map[string]int{"100000": 5}}, &sh); code != 201 {
		t.Fatalf("open a shift: %d", code)
	}
	var me struct {
		DrawerOpen bool `json:"drawer_open"`
	}
	if code, _ := p["cash-1"].do("GET", "/api/staff/me", nil, &me); code != 200 || !me.DrawerOpen {
		t.Fatalf("the page knows about the drawer: %d %+v", code, me)
	}
	var bad errorBody
	if code, _ := p["cash-1"].do("POST", "/api/clock/out", nil, &bad); code != 409 || bad.Code != "drawer_open" {
		t.Fatalf("clock out with the drawer open: %d %+v", code, bad)
	}
	if code := p["cash-1"].status("POST", "/api/shifts/"+itoa(sh.Shift.ID)+"/close", map[string]any{"counts": map[string]int{"100000": 5}}); code != 200 {
		t.Fatalf("close the shift: %d", code)
	}
	a.clk.set("2026-11-20", 18, 0)
	if code := p["cash-1"].status("POST", "/api/clock/out", nil); code != 200 {
		t.Fatalf("clock out after closing: %d", code)
	}
}

func TestWhenTheVenueAsksATillIsOpenedOnlyByAPersonWhoIsClockedIn(t *testing.T) {
	a := newStaffRig(t, true)
	p := a.people()
	open := map[string]any{"till": "main", "counts": map[string]int{"100000": 5}}
	var bad errorBody
	if code, _ := p["cash-1"].do("POST", "/api/shifts", open, &bad); code != 409 || bad.Code != "clock_in_first" {
		t.Fatalf("a till before clocking in: %d %+v", code, bad)
	}
	var me struct {
		Required bool
	}
	if code, _ := p["cash-1"].do("GET", "/api/staff/me", nil, &me); code != 200 || !me.Required {
		t.Fatalf("the page says it is asked for: %d %+v", code, me)
	}
	if code := p["cash-1"].status("POST", "/api/clock/in", nil); code != 201 {
		t.Fatalf("clock in: %d", code)
	}
	if code := p["cash-1"].status("POST", "/api/shifts", open); code != 201 {
		t.Fatalf("a till after clocking in: %d", code)
	}
	// a person who is not asked to clock in is not asked for anything else either
	if code := p["cash-2"].status("POST", "/api/shifts", map[string]any{"till": "back", "counts": map[string]int{}}); code != 409 {
		t.Fatalf("a second cashier who did not clock in: %d", code)
	}
}

func TestAManagerCorrectsAnEntryAndTheHistoryKeepsWhatItWas(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	a.clk.set("2026-11-20", 20, 0)
	var add struct{ Entry staffEntry }
	body := map[string]any{"user": "cook-1", "day": "2026-11-20", "in": "08:00", "out": "12:00", "reason": "forgot the phone"}
	if code, _ := p["mgr-1"].do("POST", "/api/timesheet", body, &add); code != 201 || !add.Entry.Manual || add.Entry.Minutes != 240 {
		t.Fatalf("add: %d %+v", code, add)
	}
	path := "/api/timesheet/" + itoa(add.Entry.ID)
	var fixed struct{ Entry staffEntry }
	fix := map[string]any{"day": "2026-11-20", "in": "08:30", "out": "12:00", "reason": "the door log says 8:30"}
	if code, _ := p["mgr-1"].do("PUT", path, fix, &fixed); code != 200 || fixed.Entry.Minutes != 210 || fixed.Entry.Corrected != 1 {
		t.Fatalf("correct: %d %+v", code, fixed)
	}
	var h struct {
		Changes []struct {
			By     string
			Reason string
		}
	}
	if code, _ := p["mgr-1"].do("GET", path+"/history", nil, &h); code != 200 || len(h.Changes) != 1 || h.Changes[0].By != "mgr-1" || !strings.Contains(h.Changes[0].Reason, "door log") {
		t.Fatalf("history: %d %+v", code, h)
	}
	if code := p["mgr-1"].status("PUT", path, map[string]any{"day": "2026-11-20", "in": "08:30", "out": "12:00"}); code != 400 {
		t.Errorf("a correction without a reason: %d", code)
	}
	if code := p["mgr-1"].status("PUT", "/api/timesheet/9999", fix); code != 404 {
		t.Errorf("a correction of nothing: %d", code)
	}
	if code := p["mgr-1"].status("PUT", "/api/timesheet/abc", fix); code != 400 {
		t.Errorf("a correction of a word: %d", code)
	}
	if code := p["mgr-1"].status("GET", "/api/timesheet/9999/history", nil); code != 404 {
		t.Errorf("the history of nothing: %d", code)
	}
	if code := p["mgr-1"].status("POST", "/api/timesheet", body); code != 409 {
		t.Errorf("the same stretch twice: %d", code)
	}
}

func TestAShiftIsCancelledAndTheWeekIsCopied(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	sh := a.plan(p["mgr-1"], "cook-1", "2026-11-16", "09:00", "17:00")
	a.plan(p["mgr-1"], "cash-1", "2026-11-17", "14:00", "22:00")
	var copied struct{ Made, Skipped int }
	if code, _ := p["mgr-1"].do("POST", "/api/rota/copy", map[string]any{"from": "2026-11-16", "to": "2026-11-22", "days": 7}, &copied); code != 200 || copied.Made != 2 || copied.Skipped != 0 {
		t.Fatalf("copy: %d %+v", code, copied)
	}
	if code, _ := p["mgr-1"].do("POST", "/api/rota/copy", map[string]any{"from": "2026-11-16", "to": "2026-11-22", "days": 7}, &copied); code != 200 || copied.Made != 0 || copied.Skipped != 2 {
		t.Fatalf("copy again: %d %+v", code, copied)
	}
	var c struct{ Shift staffShift }
	if code, _ := p["mgr-1"].do("POST", "/api/rota/"+itoa(sh.ID)+"/cancel", map[string]any{"reason": "sick"}, &c); code != 200 || c.Shift.Status != "cancelled" {
		t.Fatalf("cancel: %d %+v", code, c)
	}
	var bad errorBody
	if code, _ := p["mgr-1"].do("POST", "/api/rota/"+itoa(sh.ID)+"/cancel", map[string]any{}, &bad); code != 409 || bad.Code != "cancelled" {
		t.Fatalf("cancel twice: %d %+v", code, bad)
	}
	if code, _ := p["mgr-1"].do("PUT", "/api/rota/"+itoa(sh.ID), map[string]any{"day": "2026-11-16", "start": "10:00", "end": "17:00"}, &bad); code != 409 || bad.Code != "cancelled" {
		t.Fatalf("change a cancelled shift: %d %+v", code, bad)
	}
	var week struct{ Shifts []staffShift }
	p["mgr-1"].do("GET", "/api/rota?from=2026-11-16&to=2026-11-16", nil, &week)
	if len(week.Shifts) != 0 {
		t.Fatalf("a cancelled shift is not on the list: %+v", week)
	}
	p["mgr-1"].do("GET", "/api/rota?from=2026-11-16&to=2026-11-16&cancelled=1", nil, &week)
	if len(week.Shifts) != 1 {
		t.Fatalf("but it is when asked for: %+v", week)
	}
	// a person who is not a manager does not get to see the cancelled ones
	p["cook-1"].do("GET", "/api/rota?from=2026-11-16&to=2026-11-16&cancelled=1", nil, &week)
	if len(week.Shifts) != 0 {
		t.Fatalf("a cook sees a cancelled shift: %+v", week)
	}
}

func TestHoursAreForTheManagerOrForTheOwnerAlone(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	a.plan(p["mgr-1"], "cook-1", "2026-11-20", "08:00", "12:00")
	a.clk.set("2026-11-20", 8, 25)
	p["cook-1"].status("POST", "/api/clock/in", nil)
	a.clk.set("2026-11-20", 12, 0)
	p["cook-1"].status("POST", "/api/clock/out", nil)
	a.clk.set("2026-11-20", 13, 0)
	p["cash-1"].status("POST", "/api/clock/in", nil)
	a.clk.set("2026-11-20", 15, 0)
	p["cash-1"].status("POST", "/api/clock/out", nil)
	var h struct {
		Hours []struct {
			User      string
			Name      string
			Planned   int64 `json:"planned_minutes"`
			Worked    int64 `json:"worked_minutes"`
			Late      int64
			LateMin   int64 `json:"late_minutes"`
			PlannedSh int64 `json:"planned_shifts"`
		}
	}
	if code, _ := p["mgr-1"].do("GET", "/api/staff/hours?from=2026-11-20&to=2026-11-20", nil, &h); code != 200 || len(h.Hours) != 2 {
		t.Fatalf("the manager: %d %+v", code, h)
	}
	cook := h.Hours[1]
	if h.Hours[0].User == "cook-1" {
		cook = h.Hours[0]
	}
	if cook.User != "cook-1" || cook.Name != "Person cook-1" || cook.Planned != 240 || cook.Worked != 215 || cook.Late != 1 || cook.LateMin != 25 {
		t.Fatalf("the cook: %+v", cook)
	}
	if code, _ := p["cash-1"].do("GET", "/api/staff/hours?from=2026-11-20&to=2026-11-20&user=cook-1", nil, &h); code != 200 || len(h.Hours) != 1 || h.Hours[0].User != "cash-1" || h.Hours[0].Worked != 120 {
		t.Fatalf("a cashier sees only theirs: %d %+v", code, h)
	}
	if code := p["mgr-1"].status("GET", "/api/staff/hours?from=nope", nil); code != 400 {
		t.Errorf("a bad day: %d", code)
	}
}

func TestRatesTurnHoursIntoLabourThatTheProfitReportTakesAway(t *testing.T) {
	a := newStaffRig(t, false)
	p := a.people()
	a.clk.set("2026-11-21", 6, 0)
	// the cook worked 4 hours on the 20th, at 30,000 an hour; the cashier 2 hours with no rate yet
	if code := p["mgr-1"].status("POST", "/api/staff/rates", map[string]any{"user": "cook-1", "from": "2026-11-01", "rate": 30000}); code != 201 {
		t.Fatalf("set a rate: %d", code)
	}
	for _, e := range []map[string]any{
		{"user": "cook-1", "day": "2026-11-20", "in": "08:00", "out": "12:00", "reason": "x"},
		{"user": "cash-1", "day": "2026-11-20", "in": "13:00", "out": "15:00", "reason": "x"},
	} {
		if code := p["mgr-1"].status("POST", "/api/timesheet", e); code != 201 {
			t.Fatalf("write down the work: %d", code)
		}
	}
	for _, body := range []map[string]any{
		{"user": "cook-1", "from": "2026-11-01", "rate": -1},
		{"user": "cook-1", "from": "2026-11-01", "rate": 99_999_999},
		{"user": "cus-1", "from": "2026-11-01", "rate": 100},
		{"user": "cook-1", "from": "soon", "rate": 100},
	} {
		if code := p["mgr-1"].status("POST", "/api/staff/rates", body); code != 400 {
			t.Errorf("%v: %d, want 400", body, code)
		}
	}
	var rates struct {
		Rates []staffRate
		Max   int64 `json:"max_rate"`
	}
	if code, _ := p["mgr-1"].do("GET", "/api/staff/rates", nil, &rates); code != 200 || len(rates.Rates) != 1 || rates.Rates[0].Rate != 30000 || rates.Max != staffing.MaxRate {
		t.Fatalf("rates: %d %+v", code, rates)
	}
	var rep struct {
		Rows []struct {
			Date     string
			Labour   int64
			Expenses int64
			Profit   int64
		}
		LabourUnrated int64 `json:"labour_unrated_minutes"`
	}
	if code, _ := p["mgr-1"].do("GET", "/api/reports/profit?from=2026-11-19&to=2026-11-21", nil, &rep); code != 200 || len(rep.Rows) != 3 {
		t.Fatalf("profit: %d %+v", code, rep)
	}
	day := rep.Rows[1]
	if day.Date != "2026-11-20" || day.Labour != 120_000 || day.Profit != -120_000 || rep.Rows[0].Labour != 0 || rep.LabourUnrated != 120 {
		t.Fatalf("the 20th: %+v, unrated %d", day, rep.LabourUnrated)
	}
	code, _, csv := p["mgr-1"].raw("/api/reports/profit?from=2026-11-20&to=2026-11-20&format=csv")
	if code != 200 || !strings.Contains(string(csv), "expenses,labour,profit") || !strings.Contains(string(csv), ",120000,-120000") {
		t.Fatalf("csv: %d %s", code, csv)
	}
	// the rate of a later day changes what is paid from then on, not before
	if code := p["mgr-1"].status("POST", "/api/staff/rates", map[string]any{"user": "cook-1", "from": "2026-11-21", "rate": 40000}); code != 201 {
		t.Fatalf("a later rate: %d", code)
	}
	p["mgr-1"].do("GET", "/api/reports/profit?from=2026-11-20&to=2026-11-20", nil, &rep)
	if rep.Rows[0].Labour != 120_000 {
		t.Fatalf("a later rate changed the past: %+v", rep.Rows[0])
	}
}

type staffRate struct {
	User string
	From string
	Rate int64
}
