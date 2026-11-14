package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// fakeCluster answers NodeStatus from a table that the test changes.
type fakeCluster struct {
	mu    sync.Mutex
	state map[string]client.NodeStatus // by address; absent means the node is down
}

func (f *fakeCluster) set(addr string, st client.NodeStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[string]client.NodeStatus{}
	}
	f.state[addr] = st
}

func (f *fakeCluster) down(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state, addr)
}

func (f *fakeCluster) NodeStatus(_ context.Context, addr string) (client.NodeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.state[addr]
	if !ok {
		return client.NodeStatus{}, errors.New("connection refused")
	}
	return st, nil
}

var specs = []NodeSpec{{ID: 1, Addr: "n1"}, {ID: 2, Addr: "n2"}, {ID: 3, Addr: "n3"}}

func status(id uint64, role string, term, leader, commit, last uint64) client.NodeStatus {
	return client.NodeStatus{RaftEnabled: true, NodeID: id, Role: role, Term: term, LeaderID: leader, CommitIndex: commit, LastIndex: last}
}

func healthy(f *fakeCluster) {
	f.set("n1", status(1, "leader", 3, 1, 100, 100))
	f.set("n2", status(2, "follower", 3, 1, 100, 100))
	f.set("n3", status(3, "follower", 3, 1, 98, 99))
}

func newObs(f *fakeCluster) *Observer {
	return NewObserver(f, specs, nil, time.Second)
}

func texts(evs []Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Text)
	}
	return strings.Join(out, " | ")
}

func TestAHealthyClusterHasALeaderAndAQuorum(t *testing.T) {
	f := &fakeCluster{}
	healthy(f)
	o := newObs(f)
	o.Poll(context.Background())

	nodes, sum, events := o.Snapshot()
	if len(nodes) != 3 || sum.Up != 3 || sum.Total != 3 || sum.Quorum != 2 || sum.LeaderID != 1 || sum.Term != 3 || !sum.CanCommit {
		t.Fatalf("summary = %+v, nodes %+v", sum, nodes)
	}
	if len(events) != 0 {
		t.Fatalf("the first poll is not a change, but the timeline says %s", texts(events))
	}
	// The leader is not behind itself; a follower is behind by the difference of the logs.
	if nodes[0].Behind != nil {
		t.Fatalf("the leader is behind by %d", *nodes[0].Behind)
	}
	if nodes[1].Behind == nil || *nodes[1].Behind != 0 || nodes[2].Behind == nil || *nodes[2].Behind != 1 {
		t.Fatalf("behind = %v and %v, want 0 and 1", nodes[1].Behind, nodes[2].Behind)
	}
	if !nodes[0].Running || !nodes[0].Reachable || nodes[0].Role != "leader" || nodes[0].Commit != 100 {
		t.Fatalf("node 1 = %+v", nodes[0])
	}
}

func TestTheTimelineTellsOfFailoverAndReturn(t *testing.T) {
	f := &fakeCluster{}
	healthy(f)
	o := newObs(f)
	o.Poll(context.Background())

	// The leader dies: it stops answering, nobody leads for a moment, then another node does.
	f.down("n1")
	o.Poll(context.Background())
	_, sum, events := o.Snapshot()
	if sum.LeaderID != 1 && sum.LeaderID != 0 {
		t.Fatalf("leader = %d", sum.LeaderID)
	}
	if sum.CanCommit || sum.Up != 2 {
		t.Fatalf("with the leader down and nobody else claiming it: %+v", sum)
	}
	if got := texts(events); got != "node 1 stopped answering | the cluster has no leader" {
		t.Fatalf("timeline = %q", got)
	}

	f.set("n2", status(2, "leader", 4, 2, 101, 101))
	f.set("n3", status(3, "follower", 4, 2, 101, 101))
	o.Poll(context.Background())
	_, sum, events = o.Snapshot()
	if sum.LeaderID != 2 || sum.Term != 4 || !sum.CanCommit || sum.Up != 2 {
		t.Fatalf("after the election: %+v", sum)
	}
	if last := events[len(events)-1].Text; last != "node 2 became leader in term 4" {
		t.Fatalf("last event = %q", last)
	}

	// Node 1 comes back as a follower of the new leader.
	f.set("n1", status(1, "follower", 3, 0, 100, 100))
	o.Poll(context.Background())
	nodes, sum, events := o.Snapshot()
	if got := events[len(events)-1].Text; got != "node 1 is answering again (follower, term 3)" {
		t.Fatalf("last event = %q", got)
	}
	if sum.Up != 3 || sum.LeaderID != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	if nodes[0].Behind == nil || *nodes[0].Behind != 1 {
		t.Fatalf("node 1 is 1 entry behind the leader, got %v", nodes[0].Behind)
	}
	// A steady cluster adds nothing to the timeline.
	n := len(events)
	o.Poll(context.Background())
	o.Poll(context.Background())
	if _, _, again := o.Snapshot(); len(again) != n {
		t.Fatalf("polling a steady cluster added events: %s", texts(again[n:]))
	}
}

