package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/orderservice"
)

// The kitchen service is a real application of the cluster: its whole state is the events in one
// topic. These tests run it as a real process on a real cluster, take orders from many clients
// at once through their whole lives, kill the service and the nodes of the cluster at random,
// and then check every promise the service made (see ordercheck.go).

var (
	svcBuildOnce sync.Once
	svcBinary    string
	svcBuildErr  error
)

// orderServiceBinary builds cmd/orderservice next to the daemon.
func orderServiceBinary(t *testing.T) string {
	t.Helper()
	daemon(t) // creates binaryDir
	svcBuildOnce.Do(func() {
		name := "orderservice"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		svcBinary = filepath.Join(binaryDir, name)
		cmd := exec.Command(goTool(), "build", "-o", svcBinary, "../../cmd/orderservice")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			svcBuildErr = fmt.Errorf("build orderservice: %v\n%s", err, out)
		}
	})
	if svcBuildErr != nil {
		t.Fatal(svcBuildErr)
	}
	return svcBinary
}

// orderSvc is one orderservice process. It keeps the same port across restarts, so that the
// clients' address does not change when it is killed and started again.
type orderSvc struct {
	t       *testing.T
	args    []string
	bin     string
	addr    string
	logPath string

	mu    sync.Mutex
	cmd   *exec.Cmd
	log   *os.File
	done  chan struct{}
	kills int
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

var sampleDir = filepath.Join("..", "..", "examples", "kitchen")

func newOrderSvc(t *testing.T, c *cluster, topic, logName string) *orderSvc {
	t.Helper()
	addrs := make([]string, c.n)
	for i, nd := range c.nodes {
		addrs[i] = nd.addr
	}
	s := &orderSvc{t: t, bin: orderServiceBinary(t), addr: freeAddr(t), logPath: filepath.Join(t.TempDir(), logName)}
	s.args = []string{
		"-addr", strings.Join(addrs, ","), "-topic", topic, "-listen", s.addr, "-timezone", "UTC",
		"-menu", filepath.Join(sampleDir, "menu.json"), "-users", filepath.Join(sampleDir, "users.json"),
		"-command-timeout", "20s", "-start-timeout", "90s",
	}
	return s
}

func (s *orderSvc) url() string { return "http://" + s.addr }

func (s *orderSvc) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// start launches the process and waits until it answers.
func (s *orderSvc) start() error {
	s.mu.Lock()
	if s.cmd != nil {
		select {
		case <-s.done:
		default:
			s.mu.Unlock()
			return nil
		}
	}
	logFile, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	cmd := exec.Command(s.bin, s.args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		s.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); _ = logFile.Close(); close(done) }()
	s.cmd, s.log, s.done = cmd, logFile, done
	s.mu.Unlock()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return fmt.Errorf("the service exited while starting; see %s", s.logPath)
		default:
		}
		resp, err := (&http.Client{Timeout: time.Second}).Get(s.url() + "/healthz")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the service did not answer within 90 s; see %s", s.logPath)
}

// kill ends the process at once.
func (s *orderSvc) kill() {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	_ = cmd.Process.Kill()
	<-done
	s.mu.Lock()
	s.kills++
	s.mu.Unlock()
}

func (s *orderSvc) killCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kills
}

