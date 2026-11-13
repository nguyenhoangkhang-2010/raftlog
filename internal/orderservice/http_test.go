package orderservice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// The tokens are invented for the tests.
var tokens = map[string]rbac.Actor{
	"customer-token-0001": customer,
	"customer-token-0002": otherCust,
	"cashier-token-00001": cashier,
	"kitchen-drinks-0001": drinksCook,
	"kitchen-hot-0000001": hotCook,
	"runner-token-000001": runner,
	"manager-token-00001": manager,
	"storekeeper-token-1": storekeeper,
}

func testUsers(t testing.TB) *Users {
	t.Helper()
	var list []userFile
	for tok, a := range tokens {
		f := userFile{ID: a.ID, Token: tok, Station: a.Station}
		for _, r := range a.Roles {
			f.Roles = append(f.Roles, string(r))
		}
		list = append(list, f)
	}
	data, _ := json.Marshal(list)
	u, err := ParseUsers(data)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

type httpRig struct {
	*rig
	srv *httptest.Server
	n   int
}

func newHTTPRig(t *testing.T) *httpRig {
	t.Helper()
	r := newRig(t)
	srv := httptest.NewServer(NewAPI(r.svc, TokenAuth(testUsers(t)), nil))
	t.Cleanup(srv.Close)
	return &httpRig{rig: r, srv: srv}
}

// do sends a request as the person with the token and decodes the JSON answer into out.
func (h *httpRig) do(method, path, token string, body any, out any) int {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: answer %q is not the expected JSON: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (h *httpRig) cid() string {
	h.n++
	return fmt.Sprintf("http-cmd-%06d", h.n)
}

type errAnswer struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Lines     []struct{ Line int }
	Shortages []struct {
		Ingredient string
		Need, Have int64
	}
}

func TestOnlyTheMenuAndTheHealthCheckNeedNoToken(t *testing.T) {
	h := newHTTPRig(t)
	if code := h.do("GET", "/api/menu", "", nil, nil); code != 200 {
		t.Fatalf("menu without a token: %d", code)
	}
	if code := h.do("GET", "/healthz", "", nil, nil); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	for _, path := range []string{"/api/orders", "/api/stock", "/api/me", "/api/stream"} {
		req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s without a token: %d, WWW-Authenticate %q", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if code := h.do("GET", "/api/orders", "not-a-real-token-at-all", nil, nil); code != 401 {
		t.Fatalf("a wrong token: %d", code)
	}
	if code := h.do("POST", "/api/orders", "", `{"command_id":"x"}`, nil); code != 401 {
		t.Fatalf("a command without a token: %d", code)
	}
}

func TestAnOrderThroughHTTPFromTheCartToTheCustomersHand(t *testing.T) {
	h := newHTTPRig(t)
	for _, m := range []struct {
		ing string
		qty int64
	}{{"beans", 1000}, {"milk", 5000}, {"bread", 100}, {"cheese", 2000}} {
		var sv StockView
		if code := h.do("POST", "/api/stock/receive", "storekeeper-token-1", map[string]any{"command_id": h.cid(), "ingredient": m.ing, "qty": m.qty}, &sv); code != 200 {
			t.Fatalf("receive %s: %d", m.ing, code)
		}
	}

	var placed struct {
		Order OrderView `json:"order"`
	}
	body := map[string]any{"command_id": h.cid(), "items": []map[string]any{
		{"item": "coffee", "quantity": 2, "options": map[string][]string{"size": {"l"}}, "note": "no straw"},
		{"item": "toastie", "quantity": 1},
	}}
	if code := h.do("POST", "/api/orders", "customer-token-0001", body, &placed); code != 201 {
		t.Fatalf("placing: %d", code)
	}
	o := placed.Order
	if o.Number != 1 || o.Status != "placed" || o.Total != 135000 || o.Lines[0].Note != "no straw" || o.Lines[0].Pieces[0].Station != "drinks" {
		t.Fatalf("order = %+v", o)
	}

	base := "/api/orders/" + o.ID
	var v OrderView
	if code := h.do("POST", base+"/pay", "cashier-token-00001", map[string]any{"command_id": h.cid(), "method": "cash", "tendered": 200000}, &v); code != 200 || v.Status != "paid" || v.Payment.Change != 65000 {
		t.Fatalf("pay: %d %+v", code, v)
	}
	for _, step := range []struct{ token, station, action string }{
		{"kitchen-drinks-0001", "drinks", "start"}, {"kitchen-hot-0000001", "hot", "start"},
		{"kitchen-drinks-0001", "drinks", "finish"}, {"kitchen-hot-0000001", "hot", "finish"},
	} {
		if code := h.do("POST", base+"/tickets/"+step.station+"/"+step.action, step.token, map[string]any{"command_id": h.cid()}, &v); code != 200 {
			t.Fatalf("%s %s: %d", step.action, step.station, code)
		}
	}
	if v.Status != "ready" {
		t.Fatalf("status = %s, want ready", v.Status)
	}
	if code := h.do("POST", base+"/handover", "runner-token-000001", map[string]any{"command_id": h.cid()}, &v); code != 200 || v.Status != "completed" {
		t.Fatalf("hand over: %d %+v", code, v)
	}

	var mine struct{ Orders []OrderView }
	if code := h.do("GET", "/api/orders", "customer-token-0001", nil, &mine); code != 200 || len(mine.Orders) != 1 || mine.Orders[0].Status != "completed" {
		t.Fatalf("the customer's list: %d %+v", code, mine)
	}
	var evs struct{ Events []EventView }
	if code := h.do("GET", base+"/events", "manager-token-00001", nil, &evs); code != 200 || len(evs.Events) != 7 {
		t.Fatalf("the audit trail: %d %+v", code, evs)
	}
	var sv StockView
	if code := h.do("GET", "/api/stock", "kitchen-drinks-0001", nil, &sv); code != 200 {
		t.Fatalf("the kitchen reading the stock: %d", code)
	}
}

func TestErrorsHaveTheRightStatusAndAStableCode(t *testing.T) {
	h := newHTTPRig(t)
	h.receive("bread", 10)
	h.receive("cheese", 100)
	a := h.place(customer, toastie(1))
	base := "/api/orders/" + a.ID

	cases := []struct {
		name         string
		method, path string
		token        string
		body         any
		wantCode     int
		wantKind     string
	}{
		{"a customer paying", "POST", base + "/pay", "customer-token-0001", map[string]any{"command_id": h.cid(), "method": "card", "reference": "x"}, 403, "forbidden"},
		{"a cook of the wrong station", "POST", base + "/tickets/hot/start", "kitchen-drinks-0001", map[string]any{"command_id": h.cid()}, 403, "forbidden"},
		{"finishing before paying", "POST", base + "/tickets/hot/finish", "kitchen-hot-0000001", map[string]any{"command_id": h.cid()}, 409, "conflict"},
		{"someone else's order", "GET", base, "customer-token-0002", nil, 404, "not_found"},
		{"an order that does not exist", "GET", "/api/orders/ord-nothing", "manager-token-00001", nil, 404, "not_found"},
		{"a bad command ID", "POST", base + "/pay", "cashier-token-00001", map[string]any{"command_id": "x", "method": "card", "reference": "r"}, 400, "invalid"},
		{"a field that does not exist", "POST", base + "/pay", "cashier-token-00001", `{"command_id":"cmd-field-0001","method":"card","reference":"r","tip":5}`, 400, "invalid"},
		{"a body that is not JSON", "POST", base + "/pay", "cashier-token-00001", `not json`, 400, "invalid"},
		{"two documents in one body", "POST", base + "/pay", "cashier-token-00001", `{"command_id":"cmd-two-00001"} {"x":1}`, 400, "invalid"},
		{"a reason that is missing", "POST", base + "/cancel", "customer-token-0001", map[string]any{"command_id": h.cid(), "reason": "  "}, 400, "invalid"},
		{"a bad limit", "GET", "/api/orders?limit=abc", "manager-token-00001", nil, 400, "invalid"},
		{"the audit trail for a cashier", "GET", base + "/events", "cashier-token-00001", nil, 403, "forbidden"},
		{"the stock for a customer", "GET", "/api/stock", "customer-token-0001", nil, 403, "forbidden"},
		{"an ingredient nobody uses", "POST", "/api/stock/receive", "storekeeper-token-1", map[string]any{"command_id": h.cid(), "ingredient": "gold-dust", "qty": 5}, 400, "invalid"},
		{"a write-off of more than there is", "POST", "/api/stock/write-off", "storekeeper-token-1", map[string]any{"command_id": h.cid(), "ingredient": "bread", "qty": 999, "reason": "spilled"}, 409, "out_of_stock"},
	}
	for _, c := range cases {
		var e errAnswer
		if code := h.do(c.method, c.path, c.token, c.body, &e); code != c.wantCode || e.Code != c.wantKind {
			t.Errorf("%s: %d %q (%s), want %d %q", c.name, code, e.Code, e.Error, c.wantCode, c.wantKind)
		}
	}
}

func TestACartThatCannotBeOrderedSaysWhichLinesAndWhy(t *testing.T) {
	h := newHTTPRig(t)
	var e errAnswer
	code := h.do("POST", "/api/orders", "customer-token-0001", map[string]any{"command_id": h.cid(), "items": []map[string]any{
		{"item": "nothing-like-this", "quantity": 1},
		{"item": "toastie", "quantity": 0},
	}}, &e)
	if code != 422 || e.Code != "cart" || len(e.Lines) != 2 || e.Lines[0].Line != 1 || e.Lines[1].Line != 2 {
		t.Fatalf("%d %+v", code, e)
	}
	if code := h.do("POST", "/api/orders", "customer-token-0001", map[string]any{"command_id": h.cid(), "items": []map[string]any{}}, &e); code != 422 {
		t.Fatalf("an empty cart: %d", code)
	}
}

func TestAPaymentTheStockCannotCoverSaysWhatIsShort(t *testing.T) {
	h := newHTTPRig(t)
	h.receive("bread", 4)
	h.receive("cheese", 200)
	a := h.place(customer, toastie(2)) // 4 bread: just enough
	b := h.place(otherCust, toastie(1))
	h.pay(a.ID)
	var e errAnswer
	code := h.do("POST", "/api/orders/"+b.ID+"/pay", "cashier-token-00001", map[string]any{"command_id": h.cid(), "method": "card", "reference": "r"}, &e)
	if code != 409 || e.Code != "out_of_stock" || len(e.Shortages) != 1 || e.Shortages[0].Ingredient != "bread" || e.Shortages[0].Need != 2 || e.Shortages[0].Have != 0 {
		t.Fatalf("%d %+v", code, e)
	}
}

func TestWhenTheLogIsDownTheAnswerIsUnavailableAndRetryable(t *testing.T) {
	h := newHTTPRig(t)
	h.receive("bread", 10)
	h.receive("cheese", 100)
	h.log.FailNext = errors.New("no leader")
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/orders", strings.NewReader(`{"command_id":"cmd-down-00001","items":[{"item":"toastie","quantity":1}]}`))
	req.Header.Set("Authorization", "Bearer customer-token-0001")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// The same request again works, and makes one order.
	var placed struct{ Order OrderView }
	if code := h.do("POST", "/api/orders", "customer-token-0001", `{"command_id":"cmd-down-00001","items":[{"item":"toastie","quantity":1}]}`, &placed); code != 201 || placed.Order.Number != 1 {
		t.Fatalf("the retry: %d %+v", code, placed)
	}
}

func TestTheHealthCheckSaysWhenTheServiceIsBehindTheLog(t *testing.T) {
	h := newHTTPRig(t)
	h.log.LoseAnswerNext = errors.New("timeout")
	_, _ = h.svc.MoveStock(h.ctx, storekeeper, "stock-lost-0001", "receive", "bread", 5, "")
	var body map[string]any
	if code := h.do("GET", "/healthz", "", nil, &body); code != 503 || body["status"] != "behind the log" {
		t.Fatalf("healthz while stale: %d %v", code, body)
	}
}

func TestACustomerSeesOnlyTheirOrdersAndTheKitchenSeesTheTicketsOfItsStation(t *testing.T) {
	h := newHTTPRig(t)
	h.stockAll()
	a := h.place(customer, coffee(1, "s"))
	b := h.place(otherCust, toastie(1))
	h.pay(a.ID)
	h.pay(b.ID)

	var list struct{ Orders []OrderView }
	h.do("GET", "/api/orders", "customer-token-0002", nil, &list)
	if len(list.Orders) != 1 || list.Orders[0].Number != 2 {
		t.Fatalf("the second customer's list: %+v", list)
	}
	h.do("GET", "/api/orders?station=drinks&status=paid,preparing", "kitchen-drinks-0001", nil, &list)
	if len(list.Orders) != 1 || list.Orders[0].Number != 1 {
		t.Fatalf("the drinks station: %+v", list)
	}
}

func TestMeSaysWhoYouAreAndWhatYouMayDo(t *testing.T) {
	h := newHTTPRig(t)
	var me struct {
		ID          string          `json:"id"`
		Roles       []string        `json:"roles"`
		Station     string          `json:"station"`
		Permissions map[string]bool `json:"permissions"`
	}
	if code := h.do("GET", "/api/me", "kitchen-drinks-0001", nil, &me); code != 200 || me.ID != "k-drinks" || me.Station != "drinks" || !me.Permissions["ticket.start"] || me.Permissions["order.record_payment"] {
		t.Fatalf("%d %+v", code, me)
	}
}

func TestAStreamTellsEachPersonOnlyWhatTheyMaySee(t *testing.T) {
	h := newHTTPRig(t)
	h.stockAll()

	open := func(token string) (*bufio.Reader, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		rd := bufio.NewReader(resp.Body)
		if line, _ := rd.ReadString('\n'); !strings.HasPrefix(line, "retry:") {
			t.Fatalf("first line %q", line)
		}
		_, _ = rd.ReadString('\n')
		return rd, func() { cancel(); resp.Body.Close() }
	}
	// event reads the next event, with a time limit.
	event := func(rd *bufio.Reader) (name, data string) {
		type res struct{ name, data string }
		ch := make(chan res, 1)
		go func() {
			var r res
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					ch <- res{"error", err.Error()}
					return
				}
				line = strings.TrimRight(line, "\r\n")
				switch {
				case strings.HasPrefix(line, "event: "):
					r.name = line[7:]
				case strings.HasPrefix(line, "data: "):
					r.data = line[6:]
				case line == "" && r.name != "":
					ch <- r
					return
				}
			}
		}()
		select {
		case r := <-ch:
			return r.name, r.data
		case <-time.After(5 * time.Second):
			t.Fatal("no event within 5 seconds")
			return "", ""
		}
	}

	mine, closeMine := open("customer-token-0001")
	defer closeMine()
	theirs, closeTheirs := open("customer-token-0002")
	defer closeTheirs()
	kitchen, closeKitchen := open("kitchen-drinks-0001")
	defer closeKitchen()

	a := h.place(customer, toastie(1))
	if name, data := event(mine); name != "order" || !strings.Contains(data, `"status":"placed"`) || !strings.Contains(data, a.ID) {
		t.Fatalf("the customer's first event: %s %s", name, data)
	}
	if name, data := event(kitchen); name != "order" || !strings.Contains(data, a.ID) {
		t.Fatalf("the kitchen's first event: %s %s", name, data)
	}
	h.pay(a.ID)
	if name, data := event(mine); name != "order" || !strings.Contains(data, `"status":"paid"`) {
		t.Fatalf("the customer's second event: %s %s", name, data)
	}
	// The kitchen is told about the order, and that the stock changed.
	if name, _ := event(kitchen); name != "order" {
		t.Fatalf("the kitchen's second event: %s", name)
	}
	if name, _ := event(kitchen); name != "stock" {
		t.Fatalf("the kitchen's third event: %s", name)
	}
	// The other customer has heard nothing; the next event they get is their own.
	b := h.place(otherCust, toastie(1))
	if name, data := event(theirs); name != "order" || !strings.Contains(data, b.ID) || strings.Contains(data, a.ID) {
		t.Fatalf("the second customer's first event: %s %s", name, data)
	}
}

