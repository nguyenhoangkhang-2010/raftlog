package server

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// Trailer keys that tell a client where the leader is. They accompany a
// FailedPrecondition error from a node that is not the leader.
const (
	LeaderIDKey   = pb.LeaderIDKey
	LeaderAddrKey = pb.LeaderAddrKey
)

// Backend is what the gRPC handlers need from the broker. A standalone node
// uses Local; a node of a Raft cluster uses *broker.Replicated, which sends
// every change through the replicated log.
type Backend interface {
	CreateTopic(ctx context.Context, name string, partitions int) error
	// Produce returns the partition used and the offset of the first message.
	Produce(ctx context.Context, topic string, partition int, msgs []storage.Message) (int, uint64, error)
	CommitOffset(ctx context.Context, group, topic string, partition int, offset uint64) error
	Fetch(topic string, partition int, offset uint64, limit int) ([]storage.Message, error)
	// Watch returns a channel closed when the partition may have new data; call
	// it before Fetch to wait for messages without missing one.
	Watch(topic string, partition int) (<-chan struct{}, error)
	GroupOffset(group, topic string, partition int) (broker.OffsetInfo, error)
}

var _ Backend = (*broker.Replicated)(nil)

// Describer is what a backend offers to answer ListTopics and ListGroups. The two
// real backends have it; a backend without it makes those calls Unimplemented.
type Describer interface {
	Describe() []broker.TopicDescription
	Groups() []broker.GroupDescription
}

var _ Describer = (*broker.Replicated)(nil)

// Local returns a Backend that applies everything directly to b, for a node
// that is not part of a cluster.
func Local(b *broker.Broker) Backend { return local{b} }

type local struct{ b *broker.Broker }

func (l local) CreateTopic(_ context.Context, name string, partitions int) error {
	return l.b.CreateTopic(name, partitions)
}

func (l local) Produce(_ context.Context, topic string, partition int, msgs []storage.Message) (int, uint64, error) {
	return l.b.Produce(topic, partition, msgs)
}

func (l local) CommitOffset(_ context.Context, group, topic string, partition int, offset uint64) error {
	return l.b.CommitOffset(group, topic, partition, offset)
}

func (l local) Fetch(topic string, partition int, offset uint64, limit int) ([]storage.Message, error) {
	return l.b.Fetch(topic, partition, offset, limit)
}

func (l local) Watch(topic string, partition int) (<-chan struct{}, error) {
	return l.b.Watch(topic, partition)
}

func (l local) GroupOffset(group, topic string, partition int) (broker.OffsetInfo, error) {
	return l.b.GroupOffset(group, topic, partition)
}

func (l local) Describe() []broker.TopicDescription { return l.b.Describe() }

func (l local) Groups() []broker.GroupDescription { return l.b.Groups() }

// Options configures the gRPC handlers.
type Options struct {
	// MaxFetchMessages caps the number of messages one Fetch may return.
	// It must be at least 1.
	MaxFetchMessages int
	// MaxFetchWait caps how long one Fetch may wait for new messages (long
	// poll). 0 turns waiting off: a Fetch then always answers at once.
	MaxFetchWait time.Duration
	// Raft reports the Raft state of this node. Optional: nil means the node
	// runs standalone.
	Raft RaftStatus
	// LeaderAddr turns a node ID into the address clients can reach it at, so a
	// node that is not the leader can tell clients where to go. Optional.
	LeaderAddr func(raft.NodeID) string
	// Observer is told about the work the handlers do, so that it can be counted.
	// Optional.
	Observer Observer
}

// Observer receives what the handlers do. Implementations must not block.
type Observer interface {
	// Produced is called after a batch was stored.
	Produced(messages, bytes int)
	// Fetched is called after a fetch that returned messages.
	Fetched(messages int)
	// FetchWaiting is called with +1 when a fetch starts waiting for messages and
	// -1 when it stops.
	FetchWaiting(delta int)
}

// RaftStatus is implemented by raft.Runner.
type RaftStatus interface {
	Status() raft.Status
}

// Server implements the Broker gRPC service on top of a Backend.
type Server struct {
	pb.UnimplementedBrokerServer
	backend Backend
	opts    Options

	stopping chan struct{} // closed by Shutdown
	stopOnce sync.Once
}

// Shutdown makes every Fetch that is waiting for messages answer at once. Call
// it before the gRPC server's GracefulStop, which otherwise waits for them.
func (s *Server) Shutdown() { s.stopOnce.Do(func() { close(s.stopping) }) }

