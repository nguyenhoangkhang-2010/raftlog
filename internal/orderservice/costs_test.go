package orderservice

import (
	"bytes"
	"encoding/csv"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/expenses"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
)

// priced makes the goods of the tests come with prices: 100 breads for 150,000 (1,500 each) and 2,000 g
// of cheese for 300,000 (150 per gram), so that a toastie costs 2 x 1,500 + 40 x 150 = 9,000.
func (r *rig) priced() {
	r.t.Helper()
	if _, err := r.svc.ReceiveGoods(r.ctx, storekeeper, r.cmd(), ReceiptInput{Supplier: "sup-1", Document: "INV-1", Lines: []ReceiptLine{
		{Ingredient: "bread", Qty: 100, TotalCost: 150_000}, {Ingredient: "cheese", Qty: 2000, TotalCost: 300_000},
		{Ingredient: "beans", Qty: 1000, TotalCost: 400_000}, {Ingredient: "milk", Qty: 5000, TotalCost: 100_000}}}); err != nil {
		r.t.Fatal(err)
	}
}

func TestTheReportsKnowWhatTheGoodsOfAnOrderCostFromTheLotsTheyCameFrom(t *testing.T) {
	r := newReportRig(t)
	r.priced()
	a := r.payCash(cashier, r.place(customer, toastie(2)).ID) // 2 x 9,000 of goods
	b := r.pay(r.place(customer, toastie(1)).ID)              // 9,000 taken, then given back
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(b.ID), r.cmd(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(b.ID), r.cmd(), money.Money(b.Total)); err != nil {
		t.Fatal(err)
	}
	_ = a
	if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: stock.WriteOff, Ingredient: "cheese", Qty: 100, Cause: "spoiled", Reason: "left out"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: stock.Adjust, Ingredient: "bread", Qty: -5, Reason: "stocktake 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.MoveStockWith(r.ctx, storekeeper, r.cmd(), StockInput{Kind: stock.Adjust, Ingredient: "bread", Qty: 3, Reason: "stocktake 1"}); err != nil { // a surplus is not waste
		t.Fatal(err)
	}
	r.follow(r.proj)
	costs, err := r.store.CostsByDay(r.ctx, vn)
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 3 || costs[0].CogsMilli != 0 || costs[2].CogsMilli != 0 {
		t.Fatalf("days: %+v", costs)
	}
	day := costs[1]
	// 2 toastie + (1 taken - 1 given back); waste: 100 g of cheese at 150 and 5 breads at 1,500
	if day.CogsMilli != 18_000_000 || day.WasteMilli != 100*150_000+5*1_500_000 {
		t.Fatalf("the day: %+v", day)
	}
	unit, _ := r.store.LatestUnitCosts(r.ctx)
	if unit["bread"] != 1_500_000 || unit["cheese"] != 150_000 || unit["beans"] != 400_000 {
		t.Fatalf("latest unit costs: %v", unit)
	}
	// the ingredients report still counts quantities
	ings, _ := r.store.Ingredients(r.ctx, vn)
	byName := map[string]reports.IngredientRow{}
	for _, i := range ings {
		byName[i.Ingredient] = i
	}
	if byName["bread"].Used != 4 || byName["cheese"].Used != 80 || byName["cheese"].WrittenOff != 100 || byName["bread"].Adjusted != -2 {
		t.Fatalf("ingredients: %+v", byName)
	}
}

func TestTheCostOfAnOrderIsTheCostOfTheLotsThatAreUsedFirstAndAnExpiredLotIsNotUsed(t *testing.T) {
	r := newReportRig(t)
	deliver := func(day string, qty, cost int64) {
		if _, err := r.svc.ReceiveGoods(r.ctx, storekeeper, r.cmd(), ReceiptInput{Lines: []ReceiptLine{{Ingredient: "bread", Qty: qty, TotalCost: cost, Expires: day}}}); err != nil {
			t.Fatal(err)
		}
	}
	deliver("2026-11-21", 4, 4_000)     // 1,000 each, expires first
	deliver("2026-11-29", 100, 300_000) // 3,000 each
	if _, err := r.svc.ReceiveGoods(r.ctx, storekeeper, r.cmd(), ReceiptInput{Lines: []ReceiptLine{{Ingredient: "cheese", Qty: 400, TotalCost: 0}}}); err != nil {
		t.Fatal(err)
	}
	r.pay(r.place(customer, toastie(3)).ID) // 6 breads: 4 at 1,000 and 2 at 3,000
	r.follow(r.proj)
	costs, _ := r.store.CostsByDay(r.ctx, vn)
	if costs[1].CogsMilli != (4*1_000+2*3_000)*1000 {
		t.Fatalf("the goods of the order: %d milli-dong, want %d (the lot that expires first, then the next)", costs[1].CogsMilli, (4*1_000+2*3_000)*1000)
	}
}

