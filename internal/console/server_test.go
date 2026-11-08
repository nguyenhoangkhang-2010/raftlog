package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

type fakeController struct {
	mu      sync.Mutex
	started []int
	killed  []int
	err     error
}

func (c *fakeController) Start(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, id)
	return c.err
}

func (c *fakeController) Kill(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.killed = append(c.killed, id)
	return c.err
}

func (c *fakeController) calls() (started, killed []int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.started...), append([]int(nil), c.killed...)
}

type fakeLister struct {
	mu     sync.Mutex
	topics []client.TopicInfo
	groups []client.GroupInfo
	err    error
}

func (l *fakeLister) set(topics []client.TopicInfo, groups []client.GroupInfo, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.topics, l.groups, l.err = topics, groups, err
}

func (l *fakeLister) Topics(context.Context) ([]client.TopicInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.topics, l.err
}

func (l *fakeLister) Groups(context.Context) ([]client.GroupInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.groups, l.err
}

type rig struct {
	ts      *httptest.Server
	ctl     *fakeController
	cluster *fakeCluster
	obs     *Observer
	load    *Load
	log     *fakeLog
	lister  *fakeLister
}

func newRig(t *testing.T, withControl bool) *rig {
	t.Helper()
	r := &rig{cluster: &fakeCluster{}, log: newFakeLog(2), lister: &fakeLister{}}
	healthy(r.cluster)
	r.obs = NewObserver(r.cluster, specs, nil, time.Second)
	r.obs.Poll(context.Background())
	r.load = mustLoad(t, r.log, nil)
	opts := ServerOptions{Observer: r.obs, Load: r.load, Lister: r.lister, VerifyPatience: 300 * time.Millisecond}
	if withControl {
		r.ctl = &fakeController{}
		opts.Control = r.ctl
	}
	h, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	r.ts = httptest.NewServer(h)
	t.Cleanup(r.ts.Close)
	return r
}

