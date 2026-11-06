package client_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

func ctx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return c
}

// newClient returns a client that retries quickly, for the failure tests.
func newClient(t *testing.T, addrs []string, opts ...client.Option) *client.Client {
	t.Helper()
	c, err := client.New(addrs, append([]client.Option{client.WithRetryPause(time.Millisecond)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNewValidatesItsInput(t *testing.T) {
	cases := map[string]struct {
		addrs []string
		opts  []client.Option
	}{
		"no addresses":       {nil, nil},
		"only blank entries": {[]string{"", "  "}, nil},
		"negative attempts":  {[]string{"a:1"}, []client.Option{client.WithMaxAttempts(-1)}},
		"zero pause":         {[]string{"a:1"}, []client.Option{client.WithRetryPause(0)}},
	}
	for name, c := range cases {
		if cl, err := client.New(c.addrs, c.opts...); err == nil {
			_ = cl.Close()
			t.Errorf("%s: accepted", name)
		}
	}
	// Blanks around a real address are ignored.
	cl, err := client.New([]string{" a:1 ", ""})
	if err != nil {
		t.Fatal(err)
	}
	_ = cl.Close()
}

func TestAWriteFollowsTheLeaderHintOfAFollower(t *testing.T) {
	leader := startBroker(t)
	follower := &refusingBackend{err: notLeader(2)}
	followerAddr := serve(t, follower, server.Options{LeaderAddr: func(raft.NodeID) string { return leader }})

	// Only the follower's address is given; it names the leader.
	c := newClient(t, []string{followerAddr})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatal(err)
	}
	res, err := c.Produce(ctx(t, 10*time.Second), "events", []client.Message{{Value: []byte("hello")}})
	if err != nil || res.Offset != 0 {
		t.Fatalf("Produce = %+v, %v", res, err)
	}
	if follower.calls.Load() != 2 {
		t.Fatalf("the follower was asked %d times, want 2 (once per request)", follower.calls.Load())
	}

	// The message really is on the leader.
	recs, err := newClient(t, []string{leader}).Fetch(ctx(t, 10*time.Second), "events", 0, 0, 10)
	if err != nil || len(recs) != 1 || string(recs[0].Value) != "hello" {
		t.Fatalf("Fetch from the leader = %v, %v", recs, err)
	}
}

func TestAnUnreachableNodeIsSkipped(t *testing.T) {
	good := startBroker(t)
	c := newClient(t, []string{deadAddr(t), good})

	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatalf("CreateTopic with a dead first address: %v", err)
	}
	if _, err := c.Produce(ctx(t, 10*time.Second), "events", []client.Message{{Value: []byte("x")}}); err != nil {
		t.Fatalf("Produce: %v", err)
	}
}

func TestWhenTheLeaderIsUnknownTheNextNodeIsAsked(t *testing.T) {
	good := startBroker(t)
	// During an election a follower knows no leader and gives no hint.
	follower := &refusingBackend{err: notLeader(raft.None)}
	followerAddr := serve(t, follower, server.Options{})

	c := newClient(t, []string{followerAddr, good})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatal(err)
	}
}

func TestItGivesUpAfterTheAttemptsItWasAllowed(t *testing.T) {
	// A follower that names itself as the leader: following the hint would loop.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self := l.Addr().String()
	_ = l.Close()

	b := &refusingBackend{err: notLeader(2)}
	srv, _ := server.New(b, server.Options{MaxFetchMessages: 1, LeaderAddr: func(raft.NodeID) string { return self }})
	lis, err := net.Listen("tcp", self)
	if err != nil {
		t.Fatal(err)
	}
	gs := newGRPCServer(srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	c := newClient(t, []string{self}, client.WithMaxAttempts(4))
	err = c.CreateTopic(ctx(t, 10*time.Second), "events", 1)
	if !errors.Is(err, client.ErrUnavailable) || !strings.Contains(err.Error(), "gave up after 4 attempts") {
		t.Fatalf("err = %v, want ErrUnavailable mentioning the 4 attempts", err)
	}
	if got := b.calls.Load(); got != 4 {
		t.Fatalf("the node was asked %d times, want exactly the 4 attempts allowed", got)
	}
}

func TestAnErrorAnotherNodeCannotFixIsNotRetried(t *testing.T) {
	// The topic does not exist: asking the second node would not change that.
	first := &refusingBackend{err: broker.ErrTopicNotFound}
	second := &refusingBackend{err: broker.ErrTopicNotFound}
	c := newClient(t, []string{serve(t, first, server.Options{}), serve(t, second, server.Options{})})

	_, err := c.Produce(ctx(t, 10*time.Second), "missing", []client.Message{{Value: []byte("x")}})
	if !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 0 {
		t.Fatalf("asked %d and %d times, want 1 and 0", first.calls.Load(), second.calls.Load())
	}
}

func TestTheClientOutwaitsAnElectionAfterTheLeaderDied(t *testing.T) {
	// Right after a leader dies, the other nodes still name it. The client is
	// sent to a dead address again and again until an election has finished.
	dead := deadAddr(t)
	b := &flippingBackend{refusals: 30}
	addr := serve(t, b, server.Options{LeaderAddr: func(raft.NodeID) string { return dead }})

	c := newClient(t, []string{addr})
	if err := c.CreateTopic(ctx(t, 20*time.Second), "events", 1); err != nil {
		t.Fatalf("the client gave up before the election was over: %v", err)
	}
	if got := b.calls.Load(); got != 31 {
		t.Fatalf("the node was asked %d times, want 31 (30 refusals, then success)", got)
	}
}

func TestADeadLeaderIsNotHammered(t *testing.T) {
	// The only node we know keeps naming a leader that is down. Between two
	// tries the client must pause, not spin on the dead address.
	const pause = 50 * time.Millisecond
	follower := &refusingBackend{err: notLeader(2)}
	dead := deadAddr(t)
	addr := serve(t, follower, server.Options{LeaderAddr: func(raft.NodeID) string { return dead }})

	c := newClient(t, []string{addr}, client.WithRetryPause(pause))
	_ = c.CreateTopic(ctx(t, 400*time.Millisecond), "events", 1)

	// 400 ms at one try per pause is about 8 questions; allow generous slack
	// for slow machines, but a spinning client asks thousands of times.
	if got := follower.calls.Load(); got > 40 {
		t.Fatalf("the node was asked %d times in 400 ms with a %v pause: the client does not wait", got, pause)
	}
}

func TestTheContextBoundsTheRetries(t *testing.T) {
	// Nobody ever becomes leader. The client must not wait forever.
	follower := &refusingBackend{err: notLeader(raft.None)}
	c := newClient(t, []string{serve(t, follower, server.Options{})})

	start := time.Now()
	err := c.CreateTopic(ctx(t, 300*time.Millisecond), "events", 1)
	if err == nil {
		t.Fatal("succeeded although there is no leader")
	}
	if !errors.Is(err, client.ErrUnavailable) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrUnavailable or a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v to give up on a 300 ms deadline", elapsed)
	}
}

func TestAClosedClientRefusesRequests(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("a second Close should do nothing, got %v", err)
	}
	if err := c.CreateTopic(ctx(t, time.Second), "events", 1); !errors.Is(err, client.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestNodeStatusAsksOnlyTheNodeItIsGiven(t *testing.T) {
	status := fakeRaft{raft.Status{ID: 3, Role: raft.Follower, Term: 9, Leader: 2, CommitIndex: 40, LastIndex: 42, SnapshotIndex: 30}}
	node := serve(t, &refusingBackend{}, server.Options{Raft: status})
	c := newClient(t, []string{deadAddr(t), node})

	got, err := c.NodeStatus(ctx(t, 10*time.Second), node)
	if err != nil {
		t.Fatal(err)
	}
	want := client.NodeStatus{RaftEnabled: true, NodeID: 3, Role: "follower", Term: 9, LeaderID: 2, CommitIndex: 40, LastIndex: 42, SnapshotIndex: 30}
	if got != want {
		t.Fatalf("NodeStatus = %+v, want %+v", got, want)
	}

	// A node that is down is an error, not a reason to ask another node: the
	// answer would describe the wrong machine.
	if _, err := c.NodeStatus(ctx(t, 2*time.Second), deadAddr(t)); err == nil {
		t.Fatal("a dead node answered")
	}

	// A standalone node says so.
	alone, err := c.NodeStatus(ctx(t, 10*time.Second), startBroker(t))
	if err != nil || alone.RaftEnabled {
		t.Fatalf("standalone status = %+v, %v", alone, err)
	}
}

func TestRouterAsksTheFirstAddressForItsStatus(t *testing.T) {
	good := startBroker(t)

	dead, err := client.NewRouter([]string{deadAddr(t), good}, client.WithRetryPause(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	if _, err := dead.NodeStatus(ctx(t, 2*time.Second), nil); err == nil {
		t.Fatal("NodeStatus was answered by another node although the first address is down")
	}
	if got := dead.Addrs(); len(got) != 2 || got[1] != good {
		t.Fatalf("Addrs = %v", got)
	}

	live, err := client.NewRouter([]string{good, deadAddr(t)}, client.WithRetryPause(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	resp, err := live.NodeStatus(ctx(t, 10*time.Second), nil)
	if err != nil || resp.GetRaftEnabled() {
		t.Fatalf("NodeStatus = %+v, %v; want a standalone node", resp, err)
	}
}