func TestARecordThatTheStateRefusesIsNotCountedByTheReports(t *testing.T) {
	r := newReportRig(t)
	r.priced()
	// a write-off of more than there is, written by something that did not check
	bad := StockMove{ID: "bad-writeoff-0001", By: "x", At: r.clk.now(), Kind: stock.WriteOff, Ingredient: "bread", Qty: 99_999, Reason: "no", Cause: "other"}
	raw, err := Record{Stock: &bad}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.log.Append(r.ctx, raw); err != nil {
		t.Fatal(err)
	}
	r.pay(r.place(customer, toastie(1)).ID)
	r.follow(r.proj)
	cp, _ := r.store.Checkpoint(r.ctx, topicName)
	if cp.Skipped != 1 {
		t.Fatalf("skipped %d, want the refused write-off", cp.Skipped)
	}
	if costs, _ := r.store.CostsByDay(r.ctx, vn); costs[1].WasteMilli != 0 {
		t.Fatalf("a write-off that the stock refused is counted as waste: %+v", costs[1])
	}
	ings, _ := r.store.Ingredients(r.ctx, vn)
	for _, i := range ings {
		if i.Ingredient == "bread" && i.WrittenOff != 0 {
			t.Fatalf("bread: %+v", i)
		}
	}
	// and the service agrees: it counted the same record as skipped
	if r.svc.Health().Skipped != 1 {
		t.Fatalf("the service skipped %d", r.svc.Health().Skipped)
	}
}

func TestACostIsTheSameAfterARebuildAndAfterARestartInTheMiddleOfTheLog(t *testing.T) {
	r := newReportRig(t)
	r.priced()
	r.payCash(cashier, r.place(customer, toastie(2)).ID)
	if _, err := r.proj.Step(r.ctx, 0); err != nil { // a part of the log
		t.Fatal(err)
	}
	restarted := NewProjector(r.store, r.log, topicName, 3, nil) // a new process: its state is built from the log up to the checkpoint
	r.payCash(cashier, r.place(customer, toastie(1)).ID)
	r.follow(restarted)
	snapshot := func() []reports.CostDay { c, _ := r.store.CostsByDay(r.ctx, vn); return c }
	before := snapshot()
	if before[1].CogsMilli != 27_000_000 {
		t.Fatalf("3 toasties: %+v", before[1])
	}
	if err := r.store.Reset(r.ctx, topicName); err != nil {
		t.Fatal(err)
	}
	r.follow(NewProjector(r.store, r.log, topicName, 50, nil))
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a rebuild gives other costs:\n%+v\n%+v", before, after)
	}
}

// ---- over HTTP ------------------------------------------------------------------------------

type costHTTP struct {
	*reportHTTP
	exp *expenses.Store
}

func newCostHTTP(t *testing.T) *costHTTP {
	t.Helper()
	pool := pgtest.Pool(t)
	ctx := ctxBackground
	if _, err := pgmigrate.Apply(ctx, pool, accounts.Migrations()); err != nil {
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
	store := reports.New(pool)
	exp := expenses.New(pool, r.clk.now)
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, WithAccounts(acc, AccountsOptions{}), WithReports(store, topicName, "Asia/Ho_Chi_Minh"), WithExpenses(exp)))
	t.Cleanup(srv.Close)
	h := &reportHTTP{accRig: &accRig{rig: r, acc: acc, pool: pool, srv: srv}, store: store, proj: NewProjector(store, r.log, topicName, 50, nil)}
	return &costHTTP{reportHTTP: h, exp: exp}
}

