package orderservice

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// ErrBadStockInput is returned for a stock command whose fields do not make sense; the
// message says which.
var ErrBadStockInput = errors.New("orderservice: the stock command is not valid")

func badStock(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadStockInput, fmt.Sprintf(format, args...))
}

// maxReceiptLines bounds the lines of one goods receipt.
const maxReceiptLines = 50

// StockInput is a movement of the stock that a person makes.
type StockInput struct {
	Kind       stock.Kind
	Ingredient string
	// Qty is the amount; for an adjustment it is signed.
	Qty    int64
	Reason string

	// For goods received: the last day the lot may be used ("2026-12-31", in the venue's time zone;
	// empty for none), what was paid for all of it in whole đồng (0 if not known), the ID of the
	// supplier and the number of the document that came with it.
	Expires   string
	TotalCost int64
	Supplier  string
	Document  string
	// For a write-off: the lot to take from (empty: the lots in order), and why.
	Lot   string
	Cause string
}

// ReceiptLine is one ingredient on a goods receipt.
type ReceiptLine struct {
	Ingredient string
	Qty        int64
	TotalCost  int64
	Expires    string
}

// ReceiptInput is a delivery: what came, from whom, on which document.
type ReceiptInput struct {
	Supplier string
	Document string
	Lines    []ReceiptLine
}

// expiryOf reads the last day a lot may be used and returns the moment it stops being usable: the
// start of the day after, in the venue's time zone.
func (s *Service) expiryOf(day string) (*time.Time, error) {
	if strings.TrimSpace(day) == "" {
		return nil, nil
	}
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(day), s.loc)
	if err != nil {
		return nil, badStock("%q is not a date like 2026-12-31", day)
	}
	t := d.AddDate(0, 0, 1)
	return &t, nil
}

func (s *Service) stockMoveOf(actor rbac.Actor, id string, in StockInput) (StockMove, error) {
	if in.Kind != stock.Receive && in.Kind != stock.WriteOff && in.Kind != stock.Count && in.Kind != stock.Adjust {
		return StockMove{}, fmt.Errorf("orderservice: %q is not a movement a person can make", in.Kind)
	}
	if len(in.Supplier) > 32 || len(in.Document) > 40 || len(in.Lot) > 80 || len(in.Reason) > 200 {
		return StockMove{}, badStock("a supplier is at most 32 characters, a document 40 and a reason 200")
	}
	if in.Kind != stock.Receive && (in.Expires != "" || in.TotalCost != 0 || in.Supplier != "" || in.Document != "") {
		return StockMove{}, badStock("a date, a price, a supplier and a document are for goods received")
	}
	if in.Kind != stock.WriteOff && (in.Lot != "" || in.Cause != "") {
		return StockMove{}, badStock("a lot and a cause are for a write-off")
	}
	exp, err := s.expiryOf(in.Expires)
	if err != nil {
		return StockMove{}, err
	}
	return StockMove{ID: id, By: actor.ID, Roles: append([]rbac.Role(nil), actor.Roles...), At: s.at(), Kind: in.Kind,
		Ingredient: in.Ingredient, Qty: in.Qty, Reason: in.Reason, Expires: exp, TotalCost: in.TotalCost,
		Supplier: in.Supplier, Document: in.Document, Lot: in.Lot, Cause: in.Cause}, nil
}

func stockNeed(kind stock.Kind) rbac.Permission {
	if kind == stock.Receive {
		return rbac.ReceiveStock
	}
	return rbac.AdjustStock
}

// MoveStock records goods received, something written off, or a stocktake. cmd is the ID of
// the command. The movement is tried on a copy of the stock first, so that one the stock
// would refuse is never written to the log.
func (s *Service) MoveStock(ctx context.Context, actor rbac.Actor, cmd string, kind stock.Kind, ingredient string, qty int64, reason string) (StockView, error) {
	return s.MoveStockWith(ctx, actor, cmd, StockInput{Kind: kind, Ingredient: ingredient, Qty: qty, Reason: reason})
}

