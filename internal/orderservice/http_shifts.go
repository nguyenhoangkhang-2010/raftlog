package orderservice

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/shifts"
)

// ErrNoShift is returned for cash that is taken by a cashier who has no shift open, when the venue
// asks for one.
var ErrNoShift = errors.New("orderservice: open a shift before taking cash")

// WithShifts turns on the shifts of the cashiers and the count of their drawers, kept in the store.
// With requireShift, a payment in cash is refused unless the cashier who takes it has a shift open.
func WithShifts(store *shifts.Store, requireShift bool) APIOption {
	return func(a *API) { a.shifts, a.requireShift = store, requireShift }
}

func (a *API) shiftRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/shifts/current", a.auth(a.currentShift))
	mux.HandleFunc("GET /api/shifts", a.auth(a.listShifts))
	mux.HandleFunc("POST /api/shifts", a.auth(a.openShift))
	mux.HandleFunc("GET /api/shifts/{id}", a.auth(a.getShift))
	mux.HandleFunc("POST /api/shifts/{id}/movements", a.auth(a.shiftMovement))
	mux.HandleFunc("POST /api/shifts/{id}/close", a.auth(a.closeShift))
	mux.HandleFunc("POST /api/shifts/{id}/review", a.auth(a.reviewShift))
}

// failShift answers an error of the store.
func (a *API) failShift(w http.ResponseWriter, err error) {
	var inv *shifts.InvalidError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: strings.TrimPrefix(err.Error(), "shifts: "), Code: "invalid"})
	case errors.Is(err, shifts.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not found", Code: "not_found"})
	case errors.Is(err, shifts.ErrCashierBusy), errors.Is(err, shifts.ErrTillBusy), errors.Is(err, shifts.ErrNotOpen),
		errors.Is(err, shifts.ErrNotClosed), errors.Is(err, shifts.ErrReviewed):
		writeJSON(w, http.StatusConflict, errorBody{Error: strings.TrimPrefix(err.Error(), "shifts: "), Code: "conflict"})
	default:
		if _, kind := status(err); kind == "internal" {
			a.logger.Error("the shifts database failed", "err", err)
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the database did not answer", Code: "unavailable"})
			return
		}
		a.fail(w, err)
	}
}

func (a *API) mayWorkShifts(actor rbac.Actor) bool {
	return actor.Can(rbac.OpenShift) || actor.Can(rbac.CloseShift)
}

// mayReach says that a person may see and work on a shift: it is theirs, or they review shifts.
func mayReach(actor rbac.Actor, sh shifts.Shift) bool {
	return sh.Cashier == actor.ID || actor.Can(rbac.ReviewShift)
}

func (a *API) shiftOf(w http.ResponseWriter, r *http.Request, actor rbac.Actor) (shifts.Shift, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		a.failShift(w, &shifts.InvalidError{Reason: "a shift is a number from 1"})
		return shifts.Shift{}, false
	}
	sh, err := a.shifts.Get(r.Context(), id)
	if err != nil {
		a.failShift(w, err)
		return shifts.Shift{}, false
	}
	if !mayReach(actor, sh) {
		a.fail(w, fmt.Errorf("%w: not your shift", order.ErrNotAllowed))
		return shifts.Shift{}, false
	}
	return sh, true
}

// shiftAnswer is a shift with what the log says about its cash, as far as the person may see it.
//
// A cashier counts the drawer without being told what it should hold: the figures are shown to
// them only after the shift is closed, so that the count is what was counted. A person who reviews
// shifts sees them while the shift is open too.
type shiftAnswer struct {
	Shift shifts.Shift `json:"shift"`
	// Cash is the cash that orders put into the drawer and took out, for a closed shift or for one
	// that a reviewer looks at.
	Cash *CashView `json:"cash,omitempty"`
	// ExpectedSoFar is what the drawer should hold now, for an open shift and a reviewer.
	ExpectedSoFar *int64 `json:"expected_so_far,omitempty"`
}

func (a *API) answerShift(sh shifts.Shift, actor rbac.Actor) shiftAnswer {
	out := shiftAnswer{Shift: sh}
	switch {
	case sh.Status == shifts.Closed:
		cv := a.svc.Cash(sh.Cashier, sh.OpenedAt, *sh.ClosedAt)
		out.Cash = &cv
	case actor.Can(rbac.ReviewShift):
		cv := a.svc.Cash(sh.Cashier, sh.OpenedAt, a.svc.now())
		out.Cash = &cv
		var in, outMoney int64
		for _, m := range sh.Movements {
			if m.Kind == "in" {
				in += m.Amount
			} else {
				outMoney += m.Amount
			}
		}
		e := sh.OpeningTotal + cv.Sales - cv.Refunds + in - outMoney
		out.ExpectedSoFar = &e
	}
	return out
}

