package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

const defaultRetryPause = 100 * time.Millisecond

type config struct {
	maxAttempts int
	pause       time.Duration
	dialOptions []grpc.DialOption
}

// Option customizes a Router or a Client.
type Option func(*config)

// WithMaxAttempts limits how many times one request is tried, redirects to the
// leader included. Zero, the default, keeps trying until the request's context
// ends, which is what a client needs while a new leader is being elected.
func WithMaxAttempts(n int) Option { return func(c *config) { c.maxAttempts = n } }

// WithRetryPause sets how long to wait before trying again when no node can
// answer yet (default 100 ms). A node that could not be reached is also avoided
// for five times this long.
func WithRetryPause(d time.Duration) Option { return func(c *config) { c.pause = d } }

// WithDialOptions adds gRPC dial options, for example transport credentials.
// They come after the defaults, so they win.
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *config) { c.dialOptions = append(c.dialOptions, opts...) }
}

func buildConfig(opts []Option) (config, error) {
	cfg := config{pause: defaultRetryPause}
	for _, o := range opts {
		o(&cfg)
	}
	switch {
	case cfg.maxAttempts < 0:
		return cfg, errors.New("client: the number of attempts must not be negative")
	case cfg.pause <= 0:
		return cfg, errors.New("client: the retry pause must be positive")
	}
	return cfg, nil
}

// Router is a pb.BrokerClient for a whole cluster. A request goes to the first
// address; if that node is not the leader it names the leader and the request is
// retried there, and if a node cannot be reached the next address is tried.
//
// After a leader dies, the other nodes keep naming it until a new election has
// finished, which takes a second or two. The Router therefore keeps trying,
// pausing between rounds, until the request's context ends.
//
// A request that fails after it was sent (for example because the leader died)
// is retried, so a message can end up in the log twice: delivery is
// at-least-once. Make writes idempotent if that matters.
//
// Most programs want Client, which is easier to use. Router gives the raw
// protobuf API, which the command-line tool uses.
type Router struct {
	addrs []string
	cfg   config

	mu     sync.Mutex
	closed bool
	conns  map[string]*grpc.ClientConn
	down   map[string]time.Time // addresses that recently could not be reached
}

var _ pb.BrokerClient = (*Router)(nil)

// NewRouter returns a Router for the nodes at addrs (host:port). Nothing is
// connected until the first request.
func NewRouter(addrs []string, opts ...Option) (*Router, error) {
	cfg, err := buildConfig(opts)
	if err != nil {
		return nil, err
	}
	var clean []string
	for _, a := range addrs {
		if a = strings.TrimSpace(a); a != "" {
			clean = append(clean, a)
		}
	}
	if len(clean) == 0 {
		return nil, errors.New("client: at least one node address is required")
	}
	return &Router{addrs: clean, cfg: cfg, conns: make(map[string]*grpc.ClientConn), down: make(map[string]time.Time)}, nil
}

// Addrs returns the addresses the Router was created with.
func (r *Router) Addrs() []string { return append([]string(nil), r.addrs...) }

// ErrClosed is returned by a Router or Client that was closed.
var ErrClosed = errors.New("client: closed")

func (r *Router) conn(addr string) (*grpc.ClientConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if c, ok := r.conns[addr]; ok {
		return c, nil
	}
	// Reconnect quickly: gRPC's default backoff can keep a restarted node out for
	// a long time, and a cluster client wants to find it again.
	dial := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 50 * time.Millisecond, Multiplier: 1.5, Jitter: 0.2, MaxDelay: time.Second},
			MinConnectTimeout: 2 * time.Second,
		}),
	}, r.cfg.dialOptions...)
	c, err := grpc.NewClient(addr, dial...)
	if err != nil {
		return nil, fmt.Errorf("client: connect to %s: %w", addr, err)
	}
	r.conns[addr] = c
	return c, nil
}

// Close closes every connection. Requests after Close fail with ErrClosed.
func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	for _, c := range r.conns {
		errs = append(errs, c.Close())
	}
	r.conns = nil
	return errors.Join(errs...)
}