// New returns a Server backed by b.
func New(b Backend, opts Options) (*Server, error) {
	if opts.MaxFetchMessages < 1 {
		return nil, errors.New("server: MaxFetchMessages must be at least 1")
	}
	if opts.MaxFetchWait < 0 {
		return nil, errors.New("server: MaxFetchWait must not be negative")
	}
	return &Server{backend: b, opts: opts, stopping: make(chan struct{})}, nil
}

// Register adds the service to a gRPC server.
func Register(g *grpc.Server, s *Server) {
	pb.RegisterBrokerServer(g, s)
}

// CreateTopic creates a topic.
func (s *Server) CreateTopic(ctx context.Context, req *pb.CreateTopicRequest) (*pb.CreateTopicResponse, error) {
	if err := s.backend.CreateTopic(ctx, req.GetTopic(), int(req.GetPartitions())); err != nil {
		return nil, s.fail(ctx, err)
	}
	return &pb.CreateTopicResponse{}, nil
}

// Produce appends the messages of the request to one partition.
func (s *Server) Produce(ctx context.Context, req *pb.ProduceRequest) (*pb.ProduceResponse, error) {
	msgs := make([]storage.Message, len(req.GetMessages()))
	for i, m := range req.GetMessages() {
		msgs[i] = storage.Message{
			Timestamp: m.GetTimestampMs(),
			Key:       m.GetKey(),
			Value:     m.GetValue(),
		}
	}

	partition := broker.AnyPartition
	if req.Partition != nil {
		partition = int(req.GetPartition())
	}
	part, base, err := s.backend.Produce(ctx, req.GetTopic(), partition, msgs)
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	if s.opts.Observer != nil {
		bytes := 0
		for _, m := range msgs {
			bytes += len(m.Key) + len(m.Value)
		}
		s.opts.Observer.Produced(len(msgs), bytes)
	}
	return &pb.ProduceResponse{Partition: uint32(part), BaseOffset: base}, nil
}

// Fetch reads messages from one partition, starting at an offset. When there is
// nothing to read and the request allows it, it waits for a message to arrive.
func (s *Server) Fetch(ctx context.Context, req *pb.FetchRequest) (*pb.FetchResponse, error) {
	if req.GetMaxMessages() == 0 {
		return nil, status.Error(codes.InvalidArgument, "max_messages must be greater than 0")
	}
	limit := s.opts.MaxFetchMessages
	if n := int(req.GetMaxMessages()); n < limit {
		limit = n
	}
	wait := time.Duration(req.GetMaxWaitMs()) * time.Millisecond
	if wait > s.opts.MaxFetchWait {
		wait = s.opts.MaxFetchWait
	}

	var deadline <-chan time.Time
	var msgs []storage.Message
	for {
		// Subscribe before reading: a message that arrives between the read and
		// the wait would otherwise be missed until the next one.
		var wake <-chan struct{}
		if wait > 0 {
			var err error
			if wake, err = s.backend.Watch(req.GetTopic(), int(req.GetPartition())); err != nil {
				return nil, s.fail(ctx, err)
			}
		}
		var err error
		msgs, err = s.backend.Fetch(req.GetTopic(), int(req.GetPartition()), req.GetOffset(), limit)
		if err != nil {
			return nil, s.fail(ctx, err)
		}
		if len(msgs) > 0 || wait <= 0 {
			break
		}
		if deadline == nil {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			deadline = timer.C
			if s.opts.Observer != nil {
				s.opts.Observer.FetchWaiting(1)
				defer s.opts.Observer.FetchWaiting(-1)
			}
		}
		select {
		case <-wake:
			continue
		case <-deadline:
		case <-s.stopping:
		case <-ctx.Done():
			return nil, s.fail(ctx, ctx.Err())
		}
		break
	}

	out := make([]*pb.Message, len(msgs))
	for i, m := range msgs {
		out[i] = &pb.Message{
			Offset:      m.Offset,
			TimestampMs: m.Timestamp,
			Key:         m.Key,
			Value:       m.Value,
		}
	}
	if s.opts.Observer != nil && len(out) > 0 {
		s.opts.Observer.Fetched(len(out))
	}
	return &pb.FetchResponse{Messages: out}, nil
}

// CommitOffset records the position of a consumer group in a partition.
func (s *Server) CommitOffset(ctx context.Context, req *pb.CommitOffsetRequest) (*pb.CommitOffsetResponse, error) {
	if err := s.backend.CommitOffset(ctx, req.GetGroup(), req.GetTopic(), int(req.GetPartition()), req.GetOffset()); err != nil {
		return nil, s.fail(ctx, err)
	}
	return &pb.CommitOffsetResponse{}, nil
}

