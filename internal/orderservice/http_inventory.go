package orderservice

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/inventory"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// WithInventory turns on the suppliers, the names and units of the ingredients, and the
// stocktakes, which are kept in the store; the stock itself is always in the log.
func WithInventory(store *inventory.Store) APIOption {
	return func(a *API) { a.inv = store }
}

// Location is the venue's time zone.
func (s *Service) Location() *time.Location { return s.loc }

// warehouseRoutes are the ones that need no database: what the service knows from the log.
func (a *API) warehouseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/warehouse", a.auth(a.warehouse))
	mux.HandleFunc("GET /api/warehouse/ledger", a.auth(a.ledger))
	mux.HandleFunc("GET /api/warehouse/receipts", a.auth(a.receipts))
	mux.HandleFunc("GET /api/warehouse/waste", a.auth(a.waste))
	mux.HandleFunc("POST /api/warehouse/receive", a.auth(a.receiveGoods))
	mux.HandleFunc("POST /api/stock/adjust", a.auth(a.moveStock(stock.Adjust)))
}

// inventoryRoutes are the ones that need the store.
func (a *API) inventoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/suppliers", a.auth(a.suppliers))
	mux.HandleFunc("POST /api/suppliers", a.auth(a.createSupplier))
	mux.HandleFunc("PUT /api/suppliers/{id}", a.auth(a.updateSupplier))
	mux.HandleFunc("PUT /api/ingredients/{ingredient}", a.auth(a.saveInfo))

	mux.HandleFunc("GET /api/stocktakes", a.auth(a.stocktakes))
	mux.HandleFunc("GET /api/stocktakes/current", a.auth(a.currentStocktake))
	mux.HandleFunc("POST /api/stocktakes", a.auth(a.openStocktake))
	mux.HandleFunc("GET /api/stocktakes/{id}", a.auth(a.getStocktake))
	mux.HandleFunc("PUT /api/stocktakes/{id}/lines", a.auth(a.countLine))
	mux.HandleFunc("DELETE /api/stocktakes/{id}/lines/{ingredient}", a.auth(a.uncountLine))
	mux.HandleFunc("POST /api/stocktakes/{id}/apply", a.auth(a.applyStocktake))
	mux.HandleFunc("POST /api/stocktakes/{id}/cancel", a.auth(a.cancelStocktake))
}

// failInventory answers an error of the store, or of the stock.
func (a *API) failInventory(w http.ResponseWriter, err error) {
	var inv *inventory.InvalidError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: strings.TrimPrefix(err.Error(), "inventory: "), Code: "invalid"})
	case errors.Is(err, inventory.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not found", Code: "not_found"})
	case errors.Is(err, inventory.ErrExists), errors.Is(err, inventory.ErrNotOpen), errors.Is(err, inventory.ErrAnotherOpen):
		writeJSON(w, http.StatusConflict, errorBody{Error: strings.TrimPrefix(err.Error(), "inventory: "), Code: "conflict"})
	case isStoreError(err):
		a.logger.Error("the warehouse database failed", "err", err)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the database did not answer", Code: "unavailable"})
	default:
		a.fail(w, err)
	}
}

// isStoreError says that an error is not one of ours: a database that did not answer.
func isStoreError(err error) bool {
	if err == nil {
		return false
	}
	if _, kind := status(err); kind != "internal" {
		return false
	}
	return true
}

func (a *API) need(w http.ResponseWriter, actor rbac.Actor, p rbac.Permission) bool {
	if !actor.Can(p) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, p))
		return false
	}
	return true
}

// ---- what the service knows ------------------------------------------------------------

