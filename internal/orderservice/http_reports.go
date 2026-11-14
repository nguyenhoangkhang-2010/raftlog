package orderservice

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
)

// WithReports turns on the reports, which are read from the store that the projector fills from the
// log of the topic. zone is the name of the venue's time zone as PostgreSQL knows it (Asia/Ho_Chi_Minh):
// a day of a report is a day there.
func WithReports(store *reports.Store, topic, zone string) APIOption {
	return func(a *API) { a.reports, a.reportTopic, a.reportZone = store, topic, zone }
}

func (a *API) reportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/reports/status", a.auth(a.reportStatus))
	mux.HandleFunc("GET /api/reports/{name}", a.auth(a.report))
}

func (a *API) failReport(w http.ResponseWriter, err error) {
	var inv *reports.InvalidError
	if errors.As(err, &inv) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: strings.TrimPrefix(err.Error(), "reports: "), Code: "invalid"})
		return
	}
	if _, kind := status(err); kind == "internal" {
		a.logger.Error("the reports database failed", "err", err)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the database did not answer", Code: "unavailable"})
		return
	}
	a.fail(w, err)
}

// freshness says how old the reports are: where the projector is in the log, and how far behind the
// log it is.
type freshness struct {
	AsOf   *time.Time `json:"as_of,omitempty"`
	Behind int64      `json:"behind"`
}

func (a *API) freshness(r *http.Request) (freshness, error) {
	cp, err := a.reports.Checkpoint(r.Context(), a.reportTopic)
	if err != nil {
		return freshness{}, err
	}
	f := freshness{AsOf: cp.AsOf}
	if end := a.svc.Health().Next; end > cp.Next {
		f.Behind = int64(end - cp.Next)
	}
	return f, nil
}

func (a *API) reportStatus(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !actor.Can(rbac.ViewReports) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ViewReports))
		return
	}
	f, err := a.freshness(r)
	if err != nil {
		a.failReport(w, err)
		return
	}
	cp, _ := a.reports.Checkpoint(r.Context(), a.reportTopic)
	writeJSON(w, http.StatusOK, map[string]any{"as_of": f.AsOf, "behind": f.Behind, "next_offset": cp.Next, "skipped": cp.Skipped, "zone": a.reportZone})
}

// periodOf reads the days of a report from the query; by default the last seven days, today included.
func (a *API) periodOf(r *http.Request) reports.Period {
	loc := a.svc.Location()
	today := a.svc.now().In(loc)
	p := reports.Period{Zone: a.reportZone, To: today.Format("2006-01-02"), From: today.AddDate(0, 0, -6).Format("2006-01-02")}
	if v := r.URL.Query().Get("from"); v != "" {
		p.From = v
	}
	if v := r.URL.Query().Get("to"); v != "" {
		p.To = v
	}
	return p
}

// table is what a report gives: the columns, the rows, and which columns are text (the others are
// numbers, and are written as they are).
type table struct {
	columns []string
	text    map[int]bool
	rows    [][]string
}

func num(n int64) string { return strconv.FormatInt(n, 10) }

func (a *API) report(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	if !actor.Can(rbac.ViewReports) {
		a.fail(w, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.ViewReports))
		return
	}
	p := a.periodOf(r)
	name := r.PathValue("name")
	var data any
	var t table
	var err error
	switch name {
	case "sales":
		var rows []reports.DayRow
		if rows, err = a.reports.SalesByDay(r.Context(), p); err == nil {
			data = rows
			t = table{columns: []string{"date", "orders", "subtotal", "discount", "collected", "cash", "card", "transfer", "refunds", "net", "avg_prep_seconds"}, text: map[int]bool{0: true}}
			for _, x := range rows {
				t.rows = append(t.rows, []string{x.Date, num(x.Orders), num(x.Subtotal), num(x.Discount), num(x.Total), num(x.Cash), num(x.Card), num(x.Transfer), num(x.Refunds), num(x.Net), num(x.PrepSeconds)})
			}
		}
	case "items":
		var rows []reports.ItemRow
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if rows, err = a.reports.BestSellers(r.Context(), p, limit); err == nil {
			data = rows
			t = table{columns: []string{"kind", "id", "name", "category", "quantity", "revenue", "orders"}, text: map[int]bool{0: true, 1: true, 2: true, 3: true}}
			for _, x := range rows {
				t.rows = append(t.rows, []string{x.Kind, x.Ref, x.Name, x.Category, num(x.Quantity), num(x.Revenue), num(x.Orders)})
			}
		}
	case "ingredients":
		var rows []reports.IngredientRow
		if rows, err = a.reports.Ingredients(r.Context(), p); err == nil {
			data = rows
			t = table{columns: []string{"ingredient", "used", "received", "paid", "written_off", "adjusted"}, text: map[int]bool{0: true}}
			for _, x := range rows {
				t.rows = append(t.rows, []string{x.Ingredient, num(x.Used), num(x.Received), num(x.Cost), num(x.WrittenOff), num(x.Adjusted)})
			}
		}
	case "refunds":
		var rows []reports.RefundRow
		if rows, err = a.reports.Refunds(r.Context(), p); err == nil {
			data = rows
			t = table{columns: []string{"at", "order", "kind", "amount", "method", "by", "reason"}, text: map[int]bool{0: true, 2: true, 4: true, 5: true, 6: true}}
			for _, x := range rows {
				t.rows = append(t.rows, []string{x.At.In(a.svc.Location()).Format(time.RFC3339), num(int64(x.Order)), x.Kind, num(x.Amount), x.Method, x.By, x.Reason})
			}
		}
	case "hours":
		var rows []reports.HourRow
		if rows, err = a.reports.Hours(r.Context(), p); err == nil {
			data = rows
			t = table{columns: []string{"hour", "orders", "collected"}}
			for _, x := range rows {
				t.rows = append(t.rows, []string{num(int64(x.Hour)), num(x.Orders), num(x.Total)})
			}
		}
	default:
		writeJSON(w, http.StatusNotFound, errorBody{Error: "not found", Code: "not_found"})
		return
	}
	if err != nil {
		a.failReport(w, err)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		a.writeCSV(w, name, p, t)
		return
	}
	f, err := a.freshness(r)
	if err != nil {
		a.failReport(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": p.From, "to": p.To, "zone": p.Zone, "as_of": f.AsOf, "behind": f.Behind, "rows": data})
}

// csvSafe keeps a text cell from being read as a formula by a spreadsheet: one that begins with =, +,
// - or @ (or a tab or a return) gets a quote in front of it, as the guidance for exports says.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// writeCSV sends a report as a file. It starts with the mark that tells a spreadsheet the text is UTF-8,
// without which Vietnamese names arrive as garbage, and it never writes a cell that could run.
func (a *API) writeCSV(w http.ResponseWriter, name string, p reports.Period, t table) {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	cw := csv.NewWriter(&buf)
	_ = cw.Write(t.columns)
	for _, row := range t.rows {
		out := make([]string, len(row))
		for i, c := range row {
			if t.text[i] {
				c = csvSafe(c)
			}
			out[i] = c
		}
		_ = cw.Write(out)
	}
	cw.Flush()
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s_%s.csv"`, name, p.From, p.To))
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}
