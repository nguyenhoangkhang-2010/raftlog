package bench

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// fakeCluster is three nodes with one log. Killing the leader leaves the cluster without
// one for electionDelay, during which writes fail, as they do in a real election.
type fakeCluster struct {
	electionDelay time.Duration
	loseOnKill    bool // drop the newest stored record when the leader dies: a broken cluster
	keepNodeDown  int  // a node that Start does not bring back

	mu      sync.Mutex
	up      map[int]bool
	leader  int
	term    uint64
	records []client.Record
}

func newFakeCluster(delay time.Duration) *fakeCluster {
	return &fakeCluster{electionDelay: delay, up: map[int]bool{1: true, 2: true, 3: true}, leader: 1, term: 1}
}

func (f *fakeCluster) Kill(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up[id] = false
	if id != f.leader {
		return nil
	}
	f.leader = 0
	if f.loseOnKill && len(f.records) > 0 {
		f.records = f.records[:len(f.records)-1]
	}
	time.AfterFunc(f.electionDelay, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for next := 1; next <= 3; next++ {
			if f.up[next] {
				f.leader, f.term = next, f.term+1
				return
			}
		}
	})
	return nil
}

func (f *fakeCluster) Start(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != f.keepNodeDown {
		f.up[id] = true
	}
	return nil
}

func (f *fakeCluster) NodeStatus(_ context.Context, addr string) (client.NodeStatus, error) {
	id := int(addr[len(addr)-1] - '0')
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up[id] {
		return client.NodeStatus{}, errors.New("connection refused")
	}
	st := client.NodeStatus{RaftEnabled: true, NodeID: uint64(id), Role: "follower", Term: f.term, LeaderID: uint64(f.leader)}
	if id == f.leader {
		st.Role = "leader"
	}
	return st, nil
}

func (f *fakeCluster) Produce(_ context.Context, _ string, msgs []client.Message, _ ...client.ProduceOption) (client.ProduceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leader == 0 {
		return client.ProduceResult{}, errors.New("no leader")
	}
	base := uint64(len(f.records))
	for _, m := range msgs {
		f.records = append(f.records, client.Record{Offset: uint64(len(f.records)), Value: m.Value})
	}
	return client.ProduceResult{Offset: base}, nil
}

func (f *fakeCluster) Fetch(_ context.Context, _ string, _ int, offset uint64, limit int, _ ...client.FetchOption) ([]client.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if offset >= uint64(len(f.records)) {
		return nil, nil
	}
	end := offset + uint64(limit)
	if end > uint64(len(f.records)) {
		end = uint64(len(f.records))
	}
	return append([]client.Record(nil), f.records[offset:end]...), nil
}

func failoverCfg() FailoverConfig {
	return FailoverConfig{
		Topic: "t", Addrs: []string{"n1", "n2", "n3"}, Rounds: 2,
		Steady: 60 * time.Millisecond, SettleTimeout: 3 * time.Second,
		RequestTimeout: time.Second, PollEvery: 5 * time.Millisecond,
	}
}

func TestRunFailoverMeasuresTheTimeTheClusterHasNoLeader(t *testing.T) {
	const delay = 200 * time.Millisecond
	f := newFakeCluster(delay)
	res, err := RunFailover(context.Background(), f, f, nil, failoverCfg())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rounds) != 2 {
		t.Fatalf("%d rounds, want 2", len(res.Rounds))
	}
	for i, r := range res.Rounds {
		if r.NewLeader == 0 || r.NewLeader == r.Killed {
			t.Errorf("round %d: killed %d, new leader %d", i+1, r.Killed, r.NewLeader)
		}
		// The election takes 200 ms from the start of Kill; the clock starts when Kill
		// returns, so a little less may be measured, never much more than polling adds.
		if r.LeaderVisible < delay-20*time.Millisecond || r.LeaderVisible > delay+400*time.Millisecond {
			t.Errorf("round %d: leader visible after %v, want about %v", i+1, r.LeaderVisible, delay)
		}
		if r.FirstWrite < delay-20*time.Millisecond || r.FirstWrite > delay+400*time.Millisecond {
			t.Errorf("round %d: first write after %v, want about %v", i+1, r.FirstWrite, delay)
		}
	}
	if res.Missing != 0 || res.Acked == 0 {
		t.Fatalf("acknowledged %d, missing %d", res.Acked, res.Missing)
	}
	if res.LeaderVisible.Count != 2 || res.FirstWrite.Count != 2 {
		t.Fatalf("summaries %+v %+v", res.LeaderVisible, res.FirstWrite)
	}
}

