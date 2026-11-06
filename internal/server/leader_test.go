package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// failingBackend answers every request with err.
type failingBackend struct{ err error }

func (b failingBackend) CreateTopic(context.Context, string, int) error { return b.err }
func (b failingBackend) Produce(context.Context, string, int, []storage.Message) (int, uint64, error) {
	return 0, 0, b.err
}
func (b failingBackend) CommitOffset(context.Context, string, string, int, uint64) error {
	return b.err
}
func (b failingBackend) Fetch(string, int, uint64, int) ([]storage.Message, error) {
	return nil, b.err
}
func (b failingBackend) GroupOffset(string, string, int) (broker.OffsetInfo, error) {
	return broker.OffsetInfo{}, b.err
}

func clientFor(t *testing.T, backend Backend, opts Options) pb.BrokerClient {
	t.Helper()
	opts.MaxFetchMessages = 1
	srv, err := New(backend, opts)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	Register(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		gs.Stop()
	})
	return pb.NewBrokerClient(conn)
}

func TestANodeThatIsNotTheLeaderSaysWhoIs(t *testing.T) {
	c := clientFor(t, failingBackend{&raft.NotLeaderError{Leader: 3}}, Options{
		LeaderAddr: func(id raft.NodeID) string { return fmt.Sprintf("host-%d:9092", id) },
	})

	var trailer metadata.MD
	_, err := c.Produce(ctx(t), &pb.ProduceRequest{Topic: "t", Messages: []*pb.Message{{Value: []byte("x")}}}, grpc.Trailer(&trailer))
	wantCode(t, err, codes.FailedPrecondition)
	if got := trailer.Get(LeaderIDKey); len(got) != 1 || got[0] != "3" {
		t.Fatalf("leader id trailer = %v, want [3]", got)
	}
	if got := trailer.Get(LeaderAddrKey); len(got) != 1 || got[0] != "host-3:9092" {
		t.Fatalf("leader address trailer = %v, want [host-3:9092]", got)
	}

	// Every kind of write carries the hint.
	for name, call := range map[string]func(grpc.CallOption) error{
		"CreateTopic": func(o grpc.CallOption) error {
			_, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "t", Partitions: 1}, o)
			return err
		},
		"CommitOffset": func(o grpc.CallOption) error {
			_, err := c.CommitOffset(ctx(t), &pb.CommitOffsetRequest{Group: "g", Topic: "t"}, o)
			return err
		},
	} {
		var md metadata.MD
		wantCode(t, call(grpc.Trailer(&md)), codes.FailedPrecondition)
		if len(md.Get(LeaderAddrKey)) != 1 {
			t.Errorf("%s: no leader address in the trailer", name)
		}
	}
}

func TestWhenTheLeaderIsUnknownNoHintIsSent(t *testing.T) {
	c := clientFor(t, failingBackend{&raft.NotLeaderError{Leader: raft.None}}, Options{
		LeaderAddr: func(raft.NodeID) string { return "should-not-be-used:1" },
	})
	var trailer metadata.MD
	_, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "t", Partitions: 1}, grpc.Trailer(&trailer))
	wantCode(t, err, codes.FailedPrecondition)
	if len(trailer.Get(LeaderIDKey)) != 0 || len(trailer.Get(LeaderAddrKey)) != 0 {
		t.Fatalf("hints %v were sent although the node does not know the leader", trailer)
	}
}

func TestNoAddressIsSentWhenTheNodeCannotResolveOne(t *testing.T) {
	c := clientFor(t, failingBackend{&raft.NotLeaderError{Leader: 2}}, Options{}) // no LeaderAddr
	var trailer metadata.MD
	_, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "t", Partitions: 1}, grpc.Trailer(&trailer))
	wantCode(t, err, codes.FailedPrecondition)
	if got := trailer.Get(LeaderIDKey); len(got) != 1 || got[0] != "2" {
		t.Fatalf("leader id trailer = %v", got)
	}
	if len(trailer.Get(LeaderAddrKey)) != 0 {
		t.Fatal("an address was made up")
	}
}

func TestRaftAndContextErrorsMapToUsefulCodes(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{raft.ErrLeadershipLost, codes.Unavailable},
		{raft.ErrEntryReplaced, codes.Unavailable},
		{raft.ErrStopped, codes.Unavailable},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{context.Canceled, codes.Canceled},
		{fmt.Errorf("wrapped: %w", broker.ErrTopicNotFound), codes.NotFound},
		{errors.New("something unexpected"), codes.Internal},
	}
	for _, c := range cases {
		cl := clientFor(t, failingBackend{c.err}, Options{})
		_, err := cl.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "t", Partitions: 1})
		if got := status.Code(err); got != c.want {
			t.Errorf("%v: code = %v, want %v", c.err, got, c.want)
		}
	}
}
