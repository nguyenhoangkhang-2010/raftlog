package main

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// refusingBackend is a node that refuses every write with err, as a follower or
// a failing node would, and counts the attempts.
type refusingBackend struct {
	err   error
	calls atomic.Int32
}

func (b *refusingBackend) CreateTopic(context.Context, string, int) error {
	b.calls.Add(1)
	return b.err
}

func (b *refusingBackend) Produce(context.Context, string, int, []storage.Message) (int, uint64, error) {
	b.calls.Add(1)
	return 0, 0, b.err
}

func (b *refusingBackend) CommitOffset(context.Context, string, string, int, uint64) error {
	b.calls.Add(1)
	return b.err
}

func (b *refusingBackend) Fetch(string, int, uint64, int) ([]storage.Message, error) {
	b.calls.Add(1)
	return nil, b.err
}

func (b *refusingBackend) GroupOffset(string, string, int) (broker.OffsetInfo, error) {
	b.calls.Add(1)
	return broker.OffsetInfo{}, b.err
}

// startRefusing runs a node whose writes fail with err. leaderAddr is what it
// tells clients about the leader of node 2 (empty: it does not know).
func startRefusing(t *testing.T, err error, leaderAddr string) (string, *refusingBackend) {
	t.Helper()
	b := &refusingBackend{err: err}
	srv, serr := server.New(b, server.Options{
		MaxFetchMessages: 1,
		LeaderAddr:       func(raft.NodeID) string { return leaderAddr },
	})
	if serr != nil {
		t.Fatal(serr)
	}
	lis, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	gs := grpc.NewServer()
	server.Register(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String(), b
}

// deadAddr returns an address where nothing is listening.
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestAWriteFollowsTheLeaderHintOfAFollower(t *testing.T) {
	leader := startBroker(t)
	follower, backend := startRefusing(t, &raft.NotLeaderError{Leader: 2}, leader)

	// Only the follower's address is given; it names the leader.
	code, _, e := ctl(t, follower, "", "-retry-pause", "1ms", "create-topic", "events")
	if code != 0 {
		t.Fatalf("create-topic: code %d, stderr %q", code, e)
	}
	code, out, e := ctl(t, follower, "", "-retry-pause", "1ms", "produce", "-value", "hello", "events")
	if code != 0 || !strings.Contains(out, "base_offset=0") {
		t.Fatalf("produce: code %d, stdout %q, stderr %q", code, out, e)
	}
	if backend.calls.Load() != 2 {
		t.Fatalf("the follower was asked %d times, want 2 (once per command)", backend.calls.Load())
	}

	// The message really is on the leader.
	if _, out, _ := ctl(t, leader, "", "fetch", "events"); !strings.Contains(out, `"hello"`) {
		t.Fatalf("the leader does not hold the message: %q", out)
	}
}

func TestAnUnreachableNodeIsSkipped(t *testing.T) {
	good := startBroker(t)
	addrs := deadAddr(t) + "," + good

	if code, _, e := ctl(t, "", "", "-addr", addrs, "-retry-pause", "1ms", "create-topic", "events"); code != 0 {
		t.Fatalf("create-topic with a dead first address: code %d, stderr %q", code, e)
	}
	code, out, e := ctl(t, "", "", "-addr", addrs, "-retry-pause", "1ms", "produce", "-value", "x", "events")
	if code != 0 || !strings.Contains(out, "count=1") {
		t.Fatalf("produce: code %d, stdout %q, stderr %q", code, out, e)
	}
}

func TestWhenTheLeaderIsUnknownTheNextNodeIsAsked(t *testing.T) {
	good := startBroker(t)
	// During an election a follower knows no leader and gives no hint.
	follower, _ := startRefusing(t, &raft.NotLeaderError{Leader: raft.None}, "")

	code, _, e := ctl(t, "", "", "-addr", follower+","+good, "-retry-pause", "1ms", "create-topic", "events")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, e)
	}
}

