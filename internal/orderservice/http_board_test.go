package orderservice

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
)

// boardRig takes some orders to different points of their way, so that the board has something on it.
func boardRig(t *testing.T) (*rig, map[string]string) {
	t.Helper()
	r := newRig(t)
	r.stockAll()
	ids := map[string]string{}
	ids["unpaid"] = r.place(customer, toastie(1)).ID
	ids["paid"] = r.pay(r.place(customer, toastie(1)).ID).ID
	ids["making"] = r.pay(r.place(customer, toastie(1)).ID).ID
	if _, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(ids["making"]), r.cmd(), "hot"); err != nil {
		t.Fatal(err)
	}
	ids["ready"] = r.pay(r.place(customer, toastie(1)).ID).ID
	if _, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(ids["ready"]), r.cmd(), "hot"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.FinishTicket(r.ctx, hotCook, order.ID(ids["ready"]), r.cmd(), "hot"); err != nil {
		t.Fatal(err)
	}
	ids["done"] = r.pay(r.place(customer, toastie(1)).ID).ID
	for _, step := range []func() error{
		func() error {
			_, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(ids["done"]), r.cmd(), "hot")
			return err
		},
		func() error {
			_, err := r.svc.FinishTicket(r.ctx, hotCook, order.ID(ids["done"]), r.cmd(), "hot")
			return err
		},
		func() error { _, err := r.svc.HandOver(r.ctx, cashier, order.ID(ids["done"]), r.cmd()); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	return r, ids
}

func TestTheBoardListsTheOrdersBeingMadeAndTheOnesThatAreReadyAndNothingElse(t *testing.T) {
	r, ids := boardRig(t)
	b := r.svc.Board()
	number := func(id string) int {
		v, err := r.svc.Order(cashier, order.ID(id))
		if err != nil {
			t.Fatal(err)
		}
		return v.Number
	}
	var making, ready []int
	for _, e := range b.Preparing {
		making = append(making, e.Number)
	}
	for _, e := range b.Ready {
		ready = append(ready, e.Number)
	}
	if len(making) != 2 || making[0] != number(ids["paid"]) || making[1] != number(ids["making"]) {
		t.Fatalf("being made: %v, want the paid order and the started one, oldest first", making)
	}
	if len(ready) != 1 || ready[0] != number(ids["ready"]) {
		t.Fatalf("ready: %v", ready)
	}
	// not shown: one that is not paid, and one that was handed over
	for _, e := range append(append([]BoardEntry{}, b.Preparing...), b.Ready...) {
		if e.Number == number(ids["unpaid"]) || e.Number == number(ids["done"]) {
			t.Fatalf("order %d should not be on the board", e.Number)
		}
		if e.Since.IsZero() {
			t.Fatalf("an entry with no time: %+v", e)
		}
	}
	// handing the ready one over takes it off
	if _, err := r.svc.HandOver(r.ctx, cashier, order.ID(ids["ready"]), r.cmd()); err != nil {
		t.Fatal(err)
	}
	if n := len(r.svc.Board().Ready); n != 0 {
		t.Fatalf("%d ready after the hand-over", n)
	}
}

func TestTheOneThatHasWaitedLongestIsFirstAndTheListIsBounded(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	r.receive("bread", 400)
	r.receive("cheese", 8000)
	var first int
	for i := 0; i < boardLimit+5; i++ {
		o := r.pay(r.place(customer, toastie(1)).ID)
		if i == 0 {
			first = o.Number
		}
	}
	b := r.svc.Board()
	if len(b.Preparing) != boardLimit {
		t.Fatalf("%d numbers on the board, want the limit %d", len(b.Preparing), boardLimit)
	}
	if b.Preparing[0].Number != first {
		t.Fatalf("the first on the board is %d, want %d (the one paid first)", b.Preparing[0].Number, first)
	}
}

func TestTheBoardNeverSaysWhoOrderedOrWhat(t *testing.T) {
	r, _ := boardRig(t)
	raw, _ := json.Marshal(r.svc.Board())
	var lists struct {
		Preparing []map[string]any `json:"preparing"`
		Ready     []map[string]any `json:"ready"`
	}
	if err := json.Unmarshal(raw, &lists); err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]map[string]any{lists.Preparing, lists.Ready} {
		if len(list) == 0 {
			t.Fatal("the test needs orders on both lists")
		}
		for _, e := range list {
			for field := range e {
				if field != "number" && field != "since" {
					t.Fatalf("the entry carries %q; a board shown to a room has only a number and a time", field)
				}
			}
		}
	}
	for _, word := range []string{"cus-1", "toastie", "Toastie", "customer"} {
		if strings.Contains(string(raw), word) {
			t.Fatalf("the board mentions %q: %s", word, raw)
		}
	}
}

