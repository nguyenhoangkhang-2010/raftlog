package orderservice

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/catalog"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/inventory"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

type invRig struct {
	*accRig
	inv *inventory.Store
}

func newInvRig(t *testing.T) *invRig {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctxBackground, pool, accounts.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctxBackground, pool, catalog.Migrations(), pgmigrate.WithTable(catalog.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctxBackground, pool, inventory.Migrations(), pgmigrate.WithTable(inventory.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	acc, err := accounts.New(pool, accounts.Config{Hash: accounts.HashParams{Memory: 8, Iterations: 1, Parallelism: 1}})
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.New(pool, nil)
	r := newRig(t)
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, WithAccounts(acc, AccountsOptions{}), WithInventory(inv)))
	t.Cleanup(srv.Close)
	return &invRig{accRig: &accRig{rig: r, acc: acc, pool: pool, srv: srv}, inv: inv}
}

type warehouseAnswer struct {
	Rows []struct {
		Ingredient string
		Qty        int64
		ValueMilli *int64 `json:"value_milli"`
		Lots       []struct {
			ID            string
			Qty           int64
			Supplier      string
			Document      string
			UnitCostMilli int64 `json:"unit_cost_milli"`
			State         string
		}
	}
	Costs     bool
	Infos     map[string]struct{ Name, Unit string }
	Suppliers []struct {
		ID, Name string
		Active   bool
	}
}

func (w warehouseAnswer) row(ing string) (qty int64, lots int) {
	for _, r := range w.Rows {
		if r.Ingredient == ing {
			return r.Qty, len(r.Lots)
		}
	}
	return -1, 0
}

func TestTheWarehouseIsForThoseWhoWorkWithTheStockAndCostsAreForThoseWhoPay(t *testing.T) {
	a := newInvRig(t)
	a.user("store-1", "", rbac.Storekeeper)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cook-1", "drinks", rbac.Kitchen)
	a.user("cash-1", "", rbac.Cashier)
	a.user("cus-1", "", rbac.Customer)
	b := map[string]*browser{}
	for _, id := range []string{"store-1", "mgr-1", "cook-1", "cash-1", "cus-1"} {
		b[id] = a.browser()
		b[id].signIn(id)
	}
	anon := a.browser()
	if code := anon.status("GET", "/api/warehouse", nil); code != 401 {
		t.Errorf("without signing in: %d", code)
	}
	for _, id := range []string{"cash-1", "cus-1"} {
		for _, p := range []string{"/api/warehouse", "/api/warehouse/ledger?ingredient=bread", "/api/warehouse/receipts", "/api/warehouse/waste", "/api/suppliers"} {
			if code := b[id].status("GET", p, nil); code != 403 {
				t.Errorf("%s reading %s: %d", id, p, code)
			}
		}
	}
	delivery := map[string]any{"command_id": "deliver-0001", "supplier": "", "document": "INV-1", "lines": []map[string]any{{"ingredient": "bread", "qty": 40, "total_cost": 80000}}}
	for id, want := range map[string]int{"store-1": 200, "mgr-1": 403, "cook-1": 403, "cash-1": 403, "cus-1": 403} {
		delivery["command_id"] = "deliver-" + id
		if code := b[id].status("POST", "/api/warehouse/receive", delivery); code != want {
			t.Errorf("%s receiving goods: %d, want %d", id, code, want)
		}
	}
	for id, want := range map[string]int{"store-1": 201, "mgr-1": 201, "cook-1": 403, "cash-1": 403, "cus-1": 403} {
		if code := b[id].status("POST", "/api/suppliers", map[string]any{"id": "sup-" + id[:3], "name": "Supplier of " + id}); code != want {
			t.Errorf("%s adding a supplier: %d, want %d", id, code, want)
		}
	}
	for id, want := range map[string]int{"store-1": 200, "mgr-1": 200, "cook-1": 403, "cash-1": 403} {
		if code := b[id].status("GET", "/api/stocktakes/current", nil); code != want {
			t.Errorf("%s reading the stocktake: %d, want %d", id, code, want)
		}
		if code := b[id].status("POST", "/api/stock/adjust", map[string]any{"command_id": "adjust-" + id, "ingredient": "bread", "qty": 1, "reason": "x"}); code != want {
			t.Errorf("%s adjusting: %d, want %d", id, code, want)
		}
	}
	var seen warehouseAnswer
	if code, _ := b["cook-1"].do("GET", "/api/warehouse", nil, &seen); code != 200 || seen.Costs {
		t.Fatalf("the kitchen: %d, costs %v", code, seen.Costs)
	}
	for _, r := range seen.Rows {
		if r.ValueMilli != nil {
			t.Fatalf("the kitchen sees a value: %+v", r)
		}
		for _, l := range r.Lots {
			if l.UnitCostMilli != 0 {
				t.Fatalf("the kitchen sees a price: %+v", l)
			}
		}
	}
	for _, id := range []string{"store-1", "mgr-1"} {
		var w warehouseAnswer
		b[id].do("GET", "/api/warehouse", nil, &w)
		var val *int64
		for _, r := range w.Rows {
			if r.Ingredient == "bread" {
				val = r.ValueMilli
			}
		}
		if !w.Costs || val == nil || *val < 80_000_000 {
			t.Errorf("%s: costs %v, bread worth %v", id, w.Costs, val)
		}
	}
}