func (s *orderSvc) tailLog(n int) string {
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// --- the people: the sample users, by the token the service knows them by ------------------

const (
	tokCustomer1 = "demo-token-customer-1"
	tokCustomer2 = "demo-token-customer-2"
	tokCashier   = "demo-token-cashier-1"
	tokDrinks    = "demo-token-kitchen-drinks"
	tokHot       = "demo-token-kitchen-hot"
	tokRunner    = "demo-token-runner-1"
	tokManager   = "demo-token-manager-1"
	tokStore     = "demo-token-storekeeper-1"
)

var customers = []struct{ id, token string }{{"khach-1", tokCustomer1}, {"khach-2", tokCustomer2}}

// api calls the service.
type api struct {
	base string
	http *http.Client
}

func newAPI(base string) *api { return &api{base: base, http: &http.Client{Timeout: 30 * time.Second}} }

// call sends one request and returns the status and body; err is a failure to get an answer.
func (a *api) call(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

type refusal struct {
	Code string `json:"code"`
}

// command sends a command and sends it again, with the same ID, until it is answered or the
// time is up: what a till or a phone does. The answer is Acked for a success, Rejected for a
// definite refusal (a 4xx), Unknown if there was no answer in time.
func (a *api) command(ctx context.Context, kind, id, station, method, path, token string, body any, within time.Duration) (CmdRec, []byte) {
	rec := CmdRec{ID: id, Kind: kind, Station: station, Outcome: Unknown}
	deadline := time.Now().Add(within)
	for {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		status, raw, err := a.call(cctx, method, path, token, body)
		cancel()
		switch {
		case err == nil && status >= 200 && status < 300:
			rec.Outcome = Acked
			return rec, raw
		case err == nil && status >= 400 && status < 500:
			var r refusal
			_ = json.Unmarshal(raw, &r)
			rec.Outcome, rec.Status, rec.Code = Rejected, status, r.Code
			return rec, raw
		}
		// No answer, or a server that said it could not (5xx): try again.
		if time.Now().After(deadline) || ctx.Err() != nil {
			return rec, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// --- the clients ------------------------------------------------------------------------

type shop struct {
	t    *testing.T
	api  *api
	m    *menu.Menu
	mu   sync.Mutex
	runs []OrderRun
	// stepWithin is how long a client keeps sending a command that gets no answer.
	stepWithin time.Duration
}

func (s *shop) record(r OrderRun) {
	s.mu.Lock()
	s.runs = append(s.runs, r)
	s.mu.Unlock()
}

type placedOrder struct {
	Order struct {
		ID    string `json:"id"`
		Total int64  `json:"total"`
	} `json:"order"`
}

// A menu item the clients may order, and the station that makes it.
var menuItems = []struct {
	item, station string
	sizes         []string
}{
	{"ca-phe-sua", "drinks", []string{"m", "l"}},
	{"tra-dao", "drinks", nil},
	{"banh-mi", "hot", nil},
	{"banh-mi-trung", "hot", nil},
}

func randomItems(rng *rand.Rand) []PlanItem {
	n := 1 + rng.Intn(3)
	var items []PlanItem
	used := map[string]bool{}
	for len(items) < n {
		mi := menuItems[rng.Intn(len(menuItems))]
		if used[mi.item] {
			continue
		}
		used[mi.item] = true
		pi := PlanItem{Item: mi.item, Qty: 1 + rng.Intn(3)}
		if len(mi.sizes) > 0 {
			pi.Size = mi.sizes[rng.Intn(len(mi.sizes))]
		}
		items = append(items, pi)
	}
	return items
}

func stationsOf(items []PlanItem) []string {
	seen := map[string]bool{}
	var out []string
	for _, pi := range items {
		for _, mi := range menuItems {
			if mi.item == pi.Item && !seen[mi.station] {
				seen[mi.station] = true
				out = append(out, mi.station)
			}
		}
	}
	return out
}

func stationToken(st string) string {
	if st == "drinks" {
		return tokDrinks
	}
	return tokHot
}

func cartBody(id string, items []PlanItem) map[string]any {
	var lines []map[string]any
	for _, pi := range items {
		l := map[string]any{"item": pi.Item, "quantity": pi.Qty}
		if pi.Size != "" {
			l["options"] = map[string][]string{"size": {pi.Size}}
		}
		lines = append(lines, l)
	}
	return map[string]any{"command_id": id, "items": lines}
}

// worker takes one order after another through its life until ctx ends. Every command has an ID
// of its own and is sent again, with the same ID, when it gets no answer.
func (s *shop) worker(ctx context.Context, w int, rng *rand.Rand) {
	for seq := 1; ctx.Err() == nil; seq++ {
		cust := customers[rng.Intn(len(customers))]
		run := OrderRun{Worker: w, Customer: cust.id, Items: randomItems(rng)}
		id := func(tag string) string { return fmt.Sprintf("w%02d-%05d-%s", w, seq, tag) }

		var placed placedOrder
		rec, raw := s.api.command(context.Background(), CmdPlace, id("place"), "", "POST", "/api/orders", cust.token, cartBody(id("place"), run.Items), s.stepWithin)
		run.Place = rec
		if rec.Outcome == Acked {
			_ = json.Unmarshal(raw, &placed)
		}
		if rec.Outcome != Acked {
			s.record(run)
			time.Sleep(100 * time.Millisecond) // a customer who is told "sold out" does not ask again at once
			continue
		}
		base := "/api/orders/" + placed.Order.ID

		// step sends one command; it reports whether the order may go on.
		step := func(kind, tag, station, tok string, body map[string]any) bool {
			body["command_id"] = id(tag)
			path := base + "/" + map[string]string{
				CmdPay: "pay", CmdHandOver: "handover", CmdCancel: "cancel", CmdCancelRequest: "cancel-request",
				CmdCancelApprove: "cancel-approve", CmdCancelReject: "cancel-reject",
			}[kind]
			if kind == CmdStart || kind == CmdFinish {
				path = base + "/tickets/" + station + "/" + kind
			}
			r, _ := s.api.command(context.Background(), kind, id(tag), station, "POST", path, tok, body, s.stepWithin)
			if v, ok := body["tendered"].(int64); ok { // the checker compares the change with it
				r.Amount = v
			}
			if v, ok := body["refund"].(int64); ok { // and the refund of an approved cancellation
				r.Amount = v
			}
			run.Steps = append(run.Steps, r)
			return r.Outcome == Acked
		}
		work := func() bool { // from paid to handed over
			stations := stationsOf(run.Items)
			for _, st := range stations {
				if !step(CmdStart, "start-"+st, st, stationToken(st), map[string]any{}) {
					return false
				}
			}
			for _, st := range stations {
				if !step(CmdFinish, "finish-"+st, st, stationToken(st), map[string]any{}) {
					return false
				}
			}
			return step(CmdHandOver, "handover", "", tokRunner, map[string]any{})
		}
		pay := func() bool {
			method := "card"
			body := map[string]any{"method": method, "reference": "slip-" + id("pay")}
			if rng.Intn(2) == 0 {
				body = map[string]any{"method": "cash", "tendered": placed.Order.Total + int64(rng.Intn(5))*1000}
			}
			return step(CmdPay, "pay", "", tokCashier, body)
		}

		switch p := rng.Intn(100); {
		case p < 55: // the whole life
			if pay() {
				work()
			} else if last := run.Steps[len(run.Steps)-1]; last.Outcome == Rejected && last.Code == "out_of_stock" {
				step(CmdCancel, "cancel", "", cust.token, map[string]any{"reason": "the kitchen is out of an ingredient"})
			}
		case p < 65: // changed their mind before paying
			step(CmdCancel, "cancel", "", cust.token, map[string]any{"reason": "changed my mind"})
		case p < 77: // paid, then cancelled before any station started: everything goes back
			if pay() && step(CmdCancelRequest, "cancel-request", "", cust.token, map[string]any{"reason": "too slow"}) {
				step(CmdCancelApprove, "cancel-approve", "", tokManager, map[string]any{"refund": placed.Order.Total})
			}
		case p < 89: // a station started, then a cancellation that the manager approves with part of the money back
			if pay() {
				st := stationsOf(run.Items)[0]
				if step(CmdStart, "start-"+st, st, stationToken(st), map[string]any{}) &&
					step(CmdCancelRequest, "cancel-request", "", cust.token, map[string]any{"reason": "waited too long"}) {
					step(CmdCancelApprove, "cancel-approve", "", tokManager, map[string]any{"refund": placed.Order.Total / 2})
				}
			}
		default: // a cancellation that the manager rejects, and the order goes on
			if pay() && step(CmdCancelRequest, "cancel-request", "", cust.token, map[string]any{"reason": "just asking"}) &&
				step(CmdCancelReject, "cancel-reject", "", tokManager, map[string]any{"reason": "already in the kitchen"}) {
				work()
			}
		}
		s.record(run)
	}
}

// --- the scenario -------------------------------------------------------------------------

type orderScenario struct {
	nodes   int
	workers int
	faults  []fault
	extra   []string // more flags for the daemons
	// stock is how much of each ingredient the storekeeper receives. Scarce stock makes
	// "out_of_stock" a legitimate answer to a payment.
	stock   int64
	scarce  map[string]int64 // overrides for particular ingredients
	killSvc bool
	// giveUp is how long a client keeps sending a command that gets no answer; 60 s if zero. A
	// short time makes commands end with an unknown outcome, which the checker must allow for.
	giveUp   time.Duration
	minAcked int // orders whose placing was acknowledged, at least
}

func allIngredients(t *testing.T, m *menu.Menu) []string {
	t.Helper()
	set := map[string]bool{}
	for _, it := range m.Items() {
		for ing := range it.Recipe {
			set[string(ing)] = true
		}
		for _, g := range it.Groups {
			for _, o := range g.Options {
				for ing := range o.Recipe {
					set[string(ing)] = true
				}
			}
		}
	}
	var out []string
	for ing := range set {
		out = append(out, ing)
	}
	return out
}

func waitLeader(t *testing.T, c *cluster) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c.leaderOf() != 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the cluster has no leader")
}

func runOrderScenario(t *testing.T, s orderScenario, duration time.Duration) {
	t.Helper()
	if testing.Short() {
		t.Skip("chaos tests start real processes and take a while")
	}
	seed := chaosSeed(t)
	t.Logf("seed %d (rerun the choices with CHAOS_SEED=%d), faults for %v", seed, seed, duration)

	m, _, err := orderservice.LoadMenu(filepath.Join(sampleDir, "menu.json"))
	if err != nil {
		t.Fatal(err)
	}

	c := newCluster(t, s.nodes, s.extra...)
	for id := 1; id <= s.nodes; id++ {
		c.mustStart(id)
	}
	w := newWorkload(t, c, "unused", 1) // its clients are how the nemesis asks the nodes who leads
	t.Cleanup(w.close)
	waitLeader(t, c)

	const topic = "kitchen-chaos"
	svc := newOrderSvc(t, c, topic, "service.log")
	if err := svc.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.kill)
	sh := &shop{t: t, api: newAPI(svc.url()), m: m, stepWithin: 60 * time.Second}
	if s.giveUp > 0 {
		sh.stepWithin = s.giveUp
	}

	// The storekeeper receives the goods, before any fault: the stock is part of the log too.
	received := map[string]int64{}
	setup, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	for _, ing := range allIngredients(t, m) {
		qty := s.stock
		if v, ok := s.scarce[ing]; ok {
			qty = v
		}
		rec, _ := sh.api.command(setup, "receive", "recv-"+ing, "", "POST", "/api/stock/receive", tokStore,
			map[string]any{"command_id": "recv-" + ing, "ingredient": ing, "qty": qty}, time.Minute)
		if rec.Outcome != Acked {
			t.Fatalf("receiving %s: %+v", ing, rec)
		}
		received[ing] = qty
	}
	cancelSetup()

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var wg sync.WaitGroup
	for i := 1; i <= s.workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sh.worker(ctx, i, rand.New(rand.NewSource(seed+int64(i)*7919)))
		}(i)
	}

	// The service is killed now and then, in the middle of whatever it is doing.
	var killers sync.WaitGroup
	if s.killSvc {
		killers.Add(1)
		go func() {
			defer killers.Done()
			rng := rand.New(rand.NewSource(seed + 99))
			for ctx.Err() == nil {
				sleepCtx(ctx, time.Duration(1000+rng.Intn(1500))*time.Millisecond)
				if ctx.Err() != nil {
					return
				}
				t.Logf("killing the service (kill %d)", svc.killCount()+1)
				svc.kill()
				sleepCtx(ctx, time.Duration(200+rng.Intn(600))*time.Millisecond)
				if err := svc.start(); err != nil {
					t.Errorf("restarting the service: %v", err)
					return
				}
			}
		}()
	}

	var nem *nemesis
	if len(s.faults) > 0 {
		nem = newNemesis(t, c, w, seed+2, s.faults)
		nem.run(ctx)
	} else {
		<-ctx.Done()
	}
	killers.Wait()
	if nem != nil {
		nem.stop() // heals the network and starts every node again
	}
	if !svc.running() {
		if err := svc.start(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait() // clients finish the orders they are in the middle of, now that the faults are over
	waitLeader(t, c)

	sh.mu.Lock()
	runs := append([]OrderRun(nil), sh.runs...)
	sh.mu.Unlock()
	asked := make([]string, len(runs))
	for i, r := range runs {
		asked[i] = r.OrderID()
	}

	// What the service and the log hold.
	fin, err := finalOrderState(t, c, svc, topic, m, received, asked)
	if err != nil {
		if nem != nil {
			t.Log(nem.report())
		}
		t.Logf("the service's log:\n%s", svc.tailLog(30))
		t.Fatal(err)
	}

	tolerate := map[string]bool{}
	if len(s.scarce) > 0 {
		tolerate["out_of_stock"] = true
	}
	violations := CheckOrders(runs, fin, m, tolerate)

	t.Log(summarizeOrders(runs, fin, svc.killCount()))
	if nem != nil {
		t.Log(nem.report())
	}
	if len(violations) > 0 {
		shown := violations
		if len(shown) > 12 {
			shown = shown[:12]
		}
		lines := make([]string, len(shown))
		for i, v := range shown {
			lines[i] = "  " + v.String()
		}
		t.Logf("the service's log:\n%s", svc.tailLog(30))
		t.Fatalf("%d promises were broken (first %d):\n%s", len(violations), len(shown), strings.Join(lines, "\n"))
	}

	acked, completed := 0, 0
	for _, r := range runs {
		if r.Place.Outcome == Acked {
			acked++
		}
	}
	for _, o := range fin.Live {
		if o.Status == "completed" {
			completed++
		}
	}
	if acked < s.minAcked {
		t.Fatalf("only %d orders were acknowledged (want at least %d): the run proves too little", acked, s.minAcked)
	}
	if s.killSvc && svc.killCount() == 0 {
		t.Fatal("the service was never killed: the run proves too little")
	}
	if completed == 0 {
		t.Fatal("no order was completed: the run proves too little")
	}
}

// finalOrderState reads the state three ways: from the service that ran during the faults, from
// a new service that reads the log afterwards, and by building it here from the raw log.
//
// The orders are read one by one, by ID, because the list the API gives is cut at 500. The IDs
// are those of the raw log and those the clients asked for; the number of orders that a service
// says it holds (healthz) must equal the number it returned, so that an order that is in a
// service and in neither list is still noticed.
func finalOrderState(t *testing.T, c *cluster, live *orderSvc, topic string, m *menu.Menu, received map[string]int64, asked []string) (OrdersFinal, error) {
	t.Helper()
	fin := OrdersFinal{Received: received}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	type health struct {
		Skipped int    `json:"skipped_records"`
		Next    uint64 `json:"next_offset"`
		Orders  int    `json:"orders"`
	}
	getJSON := func(a *api, path string, out any) (int, error) {
		var lastErr error
		for ctx.Err() == nil {
			status, raw, err := a.call(ctx, "GET", path, tokManager, nil)
			if err == nil && (status == 200 || status == 404 || status == 503 && path == "/healthz") {
				if status == 200 || status == 503 {
					if err := json.Unmarshal(raw, out); err != nil {
						return status, err
					}
				}
				return status, nil
			}
			lastErr = fmt.Errorf("GET %s: status %d, %v", path, status, err)
			time.Sleep(200 * time.Millisecond)
		}
		return 0, lastErr
	}
	// read returns the service's orders (those among ids), its stock, and its health.
	read := func(a *api, ids []string) ([]orderservice.OrderView, map[string]int64, health, error) {
		var h health
		if _, err := getJSON(a, "/healthz", &h); err != nil {
			return nil, nil, h, fmt.Errorf("reading the health: %w", err)
		}
		var orders []orderservice.OrderView
		for _, id := range ids {
			var o orderservice.OrderView
			status, err := getJSON(a, "/api/orders/"+id, &o)
			if err != nil {
				return nil, nil, h, err
			}
			if status == 200 {
				orders = append(orders, o)
			}
		}
		if len(orders) != h.Orders {
			return nil, nil, h, fmt.Errorf("the service holds %d orders, and %d of them are among the %d the log or the clients know of", h.Orders, len(orders), len(ids))
		}
		var sv orderservice.StockView
		if _, err := getJSON(a, "/api/stock", &sv); err != nil {
			return nil, nil, h, fmt.Errorf("reading the stock: %w", err)
		}
		stock := map[string]int64{}
		for _, l := range sv.Levels {
			stock[l.Ingredient] = l.Qty
		}
		return orders, stock, h, nil
	}

	// A new service, reading the log from the start. It makes its state complete with a barrier,
	// so the number of records it says the log holds is the number a complete reading has.
	fresh := newOrderSvc(t, c, topic, "fresh.log")
	if err := fresh.start(); err != nil {
		return fin, fmt.Errorf("starting a fresh service: %w", err)
	}
	defer fresh.kill()
	var freshHealth health
	if _, err := getJSON(newAPI(fresh.url()), "/healthz", &freshHealth); err != nil {
		return fin, fmt.Errorf("the fresh service: %w", err)
	}

	// The raw log, read here, and the state built from it.
	addrs := make([]string, c.n)
	for i, nd := range c.nodes {
		addrs[i] = nd.addr
	}
	cl, err := client.New(addrs, client.WithRetryPause(20*time.Millisecond))
	if err != nil {
		return fin, err
	}
	defer cl.Close()
	promos, err := promo.NewSet(nil)
	if err != nil {
		return fin, err
	}
	st, err := orderservice.NewState(m, promos, nil)
	if err != nil {
		return fin, err
	}
	for _, r := range readLog(t, cl, topic, freshHealth.Next) {
		rec, derr := orderservice.DecodeRecord(r.Value)
		if derr != nil {
			rec = orderservice.Record{} // the state skips an empty record and counts it
		}
		if _, aerr := st.ApplyRecord(r.Offset, rec); aerr != nil {
			return fin, fmt.Errorf("rebuilding from the raw log: %w", aerr)
		}
	}
	fin.RebuiltSkipped = st.Skipped()
	known := map[string]bool{}
	var ids []string
	for _, o := range st.Orders() {
		fin.Rebuilt = append(fin.Rebuilt, orderservice.ViewOf(o))
		known[string(o.ID)] = true
		ids = append(ids, string(o.ID))
	}
	fin.RebuiltStock = st.StockLevels()
	for _, id := range asked {
		if !known[id] {
			ids = append(ids, id) // an order that the clients asked for and that the log does not have
		}
	}

	var h health
	if fin.Live, fin.LiveStock, h, err = read(newAPI(live.url()), ids); err != nil {
		return fin, fmt.Errorf("the live service: %w", err)
	}
	fin.Skipped = h.Skipped
	if fin.Fresh, fin.FreshStock, h, err = read(newAPI(fresh.url()), ids); err != nil {
		return fin, fmt.Errorf("the fresh service: %w", err)
	}
	fin.FreshSkipped = h.Skipped
	return fin, nil
}

func summarizeOrders(runs []OrderRun, fin OrdersFinal, kills int) string {
	var placed, unknown, refused, steps, stepsUnknown, stepsRefused int
	for _, r := range runs {
		switch r.Place.Outcome {
		case Acked:
			placed++
		case Unknown:
			unknown++
		case Rejected:
			refused++
		}
		for _, s := range r.Steps {
			steps++
			switch s.Outcome {
			case Unknown:
				stepsUnknown++
			case Rejected:
				stepsRefused++
			}
		}
	}
	status := map[string]int{}
	for _, o := range fin.Live {
		status[o.Status]++
	}
	return fmt.Sprintf("%d clients' orders: %d placed and acknowledged, %d with no answer, %d refused; %d later commands (%d with no answer, %d refused); the service was killed %d times; %d orders in the log: %v",
		len(runs), placed, unknown, refused, steps, stepsUnknown, stepsRefused, kills, len(fin.Live), status)
}

// With no faults at all the harness is under test: every command goes through and the checker
// has nothing to say.
func TestOrdersChaosHarnessWithoutFaults(t *testing.T) {
	runOrderScenario(t, orderScenario{nodes: 3, workers: 4, stock: 1_000_000, minAcked: 20}, chaosDuration(6*time.Second))
}

func TestOrdersChaosServiceAndLeaderKilledWhileOrdersAreTaken(t *testing.T) {
	runOrderScenario(t, orderScenario{
		nodes: 3, workers: 8, stock: 1_000_000, killSvc: true, minAcked: 20,
		faults: []fault{faultCrash, faultKillLeader},
	}, chaosDuration(20*time.Second))
}

func TestOrdersChaosNetworkFaultsAndAScarceStock(t *testing.T) {
	// Few baguettes and little cheese: payments compete for the last ones, and some are
	// refused for lack of stock. The stock must still be exactly what the orders imply.
	runOrderScenario(t, orderScenario{
		nodes: 5, workers: 8, stock: 1_000_000, killSvc: true, minAcked: 20, giveUp: 1 * time.Second,
		scarce: map[string]int64{"baguette": 40, "egg": 25, "coffee-beans": 1500},
		faults: []fault{faultIsolate, faultSplit, faultAsymmetric, faultStaleNode, faultKillLeader},
		extra:  []string{"-snapshot-every", "25", "-snapshot-trailing", "5"},
	}, chaosDuration(20*time.Second))
}

// readLog reads the whole topic, and goes on reading until it has at least atLeast records. A read
// of a cluster may be answered by a node that is behind, so one read could miss the newest
// records. atLeast is the number of records that a service which made its state complete says the
// log holds, so a read that returned fewer was answered by a node that had not caught up.
func readLog(t *testing.T, cl *client.Client, topic string, atLeast uint64) []client.Record {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		recs := readAll(t, cl, topic)
		if uint64(len(recs)) >= atLeast {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the log shows %d records after 60 s, and a service that read it says it holds %d", len(recs), atLeast)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