func (a *API) warehouse(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	v, err := a.svc.Warehouse(actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := map[string]any{"rows": v.Rows, "warning_hours": v.WarningHours, "costs": v.Costs, "ledger_cap": ledgerCap}
	if a.inv != nil {
		ctx := r.Context()
		infos, err := a.inv.Infos(ctx)
		if err != nil {
			a.failInventory(w, err)
			return
		}
		sups, err := a.inv.Suppliers(ctx, true)
		if err != nil {
			a.failInventory(w, err)
			return
		}
		out["infos"], out["suppliers"] = infos, sups
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ledger(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	v, err := a.svc.Ledger(actor, r.URL.Query().Get("ingredient"), limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) receipts(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	v, err := a.svc.Receipts(actor, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": v})
}

// day reads a date like 2026-11-14 in the venue's time zone.
func (a *API) day(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, a.svc.Location())
	if err != nil {
		return time.Time{}, &inventory.InvalidError{Reason: fmt.Sprintf("%q is not a date like 2026-11-14", s)}
	}
	return t, nil
}

// waste adds up the losses between two days, both included; by default the last 30 days.
func (a *API) waste(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	loc := a.svc.Location()
	today := a.svc.now().In(loc)
	toDay := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	fromDay := toDay.AddDate(0, 0, -29)
	var err error
	if s := r.URL.Query().Get("from"); s != "" {
		if fromDay, err = a.day(s); err != nil {
			a.failInventory(w, err)
			return
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		if toDay, err = a.day(s); err != nil {
			a.failInventory(w, err)
			return
		}
	}
	if toDay.Before(fromDay) || toDay.Sub(fromDay) > 366*24*time.Hour {
		a.failInventory(w, &inventory.InvalidError{Reason: "the period is from one day to a year"})
		return
	}
	v, err := a.svc.Waste(actor, fromDay, toDay.AddDate(0, 0, 1))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": fromDay.Format("2006-01-02"), "to": toDay.Format("2006-01-02"), "rows": v.Rows, "truncated": v.Truncated,
		"costs": mayCost(actor),
	})
}

// checkSupplier refuses a supplier that is not there or is not in use any more.
func (a *API) checkSupplier(r *http.Request, id string) error {
	if id == "" {
		return nil
	}
	if a.inv == nil {
		return &inventory.InvalidError{Reason: "there are no suppliers without a database"}
	}
	sp, err := a.inv.Supplier(r.Context(), id)
	if errors.Is(err, inventory.ErrNotFound) {
		return &inventory.InvalidError{Reason: fmt.Sprintf("there is no supplier %q", id)}
	}
	if err != nil {
		return err
	}
	if !sp.Active {
		return &inventory.InvalidError{Reason: fmt.Sprintf("the supplier %q is not in use any more", id)}
	}
	return nil
}

func (a *API) receiveGoods(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Command  string `json:"command_id"`
		Supplier string `json:"supplier"`
		Document string `json:"document"`
		Lines    []struct {
			Ingredient string `json:"ingredient"`
			Qty        int64  `json:"qty"`
			TotalCost  int64  `json:"total_cost"`
			Expires    string `json:"expires"`
		} `json:"lines"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.need(w, actor, rbac.ReceiveStock) {
		return
	}
	if err := a.checkSupplier(r, b.Supplier); err != nil {
		a.failInventory(w, err)
		return
	}
	in := ReceiptInput{Supplier: b.Supplier, Document: b.Document}
	for _, l := range b.Lines {
		in.Lines = append(in.Lines, ReceiptLine{Ingredient: l.Ingredient, Qty: l.Qty, TotalCost: l.TotalCost, Expires: l.Expires})
	}
	if _, err := a.svc.ReceiveGoods(r.Context(), actor, b.Command, in); err != nil {
		a.fail(w, err)
		return
	}
	a.warehouse(w, r, actor)
}

// ---- suppliers and ingredients --------------------------------------------------------

func (a *API) suppliers(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.need(w, actor, rbac.ViewStock) {
		return
	}
	list, err := a.inv.Suppliers(r.Context(), r.URL.Query().Get("all") == "1")
	if err != nil {
		a.failInventory(w, err)
		return
	}
	if list == nil {
		list = []inventory.Supplier{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"suppliers": list})
}

type supplierBody struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Phone  string `json:"phone"`
	Note   string `json:"note"`
	Active *bool  `json:"active"`
}

func (a *API) createSupplier(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b supplierBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.need(w, actor, rbac.ManageWarehouse) {
		return
	}
	sp, err := a.inv.CreateSupplier(r.Context(), inventory.Supplier{ID: b.ID, Name: b.Name, Phone: b.Phone, Note: b.Note}, actor.ID)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"supplier": sp})
}

func (a *API) updateSupplier(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b supplierBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.need(w, actor, rbac.ManageWarehouse) {
		return
	}
	if b.ID != "" && b.ID != r.PathValue("id") {
		a.failInventory(w, &inventory.InvalidError{Reason: "a supplier's ID cannot be changed"})
		return
	}
	active := true
	if b.Active != nil {
		active = *b.Active
	}
	sp, err := a.inv.UpdateSupplier(r.Context(), inventory.Supplier{ID: r.PathValue("id"), Name: b.Name, Phone: b.Phone, Note: b.Note, Active: active}, actor.ID)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"supplier": sp})
}

func (a *API) saveInfo(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Name      string `json:"name"`
		Unit      string `json:"unit"`
		ShelfDays int    `json:"shelf_days"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.need(w, actor, rbac.ManageWarehouse) {
		return
	}
	ing := r.PathValue("ingredient")
	if _, known := a.svc.StockQty(ing); !known {
		a.fail(w, fmt.Errorf("%w: %q", ErrUnknownIngredient, ing))
		return
	}
	i, err := a.inv.SaveInfo(r.Context(), inventory.Info{Ingredient: ing, Name: b.Name, Unit: b.Unit, ShelfDays: b.ShelfDays}, actor.ID)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": i})
}

// ---- stocktakes -----------------------------------------------------------------------

func stocktakeID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, &inventory.InvalidError{Reason: "a stocktake is a number from 1"}
	}
	return id, nil
}

func (a *API) stocktakes(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	list, err := a.inv.Stocktakes(r.Context(), 50)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	if list == nil {
		list = []inventory.Stocktake{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stocktakes": list})
}

func (a *API) currentStocktake(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	t, err := a.inv.Current(r.Context())
	if errors.Is(err, inventory.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"stocktake": nil})
		return
	}
	if err != nil {
		a.failInventory(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stocktake": t})
}

func (a *API) getStocktake(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	id, err := stocktakeID(r)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	t, err := a.inv.Stocktake(r.Context(), id)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stocktake": t})
}

func (a *API) openStocktake(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Note string `json:"note"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	t, err := a.inv.OpenStocktake(r.Context(), actor.ID, b.Note)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"stocktake": t})
}

// countLine records what was found of an ingredient. The service says what the stock holds at this
// moment, and that is kept with the line: applying the stocktake writes the difference between
// the two, so that what was sold since the count is not taken off twice.
func (a *API) countLine(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Ingredient string `json:"ingredient"`
		Counted    int64  `json:"counted"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	id, err := stocktakeID(r)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	expected, known := a.svc.StockQty(b.Ingredient)
	if !known {
		a.fail(w, fmt.Errorf("%w: %q", ErrUnknownIngredient, b.Ingredient))
		return
	}
	if err := a.inv.Count(r.Context(), id, inventory.Line{Ingredient: b.Ingredient, Counted: b.Counted, Expected: expected}, actor.ID); err != nil {
		a.failInventory(w, err)
		return
	}
	a.getStocktake(w, r, actor)
}

func (a *API) uncountLine(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	id, err := stocktakeID(r)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	if err := a.inv.Uncount(r.Context(), id, r.PathValue("ingredient")); err != nil {
		a.failInventory(w, err)
		return
	}
	a.getStocktake(w, r, actor)
}

func (a *API) cancelStocktake(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	id, err := stocktakeID(r)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	if err := a.inv.CancelStocktake(r.Context(), id, actor.ID); err != nil {
		a.failInventory(w, err)
		return
	}
	a.getStocktake(w, r, actor)
}

// AppliedLine says what applying a stocktake did to one ingredient.
type AppliedLine struct {
	Ingredient string `json:"ingredient"`
	// Variance is what the count found against what the stock said then; Applied is what was
	// written, which is less when what is missing is more than there is now (it was used since).
	Variance int64 `json:"variance"`
	Applied  int64 `json:"applied"`
}

// applyStocktake writes the differences of a stocktake to the log, one adjustment for each
// ingredient that differs, under the IDs stocktake-<number>-<line>: if it is cut short, applying it again
// finishes it, and nothing is written twice.
func (a *API) applyStocktake(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !a.need(w, actor, rbac.AdjustStock) {
		return
	}
	id, err := stocktakeID(r)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	t, err := a.inv.BeginApply(r.Context(), id, actor.ID)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	out := []AppliedLine{}
	for i, l := range t.Lines {
		v := l.Variance()
		if v == 0 {
			continue
		}
		cur, _ := a.svc.StockQty(l.Ingredient)
		eff := v
		if eff < -cur {
			eff = -cur
		}
		if eff != 0 {
			cmd := fmt.Sprintf("stocktake-%d-%03d", id, i+1)
			if _, err := a.svc.MoveStockWith(r.Context(), actor, cmd, StockInput{Kind: stock.Adjust, Ingredient: l.Ingredient, Qty: eff,
				Reason: fmt.Sprintf("stocktake %d", id)}); err != nil {
				// the stocktake stays "applying": the same request finishes it
				a.failInventory(w, err)
				return
			}
		}
		out = append(out, AppliedLine{Ingredient: l.Ingredient, Variance: v, Applied: eff})
	}
	if err := a.inv.FinishApply(r.Context(), id); err != nil {
		a.failInventory(w, err)
		return
	}
	done, err := a.inv.Stocktake(r.Context(), id)
	if err != nil {
		a.failInventory(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stocktake": done, "applied": out})
}