func TestProfitIsWhatWasEarnedLessTheGoodsTheWasteAndWhatWasSpent(t *testing.T) {
	a := newCostHTTP(t)
	a.user("mgr-1", "", rbac.Manager)
	m := a.browser()
	m.signIn("mgr-1")
	a.priced()
	paid := a.payCash(cashier, a.place(customer, toastie(2)).ID)                                                                                                                        // 90,000 collected, 18,000 of goods
	if _, err := a.svc.MoveStockWith(a.ctx, storekeeper, a.cmd(), StockInput{Kind: stock.WriteOff, Ingredient: "cheese", Qty: 100, Cause: "spoiled", Reason: "left out"}); err != nil { // 15,000
		t.Fatal(err)
	}
	// rent for a month is spread over its days; electricity is a day's
	for _, e := range []map[string]any{
		{"category": "Rent", "amount": 3_000_000, "from": "2026-11-01", "to": "2026-11-30", "note": "November"},
		{"category": "Power", "amount": 50_000, "from": "2026-11-20"},
	} {
		if code := m.status("POST", "/api/expenses", e); code != 201 {
			t.Fatalf("add expense: %d", code)
		}
	}
	a.follow()
	var p struct {
		Rows []ProfitDay
		Cats []struct {
			Category string
			Amount   int64
		} `json:"expenses_by_category"`
	}
	if code, _ := m.do("GET", "/api/reports/profit?from=2026-11-19&to=2026-11-21", nil, &p); code != 200 || len(p.Rows) != 3 {
		t.Fatalf("profit: %d %+v", code, p)
	}
	day := p.Rows[1]
	want := ProfitDay{Date: "2026-11-20", Collected: paid.Total, Net: paid.Total, GoodsCost: 18_000, Waste: 15_000, GrossProfit: paid.Total - 33_000, Expenses: 100_000 + 50_000, Profit: paid.Total - 33_000 - 150_000}
	if day != want {
		t.Fatalf("the day:\n got %+v\nwant %+v", day, want)
	}
	if p.Rows[0].Expenses != 100_000 || p.Rows[2].Expenses != 100_000 || p.Rows[0].Profit != -100_000 {
		t.Fatalf("a day with no sales still carries its rent: %+v %+v", p.Rows[0], p.Rows[2])
	}
	if len(p.Cats) != 2 || p.Cats[0].Category != "Rent" || p.Cats[0].Amount != 300_000 || p.Cats[1].Category != "Power" || p.Cats[1].Amount != 50_000 {
		t.Fatalf("by category: %+v", p.Cats)
	}
	// the csv
	code, h, body := m.raw("/api/reports/profit?from=2026-11-20&to=2026-11-20&format=csv")
	if code != 200 || h.Get("Content-Disposition") == "" {
		t.Fatalf("csv: %d", code)
	}
	rows, _ := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF")))).ReadAll()
	if len(rows) != 2 || rows[0][8] != "profit" || rows[1][4] != "18000" || rows[1][5] != "15000" || rows[1][7] != "150000" {
		t.Fatalf("csv rows: %v", rows)
	}
}

func TestWhatAnItemCostsIsItsRecipeAtTheLatestPricesAndSaysWhenAPriceIsMissing(t *testing.T) {
	a := newCostHTTP(t)
	a.user("mgr-1", "", rbac.Manager)
	m := a.browser()
	m.signIn("mgr-1")
	// bread and cheese are priced, the coffee's ingredients are not
	if _, err := a.svc.ReceiveGoods(a.ctx, storekeeper, a.cmd(), ReceiptInput{Lines: []ReceiptLine{{Ingredient: "bread", Qty: 100, TotalCost: 150_000}, {Ingredient: "cheese", Qty: 2000, TotalCost: 300_000}, {Ingredient: "beans", Qty: 100, TotalCost: 0}}}); err != nil {
		t.Fatal(err)
	}
	// a later delivery at a new price: the latest one counts
	if _, err := a.svc.ReceiveGoods(a.ctx, storekeeper, a.cmd(), ReceiptInput{Lines: []ReceiptLine{{Ingredient: "cheese", Qty: 1000, TotalCost: 200_000}}}); err != nil {
		t.Fatal(err)
	}
	a.follow()
	var got struct{ Rows []ItemCost }
	if code, _ := m.do("GET", "/api/reports/item-costs", nil, &got); code != 200 || len(got.Rows) != 2 {
		t.Fatalf("item costs: %d %+v", code, got)
	}
	by := map[string]ItemCost{}
	for _, c := range got.Rows {
		by[c.ID] = c
	}
	toastieCost := by["toastie"]
	// 2 x 1,500 + 40 x 200 (the latest cheese)
	if toastieCost.Cost != 11_000 || toastieCost.Price != 45_000 || toastieCost.Margin != 34_000 || toastieCost.MarginPermille != 755 || !toastieCost.Complete || len(toastieCost.Lines) != 2 {
		t.Fatalf("toastie: %+v", toastieCost)
	}
	cof := by["coffee"]
	if cof.Complete || len(cof.Missing) != 2 || cof.Cost != 0 || cof.Margin != 35_000 {
		t.Fatalf("coffee has no prices for its ingredients and says so: %+v", cof)
	}
	_, _, body := m.raw("/api/reports/item-costs?format=csv")
	rows, _ := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF")))).ReadAll()
	if len(rows) != 3 || rows[1][0] != "coffee" || rows[1][7] != "no" || rows[2][6] != "75.5" || rows[2][7] != "yes" {
		t.Fatalf("csv: %v", rows)
	}
}

