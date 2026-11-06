package raft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"sync"
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

// kvMachine is a small state machine: the ordered list of commands applied. It
// supports snapshots, so a lagging node can be brought up to date with one.
type kvMachine struct {
	mu   sync.Mutex
	cmds []string
}

func (m *kvMachine) Apply(e Entry) {
	m.mu.Lock()
	m.cmds = append(m.cmds, string(e.Data))
	m.mu.Unlock()
}

func (m *kvMachine) Snapshot() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return json.Marshal(m.cmds)
}

func (m *kvMachine) Restore(data []byte) error {
	var cmds []string
	if err := json.Unmarshal(data, &cmds); err != nil {
		return err
	}
	m.mu.Lock()
	m.cmds = cmds
	m.mu.Unlock()
	return nil
}

func (m *kvMachine) commands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.cmds...)
}

type realNode struct {
	id     NodeID
	runner *Runner
	gs     *grpc.Server
	lis    net.Listener
	tr     *GRPCTransport
	log    *FileLog
	sm     *kvMachine
}

// start begins serving and running the node. startRealCluster does it for every
// node except those it was told to keep back.
func (n *realNode) start() {
	go func() { _ = n.gs.Serve(n.lis) }()
	n.runner.Start()
}

func (n *realNode) kill() {
	n.runner.Stop()
	n.gs.Stop()
	_ = n.lis.Close()
	_ = n.tr.Close()
	_ = n.log.Close()
}

func (n *realNode) appliedData() []string { return n.sm.commands() }

// startRealCluster starts size nodes that talk to each other over gRPC.
func startRealCluster(t *testing.T, size int) []*realNode {
	return startRealClusterWith(t, size, 0, 0, nil)
}

// startRealClusterWith is startRealCluster with snapshots: every node snapshots
// after every `every` applied entries and keeps `trailing` entries below the
// snapshot. Nodes listed in hold are created but not started.
func startRealClusterWith(t *testing.T, size int, every, trailing uint64, hold map[NodeID]bool) []*realNode {
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
		fileLog, err := OpenFileLog(filepath.Join(t.TempDir(), "log.wal"))
		if err != nil {
			t.Fatal(err)
		}
		snapshots, err := NewFileSnapshotStore(filepath.Join(t.TempDir(), "snapshot.bin"))
		if err != nil {
			t.Fatal(err)
		}
		tr := NewGRPCTransport(addrs)
		node := &realNode{id: id, tr: tr, log: fileLog, lis: lis[i], sm: &kvMachine{}}
		r, err := NewRunner(Config{
			ID:               id,
			Cluster:          clusterIDs(size),
			ElectionTicks:    testElectionTicks,
			HeartbeatTicks:   testHeartbeatTicks,
			Storage:          store,
			Log:              fileLog,
			Snapshots:        snapshots,
			SnapshotTrailing: trailing,
		}, tr, realTick, WithStateMachine(node.sm), WithSnapshotEvery(every))
		if err != nil {
			t.Fatal(err)
		}
		node.runner, node.gs = r, grpc.NewServer()
		RegisterGRPC(node.gs, r)
		nodes = append(nodes, node)
		if !hold[id] {
			node.start()
		}
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

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitForState)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(realTick)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nodeByID(nodes []*realNode, id NodeID) *realNode {
	for _, n := range nodes {
		if n.id == id {
			return n
		}
	}
	return nil
}

func proposeAll(t *testing.T, r *Runner, commands []string) {
	t.Helper()
	for _, c := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := r.Propose(ctx, []byte(c))
		cancel()
		if err != nil {
			t.Fatalf("Propose(%q): %v", c, err)
		}
	}
}

func TestRealClusterReplicatesCommandsAndSurvivesTheLeaderDying(t *testing.T) {
	nodes := startRealCluster(t, 3)
	first := waitForLeader(t, nodes, nil)
	leader := nodeByID(nodes, first.ID)

	// Commands go through the leader and are applied on every node, in order.
	batch1 := []string{"a", "b", "c", "d", "e"}
	proposeAll(t, leader.runner, batch1)
	for _, n := range nodes {
		waitFor(t, fmt.Sprintf("node %d to apply batch 1", n.id), func() bool {
			return reflect.DeepEqual(n.appliedData(), batch1)
		})
	}

	// Followers refuse and say who the leader is.
	for _, n := range nodes {
		if n.id == leader.id {
			continue
		}
		_, err := n.runner.Propose(context.Background(), []byte("nope"))
		var nl *NotLeaderError
		if !errors.As(err, &nl) || nl.Leader != leader.id {
			t.Fatalf("follower %d: err = %v, want NotLeaderError pointing at node %d", n.id, err, leader.id)
		}
	}

	// The leader dies. A new one takes over and keeps everything that was acknowledged.
	leader.kill()
	second := waitForLeader(t, nodes, map[NodeID]bool{leader.id: true})
	newLeader := nodeByID(nodes, second.ID)

	batch2 := []string{"f", "g", "h"}
	proposeAll(t, newLeader.runner, batch2)
	want := append(append([]string(nil), batch1...), batch2...)
	for _, n := range nodes {
		if n.id == leader.id {
			continue
		}
		waitFor(t, fmt.Sprintf("node %d to apply both batches", n.id), func() bool {
			return reflect.DeepEqual(n.appliedData(), want)
		})
	}
}

