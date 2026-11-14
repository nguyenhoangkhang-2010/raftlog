package orderservice

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/expenses"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
)

// WithExpenses turns on the writing down of what the venue spends, and the profit report that sets it
// against what was earned. It needs the reports, which have the costs of the goods.
func WithExpenses(store *expenses.Store) APIOption {
	return func(a *API) { a.expenses = store }
}

func (a *API) expenseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/expenses", a.auth(a.listExpenses))
	mux.HandleFunc("POST /api/expenses", a.auth(a.addExpense))
	mux.HandleFunc("POST /api/expenses/{id}/void", a.auth(a.voidExpense))
}

// mayCost is for the reports of what things cost: a person who reads the reports and may see costs.
func mayReadCosts(actor rbac.Actor) bool {
	return actor.Can(rbac.ViewReports) && actor.Can(rbac.ViewCosts)
}

func (a *API) failExpense(w http.ResponseWriter, err error) {
	var inv *expenses.InvalidError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: strings.TrimPrefix(err.Error(), "expenses: "), Code: "invalid"})
	case errors.Is(err, expenses.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not found", Code: "not_found"})
	case errors.Is(err, expenses.ErrVoided):
		writeJSON(w, http.StatusConflict, errorBody{Error: strings.TrimPrefix(err.Error(), "expenses: "), Code: "conflict"})
	default:
		if _, kind := status(err); kind == "internal" {
			a.logger.Error("the expenses database failed", "err", err)
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the database did not answer", Code: "unavailable"})
			return
		}
		a.fail(w, err)
	}
}