func TestAnIdleStreamSendsKeepAlives(t *testing.T) {
	old := streamKeepAlive
	streamKeepAlive = 20 * time.Millisecond
	defer func() { streamKeepAlive = old }()
	h := newHTTPRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer manager-token-00001")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	deadline := time.After(3 * time.Second)
	got := make(chan bool, 1)
	go func() {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				got <- false
				return
			}
			if strings.HasPrefix(line, ": keep-alive") {
				got <- true
				return
			}
		}
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the stream ended without a keep-alive")
		}
	case <-deadline:
		t.Fatal("no keep-alive in 3 seconds")
	}
}

func TestABodyThatIsTooLargeIsRefused(t *testing.T) {
	h := newHTTPRig(t)
	big := `{"command_id":"cmd-big-000001","items":[],"codes":["` + strings.Repeat("x", maxBody) + `"]}`
	var e errAnswer
	if code := h.do("POST", "/api/orders", "customer-token-0001", big, &e); code != 400 {
		t.Fatalf("a body of %d bytes: %d", len(big), code)
	}
}

// --- configuration -----------------------------------------------------------------

func TestTheSampleFilesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "kitchen")
	m, low, err := LoadMenu(filepath.Join(dir, "menu.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Items()) != 4 || len(m.Combos()) != 1 || low["coffee-beans"] != 200 {
		t.Fatalf("menu: %d items, %d combos, low %v", len(m.Items()), len(m.Combos()), low)
	}
	if _, err := LoadPromotions(filepath.Join(dir, "promotions.json")); err != nil {
		t.Fatal(err)
	}
	u, err := LoadUsers(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A service can be opened with them, and the sample cashier is a cashier.
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer demo-token-cashier-1")
	if a, ok := u.Authenticate(req); !ok || !a.Has(rbac.Cashier) {
		t.Fatalf("the sample cashier: %+v %v", a, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
}

func TestAMenuFileWithAMistakeIsRefusedWithTheReason(t *testing.T) {
	good := `{"items":[{"id":"tea","name":"Tea","category":"drinks","station":"drinks","price":20000}]}`
	if _, _, err := ParseMenu([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"a key that does not exist": `{"items":[{"id":"tea","name":"Tea","category":"drinks","station":"drinks","price":20000,"prise":1}]}`,
		"a price below zero":        `{"items":[{"id":"tea","name":"Tea","category":"drinks","station":"drinks","price":-5}]}`,
		"a time that is not a time": `{"items":[{"id":"tea","name":"Tea","category":"drinks","station":"drinks","price":20000,"time_prices":[{"from":"3pm","to":"17:00","price":1}]}]}`,
		"a day that is not a day":   `{"items":[{"id":"tea","name":"Tea","category":"drinks","station":"drinks","price":20000,"time_prices":[{"days":["funday"],"from":"15:00","to":"17:00","price":1}]}]}`,
		"an item twice":             `{"items":[{"id":"tea","name":"Tea","category":"drinks","station":"drinks","price":20000},{"id":"tea","name":"Tea 2","category":"drinks","station":"drinks","price":1}]}`,
		"not JSON":                  `items: []`,
		"two documents":             good + good,
	} {
		if _, _, err := ParseMenu([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAPromotionsFileWithAMistakeIsRefused(t *testing.T) {
	if set, err := ParsePromotions([]byte(`[{"id":"p","name":"P","kind":"percent_off","percent_bp":500,"valid_from":"2026-01-01T00:00:00+07:00"}]`)); err != nil || set == nil {
		t.Fatalf("a good file: %v", err)
	}
	for name, doc := range map[string]string{
		"an unknown kind":           `[{"id":"p","name":"P","kind":"half_price"}]`,
		"a percentage over 100":     `[{"id":"p","name":"P","kind":"percent_off","percent_bp":20000}]`,
		"a date that is not a date": `[{"id":"p","name":"P","kind":"percent_off","percent_bp":500,"valid_to":"tomorrow"}]`,
		"the same code twice":       `[{"id":"a","name":"A","kind":"percent_off","percent_bp":500,"code":"X"},{"id":"b","name":"B","kind":"percent_off","percent_bp":500,"code":"X"}]`,
		"a key that does not exist": `[{"id":"p","name":"P","kind":"percent_off","percent_bp":500,"discount":1}]`,
	} {
		if _, err := ParsePromotions([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if set, err := LoadPromotions(""); err != nil || set == nil {
		t.Fatalf("no file means no promotions: %v", err)
	}
}

func TestAUsersFileWithAMistakeIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"a short token":      `[{"id":"a","roles":["cashier"],"token":"short"}]`,
		"a role that is not": `[{"id":"a","roles":["boss"],"token":"long-enough-token-1"}]`,
		"no role":            `[{"id":"a","roles":[],"token":"long-enough-token-1"}]`,
		"an ID twice":        `[{"id":"a","roles":["cashier"],"token":"long-enough-token-1"},{"id":"a","roles":["cashier"],"token":"long-enough-token-2"}]`,
		"a token twice":      `[{"id":"a","roles":["cashier"],"token":"long-enough-token-1"},{"id":"b","roles":["cashier"],"token":"long-enough-token-1"}]`,
		"nobody":             `[]`,
	} {
		if _, err := ParseUsers([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOnlyAWellFormedBearerTokenOfAKnownPersonAuthenticates(t *testing.T) {
	u := testUsers(t)
	try := func(header string) bool {
		req, _ := http.NewRequest("GET", "/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		_, ok := u.Authenticate(req)
		return ok
	}
	if !try("Bearer cashier-token-00001") || !try("Bearer  cashier-token-00001 ") {
		t.Fatal("the right token was refused")
	}
	for _, h := range []string{"", "cashier-token-00001", "Basic cashier-token-00001", "Bearer cashier-token-0000", "Bearer cashier-token-000011", "bearer cashier-token-00001", "Bearer "} {
		if try(h) {
			t.Errorf("%q was accepted", h)
		}
	}
}
