package server

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// startWaitServer is startServer with a choice of options. It also returns the
// Server, to call Shutdown on.
func startWaitServer(t *testing.T, opts Options) (pb.BrokerClient, *Server) {
	t.Helper()
	b, err := broker.Open(t.TempDir(), broker.Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxFetchMessages = testMaxFetch
	srv, err := New(Local(b), opts)
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
	return pb.NewBrokerClient(conn), srv
}

func mustCreate(t *testing.T, c pb.BrokerClient, topic string, partitions uint32) {
	t.Helper()
	if _, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: topic, Partitions: partitions}); err != nil {
		t.Fatal(err)
	}
}

func produceOne(t *testing.T, c pb.BrokerClient, topic string, partition uint32, value string) {
	t.Helper()
	if _, err := c.Produce(ctx(t), &pb.ProduceRequest{
		Topic: topic, Partition: &partition, Messages: []*pb.Message{{Value: []byte(value)}},
	}); err != nil {
		t.Fatal(err)
	}
}

type fetchOutcome struct {
	resp    *pb.FetchResponse
	err     error
	elapsed time.Duration
}

// fetchAsync starts a Fetch and delivers its outcome on the returned channel.
func fetchAsync(c pb.BrokerClient, req *pb.FetchRequest) <-chan fetchOutcome {
	out := make(chan fetchOutcome, 1)
	go func() {
		start := time.Now()
		resp, err := c.Fetch(context.Background(), req)
		out <- fetchOutcome{resp, err, time.Since(start)}
	}()
	return out
}

func await(t *testing.T, ch <-chan fetchOutcome, d time.Duration) fetchOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(d):
		t.Fatalf("the Fetch had not answered after %v", d)
		return fetchOutcome{}
	}
}

func TestFetchWaitsForAMessageAndReturnsWhenItArrives(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)

	got := fetchAsync(c, &pb.FetchRequest{Topic: "events", Offset: 0, MaxMessages: 5, MaxWaitMs: 30_000})
	select {
	case o := <-got:
		t.Fatalf("answered before any message was produced: %+v", o)
	case <-time.After(150 * time.Millisecond):
	}

	produceOne(t, c, "events", 0, "hello")
	o := await(t, got, 5*time.Second)
	if o.err != nil || len(o.resp.Messages) != 1 || string(o.resp.Messages[0].Value) != "hello" {
		t.Fatalf("outcome = %+v, want the one message", o)
	}
	if o.elapsed > 5*time.Second {
		t.Fatalf("took %v: it waited for the timeout instead of the message", o.elapsed)
	}
}

func TestFetchWithoutAWaitAnswersAtOnce(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)

	o := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "events", MaxMessages: 5}), 2*time.Second)
	if o.err != nil || len(o.resp.Messages) != 0 {
		t.Fatalf("outcome = %+v, want an empty answer", o)
	}
}

func TestFetchReturnsNothingWhenTheWaitRunsOut(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)

	o := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 200}), 5*time.Second)
	if o.err != nil || len(o.resp.Messages) != 0 {
		t.Fatalf("outcome = %+v, want an empty answer and no error", o)
	}
	if o.elapsed < 150*time.Millisecond {
		t.Fatalf("answered after %v although it was to wait 200 ms", o.elapsed)
	}
}

func TestTheServerLimitShortensTheWait(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: 150 * time.Millisecond})
	mustCreate(t, c, "events", 1)

	o := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 600_000}), 5*time.Second)
	if o.err != nil || len(o.resp.Messages) != 0 {
		t.Fatalf("outcome = %+v, want an empty answer", o)
	}
	if o.elapsed < 100*time.Millisecond {
		t.Fatalf("answered after %v, before the server's limit of 150 ms", o.elapsed)
	}
}

func TestWaitingCanBeSwitchedOff(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: 0})
	mustCreate(t, c, "events", 1)

	o := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 600_000}), 2*time.Second)
	if o.err != nil || len(o.resp.Messages) != 0 {
		t.Fatalf("outcome = %+v, want an empty answer at once", o)
	}
}

func TestFetchDoesNotWaitWhenThereIsAlreadyData(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)
	produceOne(t, c, "events", 0, "a")

	o := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 600_000}), 2*time.Second)
	if o.err != nil || len(o.resp.Messages) != 1 {
		t.Fatalf("outcome = %+v, want the message at once", o)
	}
}

func TestAWaitingFetchStillReportsErrorsAtOnce(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)

	missing := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "nope", MaxMessages: 5, MaxWaitMs: 600_000}), 2*time.Second)
	wantCode(t, missing.err, codes.NotFound)

	beyond := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "events", Offset: 50, MaxMessages: 5, MaxWaitMs: 600_000}), 2*time.Second)
	wantCode(t, beyond.err, codes.OutOfRange)

	badPartition := await(t, fetchAsync(c, &pb.FetchRequest{Topic: "events", Partition: 9, MaxMessages: 5, MaxWaitMs: 600_000}), 2*time.Second)
	wantCode(t, badPartition.err, codes.InvalidArgument)
}

