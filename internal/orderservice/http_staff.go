package orderservice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	staffing "github.com/nguyenhoangkhang-2010/raftlog/internal/staff"
)

// ErrDrawerOpen is returned to a person who clocks out while a cash drawer is still open in their name.
var ErrDrawerOpen = errors.New("orderservice: close your cash drawer before you clock out")

// ErrClockInFirst is returned to a person who opens a till without having clocked in, when the venue
// asks them to.
var ErrClockInFirst = errors.New("orderservice: clock in before you open a till")

// WithStaff turns on the schedule of the staff, the time clock and the hourly rates, kept in the store.
// With requireClockIn, a cash shift is opened only by a person who is clocked in.
func WithStaff(store *staffing.Store, requireClockIn bool) APIOption {
	return func(a *API) { a.staff, a.requireClockIn = store, requireClockIn }
}

func (a *API) staffRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/staff/me", a.auth(a.staffMe))
	mux.HandleFunc("POST /api/clock/in", a.auth(a.clockIn))
	mux.HandleFunc("POST /api/clock/out", a.auth(a.clockOut))
	mux.HandleFunc("GET /api/rota", a.auth(a.listRota))
	mux.HandleFunc("POST /api/rota", a.auth(a.addShift))
	mux.HandleFunc("POST /api/rota/copy", a.auth(a.copyRota))
	mux.HandleFunc("PUT /api/rota/{id}", a.auth(a.updateShift))
	mux.HandleFunc("POST /api/rota/{id}/cancel", a.auth(a.cancelShift))
	mux.HandleFunc("GET /api/timesheet", a.auth(a.listTimesheet))
	mux.HandleFunc("POST /api/timesheet", a.auth(a.addEntry))
	mux.HandleFunc("PUT /api/timesheet/{id}", a.auth(a.correctEntry))
	mux.HandleFunc("GET /api/timesheet/{id}/history", a.auth(a.entryHistory))
	mux.HandleFunc("GET /api/staff/hours", a.auth(a.staffHours))
	mux.HandleFunc("GET /api/staff/rates", a.auth(a.listRates))
	mux.HandleFunc("POST /api/staff/rates", a.auth(a.setRate))
}

func (a *API) failStaff(w http.ResponseWriter, err error) {
	var inv *staffing.InvalidError
	conflict := func(code string) {
		writeJSON(w, http.StatusConflict, errorBody{Error: strings.TrimPrefix(err.Error(), "staff: "), Code: code})
	}
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: strings.TrimPrefix(err.Error(), "staff: "), Code: "invalid"})
	case errors.Is(err, staffing.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not found", Code: "not_found"})
	case errors.Is(err, staffing.ErrClockedIn):
		conflict("clocked_in")
	case errors.Is(err, staffing.ErrNotClockedIn):
		conflict("not_clocked_in")
	case errors.Is(err, staffing.ErrOverlap):
		conflict("overlap")
	case errors.Is(err, staffing.ErrCancelled):
		conflict("cancelled")
	default:
		if _, kind := status(err); kind == "internal" {
			a.logger.Error("the staff database failed", "err", err)
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the database did not answer", Code: "unavailable"})
			return
		}
		a.fail(w, err)
	}
}

func (a *API) needs(w http.ResponseWriter, actor rbac.Actor, p rbac.Permission) bool {
	if actor.Can(p) {
		return true
	}
	a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, p))
	return false
}

func addDays(day string, n int) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// daysOf reads the days of a list from the query, or takes the default: `back` days before today up to
// `forward` days after it.
func (a *API) daysOf(r *http.Request, back, forward int) (from, to string) {
	today := a.staff.Day(a.staff.Now())
	from, to = addDays(today, -back), addDays(today, forward)
	if v := r.URL.Query().Get("from"); v != "" {
		from = v
	}
	if v := r.URL.Query().Get("to"); v != "" {
		to = v
	}
	return from, to
}

// Person is somebody who works shifts, as a manager picks them.
type Person struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Roles []rbac.Role `json:"roles"`
}

// people lists the accounts that work shifts, and a name for every account that is known. Without the
// accounts database (the users file of tokens) there is nobody to list.
func (a *API) people(ctx context.Context) ([]Person, map[string]string, error) {
	names := map[string]string{}
	out := []Person{}
	if a.acc == nil {
		return out, names, nil
	}
	users, err := a.acc.ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, u := range users {
		names[u.ID] = u.DisplayName
		if !u.Disabled && rbac.Can(u.Roles, rbac.UseClock) {
			out = append(out, Person{ID: u.ID, Name: u.DisplayName, Roles: u.Roles})
		}
	}
	return out, names, nil
}

// person checks that a login is an account that may be given work.
func (a *API) person(ctx context.Context, id string) error {
	if a.acc == nil {
		return nil
	}
	u, err := a.acc.GetUser(ctx, id)
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		return &staffing.InvalidError{Reason: "there is no such person"}
	case err != nil:
		return err
	case u.Disabled:
		return &staffing.InvalidError{Reason: "that account is disabled"}
	case !rbac.Can(u.Roles, rbac.UseClock):
		return &staffing.InvalidError{Reason: "that person does not work shifts"}
	}
	return nil
}