func (a *API) listExpenses(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !actor.Can(rbac.ManageExpenses) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ManageExpenses))
		return
	}
	p := a.periodOf(r)
	list, err := a.expenses.List(r.Context(), p.From, p.To, r.URL.Query().Get("voided") == "1")
	if err != nil {
		a.failExpense(w, err)
		return
	}
	cats, err := a.expenses.Categories(r.Context())
	if err != nil {
		a.failExpense(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": p.From, "to": p.To, "expenses": list, "categories": cats})
}

func (a *API) addExpense(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Category string `json:"category"`
		Amount   int64  `json:"amount"`
		From     string `json:"from"`
		To       string `json:"to"`
		Note     string `json:"note"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !actor.Can(rbac.ManageExpenses) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ManageExpenses))
		return
	}
	e, err := a.expenses.Add(r.Context(), expenses.Expense{Category: b.Category, Amount: b.Amount, From: b.From, To: b.To, Note: b.Note}, actor.ID)
	if err != nil {
		a.failExpense(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"expense": e})
}

func (a *API) voidExpense(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	if !actor.Can(rbac.ManageExpenses) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ManageExpenses))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		a.failExpense(w, &expenses.InvalidError{Reason: "an expense is a number from 1"})
		return
	}
	e, err := a.expenses.Void(r.Context(), id, actor.ID, b.Reason)
	if err != nil {
		a.failExpense(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expense": e})
}

// ---- the reports of what things cost --------------------------------------------------------

// ProfitDay is one day of what was earned and what it cost, in whole đồng.
type ProfitDay struct {
	Date      string `json:"date"`
	Collected int64  `json:"collected"`
	Refunds   int64  `json:"refunds"`
	Net       int64  `json:"net"`
	// GoodsCost is what the goods of the orders paid that day cost; Waste what was thrown away or found
	// missing. GrossProfit is net less both.
	GoodsCost   int64 `json:"goods_cost"`
	Waste       int64 `json:"waste"`
	GrossProfit int64 `json:"gross_profit"`
	// Expenses is what the venue spent, spread over the days that it covers; Labour is what the work that
	// was clocked on the day cost at the hourly rates. Profit is what is left.
	Expenses int64 `json:"expenses"`
	Labour   int64 `json:"labour"`
	Profit   int64 `json:"profit"`
}

// milliToDong rounds thousandths of a đồng to whole đồng.
func milliToDong(m int64) int64 {
	if m < 0 {
		return -((-m + 500) / 1000)
	}
	return (m + 500) / 1000
}

// profit puts the sales, the cost of the goods, the waste, the expenses and the labour of each day side by
// side. It also says how many minutes of work had no hourly rate, and so cost nothing.
func (a *API) profit(r *http.Request, p reports.Period) ([]ProfitDay, []expenses.DayAmount, int64, error) {
	sales, err := a.reports.SalesByDay(r.Context(), p)
	if err != nil {
		return nil, nil, 0, err
	}
	costs, err := a.reports.CostsByDay(r.Context(), p)
	if err != nil {
		return nil, nil, 0, err
	}
	var spent []expenses.DayAmount
	if a.expenses != nil {
		if spent, err = a.expenses.Allocated(r.Context(), p.From, p.To); err != nil {
			return nil, nil, 0, err
		}
	}
	labour := map[string]int64{}
	var unrated int64
	if a.staff != nil {
		days, err := a.staff.LabourByDay(r.Context(), p.From, p.To)
		if err != nil {
			return nil, nil, 0, err
		}
		for _, d := range days {
			labour[d.Day] = d.Cost
			unrated += d.UnratedMin
		}
	}
	perDay := map[string]int64{}
	for _, e := range spent {
		perDay[e.Day] += e.Amount
	}
	byDate := map[string]reports.CostDay{}
	for _, c := range costs {
		byDate[c.Date] = c
	}
	out := make([]ProfitDay, 0, len(sales))
	for _, s := range sales {
		c := byDate[s.Date]
		d := ProfitDay{Date: s.Date, Collected: s.Total, Refunds: s.Refunds, Net: s.Net, GoodsCost: milliToDong(c.CogsMilli), Waste: milliToDong(c.WasteMilli), Expenses: perDay[s.Date], Labour: labour[s.Date]}
		d.GrossProfit = d.Net - d.GoodsCost - d.Waste
		d.Profit = d.GrossProfit - d.Expenses - d.Labour
		out = append(out, d)
	}
	return out, spent, unrated, nil
}

// categoriesOf adds up what was spent, by category over the period.
func categoriesOf(spent []expenses.DayAmount) []map[string]any {
	sums := map[string]int64{}
	for _, e := range spent {
		sums[e.Category] += e.Amount
	}
	names := make([]string, 0, len(sums))
	for c := range sums {
		names = append(names, c)
	}
	sort.Slice(names, func(i, j int) bool {
		if sums[names[i]] != sums[names[j]] {
			return sums[names[i]] > sums[names[j]]
		}
		return names[i] < names[j]
	})
	out := make([]map[string]any, 0, len(names))
	for _, c := range names {
		out = append(out, map[string]any{"category": c, "amount": sums[c]})
	}
	return out
}

// CostLine is one ingredient of a recipe and what it costs.
type CostLine struct {
	Ingredient string `json:"ingredient"`
	Qty        int64  `json:"qty"`
	// UnitCostMilli is what one unit cost the last time it was bought, in thousandths of a đồng; 0 if
	// it was never bought with a price.
	UnitCostMilli int64 `json:"unit_cost_milli"`
	CostMilli     int64 `json:"cost_milli"`
}

// ItemCost is what an item costs to make by its recipe, at the latest prices paid.
type ItemCost struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Price    int64  `json:"price"`
	// Cost is the goods of one, in whole đồng; Margin is the price less the cost and MarginPermille is
	// that as a part of the price, in thousandths (a margin of 63.5% is 635). Complete is false when an
	// ingredient has no price yet, and the cost is then less than it is.
	Cost           int64      `json:"cost"`
	Margin         int64      `json:"margin"`
	MarginPermille int64      `json:"margin_permille"`
	Complete       bool       `json:"complete"`
	Missing        []string   `json:"missing,omitempty"`
	Lines          []CostLine `json:"lines"`
}

// itemCosts works out what each item of the menu costs, from its recipe and the latest price of each
// ingredient. Options and combos are not included: an item is its base recipe.
func (a *API) itemCosts(r *http.Request) ([]ItemCost, error) {
	unit, err := a.reports.LatestUnitCosts(r.Context())
	if err != nil {
		return nil, err
	}
	out := []ItemCost{}
	for _, it := range a.svc.Recipes() {
		c := ItemCost{ID: it.ID, Name: it.Name, Category: it.Category, Price: it.Price, Complete: true, Lines: []CostLine{}}
		var milli int64
		names := make([]string, 0, len(it.Recipe))
		for ing := range it.Recipe {
			names = append(names, ing)
		}
		sort.Strings(names)
		for _, ing := range names {
			qty := it.Recipe[ing]
			l := CostLine{Ingredient: ing, Qty: qty, UnitCostMilli: unit[ing], CostMilli: qty * unit[ing]}
			if unit[ing] == 0 {
				c.Complete = false
				c.Missing = append(c.Missing, ing)
			}
			milli += l.CostMilli
			c.Lines = append(c.Lines, l)
		}
		c.Cost = milliToDong(milli)
		c.Margin = c.Price - c.Cost
		if c.Price > 0 {
			c.MarginPermille = c.Margin * 1000 / c.Price
		}
		out = append(out, c)
	}
	return out, nil
}
