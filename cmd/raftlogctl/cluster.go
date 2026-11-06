package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

// clusterClient is a pb.BrokerClient for a cluster of nodes. A request goes to
// the first address; if that node is not the leader it names the leader and the
// request is retried there, and if a node cannot be reached the next address is
// tried. The commands of raftlogctl use it like a client of a single broker.
//
// After a leader dies, the other nodes keep naming it until a new election has
// finished, which takes a second or two. The client therefore keeps trying,
// pausing between rounds, until the request's own deadline passes.
//
// A request that fails after it was sent (for example because the leader died)
// is retried, so a message can end up in the log twice: delivery is
// at-least-once.
type clusterClient struct {
	addrs   []string
	retries int           // attempts per request; 0 means until the deadline
	pause   time.Duration // wait before trying again when nobody can answer

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
	down  map[string]time.Time // addresses that recently could not be reached
}

var _ pb.BrokerClient = (*clusterClient)(nil)

func newClusterClient(addrs []string, retries int, pause time.Duration) *clusterClient {
	return &clusterClient{
		addrs:   addrs,
		retries: retries,
		pause:   pause,
		conns:   make(map[string]*grpc.ClientConn),
		down:    make(map[string]time.Time),
	}
}

func (c *clusterClient) conn(addr string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn, ok := c.conns[addr]; ok {
		return conn, nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	c.conns[addr] = conn
	return conn, nil
}

// Close closes every connection.
func (c *clusterClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.conns {
		_ = conn.Close()
	}
	c.conns = nil
}

// downFor is how long an address that could not be reached is avoided.
func (c *clusterClient) downFor() time.Duration { return 5 * c.pause }

func (c *clusterClient) markDown(addr string) {
	c.mu.Lock()
	c.down[addr] = time.Now()
	c.mu.Unlock()
}

func (c *clusterClient) recentlyDown(addr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.down[addr]
	return ok && time.Since(t) < c.downFor()
}

// wait pauses, or returns early when ctx ends.
func (c *clusterClient) wait(ctx context.Context) {
	t := time.NewTimer(c.pause)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// do runs f against the cluster until it succeeds, fails for a reason that
// another node cannot fix, or runs out of attempts or time.
func (c *clusterClient) do(ctx context.Context, f func(pb.BrokerClient, ...grpc.CallOption) error) error {
	next := 0 // index of the address to ask after a failure
	nextAddr := func() string {
		a := c.addrs[next%len(c.addrs)]
		next++
		return a
	}
	current := nextAddr()
	var lastErr error

	for attempt := 0; c.retries == 0 || attempt < c.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}

		conn, err := c.conn(current)
		if err != nil {
			return err
		}
		var trailer metadata.MD
		err = f(pb.NewBrokerClient(conn), grpc.Trailer(&trailer))
		if err == nil {
			return nil
		}
		lastErr = err

		switch status.Code(err) {
		case codes.FailedPrecondition:
			// Not the leader. Go straight to the leader the node names, unless
			// that one was just found unreachable: then the node is out of date
			// and an election is under way, so wait for it.
			if hint := trailer.Get(server.LeaderAddrKey); len(hint) == 1 && hint[0] != "" && !c.recentlyDown(hint[0]) {
				current = hint[0]
				continue
			}
			c.wait(ctx)
			current = nextAddr()
		case codes.Unavailable:
			// The node is down, or it lost leadership while handling the request.
			c.markDown(current)
			current = nextAddr()
			if c.recentlyDown(current) {
				c.wait(ctx)
			}
		default:
			return err
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", c.retries, lastErr)
}

func (c *clusterClient) CreateTopic(ctx context.Context, in *pb.CreateTopicRequest, opts ...grpc.CallOption) (*pb.CreateTopicResponse, error) {
	var out *pb.CreateTopicResponse
	err := c.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.CreateTopic(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

func (c *clusterClient) Produce(ctx context.Context, in *pb.ProduceRequest, opts ...grpc.CallOption) (*pb.ProduceResponse, error) {
	var out *pb.ProduceResponse
	err := c.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.Produce(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

func (c *clusterClient) Fetch(ctx context.Context, in *pb.FetchRequest, opts ...grpc.CallOption) (*pb.FetchResponse, error) {
	var out *pb.FetchResponse
	err := c.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.Fetch(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

func (c *clusterClient) CommitOffset(ctx context.Context, in *pb.CommitOffsetRequest, opts ...grpc.CallOption) (*pb.CommitOffsetResponse, error) {
	var out *pb.CommitOffsetResponse
	err := c.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.CommitOffset(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

func (c *clusterClient) FetchOffset(ctx context.Context, in *pb.FetchOffsetRequest, opts ...grpc.CallOption) (*pb.FetchOffsetResponse, error) {
	var out *pb.FetchOffsetResponse
	err := c.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.FetchOffset(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

// NodeStatus asks the first address only: the question is about that node, so
// it must not be moved to another one.
func (c *clusterClient) NodeStatus(ctx context.Context, in *pb.NodeStatusRequest, opts ...grpc.CallOption) (*pb.NodeStatusResponse, error) {
	conn, err := c.conn(c.addrs[0])
	if err != nil {
		return nil, err
	}
	return pb.NewBrokerClient(conn).NodeStatus(ctx, in, opts...)
}

var errNoAddress = errors.New("no broker address given")

// splitAddrs turns "a:1, b:2" into its addresses.
func splitAddrs(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, errNoAddress
	}
	return out, nil
}