func nameOf(names map[string]string, id string) string {
	if n := names[id]; n != "" {
		return n
	}
	return id
}

type shiftView struct {
	staffing.Shift
	Name string `json:"name"`
}

type entryView struct {
	staffing.Entry
	Name string `json:"name"`
}

type hoursView struct {
	staffing.HoursRow
	Name string `json:"name"`
}

func shiftViews(list []staffing.Shift, names map[string]string) []shiftView {
	out := make([]shiftView, 0, len(list))
	for _, s := range list {
		out = append(out, shiftView{s, nameOf(names, s.User)})
	}
	return out
}

func entryViews(list []staffing.Entry, names map[string]string) []entryView {
	out := make([]entryView, 0, len(list))
	for _, e := range list {
		out = append(out, entryView{e, nameOf(names, e.User)})
	}
	return out
}

// ---- a person's own page: the shifts ahead, the clock, the last entries -----------------------

func (a *API) staffMe(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.needs(w, actor, rbac.UseClock) {
		return
	}
	ctx := r.Context()
	now := a.staff.Now()
	today := a.staff.Day(now)
	body := map[string]any{"now": now, "day": today, "required": a.requireClockIn, "open": nil}
	switch open, err := a.staff.OpenEntry(ctx, actor.ID); {
	case err == nil:
		body["open"] = open
	case !errors.Is(err, staffing.ErrNotClockedIn):
		a.failStaff(w, err)
		return
	}
	shifts, err := a.staff.Shifts(ctx, addDays(today, -1), addDays(today, 7), actor.ID, false)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	entries, err := a.staff.Entries(ctx, addDays(today, -13), today, actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	body["shifts"], body["entries"] = shifts, entries
	body["drawer_open"] = false
	if a.shifts != nil {
		open, err := a.shifts.HasOpen(ctx, actor.ID)
		if err != nil {
			a.failShift(w, err)
			return
		}
		body["drawer_open"] = open
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) clockIn(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.needs(w, actor, rbac.UseClock) {
		return
	}
	e, err := a.staff.ClockIn(r.Context(), actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"entry": e})
}

func (a *API) clockOut(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.needs(w, actor, rbac.UseClock) {
		return
	}
	// a drawer that is open in the person's name must be counted before they leave
	if a.shifts != nil {
		open, err := a.shifts.HasOpen(r.Context(), actor.ID)
		if err != nil {
			a.failShift(w, err)
			return
		}
		if open {
			a.fail(w, ErrDrawerOpen)
			return
		}
	}
	e, err := a.staff.ClockOut(r.Context(), actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

// requireClockedIn refuses a person who has not clocked in, when the venue asks them to.
func (a *API) requireClockedIn(r *http.Request, actor rbac.Actor) error {
	if !a.requireClockIn || a.staff == nil {
		return nil
	}
	if _, err := a.staff.OpenEntry(r.Context(), actor.ID); errors.Is(err, staffing.ErrNotClockedIn) {
		return ErrClockInFirst
	} else if err != nil {
		return err
	}
	return nil
}

// ---- the schedule -------------------------------------------------------------------------------

func (a *API) listRota(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	manager := actor.Can(rbac.ManageStaff)
	if !manager && !a.needs(w, actor, rbac.UseClock) {
		return
	}
	from, to := a.daysOf(r, 0, 6)
	user := actor.ID
	if manager {
		user = r.URL.Query().Get("user")
	}
	list, err := a.staff.Shifts(r.Context(), from, to, user, manager && r.URL.Query().Get("cancelled") == "1")
	if err != nil {
		a.failStaff(w, err)
		return
	}
	body := map[string]any{"from": from, "to": to, "today": a.staff.Day(a.staff.Now())}
	if manager {
		people, names, err := a.people(r.Context())
		if err != nil {
			a.failAccounts(w, err)
			return
		}
		body["people"], body["shifts"] = people, shiftViews(list, names)
	} else {
		body["shifts"] = shiftViews(list, map[string]string{})
	}
	writeJSON(w, http.StatusOK, body)
}

type rotaBody struct {
	User    string `json:"user"`
	Day     string `json:"day"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Station string `json:"station"`
	Note    string `json:"note"`
}

func (b rotaBody) input() staffing.ShiftInput {
	return staffing.ShiftInput{User: b.User, Day: b.Day, Start: b.Start, End: b.End, Station: b.Station, Note: b.Note}
}

func (a *API) addShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b rotaBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	if err := a.person(r.Context(), b.User); err != nil {
		a.failStaff(w, err)
		return
	}
	sh, err := a.staff.AddShift(r.Context(), b.input(), actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"shift": sh})
}

func idOf(r *http.Request, what string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, &staffing.InvalidError{Reason: what + " is a number from 1"}
	}
	return id, nil
}

func (a *API) updateShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b rotaBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	id, err := idOf(r, "a shift")
	if err != nil {
		a.failStaff(w, err)
		return
	}
	sh, err := a.staff.UpdateShift(r.Context(), id, b.input(), actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shift": sh})
}

func (a *API) cancelShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	id, err := idOf(r, "a shift")
	if err != nil {
		a.failStaff(w, err)
		return
	}
	sh, err := a.staff.CancelShift(r.Context(), id, actor.ID, b.Reason)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shift": sh})
}

func (a *API) copyRota(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		From string `json:"from"`
		To   string `json:"to"`
		Days int    `json:"days"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	made, skipped, err := a.staff.CopyDays(r.Context(), b.From, b.To, b.Days, actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"made": made, "skipped": skipped})
}

// ---- the time sheet -------------------------------------------------------------------------------

func (a *API) listTimesheet(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	manager := actor.Can(rbac.ManageStaff)
	if !manager && !a.needs(w, actor, rbac.UseClock) {
		return
	}
	from, to := a.daysOf(r, 13, 0)
	user := actor.ID
	if manager {
		user = r.URL.Query().Get("user")
	}
	list, err := a.staff.Entries(r.Context(), from, to, user)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	body := map[string]any{"from": from, "to": to, "today": a.staff.Day(a.staff.Now())}
	if !manager {
		body["entries"] = entryViews(list, map[string]string{})
		writeJSON(w, http.StatusOK, body)
		return
	}
	people, names, err := a.people(r.Context())
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	duty, err := a.staff.OnDuty(r.Context())
	if err != nil {
		a.failStaff(w, err)
		return
	}
	body["people"], body["entries"], body["on_duty"] = people, entryViews(list, names), entryViews(duty, names)
	writeJSON(w, http.StatusOK, body)
}

func (a *API) addEntry(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		User   string `json:"user"`
		Day    string `json:"day"`
		In     string `json:"in"`
		Out    string `json:"out"`
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	if err := a.person(r.Context(), b.User); err != nil {
		a.failStaff(w, err)
		return
	}
	in, out, err := a.staff.Span(b.Day, b.In, b.Out)
	if err == nil && out == nil {
		err = &staffing.InvalidError{Reason: "a stretch of work that is written down has ended already"}
	}
	if err != nil {
		a.failStaff(w, err)
		return
	}
	e, err := a.staff.AddEntry(r.Context(), b.User, in, *out, b.Reason, actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"entry": e})
}

func (a *API) correctEntry(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Day    string `json:"day"`
		In     string `json:"in"`
		Out    string `json:"out"`
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	id, err := idOf(r, "an entry")
	if err != nil {
		a.failStaff(w, err)
		return
	}
	in, out, err := a.staff.Span(b.Day, b.In, b.Out)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	e, err := a.staff.CorrectEntry(r.Context(), id, in, out, b.Reason, actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": e})
}

func (a *API) entryHistory(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	id, err := idOf(r, "an entry")
	if err != nil {
		a.failStaff(w, err)
		return
	}
	if _, err := a.staff.Get(r.Context(), id); err != nil {
		a.failStaff(w, err)
		return
	}
	h, err := a.staff.History(r.Context(), id)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": h})
}

// ---- hours and rates ------------------------------------------------------------------------------

func (a *API) staffHours(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	manager := actor.Can(rbac.ManageStaff)
	if !manager && !a.needs(w, actor, rbac.UseClock) {
		return
	}
	from, to := a.daysOf(r, 6, 0)
	user := actor.ID
	if manager {
		user = r.URL.Query().Get("user")
	}
	rows, err := a.staff.Hours(r.Context(), from, to, user)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	names := map[string]string{}
	if manager {
		if _, names, err = a.people(r.Context()); err != nil {
			a.failAccounts(w, err)
			return
		}
	}
	out := make([]hoursView, 0, len(rows))
	for _, h := range rows {
		out = append(out, hoursView{h, nameOf(names, h.User)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "today": a.staff.Day(a.staff.Now()), "hours": out})
}

func (a *API) listRates(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	rates, err := a.staff.Rates(r.Context(), r.URL.Query().Get("history") == "1")
	if err != nil {
		a.failStaff(w, err)
		return
	}
	people, _, err := a.people(r.Context())
	if err != nil {
		a.failAccounts(w, err)
		return
	}
	sort.Slice(people, func(i, j int) bool { return people[i].ID < people[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"rates": rates, "people": people, "max_rate": staffing.MaxRate, "today": a.staff.Day(a.staff.Now())})
}

func (a *API) setRate(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		User string `json:"user"`
		From string `json:"from"`
		Rate int64  `json:"rate"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.needs(w, actor, rbac.ManageStaff) {
		return
	}
	if err := a.person(r.Context(), b.User); err != nil {
		a.failStaff(w, err)
		return
	}
	rate, err := a.staff.SetRate(r.Context(), b.User, b.From, b.Rate, actor.ID)
	if err != nil {
		a.failStaff(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"rate": rate})
}
