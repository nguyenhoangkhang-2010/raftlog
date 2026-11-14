package main

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
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/bench"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/console"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

var sample = filepath.Join("..", "..", "examples", "kitchen")

func TestSettingsAreChecked(t *testing.T) {
	menu, users := filepath.Join(sample, "menu.json"), filepath.Join(sample, "users.json")
	good := []string{"-addr", "a:1,b:2", "-menu", menu, "-users", users}
	cfg, err := parseConfig(good, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.addrs) != 2 || cfg.listen != defaultListen || cfg.topic != defaultTopic || cfg.location == nil {
		t.Fatalf("config = %+v", cfg)
	}
	for name, args := range map[string][]string{
		"no nodes":                         {"-menu", menu, "-users", users},
		"no menu":                          {"-addr", "a:1", "-users", users},
		"no users":                         {"-addr", "a:1", "-menu", menu},
		"a bad listen":                     append([]string{"-listen", "nonsense"}, good...),
		"a bad time zone":                  append([]string{"-timezone", "Mars/Olympus"}, good...),
		"a bad timeout":                    append([]string{"-command-timeout", "0s"}, good...),
		"an argument":                      append([]string{"stray"}, good...),
		"an empty topic":                   append([]string{"-topic", ""}, good...),
		"a clock-in that needs a database": append([]string{"-require-clock-in"}, good...),
	} {
		if _, err := parseConfig(args, env(nil), io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The clock-in is asked for only where the time clock is, which is in the database.
	cfg, err = parseConfig([]string{"-addr", "a:1", "-menu", menu, "-database-url", "postgres://x", "-require-clock-in"}, env(nil), io.Discard)
	if err != nil || !cfg.requireClockIn {
		t.Fatalf("require-clock-in: %v %+v", err, cfg)
	}
	cfg, err = parseConfig([]string{"-addr", "a:1", "-menu", menu, "-database-url", "postgres://x"}, env(map[string]string{envClockIn: "true"}), io.Discard)
	if err != nil || !cfg.requireClockIn {
		t.Fatalf("the variable stands in for the flag: %v %+v", err, cfg)
	}
	// A variable stands in for a flag, and a flag wins.
	cfg, err = parseConfig([]string{"-topic", "from-flag"}, env(map[string]string{
		envAddr: "x:1", envMenu: menu, envUsers: users, envTopic: "from-env", envTimezone: "Asia/Ho_Chi_Minh",
	}), io.Discard)
	if err != nil || cfg.topic != "from-flag" || cfg.addrs[0] != "x:1" || cfg.location.String() != "Asia/Ho_Chi_Minh" {
		t.Fatalf("env and flag: %v %+v", err, cfg)
	}
}

func TestABadMenuFileStopsTheServiceBeforeItTouchesTheLog(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "menu.json")
	if err := os.WriteFile(bad, []byte(`{"items":[{"id":"x","nme":"typo"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"-addr", "127.0.0.1:1", "-menu", bad, "-users", filepath.Join(sample, "users.json")}, env(nil), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "menu") {
		t.Fatalf("a menu with a misspelt key: %v", err)
	}
}

// --- on a real cluster -----------------------------------------------------------------

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	return filepath.Join(runtime.GOROOT(), "bin", name)
}

func buildDaemon(t *testing.T) string {
	t.Helper()
	name := "raftlogd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command(goTool(), "build", "-o", out, "github.com/nguyenhoangkhang-2010/raftlog/cmd/raftlogd")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building raftlogd: %v\n%s", err, b)
	}
	return out
}

// freePorts returns the first of n consecutive free ports, below the range that the operating
// system hands out to outgoing connections.
func freePorts(t *testing.T, n int) int {
	t.Helper()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for try := 0; try < 200; try++ {
		base := 20000 + rng.Intn(9000)
		var held []net.Listener
		ok := true
		for i := 0; i < n; i++ {
			h, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			held = append(held, h)
		}
		for _, h := range held {
			_ = h.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free consecutive ports")
	return 0
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runningService is the service started inside the test.
type runningService struct {
	url    string
	stop   func()
	stderr *syncBuffer
}

func startService(t *testing.T, addrs []string, topic string) *runningService {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", strings.Join(addrs, ","), "-topic", topic, "-listen", "127.0.0.1:0", "-timezone", "Asia/Ho_Chi_Minh",
			"-menu", filepath.Join(sample, "menu.json"), "-promotions", filepath.Join(sample, "promotions.json"), "-users", filepath.Join(sample, "users.json"),
			"-command-timeout", "30s",
		}, env(nil), &out, &errOut)
	}()
	re := regexp.MustCompile(`listening on (http://\S+),`)
	var url string
	for deadline := time.Now().Add(60 * time.Second); url == ""; {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			url = m[1]
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the service ended while starting: %v\n%s", err, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the service did not start in 60 s\n%s", errOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the service did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return &runningService{url: url, stop: stop, stderr: &errOut}
}

// call sends a JSON request with the sample token of the person and decodes the answer.
func (s *runningService) call(t *testing.T, method, path, token string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %q is not the expected JSON: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

type orderAnswer struct {
	ID       string `json:"id"`
	Number   int    `json:"number"`
	Status   string `json:"status"`
	Total    int64  `json:"total"`
	Customer string `json:"customer"`
}

const (
	customerTok  = "demo-token-customer-1"
	cashierTok   = "demo-token-cashier-1"
	drinksTok    = "demo-token-kitchen-drinks"
	hotTok       = "demo-token-kitchen-hot"
	runnerTok    = "demo-token-runner-1"
	storeTok     = "demo-token-storekeeper-1"
	managerTok   = "demo-token-manager-1"
	fastRaftArgs = "-tick-interval 50ms -election-ticks 20 -heartbeat-ticks 2"
)

func cmdID(prefix string, n int) string {
	return fmt.Sprintf("%s-%04d-%d", prefix, n, time.Now().UnixNano()%1e9)
}

func TestTheKitchenOnARealClusterSurvivesARestartOfTheServiceAndTheLossOfTheLeader(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts real processes")
	}
	bin := buildDaemon(t)
	base := freePorts(t, 3)
	var nodes []console.NodeSpec
	var addrs []string
	for i := 0; i < 3; i++ {
		a := fmt.Sprintf("127.0.0.1:%d", base+i)
		nodes = append(nodes, console.NodeSpec{ID: i + 1, Addr: a})
		addrs = append(addrs, a)
	}
	sup, err := console.NewSupervisor(console.SupervisorConfig{Binary: bin, DataDir: filepath.Join(t.TempDir(), "nodes"), Nodes: nodes, ExtraArgs: strings.Fields(fastRaftArgs)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)
	if err := sup.StartAll(); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(addrs, client.WithRetryPause(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	leader, err := bench.WaitForLeader(context.Background(), cl, addrs, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	svc := startService(t, addrs, "kitchen-test")
	n := 0
	next := func(prefix string) string { n++; return cmdID(prefix, n) }

	// The storekeeper receives goods: the stock is part of the log, like everything else.
	for ing, qty := range map[string]int64{"coffee-beans": 2000, "condensed-milk": 2000, "ice": 10000, "baguette": 50, "pate": 500, "cucumber": 500, "egg": 20, "tea-leaves": 100, "peach-syrup": 1000} {
		if code := svc.call(t, "POST", "/api/stock/receive", storeTok, map[string]any{"command_id": next("recv"), "ingredient": ing, "qty": qty}, nil); code != 200 {
			t.Fatalf("receive %s: %d", ing, code)
		}
	}

	order := func(s *runningService) orderAnswer {
		var placed struct{ Order orderAnswer }
		code := s.call(t, "POST", "/api/orders", customerTok, map[string]any{"command_id": next("place"), "items": []map[string]any{
			{"item": "ca-phe-sua", "quantity": 2, "options": map[string][]string{"size": {"m"}}},
			{"item": "banh-mi", "quantity": 1},
		}}, &placed)
		if code != 201 {
			t.Fatalf("placing an order: %d\n%s", code, s.stderr.String())
		}
		return placed.Order
	}
	pay := func(s *runningService, id string) {
		var v orderAnswer
		if code := s.call(t, "POST", "/api/orders/"+id+"/pay", cashierTok, map[string]any{"command_id": next("pay"), "method": "card", "reference": "slip"}, &v); code != 200 || v.Status != "paid" {
			t.Fatalf("paying %s: %d %+v\n%s", id, code, v, s.stderr.String())
		}
	}

	first := order(svc)
	if first.Number != 1 || first.Total != 83000 || first.Customer != "khach-1" {
		t.Fatalf("first order = %+v: two 29,000 coffees and a 25,000 bánh mì", first)
	}
	pay(svc, first.ID)
	for _, st := range []struct{ tok, station string }{{drinksTok, "drinks"}, {hotTok, "hot"}} {
		if code := svc.call(t, "POST", "/api/orders/"+first.ID+"/tickets/"+st.station+"/start", st.tok, map[string]any{"command_id": next("start")}, nil); code != 200 {
			t.Fatalf("start %s: %d", st.station, code)
		}
	}
	second := order(svc)
	if second.Number != 2 {
		t.Fatalf("second order number = %d", second.Number)
	}

	// The service is stopped, and a new one starts. It has nothing but the log.
	svc.stop()
	svc2 := startService(t, addrs, "kitchen-test")
	var list struct{ Orders []orderAnswer }
	if code := svc2.call(t, "GET", "/api/orders", managerTok, nil, &list); code != 200 || len(list.Orders) != 2 || list.Orders[1].Status != "preparing" || list.Orders[0].Status != "placed" {
		t.Fatalf("after a restart of the service: %d %+v", code, list)
	}
	third := order(svc2)
	if third.Number != 3 {
		t.Fatalf("the first order after a restart got number %d, want 3", third.Number)
	}
	var stockAfter struct {
		Levels []struct {
			Ingredient string
			Qty        int64
		}
	}
	svc2.call(t, "GET", "/api/stock", managerTok, nil, &stockAfter)
	for _, l := range stockAfter.Levels {
		if l.Ingredient == "baguette" && l.Qty != 49 {
			t.Fatalf("baguettes = %d after one paid order of one bánh mì, want 49: the payment was lost or counted twice by the rebuild", l.Qty)
		}
	}

	// The leader of the raftlog cluster is killed. Orders go on, after the election.
	if err := sup.Kill(leader); err != nil {
		t.Fatal(err)
	}
	pay(svc2, second.ID) // waits out the election inside the client's retries
	fourth := order(svc2)
	pay(svc2, fourth.ID)
	if fourth.Number != 4 {
		t.Fatalf("number after the failover = %d, want 4", fourth.Number)
	}
	if err := sup.Start(leader); err != nil {
		t.Fatal(err)
	}

	// A third service reads everything: the orders and the stock are the same.
	svc2.stop()
	svc3 := startService(t, addrs, "kitchen-test")
	if code := svc3.call(t, "GET", "/api/orders", managerTok, nil, &list); code != 200 || len(list.Orders) != 4 {
		t.Fatalf("after the failover and another restart: %d %+v", code, list)
	}
	status := map[int]string{}
	for _, o := range list.Orders {
		status[o.Number] = o.Status
	}
	if status[1] != "preparing" || status[2] != "paid" || status[3] != "placed" || status[4] != "paid" {
		t.Fatalf("statuses by order number = %v", status)
	}
	var health struct {
		Skipped int `json:"skipped_records"`
	}
	if code := svc3.call(t, "GET", "/healthz", managerTok, nil, &health); code != 200 || health.Skipped != 0 {
		t.Fatalf("healthz: %d %+v", code, health)
	}
	_ = runnerTok
}