func (a *API) currentShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.mayWorkShifts(actor) && !actor.Can(rbac.ReviewShift) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.OpenShift))
		return
	}
	body := map[string]any{"denominations": shifts.Denominations, "required": a.requireShift, "shift": nil}
	sh, err := a.shifts.OpenOf(r.Context(), actor.ID)
	switch {
	case err == nil:
		body["shift"] = a.answerShift(sh, actor).Shift
	case !errors.Is(err, shifts.ErrNotFound):
		a.failShift(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) listShifts(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	f := shifts.Filter{Status: r.URL.Query().Get("status")}
	if f.Status != "" && f.Status != shifts.Open && f.Status != shifts.Closed {
		a.failShift(w, &shifts.InvalidError{Reason: "a status is open or closed"})
		return
	}
	f.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	switch {
	case actor.Can(rbac.ReviewShift):
		// every shift
	case a.mayWorkShifts(actor):
		f.Cashier = actor.ID
	default:
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.OpenShift))
		return
	}
	list, err := a.shifts.List(r.Context(), f)
	if err != nil {
		a.failShift(w, err)
		return
	}
	if list == nil {
		list = []shifts.Shift{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"shifts": list})
}

func (a *API) openShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Till   string        `json:"till"`
		Counts shifts.Counts `json:"counts"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !actor.Can(rbac.OpenShift) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.OpenShift))
		return
	}
	if err := a.requireClockedIn(r, actor); err != nil {
		a.fail(w, err)
		return
	}
	sh, err := a.shifts.Open(r.Context(), actor.ID, b.Till, b.Counts)
	if err != nil {
		a.failShift(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.answerShift(sh, actor))
}

func (a *API) getShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	sh, ok := a.shiftOf(w, r, actor)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.answerShift(sh, actor))
}

func (a *API) shiftMovement(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Kind   string `json:"kind"`
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	sh, ok := a.shiftOf(w, r, actor)
	if !ok {
		return
	}
	// money moves in and out of a drawer by the hand of the person who has it
	if sh.Cashier != actor.ID || !actor.Can(rbac.CloseShift) {
		a.fail(w, fmt.Errorf("%w: only the cashier of a shift records money in and out of its drawer", order.ErrNotAllowed))
		return
	}
	if _, err := a.shifts.AddMovement(r.Context(), sh.ID, b.Kind, b.Amount, b.Reason, actor.ID); err != nil {
		a.failShift(w, err)
		return
	}
	fresh, err := a.shifts.Get(r.Context(), sh.ID)
	if err != nil {
		a.failShift(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.answerShift(fresh, actor))
}

// closeShift ends a shift with the count of the drawer. The cash that orders put in and took out is
// read from the log up to the moment of closing, and the store works out the rest. A cashier closes
// their own shift; a person who reviews shifts may close one that was left open.
func (a *API) closeShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Counts shifts.Counts `json:"counts"`
		Note   string        `json:"note"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	sh, ok := a.shiftOf(w, r, actor)
	if !ok {
		return
	}
	if !actor.Can(rbac.CloseShift) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.CloseShift))
		return
	}
	at := a.shifts.Now()
	cv := a.svc.Cash(sh.Cashier, sh.OpenedAt, at)
	closed, err := a.shifts.Close(r.Context(), sh.ID, actor.ID, at, b.Counts, shifts.Figures{Sales: cv.Sales, Refunds: cv.Refunds}, b.Note)
	if err != nil {
		a.failShift(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.answerShift(closed, actor))
}

func (a *API) reviewShift(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Note string `json:"note"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !actor.Can(rbac.ReviewShift) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ReviewShift))
		return
	}
	sh, ok := a.shiftOf(w, r, actor)
	if !ok {
		return
	}
	reviewed, err := a.shifts.Review(r.Context(), sh.ID, actor.ID, b.Note)
	if err != nil {
		a.failShift(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.answerShift(reviewed, actor))
}

// requireOpenShift refuses a payment in cash by a cashier who has no shift open.
func (a *API) requireOpenShift(r *http.Request, actor rbac.Actor, method string) error {
	if !a.requireShift || a.shifts == nil || method != order.Cash {
		return nil
	}
	ok, err := a.shifts.HasOpen(r.Context(), actor.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoShift
	}
	return nil
}