// FetchOffset returns the committed offset of a consumer group and its lag.
func (s *Server) FetchOffset(ctx context.Context, req *pb.FetchOffsetRequest) (*pb.FetchOffsetResponse, error) {
	info, err := s.backend.GroupOffset(req.GetGroup(), req.GetTopic(), int(req.GetPartition()))
	if err != nil {
		return nil, s.fail(ctx, err)
	}
	return &pb.FetchOffsetResponse{
		Committed: info.Committed,
		Offset:    info.Offset,
		EndOffset: info.End,
		Lag:       info.Lag(),
	}, nil
}

// ListTopics describes every topic of this node.
func (s *Server) ListTopics(context.Context, *pb.ListTopicsRequest) (*pb.ListTopicsResponse, error) {
	d, ok := s.backend.(Describer)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "this backend cannot list topics")
	}
	topics := d.Describe()
	out := &pb.ListTopicsResponse{Topics: make([]*pb.TopicDescription, len(topics))}
	for i, t := range topics {
		out.Topics[i] = &pb.TopicDescription{Name: t.Name, EndOffsets: t.Ends}
	}
	return out, nil
}

// ListGroups describes every consumer group of this node that has committed an offset.
func (s *Server) ListGroups(context.Context, *pb.ListGroupsRequest) (*pb.ListGroupsResponse, error) {
	d, ok := s.backend.(Describer)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "this backend cannot list groups")
	}
	groups := d.Groups()
	out := &pb.ListGroupsResponse{Groups: make([]*pb.GroupDescription, len(groups))}
	for i, g := range groups {
		gd := &pb.GroupDescription{Group: g.Group, Partitions: make([]*pb.GroupPartitionInfo, len(g.Partitions))}
		for j, p := range g.Partitions {
			gd.Partitions[j] = &pb.GroupPartitionInfo{
				Topic: p.Topic, Partition: uint32(p.Partition), Offset: p.Offset, EndOffset: p.End, Lag: p.Lag(),
			}
		}
		out.Groups[i] = gd
	}
	return out, nil
}

// NodeStatus reports the Raft role of this node.
func (s *Server) NodeStatus(context.Context, *pb.NodeStatusRequest) (*pb.NodeStatusResponse, error) {
	if s.opts.Raft == nil {
		return &pb.NodeStatusResponse{RaftEnabled: false}, nil
	}
	st := s.opts.Raft.Status()
	return &pb.NodeStatusResponse{
		RaftEnabled: true,
		NodeId:      uint64(st.ID),
		Role:        st.Role.String(),
		Term:        st.Term,
		LeaderId:    uint64(st.Leader),
		CommitIndex: st.CommitIndex,
		LastIndex:   st.LastIndex,

		SnapshotIndex: st.SnapshotIndex,
	}, nil
}

// fail turns an error into a gRPC status. For a node that is not the leader it
// also sends the leader's identity and address as trailers, so that the client
// can retry at the right place.
func (s *Server) fail(ctx context.Context, err error) error {
	var notLeader *raft.NotLeaderError
	if errors.As(err, &notLeader) {
		md := metadata.MD{}
		if notLeader.Leader != raft.None {
			md.Set(LeaderIDKey, strconv.FormatUint(uint64(notLeader.Leader), 10))
			if s.opts.LeaderAddr != nil {
				if addr := s.opts.LeaderAddr(notLeader.Leader); addr != "" {
					md.Set(LeaderAddrKey, addr)
				}
			}
		}
		_ = grpc.SetTrailer(ctx, md)
	}
	return toStatus(err)
}

// toStatus maps broker, storage and raft errors to gRPC status codes.
func toStatus(err error) error {
	switch {
	case errors.Is(err, raft.ErrNotLeader):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, raft.ErrLeadershipLost), errors.Is(err, raft.ErrEntryReplaced), errors.Is(err, raft.ErrStopped):
		// The command may or may not have been applied: the client may retry,
		// and must expect that a retry can duplicate a message.
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, broker.ErrTopicNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, broker.ErrTopicExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, broker.ErrInvalidTopicName),
		errors.Is(err, broker.ErrInvalidPartitions),
		errors.Is(err, broker.ErrInvalidPartition),
		errors.Is(err, broker.ErrEmptyBatch),
		errors.Is(err, broker.ErrInvalidGroup),
		errors.Is(err, storage.ErrRecordTooLarge):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, storage.ErrOffsetOutOfRange), errors.Is(err, broker.ErrOffsetBeyondEnd):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, broker.ErrClosed), errors.Is(err, storage.ErrClosed):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