func TestADeliveryNamesASupplierThatIsThereAndInUse(t *testing.T) {
	a := newInvRig(t)
	a.user("store-1", "", rbac.Storekeeper)
	s := a.browser()
	s.signIn("store-1")
	deliver := func(cmd, supplier string) int {
		return s.status("POST", "/api/warehouse/receive", map[string]any{"command_id": cmd, "supplier": supplier, "document": "INV-7",
			"lines": []map[string]any{{"ingredient": "cheese", "qty": 500, "total_cost": 100000, "expires": "2026-12-31"}}})
	}
	if code := deliver("deliver-0001", "milk-co"); code != 400 {
		t.Fatalf("a supplier that is not there: %d", code)
	}
	if code := s.status("POST", "/api/suppliers", map[string]any{"id": "milk-co", "name": "Milk Co", "phone": "0901"}); code != 201 {
		t.Fatalf("add: %d", code)
	}
	if code := s.status("POST", "/api/suppliers", map[string]any{"id": "milk-co", "name": "Again"}); code != 409 {
		t.Fatalf("the same ID again: %d", code)
	}
	if code := deliver("deliver-0001", "milk-co"); code != 200 {
		t.Fatalf("a delivery from a supplier that is there: %d", code)
	}
	if code := s.status("PUT", "/api/suppliers/milk-co", map[string]any{"name": "Milk Co", "active": false}); code != 200 {
		t.Fatalf("make inactive: %d", code)
	}
	if code := deliver("deliver-0002", "milk-co"); code != 400 {
		t.Fatalf("a supplier that is not in use any more: %d", code)
	}
	if code := s.status("PUT", "/api/suppliers/milk-co", map[string]any{"id": "other", "name": "x"}); code != 400 {
		t.Fatalf("changing the ID: %d", code)
	}
	if code := s.status("PUT", "/api/suppliers/ghost-co", map[string]any{"name": "x"}); code != 404 {
		t.Fatalf("a supplier that is not there: %d", code)
	}
	var w warehouseAnswer
	s.do("GET", "/api/warehouse", nil, &w)
	if qty, lots := w.row("cheese"); qty != 500 || lots != 1 {
		t.Fatalf("cheese: %d in %d lots", qty, lots)
	}
	if len(w.Suppliers) != 1 || w.Suppliers[0].Active {
		t.Fatalf("suppliers in the answer: %+v (the inactive ones are named too, so that an old lot still shows who it came from)", w.Suppliers)
	}
	// repeating the same command writes nothing more
	if code := deliver("deliver-0001", "milk-co"); code != 400 { // inactive now: refused before it can be a repeat
		t.Logf("repeat after the supplier was made inactive: %d", code)
	}
	var list struct{ Suppliers []struct{ ID string } }
	s.do("GET", "/api/suppliers", nil, &list)
	if len(list.Suppliers) != 0 {
		t.Fatalf("the list of those in use: %+v", list)
	}
	s.do("GET", "/api/suppliers?all=1", nil, &list)
	if len(list.Suppliers) != 1 {
		t.Fatalf("the list of all: %+v", list)
	}
	var rec struct {
		Receipts []struct {
			Document, Supplier string
			Lines              []struct{ Ingredient string }
		}
	}
	s.do("GET", "/api/warehouse/receipts", nil, &rec)
	if len(rec.Receipts) != 1 || rec.Receipts[0].Document != "INV-7" || rec.Receipts[0].Supplier != "milk-co" || len(rec.Receipts[0].Lines) != 1 {
		t.Fatalf("receipts: %+v", rec)
	}
}

