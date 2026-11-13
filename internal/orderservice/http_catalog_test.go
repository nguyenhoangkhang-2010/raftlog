package orderservice

import (
	"bufio"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/catalog"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

// The menu that the tests start with, as the document that a file or the database holds.
const menuV1 = `{"items":[
 {"id":"coffee","name":"Coffee","category":"drinks","station":"drinks","price":35000,"recipe":{"beans":20,"milk":150},
  "groups":[{"id":"size","name":"Size","min":1,"max":1,"options":[{"id":"s","name":"Small","delta":0},{"id":"l","name":"Large","delta":10000,"recipe":{"beans":10}}]}]},
 {"id":"toastie","name":"Toastie","category":"food","station":"hot","price":45000,"recipe":{"bread":2,"cheese":40}}
]}`

type catRig struct {
	*accRig
	store *catalog.Store
}

func newCatRig(t *testing.T) *catRig {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctxBackground, pool, accounts.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctxBackground, pool, catalog.Migrations(), pgmigrate.WithTable(catalog.MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	acc, err := accounts.New(pool, accounts.Config{Hash: accounts.HashParams{Memory: 8, Iterations: 1, Parallelism: 1}})
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(pool, nil)
	if _, err := store.Save(ctxBackground, catalog.Menu, 0, []byte(menuV1), "system", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctxBackground, catalog.Promotions, 0, []byte(`[]`), "system", "first"); err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, WithAccounts(acc, AccountsOptions{}), WithCatalog(store)))
	t.Cleanup(srv.Close)
	c := &catRig{accRig: &accRig{rig: r, acc: acc, pool: pool, srv: srv}, store: store}
	c.stockAll()
	return c
}

func doc(items string) map[string]any {
	return map[string]any{"base_version": 1, "note": "test", "doc": json.RawMessage(items)}
}