func TestAMessageForAnotherPartitionDoesNotEndTheWait(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 2)

	got := fetchAsync(c, &pb.FetchRequest{Topic: "events", Partition: 0, MaxMessages: 5, MaxWaitMs: 600_000})
	time.Sleep(50 * time.Millisecond)
	produceOne(t, c, "events", 1, "elsewhere")
	select {
	case o := <-got:
		t.Fatalf("a message for partition 1 ended the wait on partition 0: %+v", o)
	case <-time.After(150 * time.Millisecond):
	}
	produceOne(t, c, "events", 0, "here")
	o := await(t, got, 5*time.Second)
	if o.err != nil || len(o.resp.Messages) != 1 || string(o.resp.Messages[0].Value) != "here" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestManyWaitingFetchesAreAllWoken(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)

	const readers = 20
	outs := make([]<-chan fetchOutcome, readers)
	for i := range outs {
		outs[i] = fetchAsync(c, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 600_000})
	}
	time.Sleep(100 * time.Millisecond)
	produceOne(t, c, "events", 0, "x")
	for i, ch := range outs {
		if o := await(t, ch, 5*time.Second); o.err != nil || len(o.resp.Messages) != 1 {
			t.Fatalf("reader %d: %+v", i, o)
		}
	}
}

func TestACancelledFetchStopsWaiting(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)

	cctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Fetch(cctx, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 600_000})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		wantCode(t, err, codes.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("the Fetch kept waiting after the caller gave up")
	}
}

func TestShutdownReleasesWaitingFetches(t *testing.T) {
	c, srv := startWaitServer(t, Options{MaxFetchWait: time.Minute})
	mustCreate(t, c, "events", 1)

	got := fetchAsync(c, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 600_000})
	time.Sleep(100 * time.Millisecond)
	srv.Shutdown()
	srv.Shutdown() // calling it twice is harmless
	o := await(t, got, 5*time.Second)
	if o.err != nil || len(o.resp.Messages) != 0 {
		t.Fatalf("outcome = %+v, want an empty answer", o)
	}
}

func TestConcurrentProducersAndWaitingReaders(t *testing.T) {
	c, _ := startWaitServer(t, Options{MaxFetchWait: 5 * time.Second})
	mustCreate(t, c, "events", 1)

	const total = 60
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var part uint32
		for i := 0; i < total; i++ {
			if _, err := c.Produce(context.Background(), &pb.ProduceRequest{
				Topic: "events", Partition: &part, Messages: []*pb.Message{{Value: []byte("m")}},
			}); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// One reader that only ever waits: it must get every message, none late.
	read := uint64(0)
	deadline := time.Now().Add(20 * time.Second)
	for read < total {
		if time.Now().After(deadline) {
			t.Fatalf("read %d of %d messages", read, total)
		}
		resp, err := c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", Offset: read, MaxMessages: 5, MaxWaitMs: 5_000})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range resp.Messages {
			if m.Offset != read {
				t.Fatalf("got offset %d, want %d", m.Offset, read)
			}
			read++
		}
	}
	wg.Wait()
}

// lateMessageBackend is a partition where a message arrives in the gap between
// a read that found nothing and the wait that follows it.
type lateMessageBackend struct {
	mu      sync.Mutex
	wake    chan struct{}
	msgs    []storage.Message
	fetches int
}

func (b *lateMessageBackend) Watch(string, int) (<-chan struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wake, nil
}

func (b *lateMessageBackend) Fetch(string, int, uint64, int) ([]storage.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fetches++
	if b.fetches == 1 {
		// This read finds nothing; right after it, a message is written.
		b.msgs = []storage.Message{{Offset: 0, Value: []byte("in the gap")}}
		close(b.wake)
		b.wake = make(chan struct{})
		return nil, nil
	}
	return b.msgs, nil
}

func (*lateMessageBackend) CreateTopic(context.Context, string, int) error { return nil }
func (*lateMessageBackend) Produce(context.Context, string, int, []storage.Message) (int, uint64, error) {
	return 0, 0, nil
}
func (*lateMessageBackend) CommitOffset(context.Context, string, string, int, uint64) error {
	return nil
}
func (*lateMessageBackend) GroupOffset(string, string, int) (broker.OffsetInfo, error) {
	return broker.OffsetInfo{}, nil
}

func TestAMessageArrivingBetweenTheReadAndTheWaitIsNotMissed(t *testing.T) {
	b := &lateMessageBackend{wake: make(chan struct{})}
	srv, err := New(b, Options{MaxFetchMessages: 5, MaxFetchWait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := srv.Fetch(cctx, &pb.FetchRequest{Topic: "t", MaxMessages: 5, MaxWaitMs: 60_000})
	if err != nil || len(resp.Messages) != 1 {
		t.Fatalf("Fetch = %v, %v: the message written in the gap was only noticed after the whole wait", resp, err)
	}
}

func TestNewRefusesANegativeWait(t *testing.T) {
	if _, err := New(nil, Options{MaxFetchMessages: 1, MaxFetchWait: -time.Second}); err == nil {
		t.Fatal("accepted a negative MaxFetchWait")
	}
}
