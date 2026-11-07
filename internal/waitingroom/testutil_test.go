package waitingroom

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

// startBroker runs a real standalone broker with a real gRPC server and returns
// its address.
func startBroker(t *testing.T) string {
	t.Helper()
	b, err := broker.Open(filepath.Join(t.TempDir(), "data"), broker.Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	srv, err := server.New(server.Local(b), server.Options{MaxFetchMessages: 1000, MaxFetchWait: time.Minute})
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
	t.Cleanup(func() {
		srv.Shutdown()
		gs.Stop()
	})
	return lis.Addr().String()
}

func newClient(t *testing.T, addr string) *client.Client {
	t.Helper()
	c, err := client.New([]string{addr}, client.WithRetryPause(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func testConfig() Config {
	return Config{
		QueueTopic: DefaultQueueTopic, GateTopic: DefaultGateTopic,
		PollWait: 5 * time.Second, RetryPause: 10 * time.Millisecond,
		FetchLimit: 100, RequestTimeout: 10 * time.Second,
	}
}

// startRoom starts a room on a log and runs it until the test ends.
func startRoom(t *testing.T, log Log, cfg Config) *Room {
	t.Helper()
	r, err := New(log, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(runCtx); close(done) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the room did not stop")
		}
	})
	return r
}

func bg(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// flakyLog fails requests on demand, as a cluster without a leader would.
type flakyLog struct {
	Log
	failReads  atomic.Bool
	failWrites atomic.Bool
}

var errDown = errors.New("log is down")

func (f *flakyLog) Fetch(ctx context.Context, topic string, p int, off uint64, limit int, opts ...client.FetchOption) ([]client.Record, error) {
	if f.failReads.Load() {
		return nil, errDown
	}
	recs, err := f.Log.Fetch(ctx, topic, p, off, limit, opts...)
	if f.failReads.Load() {
		return nil, errDown // the outage began while the read was waiting: its answer is lost
	}
	return recs, err
}

func (f *flakyLog) Produce(ctx context.Context, topic string, msgs []client.Message, opts ...client.ProduceOption) (client.ProduceResult, error) {
	if f.failWrites.Load() {
		return client.ProduceResult{}, errDown
	}
	return f.Log.Produce(ctx, topic, msgs, opts...)
}

// eventually polls cond until it holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