// do sends a request; marked adds the header the page always sends.
func (r *rig) do(t *testing.T, method, path, body string, marked bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if marked {
		req.Header.Set("X-Requested-With", "console")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (r *rig) state(t *testing.T) State {
	t.Helper()
	code, b := r.do(t, "GET", "/api/state", "", false)
	if code != 200 {
		t.Fatalf("state: %d %s", code, b)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("state: %v\n%s", err, b)
	}
	return st
}

func TestTheServerNeedsItsParts(t *testing.T) {
	r := newRig(t, false)
	good := ServerOptions{Observer: r.obs, Load: r.load, Lister: r.lister, VerifyPatience: time.Second}
	for name, mutate := range map[string]func(*ServerOptions){
		"no observer": func(o *ServerOptions) { o.Observer = nil },
		"no load":     func(o *ServerOptions) { o.Load = nil },
		"no lister":   func(o *ServerOptions) { o.Lister = nil },
		"no patience": func(o *ServerOptions) { o.VerifyPatience = 0 },
	} {
		o := good
		mutate(&o)
		if _, err := NewServer(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestThePageAndTheHealthEndpointAreServed(t *testing.T) {
	r := newRig(t, true)
	code, body := r.do(t, "GET", "/", "", false)
	if code != 200 || !strings.Contains(string(body), "/api/state") || !strings.Contains(string(body), "X-Requested-With") {
		t.Fatalf("GET /: %d, the page does not use the API as expected", code)
	}
	if code, _ := r.do(t, "GET", "/healthz", "", false); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	if code, _ := r.do(t, "GET", "/nowhere", "", false); code != 404 {
		t.Fatalf("GET /nowhere: %d", code)
	}
}

func TestTheStateHasEverythingThePageShows(t *testing.T) {
	r := newRig(t, true)
	r.lister.set(
		[]client.TopicInfo{{Name: "orders", Ends: []uint64{10, 5}}, {Name: "audit", Ends: []uint64{0}}},
		[]client.GroupInfo{{Name: "billing", Partitions: []client.GroupPartition{{Lag: 3}, {Lag: 4}}}}, nil)

	st := r.state(t)
	if !st.Control || st.Summary.LeaderID != 1 || !st.Summary.CanCommit || len(st.Nodes) != 3 || st.Now.IsZero() {
		t.Fatalf("state = %+v", st)
	}
	if len(st.Topics) != 2 || st.Topics[0].Name != "orders" || st.Topics[0].Partitions != 2 || st.Topics[0].Messages != 15 || st.Topics[1].Messages != 0 {
		t.Fatalf("topics = %+v", st.Topics)
	}
	if len(st.Groups) != 1 || st.Groups[0].Name != "billing" || st.Groups[0].Lag != 7 {
		t.Fatalf("groups = %+v", st.Groups)
	}
	if st.TopicsError != "" || st.Load.Running {
		t.Fatalf("topics error %q, load %+v", st.TopicsError, st.Load)
	}
}

func TestWhenTheTopicsCannotBeReadTheLastAnswerStaysAndTheErrorIsShown(t *testing.T) {
	r := newRig(t, true)
	r.lister.set([]client.TopicInfo{{Name: "orders", Ends: []uint64{4}}}, nil, nil)
	if st := r.state(t); len(st.Topics) != 1 {
		t.Fatalf("topics = %+v", st.Topics)
	}
	time.Sleep(1100 * time.Millisecond) // the server asks again at most once a second
	r.lister.set(nil, nil, errors.New("no node answered"))
	st := r.state(t)
	if len(st.Topics) != 1 || st.Topics[0].Name != "orders" || !strings.Contains(st.TopicsError, "no node answered") {
		t.Fatalf("topics %+v, error %q", st.Topics, st.TopicsError)
	}
}

func TestARequestThatChangesSomethingMustCarryTheMarker(t *testing.T) {
	r := newRig(t, true)
	for _, path := range []string{"/api/nodes/2/kill", "/api/nodes/2/start", "/api/load/stop", "/api/load/reset", "/api/verify"} {
		if code, _ := r.do(t, "POST", path, "", false); code != 403 {
			t.Errorf("POST %s without the marker: %d, want 403", path, code)
		}
	}
	if code, _ := r.do(t, "POST", "/api/load", `{"rate":10}`, false); code != 403 {
		t.Errorf("POST /api/load without the marker: %d, want 403", code)
	}
	started, killed := r.ctl.calls()
	if len(started)+len(killed) != 0 || r.load.Stats().Running {
		t.Fatal("a request without the marker had an effect")
	}
	// Reading never needs it.
	if code, _ := r.do(t, "GET", "/api/state", "", false); code != 200 {
		t.Fatal("state needs no marker")
	}
}

func TestKillingAndStartingANode(t *testing.T) {
	r := newRig(t, true)
	if code, b := r.do(t, "POST", "/api/nodes/2/kill", "", true); code != 200 {
		t.Fatalf("kill: %d %s", code, b)
	}
	if code, b := r.do(t, "POST", "/api/nodes/2/start", "", true); code != 200 {
		t.Fatalf("start: %d %s", code, b)
	}
	started, killed := r.ctl.calls()
	if len(killed) != 1 || killed[0] != 2 || len(started) != 1 || started[0] != 2 {
		t.Fatalf("started %v, killed %v", started, killed)
	}
	if got := texts(mustEvents(r.obs)); !strings.Contains(got, "you killed node 2") || !strings.Contains(got, "you started node 2") {
		t.Fatalf("timeline = %q", got)
	}

	if code, _ := r.do(t, "POST", "/api/nodes/9/kill", "", true); code != 404 {
		t.Errorf("an unknown node: %d, want 404", code)
	}
	for _, id := range []string{"x", "0", "-1"} {
		if code, _ := r.do(t, "POST", "/api/nodes/"+id+"/kill", "", true); code != 400 {
			t.Errorf("node %q: %d, want 400", id, code)
		}
	}
	r.ctl.err = errors.New("cannot kill")
	if code, b := r.do(t, "POST", "/api/nodes/1/kill", "", true); code != 500 || !strings.Contains(string(b), "cannot kill") {
		t.Errorf("a failing controller: %d %s", code, b)
	}
}

func TestAConsoleThatOnlyWatchesCannotStopNodes(t *testing.T) {
	r := newRig(t, false)
	if st := r.state(t); st.Control {
		t.Fatal("the state says the console is in control")
	}
	for _, path := range []string{"/api/nodes/1/kill", "/api/nodes/1/start"} {
		if code, b := r.do(t, "POST", path, "", true); code != 403 || !strings.Contains(string(b), "only watches") {
			t.Errorf("POST %s: %d %s, want 403", path, code, b)
		}
	}
}

func TestStartingAndStoppingTheLoad(t *testing.T) {
	r := newRig(t, true)
	for _, body := range []string{`{}`, `{"rate":0}`, `{"rate":-5}`, `{"rate":999999}`, `nope`, `{"rate":10,"extra":1}`} {
		if code, _ := r.do(t, "POST", "/api/load", body, true); code != 400 {
			t.Errorf("body %s: %d, want 400", body, code)
		}
	}
	if code, b := r.do(t, "POST", "/api/load", `{"rate":100}`, true); code != 200 {
		t.Fatalf("start: %d %s", code, b)
	}
	if st := r.state(t); !st.Load.Running || st.Load.Rate != 100 {
		t.Fatalf("load = %+v", st.Load)
	}
	if code, _ := r.do(t, "POST", "/api/load", `{"rate":100}`, true); code != 400 {
		t.Errorf("starting twice: %d, want 400", code)
	}
	if code, b := r.do(t, "POST", "/api/load/reset", "", true); code != 409 {
		t.Errorf("reset while running: %d %s, want 409", code, b)
	}
	waitFor(t, "a write", func() bool { return r.load.Stats().Acked > 0 })
	if code, _ := r.do(t, "POST", "/api/load/stop", "", true); code != 200 {
		t.Fatal("stop failed")
	}
	if st := r.state(t); st.Load.Running {
		t.Fatalf("load = %+v after stop", st.Load)
	}
	waitFor(t, "writes in flight", func() bool { s := r.load.Stats(); return s.Sent == s.Acked+s.Failed })
	if code, _ := r.do(t, "POST", "/api/load/reset", "", true); code != 200 {
		t.Fatal("reset failed")
	}
	if st := r.state(t); st.Load.Acked != 0 {
		t.Fatalf("after reset: %+v", st.Load)
	}
}

func TestVerifyOverHTTP(t *testing.T) {
	r := newRig(t, true)
	if err := r.load.Start(200); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "writes", func() bool { return r.load.Stats().Acked >= 20 })
	r.load.Stop()
	waitFor(t, "writes in flight", func() bool { s := r.load.Stats(); return s.Sent == s.Acked+s.Failed })

	code, b := r.do(t, "POST", "/api/verify", "", true)
	var v Verification
	if code != 200 || json.Unmarshal(b, &v) != nil || v.Missing != 0 || v.Acked < 20 {
		t.Fatalf("verify: %d %s", code, b)
	}
	if got := texts(mustEvents(r.obs)); !strings.Contains(got, "0 missing") {
		t.Fatalf("timeline = %q", got)
	}

	// A log that loses acknowledged writes is reported, not hidden.
	r.log.mu.Lock()
	r.log.parts[0] = r.log.parts[0][:len(r.log.parts[0])/2]
	r.log.mu.Unlock()
	code, b = r.do(t, "POST", "/api/verify", "", true)
	if code != 200 || json.Unmarshal(b, &v) != nil || v.Missing == 0 {
		t.Fatalf("verify after a loss: %d %s", code, b)
	}
}

func TestVerifyThatCannotReadTheLogIsAnError(t *testing.T) {
	r := newRig(t, true)
	r2 := mustLoad(t, &errFetchLog{fakeLog: r.log, calls: new(atomic.Int32)}, nil)
	h, err := NewServer(ServerOptions{Observer: r.obs, Load: r2, Lister: r.lister, VerifyPatience: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/verify", nil)
	req.Header.Set("X-Requested-With", "console")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}