func TestTheNameAndUnitOfAnIngredientAreShownWithTheStock(t *testing.T) {
	a := newInvRig(t)
	a.user("store-1", "", rbac.Storekeeper)
	a.user("cook-1", "drinks", rbac.Kitchen)
	s, k := a.browser(), a.browser()
	s.signIn("store-1")
	k.signIn("cook-1")
	if code := s.status("PUT", "/api/ingredients/bread", map[string]any{"name": "Baguette", "unit": "pc", "shelf_days": 2}); code != 200 {
		t.Fatalf("save: %d", code)
	}
	if code := s.status("PUT", "/api/ingredients/unobtainium", map[string]any{"name": "x", "unit": "g"}); code != 400 {
		t.Fatalf("an ingredient that no recipe uses: %d", code)
	}
	if code := s.status("PUT", "/api/ingredients/bread", map[string]any{"name": "", "unit": "pc"}); code != 400 {
		t.Fatalf("no name: %d", code)
	}
	if code := k.status("PUT", "/api/ingredients/bread", map[string]any{"name": "Mine", "unit": "pc"}); code != 403 {
		t.Fatalf("a cook renaming: %d", code)
	}
	var w warehouseAnswer
	k.do("GET", "/api/warehouse", nil, &w)
	if w.Infos["bread"].Name != "Baguette" || w.Infos["bread"].Unit != "pc" {
		t.Fatalf("infos: %+v", w.Infos)
	}
}