// MoveStockWith is MoveStock with everything a movement can say: the date, price, supplier and
// document of a lot, the lot and the cause of a write-off.
func (s *Service) MoveStockWith(ctx context.Context, actor rbac.Actor, cmd string, in StockInput) (StockView, error) {
	if err := validCommand(cmd); err != nil {
		return StockView{}, err
	}
	if err := actor.Validate(); err != nil {
		return StockView{}, err
	}
	if need := stockNeed(in.Kind); !actor.Can(need) {
		return StockView{}, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, need)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFresh(ctx); err != nil {
		return StockView{}, err
	}
	if s.st.moveSeen[cmd] {
		return s.stockView(), nil // a repeat of a command that worked
	}
	if !s.ingredients[stock.Ingredient(in.Ingredient)] {
		return StockView{}, fmt.Errorf("%w: %q", ErrUnknownIngredient, in.Ingredient)
	}
	mv, err := s.stockMoveOf(actor, cmd, in)
	if err != nil {
		return StockView{}, err
	}
	if err := s.st.stock.Clone().Apply(mv.Movement()); err != nil {
		return StockView{}, err
	}
	if err := s.appendRecord(ctx, Record{Stock: &mv}); err != nil {
		return StockView{}, err
	}
	return s.stockView(), nil
}

// ReceiveGoods records a delivery: one lot for each line, all with the same supplier and document.
// The lines are written one by one under the IDs cmd-01, cmd-02 and so on, so a delivery that was
// cut short by an error is finished by sending it again with the same command ID: the lines that
// were written are not written twice. Before anything is written, every line is tried, in order,
// on a copy of the stock.
func (s *Service) ReceiveGoods(ctx context.Context, actor rbac.Actor, cmd string, in ReceiptInput) (StockView, error) {
	if err := validCommand(cmd); err != nil {
		return StockView{}, err
	}
	if len(cmd) > 60 {
		return StockView{}, badStock("a command ID of a delivery is at most 60 characters")
	}
	if err := actor.Validate(); err != nil {
		return StockView{}, err
	}
	if !actor.Can(rbac.ReceiveStock) {
		return StockView{}, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ReceiveStock)
	}
	if len(in.Lines) == 0 || len(in.Lines) > maxReceiptLines {
		return StockView{}, badStock("a delivery has from 1 to %d lines", maxReceiptLines)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFresh(ctx); err != nil {
		return StockView{}, err
	}
	var moves []StockMove
	trial := s.st.stock.Clone()
	for i, l := range in.Lines {
		id := fmt.Sprintf("%s-%02d", cmd, i+1)
		if s.st.moveSeen[id] {
			continue
		}
		if !s.ingredients[stock.Ingredient(l.Ingredient)] {
			return StockView{}, fmt.Errorf("%w: %q (line %d)", ErrUnknownIngredient, l.Ingredient, i+1)
		}
		mv, err := s.stockMoveOf(actor, id, StockInput{Kind: stock.Receive, Ingredient: l.Ingredient, Qty: l.Qty, Expires: l.Expires,
			TotalCost: l.TotalCost, Supplier: in.Supplier, Document: in.Document})
		if err != nil {
			return StockView{}, fmt.Errorf("line %d: %w", i+1, err)
		}
		if err := trial.Apply(mv.Movement()); err != nil {
			return StockView{}, fmt.Errorf("line %d: %w", i+1, err)
		}
		moves = append(moves, mv)
	}
	for i := range moves {
		if err := s.appendRecord(ctx, Record{Stock: &moves[i]}); err != nil {
			return StockView{}, err
		}
	}
	return s.stockView(), nil
}

// ---- what the storekeeper sees ---------------------------------------------------------