func TestRealClusterBringsALateNodeUpToDateWithASnapshot(t *testing.T) {
	// Node 3 is created but not started, so nodes 1 and 2 are a majority and
	// carry on without it. Every 10 applied entries they snapshot and cut their
	// logs, so when node 3 finally starts, the entries it is missing are gone.
	nodes := startRealClusterWith(t, 3, 10, 0, map[NodeID]bool{3: true})
	first := waitForLeader(t, nodes, map[NodeID]bool{3: true})
	leader := nodeByID(nodes, first.ID)

	commands := make([]string, 60)
	for i := range commands {
		commands[i] = fmt.Sprintf("cmd-%03d", i)
	}
	proposeAll(t, leader.runner, commands)
	waitFor(t, "the leader to snapshot its state", func() bool { return leader.runner.Status().SnapshotIndex > 0 })

	late := nodeByID(nodes, 3)
	late.start()

	waitFor(t, "the late node to apply every command", func() bool {
		return reflect.DeepEqual(late.appliedData(), commands)
	})
	if got := late.runner.Status().SnapshotIndex; got == 0 {
		t.Fatal("the late node caught up without receiving a snapshot")
	}
	for _, n := range nodes {
		waitFor(t, fmt.Sprintf("node %d to hold every command", n.id), func() bool {
			return reflect.DeepEqual(n.appliedData(), commands)
		})
	}

	// And it takes part normally afterwards: a command still reaches all three.
	proposeAll(t, leader.runner, []string{"after"})
	want := append(append([]string(nil), commands...), "after")
	for _, n := range nodes {
		waitFor(t, fmt.Sprintf("node %d to apply the last command", n.id), func() bool {
			return reflect.DeepEqual(n.appliedData(), want)
		})
	}
}

func TestRunnerRefusesSnapshotsWithoutAStateMachine(t *testing.T) {
	cfg := testConfig(1, clusterIDs(3), &memStorage{}, 1)
	if _, err := NewRunner(cfg, nopTransport{}, realTick, WithSnapshotEvery(10)); err == nil {
		t.Fatal("WithSnapshotEvery was accepted without a state machine")
	}
	cfg = testConfig(1, clusterIDs(3), &memStorage{}, 1)
	if _, err := NewRunner(cfg, nopTransport{}, realTick, WithApplier(func(Entry) {}), WithSnapshotEvery(10)); err == nil {
		t.Fatal("a plain applier cannot snapshot, but WithSnapshotEvery was accepted")
	}
}

func TestProposeGivesUpWhenThereIsNoMajority(t *testing.T) {
	nodes := startRealCluster(t, 3)
	first := waitForLeader(t, nodes, nil)
	leader := nodeByID(nodes, first.ID)
	for _, n := range nodes {
		if n.id != leader.id {
			n.kill()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := leader.runner.Propose(ctx, []byte("unreachable"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline: nothing can commit without a majority", err)
	}
	if got := leader.appliedData(); len(got) != 0 {
		t.Fatalf("the leader applied %v alone", got)
	}
}

func TestProposeAfterStopReturnsPromptly(t *testing.T) {
	store, _ := NewFileStorage(filepath.Join(t.TempDir(), "state.json"))
	var applied []string
	var mu sync.Mutex
	r, err := NewRunner(Config{
		ID: 1, Cluster: []NodeID{1}, ElectionTicks: testElectionTicks, HeartbeatTicks: testHeartbeatTicks, Storage: store,
	}, nopTransport{}, realTick, WithApplier(func(e Entry) {
		mu.Lock()
		applied = append(applied, string(e.Data))
		mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	waitFor(t, "the single node to lead", func() bool { return r.Status().Role == Leader })

	// A one-node cluster commits on its own, and Propose returns once applied.
	proposeAll(t, r, []string{"only"})
	mu.Lock()
	got := append([]string(nil), applied...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"only"}) {
		t.Fatalf("applied %v, want [only]", got)
	}

	r.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = r.Propose(ctx, []byte("late"))
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("Propose on a stopped runner: err = %v after %v; want a prompt error", err, time.Since(start))
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
			r.dispatch([]Message{{Type: MsgAppend, From: 1, To: 2, Term: 1}})
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
	in := Message{
		Type: MsgAppend, From: 3, To: 5, Term: 9, LastLogIndex: 4, LastLogTerm: 2, Granted: true, Success: true,
		PrevLogIndex: 7, PrevLogTerm: 8, LeaderCommit: 6, MatchIndex: 12, HintIndex: 3,
		Entries: []Entry{
			{Index: 8, Term: 9, Type: EntryNoop},
			{Index: 9, Term: 9, Type: EntryNormal, Data: []byte("payload")},
		},
	}
	p, err := messageToPB(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := messageFromPB(p)
	if err != nil || !reflect.DeepEqual(out, in) {
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
