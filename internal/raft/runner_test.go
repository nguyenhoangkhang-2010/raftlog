package raft

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// These tests run real runners over real gRPC on the loopback interface, with
// a short tick so that elections take a fraction of a second.
const (
	realTick     = 10 * time.Millisecond
	waitForState = 15 * time.Second
)

type realNode struct {
	id     NodeID
	runner *Runner
	gs     *grpc.Server
	tr     *GRPCTransport
}

func (n *realNode) kill() {
	n.runner.Stop()
	n.gs.Stop()
	_ = n.tr.Close()
}

// startRealCluster starts size nodes that talk to each other over gRPC.
func startRealCluster(t *testing.T, size int) []*realNode {
	t.Helper()

	// Listen first, so every node knows every address before it starts.
	lis := make([]net.Listener, size)
	addrs := make(map[NodeID]string)
	for i := range lis {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lis[i] = l
		addrs[NodeID(i+1)] = l.Addr().String()
	}

	var nodes []*realNode
	for i := range lis {
		id := NodeID(i + 1)
		store, err := NewFileStorage(filepath.Join(t.TempDir(), "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		tr := NewGRPCTransport(addrs)
		r, err := NewRunner(Config{
			ID:             id,
			Cluster:        clusterIDs(size),
			ElectionTicks:  testElectionTicks,
			HeartbeatTicks: testHeartbeatTicks,
			Storage:        store,
		}, tr, realTick)
		if err != nil {
			t.Fatal(err)
		}
		gs := grpc.NewServer()
		RegisterGRPC(gs, r)
		go func(l net.Listener) { _ = gs.Serve(l) }(lis[i])
		r.Start()
		nodes = append(nodes, &realNode{id: id, runner: r, gs: gs, tr: tr})
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.kill()
		}
	})
	return nodes
}

// waitForLeader waits until exactly one of the live nodes is leader and all
// the other live nodes follow it in the same term.
func waitForLeader(t *testing.T, nodes []*realNode, dead map[NodeID]bool) Status {
	t.Helper()
	deadline := time.Now().Add(waitForState)
	for time.Now().Before(deadline) {
		var leader Status
		leaders := 0
		for _, n := range nodes {
			if dead[n.id] {
				continue
			}
			if st := n.runner.Status(); st.Role == Leader {
				leader = st
				leaders++
			}
		}
		if leaders == 1 {
			agreed := true
			for _, n := range nodes {
				if dead[n.id] || n.id == leader.ID {
					continue
				}
				st := n.runner.Status()
				if st.Role != Follower || st.Leader != leader.ID || st.Term != leader.Term {
					agreed = false
				}
			}
			if agreed {
				return leader
			}
		}
		time.Sleep(realTick)
	}
	t.Fatal("no stable leader before the deadline")
	return Status{}
}

func TestRealClusterElectsALeaderAndReplacesItWhenItDies(t *testing.T) {
	nodes := startRealCluster(t, 3)

	first := waitForLeader(t, nodes, nil)
	t.Logf("first leader: node %d in term %d", first.ID, first.Term)

	// Kill the leader: stop its protocol loop and its network endpoint.
	var victim *realNode
	for _, n := range nodes {
		if n.id == first.ID {
			victim = n
		}
	}
	victim.kill()

	second := waitForLeader(t, nodes, map[NodeID]bool{first.ID: true})
	t.Logf("second leader: node %d in term %d", second.ID, second.Term)
	if second.ID == first.ID {
		t.Fatal("the dead node is still reported as leader")
	}
	if second.Term <= first.Term {
		t.Fatalf("new leader's term %d is not greater than %d", second.Term, first.Term)
	}
}

func TestRunnerStartAndStopAreIdempotent(t *testing.T) {
	store, _ := NewFileStorage(filepath.Join(t.TempDir(), "state.json"))
	r, err := NewRunner(Config{
		ID: 1, Cluster: []NodeID{1}, ElectionTicks: testElectionTicks, HeartbeatTicks: testHeartbeatTicks, Storage: store,
	}, nopTransport{}, realTick)
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	r.Start() // second call is a no-op

	deadline := time.Now().Add(waitForState)
	for r.Status().Role != Leader && time.Now().Before(deadline) {
		time.Sleep(realTick)
	}
	if r.Status().Role != Leader {
		t.Fatal("a one-node cluster did not elect itself")
	}
	r.Stop()
	r.Stop()
}

func TestNewRunnerValidates(t *testing.T) {
	store := &memStorage{}
	cfg := testConfig(1, clusterIDs(3), store, 1)
	if _, err := NewRunner(cfg, nil, realTick); err == nil {
		t.Error("nil transport accepted")
	}
	if _, err := NewRunner(cfg, nopTransport{}, 0); err == nil {
		t.Error("zero tick interval accepted")
	}
	cfg.ID = 0
	if _, err := NewRunner(cfg, nopTransport{}, realTick); err == nil {
		t.Error("invalid node config accepted")
	}
}

func TestSlowPeerDoesNotBlockDispatch(t *testing.T) {
	// A transport that never returns until released: dispatch must still return
	// immediately and drop old messages instead of growing without bound.
	release := make(chan struct{})
	tr := &blockingTransport{release: release}
	r, err := NewRunner(testConfig(1, clusterIDs(3), &memStorage{}, 1), tr, realTick)
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	defer func() {
		close(release)
		r.Stop()
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10*peerQueueSize; i++ {
			r.dispatch([]Message{{Type: MsgHeartbeat, From: 1, To: 2, Term: 1}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch blocked on a slow peer")
	}
	if got := len(r.queues[2]); got > peerQueueSize {
		t.Fatalf("queue grew to %d, above the limit %d", got, peerQueueSize)
	}
}

func TestMessageConversionRoundTrip(t *testing.T) {
	in := Message{Type: MsgRequestVote, From: 3, To: 5, Term: 9, LastLogIndex: 4, LastLogTerm: 2, Granted: true, Success: true}
	p, err := messageToPB(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := messageFromPB(p)
	if err != nil || out != in {
		t.Fatalf("round trip: %+v, err %v; want %+v", out, err, in)
	}
	if _, err := messageToPB(Message{Type: 99}); err == nil {
		t.Error("unknown type was converted")
	}
	p.Type = 0
	if _, err := messageFromPB(p); err == nil {
		t.Error("unspecified type was accepted")
	}
}

type nopTransport struct{}

func (nopTransport) Send(context.Context, Message) error { return nil }

type blockingTransport struct {
	release chan struct{}
}

func (b *blockingTransport) Send(ctx context.Context, _ Message) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}
