package server

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
)

const testMaxFetch = 5

// startServer runs a real gRPC server on a free local port and returns a client.
func startServer(t *testing.T) pb.BrokerClient {
	t.Helper()
	b, err := broker.Open(t.TempDir(), broker.Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(b, Options{MaxFetchMessages: testMaxFetch})
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
		_ = b.Close()
	})
	return pb.NewBrokerClient(conn)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v (err: %v), want %v", got, err, want)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("expected error for MaxFetchMessages = 0")
	}
}

func TestProduceAndFetchOverGRPC(t *testing.T) {
	c := startServer(t)
	if _, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "events", Partitions: 3}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	p, err := c.Produce(ctx(t), &pb.ProduceRequest{
		Topic: "events",
		Messages: []*pb.Message{
			{Key: []byte("u1"), Value: []byte("first"), TimestampMs: 1234},
			{Key: []byte("u1"), Value: []byte("second")},
		},
	})
	if err != nil {
		t.Fatalf("Produce: %v", err)
	}
	if p.BaseOffset != 0 {
		t.Fatalf("base offset = %d, want 0", p.BaseOffset)
	}

	f, err := c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", Partition: p.Partition, Offset: 0, MaxMessages: 10})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(f.Messages) != 2 {
		t.Fatalf("got %d msgs, want 2", len(f.Messages))
	}
	first, second := f.Messages[0], f.Messages[1]
	if string(first.Value) != "first" || string(first.Key) != "u1" || first.TimestampMs != 1234 || first.Offset != 0 {
		t.Fatalf("first = %+v", first)
	}
	if string(second.Value) != "second" || second.Offset != 1 || second.TimestampMs == 0 {
		t.Fatalf("second = %+v (server should fill in a timestamp)", second)
	}
}

func TestExplicitPartitionOverGRPC(t *testing.T) {
	c := startServer(t)
	if _, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "events", Partitions: 3}); err != nil {
		t.Fatal(err)
	}
	two := uint32(2)
	p, err := c.Produce(ctx(t), &pb.ProduceRequest{
		Topic:     "events",
		Partition: &two,
		Messages:  []*pb.Message{{Value: []byte("x")}},
	})
	if err != nil || p.Partition != 2 {
		t.Fatalf("partition = %v, err = %v; want 2", p.GetPartition(), err)
	}

	// Partition 0 is a real, valid partition; it must not be mistaken for "unset".
	zero := uint32(0)
	p, err = c.Produce(ctx(t), &pb.ProduceRequest{
		Topic:     "events",
		Partition: &zero,
		Messages:  []*pb.Message{{Value: []byte("y")}},
	})
	if err != nil || p.Partition != 0 {
		t.Fatalf("partition = %v, err = %v; want 0", p.GetPartition(), err)
	}
}

func TestFetchIsCappedByServerLimit(t *testing.T) {
	c := startServer(t)
	if _, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "events", Partitions: 1}); err != nil {
		t.Fatal(err)
	}
	var batch []*pb.Message
	for i := 0; i < testMaxFetch*2; i++ {
		batch = append(batch, &pb.Message{Value: []byte(fmt.Sprint(i))})
	}
	if _, err := c.Produce(ctx(t), &pb.ProduceRequest{Topic: "events", Messages: batch}); err != nil {
		t.Fatal(err)
	}

	f, err := c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", MaxMessages: 1000})
	if err != nil || len(f.Messages) != testMaxFetch {
		t.Fatalf("got %d msgs (err %v), want the server cap %d", len(f.Messages), err, testMaxFetch)
	}
	f, err = c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", MaxMessages: 2})
	if err != nil || len(f.Messages) != 2 {
		t.Fatalf("got %d msgs (err %v), want 2", len(f.Messages), err)
	}
}

func TestErrorCodes(t *testing.T) {
	c := startServer(t)
	if _, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "events", Partitions: 2}); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "events", Partitions: 2})
	wantCode(t, err, codes.AlreadyExists)

	_, err = c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "bad/name", Partitions: 1})
	wantCode(t, err, codes.InvalidArgument)

	_, err = c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: "zero", Partitions: 0})
	wantCode(t, err, codes.InvalidArgument)

	_, err = c.Produce(ctx(t), &pb.ProduceRequest{Topic: "nope", Messages: []*pb.Message{{Value: []byte("x")}}})
	wantCode(t, err, codes.NotFound)

	_, err = c.Produce(ctx(t), &pb.ProduceRequest{Topic: "events"})
	wantCode(t, err, codes.InvalidArgument) // empty batch

	bad := uint32(9)
	_, err = c.Produce(ctx(t), &pb.ProduceRequest{Topic: "events", Partition: &bad, Messages: []*pb.Message{{Value: []byte("x")}}})
	wantCode(t, err, codes.InvalidArgument)

	_, err = c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", MaxMessages: 0})
	wantCode(t, err, codes.InvalidArgument)

	_, err = c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", Offset: 100, MaxMessages: 1})
	wantCode(t, err, codes.OutOfRange)

	_, err = c.Fetch(ctx(t), &pb.FetchRequest{Topic: "nope", MaxMessages: 1})
	wantCode(t, err, codes.NotFound)

	_, err = c.CommitOffset(ctx(t), &pb.CommitOffsetRequest{Group: "g", Topic: "events"})
	wantCode(t, err, codes.Unimplemented)
}
