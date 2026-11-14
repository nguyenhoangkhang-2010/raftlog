package orderservice

import (
	"bytes"
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
)

type reportHTTP struct {
	*accRig
	store *reports.Store
	proj  *Projector
}

func newReportHTTP(t *testing.T) *reportHTTP {
	t.Helper()
	pool := pgtest.Pool(t)
	ctx := ctxBackground
	if _, err := pgmigrate.Apply(ctx, pool, accounts.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctx, pool, reports.Migrations(), pgmigrate.WithTable(reports.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	acc, err := accounts.New(pool, accounts.Config{Hash: accounts.HashParams{Memory: 8, Iterations: 1, Parallelism: 1}})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	store := reports.New(pool)
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, WithAccounts(acc, AccountsOptions{}), WithReports(store, topicName, "Asia/Ho_Chi_Minh")))
	t.Cleanup(srv.Close)
	return &reportHTTP{accRig: &accRig{rig: r, acc: acc, pool: pool, srv: srv}, store: store, proj: NewProjector(store, r.log, topicName, 50, nil)}
}

func (a *reportHTTP) follow() {
	a.t.Helper()
	for {
		n, err := a.proj.Step(a.ctx, 0)
		if err != nil {
			a.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (b *browser) raw(path string) (int, http.Header, []byte) {
	b.t.Helper()
	resp, err := b.client.Do(b.request("GET", path, nil))
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

func TestOnlyThoseWhoMayViewReportsSeeThem(t *testing.T) {
	a := newReportHTTP(t)
	for id, role := range map[string]rbac.Role{"mgr-1": rbac.Manager, "cash-1": rbac.Cashier, "store-1": rbac.Storekeeper, "cus-1": rbac.Customer, "adm-1": rbac.Admin} {
		a.user(id, "", role)
	}
	anon := a.browser()
	paths := []string{"/api/reports/status", "/api/reports/sales", "/api/reports/items", "/api/reports/ingredients", "/api/reports/refunds", "/api/reports/hours", "/api/reports/sales?format=csv"}
	for _, p := range paths {
		if code := anon.status("GET", p, nil); code != 401 {
			t.Errorf("%s without signing in: %d", p, code)
		}
	}
	for id, want := range map[string]int{"mgr-1": 200, "cash-1": 403, "store-1": 403, "cus-1": 403, "adm-1": 403} {
		b := a.browser()
		b.signIn(id)
		for _, p := range paths {
			if code := b.status("GET", p, nil); code != want {
				t.Errorf("%s reading %s: %d, want %d", id, p, code, want)
			}
		}
	}
	m := a.browser()
	m.signIn("mgr-1")
	if code := m.status("GET", "/api/reports/profits", nil); code != 404 {
		t.Errorf("a report that does not exist: %d", code)
	}
}

func TestAReportAnswersWithItsRowsAndHowOldTheyAre(t *testing.T) {
	a := newReportHTTP(t)
	a.user("mgr-1", "", rbac.Manager)
	m := a.browser()
	m.signIn("mgr-1")
	a.stockAll()
	paid := a.payCash(cashier, a.place(customer, toastie(2)).ID)
	var st struct {
		Behind int64
		AsOf   *string `json:"as_of"`
		Zone   string
	}
	m.do("GET", "/api/reports/status", nil, &st)
	if st.Behind == 0 || st.AsOf != nil || st.Zone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("before the projector has read anything: %+v", st)
	}
	a.follow()
	m.do("GET", "/api/reports/status", nil, &st)
	if st.Behind != 0 || st.AsOf == nil {
		t.Fatalf("after: %+v", st)
	}
	var sales struct {
		From, To, Zone string
		Behind         int64
		Rows           []struct {
			Date   string
			Orders int64
			Total  int64
			Cash   int64
			Net    int64
		}
	}
	if code, _ := m.do("GET", "/api/reports/sales?from=2026-11-19&to=2026-11-21", nil, &sales); code != 200 {
		t.Fatalf("sales: %d", code)
	}
	if len(sales.Rows) != 3 || sales.Rows[1].Date != "2026-11-20" || sales.Rows[1].Orders != 1 || sales.Rows[1].Total != paid.Total || sales.Rows[1].Cash != paid.Total || sales.Behind != 0 || sales.Zone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("sales: %+v", sales)
	}
	for _, bad := range []string{"from=tomorrow", "to=2026-13-45", "from=2026-11-20&to=2026-11-01", "from=2020-01-01&to=2026-11-01"} {
		if code := m.status("GET", "/api/reports/sales?"+bad, nil); code != 400 {
			t.Errorf("sales?%s: %d", bad, code)
		}
	}
	// by default the last seven days, today included: seven rows
	var def struct{ Rows []struct{ Date string } }
	m.do("GET", "/api/reports/sales", nil, &def)
	if len(def.Rows) != 7 {
		t.Fatalf("the default period has %d days", len(def.Rows))
	}
}

func TestACSVIsUTF8WithTheMarkNeverRunsAsAFormulaAndNamesItsFile(t *testing.T) {
	a := newReportHTTP(t)
	a.user("mgr-1", "", rbac.Manager)
	m := a.browser()
	m.signIn("mgr-1")
	a.stockAll()
	o := a.pay(a.place(customer, toastie(1)).ID)
	for _, step := range []func() error{
		func() error { _, err := a.svc.StartTicket(a.ctx, hotCook, order.ID(o.ID), a.cmd(), "hot"); return err },
		func() error { _, err := a.svc.FinishTicket(a.ctx, hotCook, order.ID(o.ID), a.cmd(), "hot"); return err },
		func() error { _, err := a.svc.HandOver(a.ctx, cashier, order.ID(o.ID), a.cmd()); return err },
		func() error {
			_, err := a.svc.IssueRefund(a.ctx, manager, order.ID(o.ID), a.cmd(), money.Money(5000), `=HYPERLINK("http://example.invalid/","click")`)
			return err
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	a.follow()
	code, h, body := m.raw("/api/reports/refunds?from=2026-11-20&to=2026-11-20&format=csv")
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") || !strings.Contains(h.Get("Content-Disposition"), `filename="refunds-2026-11-20_2026-11-20.csv"`) {
		t.Fatalf("csv: %d %v", code, h)
	}
	if !bytes.HasPrefix(body, []byte("\xEF\xBB\xBF")) {
		t.Fatal("no byte-order mark: a spreadsheet reads Vietnamese as garbage")
	}
	recs, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF")))).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("rows: %v %v", recs, err)
	}
	if strings.Join(recs[0], ",") != "at,order,kind,amount,method,by,reason" {
		t.Fatalf("header: %v", recs[0])
	}
	if recs[1][6] != `'=HYPERLINK("http://example.invalid/","click")` || recs[1][3] != "5000" || recs[1][2] != "refund" {
		t.Fatalf("a cell that a spreadsheet would run: %q", recs[1])
	}
	// every report has a csv, and a number is never quoted into text
	for _, name := range []string{"sales", "items", "ingredients", "hours"} {
		if code, _, b := m.raw("/api/reports/" + name + "?from=2026-11-20&to=2026-11-20&format=csv"); code != 200 || !bytes.HasPrefix(b, []byte("\xEF\xBB\xBF")) {
			t.Errorf("%s as csv: %d", name, code)
		}
	}
	_, _, sales := m.raw("/api/reports/sales?from=2026-11-20&to=2026-11-20&format=csv")
	rows, _ := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(sales, []byte("\xEF\xBB\xBF")))).ReadAll()
	if len(rows) != 2 || rows[1][0] != "2026-11-20" || rows[1][9] != "40000" { // net: 45,000 collected less 5,000 given back
		t.Fatalf("sales csv: %v", rows)
	}
}

func TestOnlyTextThatCouldRunIsMarked(t *testing.T) {
	for in, want := range map[string]string{
		"=1+1": "'=1+1", "+cmd": "'+cmd", "-2+3": "'-2+3", "@SUM(A1)": "'@SUM(A1)", "\tcmd": "'\tcmd", "\rcmd": "'\rcmd",
		"Cà phê sữa": "Cà phê sữa", "": "", "a=b": "a=b", "12": "12",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