func TestRunFailoverReportsAnAcknowledgedWriteThatWasLost(t *testing.T) {
	f := newFakeCluster(50 * time.Millisecond)
	f.loseOnKill = true
	cfg := failoverCfg()
	cfg.SettleTimeout = 700 * time.Millisecond
	res, err := RunFailover(context.Background(), f, f, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Missing == 0 {
		t.Fatal("a cluster that lost an acknowledged write was reported as sound")
	}
}

func TestRunFailoverFailsWhenTheKilledNodeNeverComesBack(t *testing.T) {
	f := newFakeCluster(20 * time.Millisecond)
	f.keepNodeDown = 1
	cfg := failoverCfg()
	cfg.SettleTimeout = 400 * time.Millisecond
	_, err := RunFailover(context.Background(), f, f, nil, cfg)
	if err == nil || !strings.Contains(err.Error(), "did not settle") {
		t.Fatalf("error %v, want one saying the cluster did not settle", err)
	}
}

func TestFailoverConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*FailoverConfig){
		"two nodes":  func(c *FailoverConfig) { c.Addrs = c.Addrs[:2] },
		"no rounds":  func(c *FailoverConfig) { c.Rounds = 0 },
		"no steady":  func(c *FailoverConfig) { c.Steady = 0 },
		"no timeout": func(c *FailoverConfig) { c.RequestTimeout = 0 },
	} {
		cfg := failoverCfg()
		mutate(&cfg)
		f := newFakeCluster(time.Millisecond)
		if _, err := RunFailover(context.Background(), f, f, nil, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSettledLeaderNeedsOneLeaderAndFollowersInTheSameTerm(t *testing.T) {
	ok := map[int]client.NodeStatus{
		1: {Role: "leader", Term: 3, LeaderID: 1},
		2: {Role: "follower", Term: 3, LeaderID: 1},
		3: {Role: "follower", Term: 3, LeaderID: 1},
	}
	if l, settled := settledLeader(ok, 3, nil); !settled || l != 1 {
		t.Fatalf("settled cluster: leader %d settled %v", l, settled)
	}
	behind := map[int]client.NodeStatus{
		1: {Role: "leader", Term: 3, LeaderID: 1},
		2: {Role: "follower", Term: 2, LeaderID: 1},
		3: {Role: "follower", Term: 3, LeaderID: 1},
	}
	if _, settled := settledLeader(behind, 3, nil); settled {
		t.Fatal("a follower in an older term counted as settled")
	}
	missing := map[int]client.NodeStatus{1: ok[1], 2: ok[2]}
	if _, settled := settledLeader(missing, 3, nil); settled {
		t.Fatal("a cluster with a node that does not answer counted as settled")
	}
	if _, settled := settledLeader(missing, 3, map[int]bool{3: true}); !settled {
		t.Fatal("a node that is excepted was still required")
	}
	two := map[int]client.NodeStatus{
		1: {Role: "leader", Term: 3, LeaderID: 1},
		2: {Role: "leader", Term: 3, LeaderID: 2},
		3: ok[3],
	}
	if _, settled := settledLeader(two, 3, nil); settled {
		t.Fatal("two leaders counted as settled")
	}
}

// tailHider shows a node that has not applied its newest entries: until it has been read
// from the start hideFor times, the last five records of the log are missing.
type tailHider struct {
	inner   *fakeCluster
	hideFor int
	reads   int
}

func (h *tailHider) Fetch(ctx context.Context, topic string, partition int, offset uint64, limit int, opts ...client.FetchOption) ([]client.Record, error) {
	if offset == 0 {
		h.reads++
	}
	recs, err := h.inner.Fetch(ctx, topic, partition, offset, limit, opts...)
	if err != nil || h.reads > h.hideFor {
		return recs, err
	}
	h.inner.mu.Lock()
	visibleEnd := uint64(len(h.inner.records)) - 5
	h.inner.mu.Unlock()
	for len(recs) > 0 && recs[len(recs)-1].Offset >= visibleEnd {
		recs = recs[:len(recs)-1]
	}
	return recs, nil
}

func TestAReaderThatIsBehindIsGivenTimeToCatchUpBeforeItIsCalledLossy(t *testing.T) {
	f := newFakeCluster(30 * time.Millisecond)
	behind := &tailHider{inner: f, hideFor: 2}
	res, err := RunFailover(context.Background(), f, f, []Fetcher{behind}, failoverCfg())
	if err != nil {
		t.Fatal(err)
	}
	if res.Missing != 0 {
		t.Fatalf("a node that was only behind was reported as having lost %d writes (%v)", res.Missing, res.MissingSeqs)
	}
	if behind.reads < 3 {
		t.Fatalf("the node was read %d times: it was not given a second chance", behind.reads)
	}
}

func TestAReaderThatNeverCatchesUpIsReportedAsMissingTheWrites(t *testing.T) {
	f := newFakeCluster(30 * time.Millisecond)
	stuck := &tailHider{inner: f, hideFor: 1 << 30}
	cfg := failoverCfg()
	cfg.SettleTimeout = 700 * time.Millisecond
	res, err := RunFailover(context.Background(), f, f, []Fetcher{f, stuck}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The five hidden records are the newest five stored. The writer is stopped by
	// cancelling it, so the very newest may be a write that was stored but never recorded
	// as acknowledged: node 2 lacks four or five of the acknowledged ones.
	if len(res.Nodes) != 2 || res.Nodes[0].Missing != 0 || res.Nodes[1].Missing < 4 || res.Nodes[1].Missing > 5 || res.Missing != res.Nodes[1].Missing {
		t.Fatalf("missing %d, nodes %+v: want node 2 alone to lack its newest writes", res.Missing, res.Nodes)
	}
	if len(res.MissingSeqs) == 0 || res.MissingSeqs[len(res.MissingSeqs)-1] != res.LastAckedSeq {
		t.Fatalf("missing numbers %v, last acknowledged %d", res.MissingSeqs, res.LastAckedSeq)
	}
}