func (b *browser) getJSON(path string, out any) int {
	b.t.Helper()
	code, _ := b.do("GET", path, nil, out)
	return code
}

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func (b *browser) put(path string, body []byte) (int, http.Header, []byte) {
	b.t.Helper()
	req := b.request("PUT", path, nil)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

type menuPage struct {
	Items []struct {
		ID    string
		Name  string
		Price int64
		Image string
	}
}

func priceOf(m menuPage, id string) int64 {
	for _, it := range m.Items {
		if it.ID == id {
			return it.Price
		}
	}
	return -1
}

func TestOnlyThoseWhoMayManageTheMenuCanReadOrChangeIt(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cash-1", "", rbac.Cashier)
	a.user("cus-1", "", rbac.Customer)
	anon, cash, cus, mgr := a.browser(), a.browser(), a.browser(), a.browser()
	cash.signIn("cash-1")
	cus.signIn("cus-1")
	mgr.signIn("mgr-1")
	for _, p := range []string{"/api/admin/menu", "/api/admin/menu/versions", "/api/admin/promotions", "/api/admin/promotions/versions"} {
		if code := anon.status("GET", p, nil); code != 401 {
			t.Errorf("%s without signing in: %d", p, code)
		}
		for name, b := range map[string]*browser{"cashier": cash, "customer": cus} {
			if code := b.status("GET", p, nil); code != 403 {
				t.Errorf("%s reading %s: %d", name, p, code)
			}
		}
		if code := mgr.status("GET", p, nil); code != 200 {
			t.Errorf("the manager reading %s: %d", p, code)
		}
	}
	for name, b := range map[string]*browser{"cashier": cash, "customer": cus} {
		if code := b.status("PUT", "/api/admin/menu", doc(menuV1)); code != 403 {
			t.Errorf("%s saving the menu: %d", name, code)
		}
		if code, _, _ := b.put("/api/admin/menu/items/coffee/image", pngBytes(100, 100)); code != 403 {
			t.Errorf("%s putting a photo: %d", name, code)
		}
	}
}

func TestAChangeOfThePriceCountsAtOnceAndOldOrdersKeepTheirs(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cus-1", "", rbac.Customer)
	mgr, cus := a.browser(), a.browser()
	mgr.signIn("mgr-1")
	cus.signIn("cus-1")

	var m menuPage
	cus.getJSON("/api/menu", &m)
	if priceOf(m, "toastie") != 45000 {
		t.Fatalf("the starting price: %+v", m)
	}
	var placed struct {
		Order struct {
			ID    string
			Total int64
		}
	}
	if code, _ := cus.do("POST", "/api/orders", map[string]any{"command_id": "cat-order-0001", "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &placed); code != 201 || placed.Order.Total != 45000 {
		t.Fatalf("the first order: %d %+v", code, placed)
	}

	newMenu := strings.Replace(menuV1, `"price":45000`, `"price":50000`, 1)
	end := strings.LastIndex(newMenu, `]}`)
	newMenu = newMenu[:end] + `,{"id":"tea","name":"Peach tea","category":"drinks","station":"drinks","price":30000,"recipe":{"leaves":2}}]}`
	var saved struct{ Version versionView }
	if code, _ := mgr.do("PUT", "/api/admin/menu", doc(newMenu), &saved); code != 200 || saved.Version.Version != 2 || saved.Version.By != "mgr-1" {
		t.Fatalf("save: %d %+v", code, saved)
	}
	cus.getJSON("/api/menu", &m)
	if priceOf(m, "toastie") != 50000 || priceOf(m, "tea") != 30000 {
		t.Fatalf("the menu after the change: %+v", m)
	}
	// The order taken before keeps its price; an order taken now has the new one.
	var old struct{ Total int64 }
	if code, _ := cus.do("GET", "/api/orders/"+placed.Order.ID, nil, &old); code != 200 || old.Total != 45000 {
		t.Fatalf("the old order: %d %+v", code, old)
	}
	if code, _ := cus.do("POST", "/api/orders", map[string]any{"command_id": "cat-order-0002", "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &placed); code != 201 || placed.Order.Total != 50000 {
		t.Fatalf("an order after the change: %d %+v", code, placed)
	}
	// The new item uses an ingredient that did not exist: until the goods are received it is sold out,
	// and the storekeeper can receive it, because the menu now uses it.
	var m2 struct {
		Items []struct {
			ID      string
			SoldOut bool `json:"sold_out"`
		}
	}
	cus.getJSON("/api/menu", &m2)
	for _, it := range m2.Items {
		if it.ID == "tea" && !it.SoldOut {
			t.Fatal("an item with no stock of its ingredient is for sale")
		}
	}
	if _, err := a.svc.MoveStock(a.ctx, storekeeper, a.cmd(), "receive", "leaves", 50, ""); err != nil {
		t.Fatalf("receiving an ingredient that the new menu uses: %v", err)
	}
}

func TestADocumentThatDoesNotPassTheChecksChangesNothing(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	mgr := a.browser()
	mgr.signIn("mgr-1")
	cases := map[string]string{
		"a misspelt key":          `{"items":[{"id":"x","nme":"typo"}]}`,
		"a repeated id":           strings.Replace(menuV1, `"id":"toastie"`, `"id":"coffee"`, 1),
		"a price below zero":      strings.Replace(menuV1, `"price":45000`, `"price":-1`, 1),
		"an item with no station": strings.Replace(menuV1, `"station":"hot"`, `"station":""`, 1),
		"a time that is not one":  strings.Replace(menuV1, `"price":45000`, `"price":45000,"time_prices":[{"from":"25:00","to":"26:00","price":1}]`, 1),
		"not JSON":                `{"items":`,
	}
	for name, d := range cases {
		var e errAnswer
		code, _ := mgr.do("PUT", "/api/admin/menu", doc(d), &e)
		if code != 400 {
			t.Errorf("%s: %d %+v", name, code, e)
		}
		if name != "not JSON" && e.Code != "invalid_document" {
			t.Errorf("%s: code %q, want invalid_document", name, e.Code)
		}
	}
	var got struct{ Version versionView }
	mgr.getJSON("/api/admin/menu", &got)
	if got.Version.Version != 1 {
		t.Fatalf("a refused document made version %d", got.Version.Version)
	}
	var m menuPage
	mgr.getJSON("/api/menu", &m)
	if priceOf(m, "toastie") != 45000 {
		t.Fatalf("a refused document changed the menu: %+v", m)
	}
	// the message is the one of the file's checks, so that a manager is told what the file would say
	var e errAnswer
	mgr.do("PUT", "/api/admin/menu", doc(`{"items":[{"id":"x","nme":"typo"}]}`), &e)
	if !strings.Contains(e.Error, "menu:") {
		t.Fatalf("the message: %q", e.Error)
	}
}

func TestTwoManagersEditingDoNotOverwriteEachOther(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	a.user("mgr-2", "", rbac.Manager)
	one, two := a.browser(), a.browser()
	one.signIn("mgr-1")
	two.signIn("mgr-2")
	if code := one.status("PUT", "/api/admin/menu", doc(strings.Replace(menuV1, `"price":45000`, `"price":46000`, 1))); code != 200 {
		t.Fatalf("the first save: %d", code)
	}
	var e errAnswer
	code, _ := two.do("PUT", "/api/admin/menu", doc(strings.Replace(menuV1, `"price":45000`, `"price":47000`, 1)), &e)
	if code != 409 || e.Code != "stale" {
		t.Fatalf("the second, from the old version: %d %+v", code, e)
	}
	var m menuPage
	two.getJSON("/api/menu", &m)
	if priceOf(m, "toastie") != 46000 {
		t.Fatalf("the refused save changed the menu: %+v", m)
	}
}

func TestAnOldVersionCanBePutBackAndTheHistoryStays(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	mgr := a.browser()
	mgr.signIn("mgr-1")
	if code := mgr.status("PUT", "/api/admin/menu", doc(strings.Replace(menuV1, `"price":45000`, `"price":99000`, 1))); code != 200 {
		t.Fatalf("save: %d", code)
	}
	var list struct{ Versions []versionView }
	if code := mgr.getJSON("/api/admin/menu/versions", &list); code != 200 || len(list.Versions) != 2 || list.Versions[0].Version != 2 || list.Versions[1].By != "system" {
		t.Fatalf("history: %d %+v", code, list)
	}
	var old struct{ Doc json.RawMessage }
	if code := mgr.getJSON("/api/admin/menu/versions/1", &old); code != 200 || !strings.Contains(string(old.Doc), "45000") {
		t.Fatalf("version 1: %d %s", code, old.Doc)
	}
	var e errAnswer
	if code, _ := mgr.do("POST", "/api/admin/menu/versions/1/restore", map[string]any{"base_version": 1}, &e); code != 409 || e.Code != "stale" {
		t.Fatalf("restoring on a stale base: %d %+v", code, e)
	}
	var back struct{ Version versionView }
	if code, _ := mgr.do("POST", "/api/admin/menu/versions/1/restore", map[string]any{"base_version": 2}, &back); code != 200 || back.Version.Version != 3 || !strings.Contains(back.Version.Note, "restored version 1") {
		t.Fatalf("restore: %d %+v", code, back)
	}
	var m menuPage
	mgr.getJSON("/api/menu", &m)
	if priceOf(m, "toastie") != 45000 {
		t.Fatalf("after restoring: %+v", m)
	}
	mgr.getJSON("/api/admin/menu/versions", &list)
	if len(list.Versions) != 3 {
		t.Fatalf("restoring rewrote the history: %+v", list)
	}
	for _, n := range []string{"0", "x", "99"} {
		if code := mgr.status("GET", "/api/admin/menu/versions/"+n, nil); code != 400 && code != 404 {
			t.Errorf("version %s: %d", n, code)
		}
	}
}

func TestAPromotionThatIsSavedWorksOnTheNextOrder(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cus-1", "", rbac.Customer)
	mgr, cus := a.browser(), a.browser()
	mgr.signIn("mgr-1")
	cus.signIn("cus-1")
	order := func(id string) (int64, []string) {
		var out struct {
			Order    struct{ Total int64 }
			Rejected []string `json:"rejected_codes"`
		}
		if code, _ := cus.do("POST", "/api/orders", map[string]any{"command_id": id, "codes": []string{"TEN"}, "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &out); code != 201 {
			t.Fatalf("order %s: %d", id, code)
		}
		return out.Order.Total, out.Rejected
	}
	if total, rejected := order("promo-order-01"); total != 45000 || len(rejected) != 1 {
		t.Fatalf("before the promotion: %d %v", total, rejected)
	}
	promos := `[{"id":"ten","name":"Ten percent","kind":"percent_off","code":"TEN","percent_bp":1000}]`
	if code := mgr.status("PUT", "/api/admin/promotions", doc(promos)); code != 200 {
		t.Fatalf("save promotions: %d", code)
	}
	if total, rejected := order("promo-order-02"); total != 40500 || len(rejected) != 0 {
		t.Fatalf("after the promotion: %d %v", total, rejected)
	}
	var e errAnswer
	if code, _ := mgr.do("PUT", "/api/admin/promotions", map[string]any{"base_version": 2, "doc": json.RawMessage(`[{"id":"x","kind":"percent_off","percent_bp":99999}]`)}, &e); code != 400 || e.Code != "invalid_document" {
		t.Fatalf("a bad promotion: %d %+v", code, e)
	}
}

func TestAPhotoIsKeptShownToEveryoneAndCheckedForWhatItIs(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	mgr, anyone := a.browser(), a.browser()
	mgr.signIn("mgr-1")
	good := pngBytes(120, 90)
	if code, _, raw := mgr.put("/api/admin/menu/items/toastie/image", good); code != 200 {
		t.Fatalf("put: %d %s", code, raw)
	}
	var m menuPage
	anyone.getJSON("/api/menu", &m) // nobody has signed in: the menu and its photos are public
	var url string
	for _, it := range m.Items {
		if it.ID == "toastie" {
			url = it.Image
		}
	}
	if !strings.HasPrefix(url, "/api/images/toastie?v=") {
		t.Fatalf("the menu has no address for the photo: %+v", m)
	}
	resp, err := http.Get(a.srv.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, good) {
		t.Fatalf("the photo: %d %q %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers of a photo: %v", resp.Header)
	}
	req, _ := http.NewRequest("GET", a.srv.URL+url, nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	again, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusNotModified {
		t.Fatalf("a photo that has not changed: %d", again.StatusCode)
	}

	// What is not a photo is refused, and what is for an item that does not exist.
	for name, data := range map[string][]byte{
		"text":   []byte("<script>alert(1)</script>"),
		"an SVG": []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200"><script>alert(1)</script></svg>`),
		"tiny":   pngBytes(8, 8),
		"huge":   bytes.Repeat([]byte{1}, catalog.MaxPhotoBytes+10),
	} {
		if code, _, _ := mgr.put("/api/admin/menu/items/toastie/image", data); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _, _ := mgr.put("/api/admin/menu/items/nothing/image", good); code != 404 {
		t.Errorf("a photo for an item that does not exist: %d", code)
	}
	if code := anyone.status("GET", "/api/images/nothing", nil); code != 404 {
		t.Errorf("a photo that is not there: %d", code)
	}
	// Taking it away is seen by the menu.
	if code := mgr.status("DELETE", "/api/admin/menu/items/toastie/image", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	m = menuPage{} // decoding into the old value would keep the field that is now absent
	anyone.getJSON("/api/menu", &m)
	for _, it := range m.Items {
		if it.ID == "toastie" && it.Image != "" {
			t.Fatalf("the menu still has the photo: %+v", it)
		}
	}
}

func TestScreensAreToldWhenTheMenuChanges(t *testing.T) {
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cus-1", "", rbac.Customer)
	mgr, cus := a.browser(), a.browser()
	mgr.signIn("mgr-1")
	cus.signIn("cus-1")
	resp, err := (&http.Client{Jar: cus.jar}).Do(cus.request("GET", "/api/stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan bool, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if strings.HasPrefix(line, "event: menu") {
				got <- true
				return
			}
			if err != nil {
				got <- false
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond) // the stream is open
	if code := mgr.status("PUT", "/api/admin/menu", doc(strings.Replace(menuV1, `"price":45000`, `"price":46000`, 1))); code != 200 {
		t.Fatalf("save: %d", code)
	}
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the stream ended without a menu event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a customer's screen was not told that the menu changed")
	}
}

func TestTheMenuIsRebuiltFromTheDatabaseAtStart(t *testing.T) {
	// What a manager saved is what the next start uses: the latest version is the one that loads.
	a := newCatRig(t)
	a.user("mgr-1", "", rbac.Manager)
	mgr := a.browser()
	mgr.signIn("mgr-1")
	if code := mgr.status("PUT", "/api/admin/menu", doc(strings.Replace(menuV1, `"price":45000`, `"price":48000`, 1))); code != 200 {
		t.Fatalf("save: %d", code)
	}
	v, ok, err := a.store.Latest(ctxBackground, catalog.Menu)
	if err != nil || !ok || v.Number != 2 {
		t.Fatalf("latest: %+v %v %v", v, ok, err)
	}
	m, _, err := ParseMenu(v.Doc)
	if err != nil {
		t.Fatalf("the saved document does not load again: %v", err)
	}
	it, _ := m.Item("toastie")
	if it.Price != 48000 {
		t.Fatalf("the price that loads: %d", it.Price)
	}
}