// LotView is a lot of an ingredient.
type LotView struct {
	ID         string     `json:"id"`
	Qty        int64      `json:"qty"`
	Received   int64      `json:"received"`
	ReceivedAt time.Time  `json:"received_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	// State is "expired", "soon" (within the warning time) or empty.
	State    string `json:"state,omitempty"`
	Supplier string `json:"supplier,omitempty"`
	Document string `json:"document,omitempty"`
	// UnitCostMilli is the price of one unit in thousandths of a đồng; only for those who may see
	// costs, and 0 where it is not known.
	UnitCostMilli int64 `json:"unit_cost_milli,omitempty"`
}

// WarehouseRow is one ingredient with its lots.
type WarehouseRow struct {
	Ingredient string `json:"ingredient"`
	Qty        int64  `json:"qty"`
	// Usable is what is not in an expired lot.
	Usable int64     `json:"usable"`
	LowAt  int64     `json:"low_at,omitempty"`
	Low    bool      `json:"low,omitempty"`
	Soon   int64     `json:"soon,omitempty"`
	Gone   int64     `json:"expired,omitempty"`
	Lots   []LotView `json:"lots"`
	// ValueMilli and Unpriced are only for those who may see costs: what the stock is worth at the
	// price of its lots, and how much of the quantity has no price.
	ValueMilli *int64 `json:"value_milli,omitempty"`
	Unpriced   int64  `json:"unpriced,omitempty"`
}

// WarehouseView is the stock with its lots and what needs attention.
type WarehouseView struct {
	Rows []WarehouseRow `json:"rows"`
	// WarningHours is how long before it expires a lot is flagged as "soon".
	WarningHours int `json:"warning_hours"`
	// Costs says that prices and values are shown.
	Costs bool `json:"costs"`
}

func mayCost(a rbac.Actor) bool { return a.Can(rbac.ViewCosts) }

// Warehouse returns every ingredient with its lots. A person who may not see costs gets none.
func (s *Service) Warehouse(actor rbac.Actor) (WarehouseView, error) {
	if !actor.Can(rbac.ViewStock) {
		return WarehouseView{}, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ViewStock)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.at()
	costs := mayCost(actor)
	names := map[stock.Ingredient]bool{}
	for ing := range s.st.stock.Snapshot() {
		names[ing] = true
	}
	for ing := range s.ingredients {
		names[ing] = true
	}
	low := map[stock.Ingredient]bool{}
	for _, ing := range s.st.stock.Low() {
		low[ing] = true
	}
	v := WarehouseView{WarningHours: int(s.expiryWarn / time.Hour), Costs: costs}
	for ing := range names {
		row := WarehouseRow{Ingredient: string(ing), Qty: s.st.stock.Qty(ing), Usable: s.st.stock.Usable(ing, now),
			LowAt: s.st.stock.LowAt(ing), Low: low[ing], Lots: []LotView{}}
		for _, l := range s.st.stock.Lots(ing) {
			lv := LotView{ID: l.ID, Qty: l.Qty, Received: l.Received, ReceivedAt: l.ReceivedAt, Supplier: l.Supplier, Document: l.Document}
			if !l.ExpiresAt.IsZero() {
				e := l.ExpiresAt
				lv.ExpiresAt = &e
				switch {
				case !now.Before(e):
					lv.State = "expired"
					row.Gone += l.Qty
				case e.Sub(now) <= s.expiryWarn:
					lv.State = "soon"
					row.Soon += l.Qty
				}
			}
			if costs {
				lv.UnitCostMilli = l.UnitCostMilli
			}
			row.Lots = append(row.Lots, lv)
		}
		if costs {
			val, unpriced := s.st.stock.ValueMilli(ing)
			row.ValueMilli, row.Unpriced = &val, unpriced
		}
		v.Rows = append(v.Rows, row)
	}
	sort.Slice(v.Rows, func(i, j int) bool { return v.Rows[i].Ingredient < v.Rows[j].Ingredient })
	return v, nil
}

// LedgerView is the history of one ingredient.
type LedgerView struct {
	Ingredient string        `json:"ingredient"`
	Entries    []LedgerEntry `json:"entries"`
	// Truncated says that older movements were dropped to bound memory; the log has them.
	Truncated bool `json:"truncated,omitempty"`
}

func stripCost(e LedgerEntry, costs bool) LedgerEntry {
	if !costs {
		e.ValueMilli = 0
	}
	return e
}

// Ledger returns the movements of an ingredient, newest first.
func (s *Service) Ledger(actor rbac.Actor, ingredient string, limit int) (LedgerView, error) {
	if !actor.Can(rbac.ViewStock) {
		return LedgerView{}, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ViewStock)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	list, dropped := s.st.Ledger(ingredient, limit)
	costs := mayCost(actor)
	v := LedgerView{Ingredient: ingredient, Entries: make([]LedgerEntry, 0, len(list)), Truncated: dropped}
	for _, e := range list {
		v.Entries = append(v.Entries, stripCost(e, costs))
	}
	return v, nil
}

// ReceiptView is a delivery, from the lines that were written.
type ReceiptView struct {
	Document string    `json:"document"`
	Supplier string    `json:"supplier,omitempty"`
	At       time.Time `json:"at"`
	By       string    `json:"by"`
	Lines    []struct {
		Ingredient string `json:"ingredient"`
		Qty        int64  `json:"qty"`
		Lot        string `json:"lot"`
		// ValueMilli is the price of the line; only for those who may see costs.
		ValueMilli int64 `json:"value_milli,omitempty"`
	} `json:"lines"`
}

// Receipts returns the latest deliveries, newest first, grouped by supplier and document. A line
// with no document is a delivery of its own.
func (s *Service) Receipts(actor rbac.Actor, limit int) ([]ReceiptView, error) {
	if !actor.Can(rbac.ViewStock) {
		return nil, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ViewStock)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	costs := mayCost(actor)
	type key struct {
		supplier, doc, by string
		day               string
	}
	groups := map[key]*ReceiptView{}
	var order []key
	var all []LedgerEntry
	for _, ing := range s.st.ledgerIngredients() {
		for _, e := range s.st.ledger[stock.Ingredient(ing)] {
			if e.Kind == stock.Receive {
				all = append(all, e)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Offset > all[j].Offset })
	for _, e := range all {
		k := key{e.Supplier, e.Document, e.By, e.At.In(s.loc).Format("2006-01-02")}
		if e.Document == "" {
			k.doc = e.Ref // no document: its own
		}
		g := groups[k]
		if g == nil {
			if len(order) == limit {
				continue
			}
			g = &ReceiptView{Document: e.Document, Supplier: e.Supplier, At: e.At, By: e.By}
			groups[k] = g
			order = append(order, k)
		}
		line := struct {
			Ingredient string `json:"ingredient"`
			Qty        int64  `json:"qty"`
			Lot        string `json:"lot"`
			ValueMilli int64  `json:"value_milli,omitempty"`
		}{e.Ingredient, e.Delta, e.Lot, 0}
		if costs {
			line.ValueMilli = e.ValueMilli
		}
		g.Lines = append(g.Lines, line)
	}
	out := make([]ReceiptView, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out, nil
}

// WasteRow is what was lost of one ingredient for one cause.
type WasteRow struct {
	Ingredient string `json:"ingredient"`
	Cause      string `json:"cause"`
	Qty        int64  `json:"qty"`
	// ValueMilli is what it cost, at the price of the lots; only for those who may see costs.
	ValueMilli int64 `json:"value_milli,omitempty"`
}

// WasteView is what was written off or found missing in a period.
type WasteView struct {
	From, To time.Time
	Rows     []WasteRow `json:"rows"`
	// Truncated says that the history of some ingredient does not reach back to From.
	Truncated bool `json:"truncated,omitempty"`
}

// CauseStocktake is the cause given to what a stocktake found missing.
const CauseStocktake = "stocktake"

// Waste adds up what was written off, and what stocktakes found missing, from the history of each
// ingredient, between two moments (the end is not included).
func (s *Service) Waste(actor rbac.Actor, from, to time.Time) (WasteView, error) {
	if !actor.Can(rbac.ViewStock) {
		return WasteView{}, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ViewStock)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	costs := mayCost(actor)
	type key struct{ ing, cause string }
	sums := map[key]*WasteRow{}
	v := WasteView{From: from, To: to}
	for _, ing := range s.st.ledgerIngredients() {
		list := s.st.ledger[stock.Ingredient(ing)]
		if s.st.ledgerDropped[stock.Ingredient(ing)] && len(list) > 0 && list[0].At.After(from) {
			v.Truncated = true
		}
		for _, e := range list {
			if e.At.Before(from) || !e.At.Before(to) {
				continue
			}
			var cause string
			switch {
			case e.Kind == stock.WriteOff:
				cause = e.Cause
				if cause == "" {
					cause = stock.CauseOther
				}
			case (e.Kind == stock.Count || e.Kind == stock.Adjust) && e.Delta < 0:
				cause = CauseStocktake
			default:
				continue
			}
			k := key{ing, cause}
			r := sums[k]
			if r == nil {
				r = &WasteRow{Ingredient: ing, Cause: cause}
				sums[k] = r
			}
			r.Qty += -e.Delta
			if costs {
				r.ValueMilli += e.ValueMilli
			}
		}
	}
	for _, r := range sums {
		v.Rows = append(v.Rows, *r)
	}
	sort.Slice(v.Rows, func(i, j int) bool {
		if v.Rows[i].Ingredient != v.Rows[j].Ingredient {
			return v.Rows[i].Ingredient < v.Rows[j].Ingredient
		}
		return v.Rows[i].Cause < v.Rows[j].Cause
	})
	return v, nil
}

// StockQty is what the stocktake needs: the quantity of an ingredient now, and whether the menu
// knows it.
func (s *Service) StockQty(ingredient string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ing := stock.Ingredient(ingredient)
	_, listed := s.st.stock.Snapshot()[ing]
	return s.st.stock.Qty(ing), listed || s.ingredients[ing]
}