func boardServer(t *testing.T, r *rig, public bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewAPI(r.svc, TokenAuth(testUsers(t)), nil, WithPublicBoard(public)))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestAPublicBoardNeedsNoSignInAndAPrivateOneNeedsTheRightToSeeOrders(t *testing.T) {
	r, _ := boardRig(t)
	public, private := boardServer(t, r, true), boardServer(t, r, false)
	if code, raw := get(t, public.URL+"/api/board", ""); code != 200 || !strings.Contains(string(raw), `"ready"`) {
		t.Fatalf("a public board without signing in: %d %s", code, raw)
	}
	if code, _ := get(t, private.URL+"/api/board", ""); code != 401 {
		t.Fatalf("a private board without signing in: %d", code)
	}
	if code, _ := get(t, private.URL+"/api/board", "customer-token-0001"); code != 403 {
		t.Fatalf("a private board for a customer: %d", code)
	}
	for _, tok := range []string{"cashier-token-00001", "runner-token-000001", "kitchen-drinks-0001", "manager-token-00001"} {
		if code, _ := get(t, private.URL+"/api/board", tok); code != 200 {
			t.Errorf("a private board for %s: %d", tok, code)
		}
	}
	// the answers are not kept by a cache: a board that is old is wrong
	req, _ := http.NewRequest("GET", public.URL+"/api/board", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control: %q", resp.Header.Get("Cache-Control"))
	}
}

// readEvents reads a stream until it sees the wanted event, or gives up.
func readEvent(t *testing.T, body io.Reader, want string, within time.Duration) bool {
	t.Helper()
	found := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: "+want) {
				found <- true
				return
			}
		}
		found <- false
	}()
	select {
	case ok := <-found:
		return ok
	case <-time.After(within):
		return false
	}
}

func TestTheBoardStreamSaysSoWhenAnOrderChanges(t *testing.T) {
	r, ids := boardRig(t)
	srv := boardServer(t, r, true)
	resp, err := http.Get(srv.URL + "/api/board/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	time.Sleep(150 * time.Millisecond) // the subscription is in place
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = r.svc.FinishTicket(r.ctx, hotCook, order.ID(ids["making"]), r.cmd(), "hot")
	}()
	if !readEvent(t, resp.Body, "board", 3*time.Second) {
		t.Fatal("the screen was not told that an order became ready")
	}
}

func TestAnEventOfTheStockOrTheMenuIsNotAnEventOfTheBoard(t *testing.T) {
	r := newRig(t)
	sub := r.svc.SubscribeBoard()
	defer sub.Close()
	r.receive("bread", 5)
	if err := r.svc.SetMenu(testMenu(t), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ch := <-sub.C:
		t.Fatalf("the board was told about %+v", ch)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAPrivateBoardStreamEndsWhenTheSessionDoes(t *testing.T) {
	old := streamKeepAlive
	streamKeepAlive = 50 * time.Millisecond
	defer func() { streamKeepAlive = old }()
	r, _ := boardRig(t)
	srv := boardServer(t, r, false)
	req, _ := http.NewRequest("GET", srv.URL+"/api/board/stream", nil)
	req.Header.Set("Authorization", "Bearer cashier-token-00001")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream: %v %v", resp, err)
	}
	defer resp.Body.Close()
	// a token that stays good keeps the stream open: it gets keep-alives and no reset
	if readEvent(t, resp.Body, "reset", 400*time.Millisecond) {
		t.Fatal("the stream was reset for a person who may still watch")
	}
}

func TestOnlyAGivenNumberOfBoardStreamsAreServed(t *testing.T) {
	r, _ := boardRig(t)
	srv := boardServer(t, r, true)
	var open []*http.Response
	defer func() {
		for _, resp := range open {
			resp.Body.Close()
		}
	}()
	refused := 0
	for i := 0; i < maxBoardStreams+5; i++ {
		resp, err := http.Get(srv.URL + "/api/board/stream")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusServiceUnavailable {
			refused++
			resp.Body.Close()
			continue
		}
		open = append(open, resp)
	}
	if len(open) != maxBoardStreams || refused != 5 {
		t.Fatalf("%d open and %d refused, want %d and 5", len(open), refused, maxBoardStreams)
	}
	// one that closes makes room for another
	open[0].Body.Close()
	time.Sleep(200 * time.Millisecond)
	resp, err := http.Get(srv.URL + "/api/board/stream")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("after one closed: %v %v", resp, err)
	}
	open = append(open, resp)
}

func TestTheBoardPageIsServedWithItsScriptUnderTheStrictPolicy(t *testing.T) {
	h := newHTTPRig(t)
	resp, err := http.Get(h.srv.URL + "/board")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("/board: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", resp.Header)
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="(/[^"]*)"`).FindAllStringSubmatch(html, -1) {
		if _, ok := assets[m[1]]; !ok {
			t.Errorf("the board page refers to %q, which the service does not serve", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`<script[^>]*>([\s\S]*?)</script>`).FindAllStringSubmatch(html, -1) {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("an inline script: %.60q", m[1])
		}
	}
	if regexp.MustCompile(`\sstyle\s*=|\son[a-z]+\s*=|<style`).MatchString(html) {
		t.Error("the board page has an inline style or an event attribute: the security policy would block it")
	}
	if i, j := strings.Index(html, `src="/i18n.js"`), strings.Index(html, `src="/board.js"`); i < 0 || j < 0 || i > j {
		t.Error("i18n.js must be loaded before board.js")
	}
}