func TestAStocktakeIsCountedAndAppliedAsAdjustments(t *testing.T) {
	a := newInvRig(t)
	a.user("store-1", "", rbac.Storekeeper)
	a.user("cus-1", "", rbac.Customer)
	s, c := a.browser(), a.browser()
	s.signIn("store-1")
	c.signIn("cus-1")
	a.stockAll() // bread 100, cheese 2000, milk 5000, beans 1000

	var cur struct{ Stocktake *inventory.Stocktake }
	if s.do("GET", "/api/stocktakes/current", nil, &cur); cur.Stocktake != nil {
		t.Fatalf("a stocktake before any was opened: %+v", cur)
	}
	var opened struct{ Stocktake inventory.Stocktake }
	if code, _ := s.do("POST", "/api/stocktakes", map[string]any{"note": "end of day"}, &opened); code != 201 || opened.Stocktake.Status != "open" {
		t.Fatalf("open: %d %+v", code, opened)
	}
	base := "/api/stocktakes/" + itoa(opened.Stocktake.ID)
	if code := s.status("POST", "/api/stocktakes", map[string]any{}); code != 409 {
		t.Fatalf("a second one: %d", code)
	}
	var st struct{ Stocktake inventory.Stocktake }
	if code, _ := s.do("PUT", base+"/lines", map[string]any{"ingredient": "bread", "counted": 93}, &st); code != 200 || len(st.Stocktake.Lines) != 1 || st.Stocktake.Lines[0].Expected != 100 {
		t.Fatalf("count bread: %d %+v", code, st)
	}
	if code := s.status("PUT", base+"/lines", map[string]any{"ingredient": "cheese", "counted": 2010}); code != 200 {
		t.Fatalf("count cheese: %d", code)
	}
	if code := s.status("PUT", base+"/lines", map[string]any{"ingredient": "milk", "counted": 5000}); code != 200 { // no difference
		t.Fatalf("count milk: %d", code)
	}
	if code := s.status("PUT", base+"/lines", map[string]any{"ingredient": "unobtainium", "counted": 1}); code != 400 {
		t.Fatalf("an ingredient that is not known: %d", code)
	}
	if code := s.status("PUT", base+"/lines", map[string]any{"ingredient": "bread", "counted": -1}); code != 400 {
		t.Fatalf("a negative count: %d", code)
	}
	// bread is used while the stocktake is open: 2 orders of one toastie take 4
	for i := 0; i < 2; i++ {
		a.pay(a.place(customer, toastie(1)).ID)
	}
	if q, _ := a.svc.StockQty("bread"); q != 96 {
		t.Fatalf("bread after two orders: %d", q)
	}
	var applied struct {
		Stocktake inventory.Stocktake
		Applied   []AppliedLine
	}
	if code, _ := c.do("POST", base+"/apply", map[string]any{}, &applied); code != 403 {
		t.Fatalf("a customer applying: %d", code)
	}
	if code, _ := s.do("POST", base+"/apply", map[string]any{}, &applied); code != 200 {
		t.Fatalf("apply: %d %+v", code, applied)
	}
	if code, _ := s.do("GET", base, nil, &st); code != 200 || st.Stocktake.Status != "applied" {
		t.Fatalf("apply: %d %+v", code, applied)
	}
	// the count of 93 against 100 found 7 missing; the 4 that were sold since are not taken off twice
	if q, _ := a.svc.StockQty("bread"); q != 96-7 {
		t.Fatalf("bread after applying: %d, want %d", q, 96-7)
	}
	if q, _ := a.svc.StockQty("cheese"); q != 2000-80+10 {
		t.Fatalf("cheese after applying: %d", q)
	}
	if len(applied.Applied) != 2 || applied.Applied[0].Ingredient != "bread" || applied.Applied[0].Variance != -7 || applied.Applied[0].Applied != -7 {
		t.Fatalf("applied: %+v (milk had no difference and is not written)", applied.Applied)
	}
	// it cannot be applied again, or changed
	if code := s.status("POST", base+"/apply", map[string]any{}); code != 409 {
		t.Fatalf("applying twice: %d", code)
	}
	if code := s.status("PUT", base+"/lines", map[string]any{"ingredient": "egg", "counted": 1}); code != 409 && code != 400 {
		t.Fatalf("a count after it was applied: %d", code)
	}
	// and the history of the ingredient says why
	var led LedgerView
	s.do("GET", "/api/warehouse/ledger?ingredient=bread&limit=5", nil, &led)
	if len(led.Entries) == 0 || led.Entries[0].Kind != "adjust" || led.Entries[0].Delta != -7 || !strings.Contains(led.Entries[0].Reason, "stocktake") {
		t.Fatalf("ledger: %+v", led.Entries)
	}
	// the waste report counts it under the stocktake
	var waste struct {
		Rows []WasteRow
	}
	s.do("GET", "/api/warehouse/waste", nil, &waste)
	found := false
	for _, r := range waste.Rows {
		found = found || (r.Ingredient == "bread" && r.Cause == CauseStocktake && r.Qty == 7)
	}
	if !found {
		t.Fatalf("waste: %+v", waste.Rows)
	}
	// a new one may be opened now, and thrown away
	var next struct{ Stocktake inventory.Stocktake }
	if code, _ := s.do("POST", "/api/stocktakes", map[string]any{}, &next); code != 201 {
		t.Fatalf("a new one after the first is done: %d", code)
	}
	if code := s.status("POST", "/api/stocktakes/"+itoa(next.Stocktake.ID)+"/cancel", map[string]any{}); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	if code := s.status("GET", "/api/stocktakes/9999", nil); code != 404 {
		t.Fatalf("one that is not there: %d", code)
	}
	if code := s.status("GET", "/api/stocktakes/abc", nil); code != 400 {
		t.Fatalf("a bad number: %d", code)
	}
}