func TestItGivesUpWhenNoNodeCanAnswer(t *testing.T) {
	// A follower that points at itself: following the hint would loop forever.
	var self string
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self = l.Addr().String()
	_ = l.Close()

	b := &refusingBackend{err: &raft.NotLeaderError{Leader: 2}}
	srv, _ := server.New(b, server.Options{MaxFetchMessages: 1, LeaderAddr: func(raft.NodeID) string { return self }})
	lis, err := net.Listen("tcp", self)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	server.Register(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	code, _, e := ctl(t, "", "", "-addr", self, "-retries", "4", "-retry-pause", "1ms", "create-topic", "events")
	if code != 1 || !strings.Contains(e, "gave up after 4 attempts") {
		t.Fatalf("code %d, stderr %q; want exit 1 and a message about giving up", code, e)
	}
	if got := b.calls.Load(); got != 4 {
		t.Fatalf("the node was asked %d times, want exactly the 4 attempts allowed", got)
	}
}

// flippingBackend refuses writes as a follower of a dead leader until it has
// been asked `refusals` times, then accepts them.
type flippingBackend struct {
	refusals int32
	calls    atomic.Int32
}

func (b *flippingBackend) refuse() error {
	if b.calls.Add(1) <= b.refusals {
		return &raft.NotLeaderError{Leader: 2}
	}
	return nil
}

func (b *flippingBackend) CreateTopic(context.Context, string, int) error { return b.refuse() }
func (b *flippingBackend) Produce(context.Context, string, int, []storage.Message) (int, uint64, error) {
	return 0, 0, b.refuse()
}
func (b *flippingBackend) CommitOffset(context.Context, string, string, int, uint64) error {
	return b.refuse()
}
func (b *flippingBackend) Fetch(string, int, uint64, int) ([]storage.Message, error) { return nil, nil }
func (b *flippingBackend) GroupOffset(string, string, int) (broker.OffsetInfo, error) {
	return broker.OffsetInfo{}, nil
}

func TestTheClientOutwaitsAnElectionAfterTheLeaderDied(t *testing.T) {
	// Right after a leader dies, the other nodes still name it. The client is
	// sent to a dead address again and again until an election has finished,
	// which takes many more attempts than a handful.
	dead := deadAddr(t)
	b := &flippingBackend{refusals: 30}
	srv, err := server.New(b, server.Options{MaxFetchMessages: 1, LeaderAddr: func(raft.NodeID) string { return dead }})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	server.Register(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	code, _, e := ctl(t, "", "", "-addr", lis.Addr().String(), "-retry-pause", "1ms", "-timeout", "20s", "create-topic", "events")
	if code != 0 {
		t.Fatalf("code %d, stderr %q: the client gave up before the election was over", code, e)
	}
	if got := b.calls.Load(); got != 31 {
		t.Fatalf("the node was asked %d times, want 31 (30 refusals, then success)", got)
	}
}

func TestAnErrorAnotherNodeCannotFixIsNotRetried(t *testing.T) {
	// The topic does not exist: asking the second node would not change that.
	first, refusing := startRefusing(t, broker.ErrTopicNotFound, "")
	second, other := startRefusing(t, broker.ErrTopicNotFound, "")

	code, _, e := ctl(t, "", "", "-addr", first+","+second, "-retry-pause", "1ms", "produce", "-value", "x", "missing")
	if code != 1 || !strings.Contains(e, "NotFound") {
		t.Fatalf("code %d, stderr %q", code, e)
	}
	if refusing.calls.Load() != 1 || other.calls.Load() != 0 {
		t.Fatalf("asked %d and %d times, want 1 and 0", refusing.calls.Load(), other.calls.Load())
	}
}

func TestStatusAsksOnlyTheFirstAddress(t *testing.T) {
	good := startBroker(t)
	// The question is about one node; answering from another would be a lie.
	code, _, _ := ctl(t, "", "", "-addr", deadAddr(t)+","+good, "-timeout", "2s", "-retry-pause", "1ms", "status")
	if code != 1 {
		t.Fatalf("status with a dead first address: code %d, want 1", code)
	}
	if code, out, _ := ctl(t, "", "", "-addr", good+","+deadAddr(t), "status"); code != 0 || !strings.Contains(out, "raft=disabled") {
		t.Fatalf("status of the first address: code %d, stdout %q", code, out)
	}
}

func TestClusterFlagsAreValidated(t *testing.T) {
	addr := startBroker(t)
	for _, args := range [][]string{
		{"-addr", " , ,", "status"},
		{"-addr", addr, "-retries", "-1", "status"},
	} {
		if code, _, _ := ctl(t, "", "", args...); code != 2 {
			t.Errorf("args %v: exit code %d, want 2", args, code)
		}
	}
}