func TestADeposedLeaderThatDoesNotKnowItYetIsNotTheLeader(t *testing.T) {
	f := &fakeCluster{}
	f.set("n1", status(1, "leader", 3, 1, 90, 90)) // cut off, still says leader in term 3
	f.set("n2", status(2, "leader", 4, 2, 100, 100))
	f.set("n3", status(3, "follower", 4, 2, 95, 95))
	o := newObs(f)
	o.Poll(context.Background())
	nodes, sum, _ := o.Snapshot()
	if sum.LeaderID != 2 || sum.Term != 4 {
		t.Fatalf("leader = %d in term %d, want node 2 in term 4: the higher term wins", sum.LeaderID, sum.Term)
	}
	// "Behind" is measured against the real leader, not the one that has not noticed.
	if nodes[2].Behind == nil || *nodes[2].Behind != 5 || nodes[0].Behind == nil || *nodes[0].Behind != 10 {
		t.Fatalf("behind = %v (node 3) and %v (node 1), want 5 and 10 against node 2", nodes[2].Behind, nodes[0].Behind)
	}
	// And the timeline names the real leader when it changes.
	f.down("n2")
	o.Poll(context.Background())
	f.set("n2", status(2, "leader", 4, 2, 100, 100))
	o.Poll(context.Background())
	if got := texts(mustEvents(o)); !strings.Contains(got, "node 2 became leader in term 4") {
		t.Fatalf("timeline = %q", got)
	}
}

func TestAMajorityIsNeededToCommit(t *testing.T) {
	f := &fakeCluster{}
	healthy(f)
	o := newObs(f)
	o.Poll(context.Background())
	f.down("n2")
	f.down("n3")
	o.Poll(context.Background())
	_, sum, _ := o.Snapshot()
	if sum.Up != 1 || sum.CanCommit {
		t.Fatalf("one node of three cannot commit, but %+v", sum)
	}
}

func TestANodeThatIsNotPartOfARaftClusterIsNotShownAsUp(t *testing.T) {
	f := &fakeCluster{}
	f.set("n1", client.NodeStatus{RaftEnabled: false})
	o := NewObserver(f, specs[:1], nil, time.Second)
	o.Poll(context.Background())
	nodes, sum, _ := o.Snapshot()
	if nodes[0].Reachable || sum.Up != 0 {
		t.Fatalf("a standalone node counted as a cluster member: %+v", nodes[0])
	}
}

func TestRunningComesFromTheSupervisorWhenThereIsOne(t *testing.T) {
	f := &fakeCluster{}
	healthy(f)
	alive := map[int]bool{1: true, 2: true, 3: false}
	o := NewObserver(f, specs, func(id int) (bool, bool) { return true, alive[id] }, time.Second)
	o.Poll(context.Background())
	nodes, _, _ := o.Snapshot()
	// Node 3 still answers (its process is just being killed), but the supervisor says it is not running.
	if !nodes[0].Running || nodes[2].Running || !nodes[2].Reachable {
		t.Fatalf("running flags: %+v", nodes)
	}
}

func TestAnUnreachableNodeIsLabelledUpOnlyWhenItAnswersAfterHavingBeenUp(t *testing.T) {
	f := &fakeCluster{}
	f.set("n1", status(1, "leader", 1, 1, 1, 1))
	o := newObs(f)
	o.Poll(context.Background()) // n2 and n3 are not up yet
	f.set("n2", status(2, "follower", 1, 1, 1, 1))
	o.Poll(context.Background())
	if got := texts(mustEvents(o)); got != "node 2 is up (follower, term 1)" {
		t.Fatalf("timeline = %q: a node that was never up is starting, not coming back", got)
	}
}

func mustEvents(o *Observer) []Event {
	_, _, ev := o.Snapshot()
	return ev
}

func TestTheTimelineKeepsTheMostRecentEvents(t *testing.T) {
	o := newObs(&fakeCluster{})
	for i := 0; i < maxEvents+25; i++ {
		o.Record("action", "event "+string(rune('A'+i%26)))
	}
	ev := mustEvents(o)
	if len(ev) != maxEvents {
		t.Fatalf("%d events, want the last %d", len(ev), maxEvents)
	}
	if !strings.HasPrefix(ev[len(ev)-1].Text, "event ") || ev[len(ev)-1].Kind != "action" {
		t.Fatalf("last event = %+v", ev[len(ev)-1])
	}
}

func TestAStatusThatTimesOutIsTakenForADownNode(t *testing.T) {
	slow := proberFunc(func(ctx context.Context, _ string) (client.NodeStatus, error) {
		<-ctx.Done()
		return client.NodeStatus{}, ctx.Err()
	})
	o := NewObserver(slow, specs, nil, 30*time.Millisecond)
	start := time.Now()
	o.Poll(context.Background())
	// The three nodes are asked at the same time, so the poll takes one timeout, not three.
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the poll took %v for three silent nodes", time.Since(start))
	}
	if _, sum, _ := o.Snapshot(); sum.Up != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

type proberFunc func(ctx context.Context, addr string) (client.NodeStatus, error)

func (f proberFunc) NodeStatus(ctx context.Context, addr string) (client.NodeStatus, error) {
	return f(ctx, addr)
}

func TestRunPollsUntilItIsStopped(t *testing.T) {
	f := &fakeCluster{}
	healthy(f)
	o := newObs(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, sum, _ := o.Snapshot(); sum.Up == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never polled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.down("n3")
	for {
		if _, sum, _ := o.Snapshot(); sum.Up == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not notice that a node went down")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