func TestOnlyAManagerSeesProfitAndWritesExpensesAndVoidingKeepsThem(t *testing.T) {
	a := newCostHTTP(t)
	for id, role := range map[string]rbac.Role{"mgr-1": rbac.Manager, "store-1": rbac.Storekeeper, "cash-1": rbac.Cashier} {
		a.user(id, "", role)
	}
	m, s, c, anon := a.browser(), a.browser(), a.browser(), a.browser()
	m.signIn("mgr-1")
	s.signIn("store-1")
	c.signIn("cash-1")
	add := map[string]any{"category": "Repairs", "amount": 700_000, "from": "2026-11-12"}
	if code := anon.status("GET", "/api/reports/profit", nil); code != 401 {
		t.Errorf("without signing in: %d", code)
	}
	for name, b := range map[string]*browser{"storekeeper": s, "cashier": c} {
		// the storekeeper may see what things cost but does not read the reports
		for _, p := range []string{"/api/reports/profit", "/api/reports/item-costs"} {
			if code := b.status("GET", p, nil); code != 403 {
				t.Errorf("%s reading %s: %d", name, p, code)
			}
		}
		if code := b.status("POST", "/api/expenses", add); code != 403 {
			t.Errorf("%s writing an expense: %d", name, code)
		}
		if code := b.status("GET", "/api/expenses", nil); code != 403 {
			t.Errorf("%s listing expenses: %d", name, code)
		}
	}
	var created struct{ Expense expenses.Expense }
	if code, _ := m.do("POST", "/api/expenses", add, &created); code != 201 || created.Expense.CreatedBy != "mgr-1" || created.Expense.To != "2026-11-12" {
		t.Fatalf("add: %d %+v", code, created)
	}
	for name, bad := range map[string]map[string]any{
		"no amount": {"category": "x", "amount": 0, "from": "2026-11-12"}, "no category": {"category": "", "amount": 5, "from": "2026-11-12"},
		"a bad day": {"category": "x", "amount": 5, "from": "12/11/2026"}, "an unknown field": {"category": "x", "amount": 5, "from": "2026-11-12", "paid": true},
	} {
		if code := m.status("POST", "/api/expenses", bad); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	var list struct {
		Expenses   []expenses.Expense
		Categories []string
	}
	m.do("GET", "/api/expenses?from=2026-11-01&to=2026-11-30", nil, &list)
	if len(list.Expenses) != 1 || len(list.Categories) != 1 || list.Categories[0] != "Repairs" {
		t.Fatalf("list: %+v", list)
	}
	base := "/api/expenses/" + itoa(created.Expense.ID)
	if code := c.status("POST", base+"/void", map[string]any{"reason": "x"}); code != 403 {
		t.Errorf("a cashier voiding: %d", code)
	}
	var voided struct{ Expense expenses.Expense }
	if code, _ := m.do("POST", base+"/void", map[string]any{"reason": "written twice"}, &voided); code != 200 || voided.Expense.VoidedBy != "mgr-1" {
		t.Fatalf("void: %d %+v", code, voided)
	}
	if code := m.status("POST", base+"/void", map[string]any{"reason": "again"}); code != 409 {
		t.Errorf("voiding twice: %d", code)
	}
	if code := m.status("POST", "/api/expenses/9999/void", map[string]any{}); code != 404 {
		t.Errorf("an expense that is not there: %d", code)
	}
	if code := m.status("POST", "/api/expenses/abc/void", map[string]any{}); code != 400 {
		t.Errorf("a bad number: %d", code)
	}
	m.do("GET", "/api/expenses?from=2026-11-01&to=2026-11-30", nil, &list)
	if len(list.Expenses) != 0 {
		t.Fatalf("a voided expense is listed: %+v", list)
	}
	m.do("GET", "/api/expenses?from=2026-11-01&to=2026-11-30&voided=1", nil, &list)
	if len(list.Expenses) != 1 || list.Expenses[0].VoidedAt == nil {
		t.Fatalf("with the voided ones: %+v", list)
	}
	a.follow()
	var p struct{ Rows []ProfitDay }
	m.do("GET", "/api/reports/profit?from=2026-11-12&to=2026-11-12", nil, &p)
	if len(p.Rows) != 1 || p.Rows[0].Expenses != 0 {
		t.Fatalf("a voided expense is in the profit: %+v", p.Rows)
	}
}

func TestMilliAreRoundedToDongAwayFromZeroAtHalf(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, 499: 0, 500: 1, 1499: 1, 1500: 2, 2_000_000: 2000, -499: 0, -500: -1, -1500: -2} {
		if got := milliToDong(in); got != want {
			t.Errorf("milliToDong(%d) = %d, want %d", in, got, want)
		}
	}
}
