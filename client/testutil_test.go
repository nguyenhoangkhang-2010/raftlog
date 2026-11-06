package client_test

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// serve runs a gRPC server for backend on a free local port and returns its address.
func serve(t *testing.T, backend server.Backend, opts server.Options) string {
	t.Helper()
	if opts.MaxFetchMessages == 0 {
		opts.MaxFetchMessages = 100
	}
	srv, err := server.New(backend, opts)
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
	return lis.Addr().String()
}

// newGRPCServer returns a gRPC server that serves srv; the caller starts it.
func newGRPCServer(srv *server.Server) *grpc.Server {
	gs := grpc.NewServer()
	server.Register(gs, srv)
	return gs
}

// startBroker runs a real standalone broker with its data in a temporary directory.
func startBroker(t *testing.T) string {
	t.Helper()
	b, err := broker.Open(filepath.Join(t.TempDir(), "data"), broker.Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return serve(t, server.Local(b), server.Options{})
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

// refusingBackend refuses every request with err, as a follower or a failing
// node would, and counts the attempts.
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

// notLeader is what a follower answers; leader is the node it names, or raft.None.
func notLeader(leader raft.NodeID) error { return &raft.NotLeaderError{Leader: leader} }

// flippingBackend answers like a follower of a dead leader until it has been
// asked `refusals` times, then accepts writes.
type flippingBackend struct {
	refusals int32
	calls    atomic.Int32
}

func (b *flippingBackend) refuse() error {
	if b.calls.Add(1) <= b.refusals {
		return notLeader(2)
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

// fakeRaft reports a fixed Raft status.
type fakeRaft struct{ st raft.Status }

func (f fakeRaft) Status() raft.Status { return f.st }