func TestWhatIsMissingIsNotMoreThanWhatIsThere(t *testing.T) {
	a := newInvRig(t)
	a.user("store-1", "", rbac.Storekeeper)
	s := a.browser()
	s.signIn("store-1")
	a.receive("bread", 10)
	var opened struct{ Stocktake inventory.Stocktake }
	s.do("POST", "/api/stocktakes", map[string]any{}, &opened)
	base := "/api/stocktakes/" + itoa(opened.Stocktake.ID)
	s.status("PUT", base+"/lines", map[string]any{"ingredient": "bread", "counted": 0}) // found nothing: 10 missing
	a.stockAll()                                                                        // meanwhile goods arrived
	a.pay(a.place(customer, toastie(5)).ID)                                             // and 10 were used: the 10 are gone already
	var applied struct{ Applied []AppliedLine }
	if code, _ := s.do("POST", base+"/apply", map[string]any{}, &applied); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	if q, _ := a.svc.StockQty("bread"); q < 0 {
		t.Fatalf("bread is %d", q)
	}
	if len(applied.Applied) != 1 || applied.Applied[0].Variance != -10 || applied.Applied[0].Applied < -10 {
		t.Fatalf("applied %+v", applied.Applied)
	}
}

func TestAWriteOffNamesItsCauseAndTheWasteReportAddsItUp(t *testing.T) {
	a := newInvRig(t)
	a.user("store-1", "", rbac.Storekeeper)
	s := a.browser()
	s.signIn("store-1")
	a.stockAll()
	w := func(cmd string, qty int64, cause string) int {
		return s.status("POST", "/api/stock/write-off", map[string]any{"command_id": cmd, "ingredient": "milk", "qty": qty, "reason": "r", "cause": cause})
	}
	if code := w("writeoff-001", 100, "spoiled"); code != 200 {
		t.Fatalf("a write-off: %d", code)
	}
	if code := w("writeoff-002", 50, "spoiled"); code != 200 {
		t.Fatalf("a write-off: %d", code)
	}
	if code := w("writeoff-003", 10, "stolen"); code != 400 {
		t.Fatalf("a cause that is not one: %d", code)
	}
	var waste struct {
		From, To string
		Rows     []WasteRow
	}
	if code, _ := s.do("GET", "/api/warehouse/waste", nil, &waste); code != 200 || len(waste.Rows) != 1 || waste.Rows[0].Qty != 150 || waste.Rows[0].Cause != "spoiled" {
		t.Fatalf("waste: %d %+v", code, waste)
	}
	if waste.From == "" || waste.To == "" {
		t.Fatalf("the period is not told: %+v", waste)
	}
	for _, q := range []string{"?from=yesterday", "?to=2026-13-40", "?from=2026-11-20&to=2026-11-01", "?from=2020-01-01&to=2026-11-20"} {
		if code := s.status("GET", "/api/warehouse/waste"+q, nil); code != 400 {
			t.Errorf("waste%s: %d", q, code)
		}
	}
}

func TestADeliveryWithTheLinesOfOneTypeInTheOldSingleLineEndpointStillWorks(t *testing.T) {
	a := newInvRig(t)
	a.user("store-1", "", rbac.Storekeeper)
	s := a.browser()
	s.signIn("store-1")
	body, _ := json.Marshal(map[string]any{"command_id": "single-0001", "ingredient": "bread", "qty": 12, "expires": "2026-12-01", "total_cost": 1200, "document": "INV-2"})
	if code := s.status("POST", "/api/stock/receive", string(body)); code != 200 {
		t.Fatalf("receive: %d", code)
	}
	var w warehouseAnswer
	s.do("GET", "/api/warehouse", nil, &w)
	if qty, lots := w.row("bread"); qty != 12 || lots != 1 {
		t.Fatalf("bread: %d in %d lots", qty, lots)
	}
	if w.Rows[0].Ingredient == "bread" && w.Rows[0].Lots[0].Document != "INV-2" {
		t.Fatalf("lot: %+v", w.Rows[0].Lots)
	}
	// as before: the old body with only a quantity and a reason
	if code := s.status("POST", "/api/stock/write-off", map[string]any{"command_id": "single-0002", "ingredient": "bread", "qty": 2, "reason": "dropped"}); code != 200 {
		t.Fatalf("write-off: %d", code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