func (r *Router) markDown(addr string) {
	r.mu.Lock()
	r.down[addr] = time.Now()
	r.mu.Unlock()
}

func (r *Router) recentlyDown(addr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.down[addr]
	return ok && time.Since(t) < 5*r.cfg.pause
}

// wait pauses, or returns early when ctx ends.
func (r *Router) wait(ctx context.Context) {
	t := time.NewTimer(r.cfg.pause)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// do runs f against the cluster until it succeeds, fails for a reason that
// another node cannot fix, or runs out of attempts or time.
func (r *Router) do(ctx context.Context, f func(pb.BrokerClient, ...grpc.CallOption) error) error {
	next := 0 // index of the address to ask after a failure
	nextAddr := func() string {
		a := r.addrs[next%len(r.addrs)]
		next++
		return a
	}
	current := nextAddr()
	var lastErr error

	for attempt := 0; r.cfg.maxAttempts == 0 || attempt < r.cfg.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}

		conn, err := r.conn(current)
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
			if hint := trailer.Get(pb.LeaderAddrKey); len(hint) == 1 && hint[0] != "" && !r.recentlyDown(hint[0]) {
				current = hint[0]
				continue
			}
			r.wait(ctx)
			current = nextAddr()
		case codes.Unavailable:
			// The node is down, or it lost leadership while handling the request.
			r.markDown(current)
			current = nextAddr()
			if r.recentlyDown(current) {
				r.wait(ctx)
			}
		default:
			return err
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", r.cfg.maxAttempts, lastErr)
}

// CreateTopic implements pb.BrokerClient.
func (r *Router) CreateTopic(ctx context.Context, in *pb.CreateTopicRequest, opts ...grpc.CallOption) (*pb.CreateTopicResponse, error) {
	var out *pb.CreateTopicResponse
	err := r.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.CreateTopic(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

// Produce implements pb.BrokerClient.
func (r *Router) Produce(ctx context.Context, in *pb.ProduceRequest, opts ...grpc.CallOption) (*pb.ProduceResponse, error) {
	var out *pb.ProduceResponse
	err := r.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.Produce(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

// Fetch implements pb.BrokerClient.
func (r *Router) Fetch(ctx context.Context, in *pb.FetchRequest, opts ...grpc.CallOption) (*pb.FetchResponse, error) {
	var out *pb.FetchResponse
	err := r.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.Fetch(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

// CommitOffset implements pb.BrokerClient.
func (r *Router) CommitOffset(ctx context.Context, in *pb.CommitOffsetRequest, opts ...grpc.CallOption) (*pb.CommitOffsetResponse, error) {
	var out *pb.CommitOffsetResponse
	err := r.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.CommitOffset(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

// FetchOffset implements pb.BrokerClient.
func (r *Router) FetchOffset(ctx context.Context, in *pb.FetchOffsetRequest, opts ...grpc.CallOption) (*pb.FetchOffsetResponse, error) {
	var out *pb.FetchOffsetResponse
	err := r.do(ctx, func(cl pb.BrokerClient, o ...grpc.CallOption) (err error) {
		out, err = cl.FetchOffset(ctx, in, append(opts, o...)...)
		return err
	})
	return out, err
}

// NodeStatus asks the first address only: the question is about that node, so
// it must not be moved to another one. Use NodeStatusAt for another node.
func (r *Router) NodeStatus(ctx context.Context, in *pb.NodeStatusRequest, opts ...grpc.CallOption) (*pb.NodeStatusResponse, error) {
	return r.NodeStatusAt(ctx, r.addrs[0], in, opts...)
}

// NodeStatusAt asks one particular node, and no other, for its status.
func (r *Router) NodeStatusAt(ctx context.Context, addr string, in *pb.NodeStatusRequest, opts ...grpc.CallOption) (*pb.NodeStatusResponse, error) {
	conn, err := r.conn(addr)
	if err != nil {
		return nil, err
	}
	return pb.NewBrokerClient(conn).NodeStatus(ctx, in, opts...)
}
