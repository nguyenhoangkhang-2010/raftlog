package server

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// Options configures the gRPC handlers.
type Options struct {
	// MaxFetchMessages caps the number of messages one Fetch may return.
	// It must be at least 1.
	MaxFetchMessages int
}

// Server implements the Broker gRPC service on top of a broker.Broker.
type Server struct {
	pb.UnimplementedBrokerServer
	broker *broker.Broker
	opts   Options
}

// New returns a Server backed by b.
func New(b *broker.Broker, opts Options) (*Server, error) {
	if opts.MaxFetchMessages < 1 {
		return nil, errors.New("server: MaxFetchMessages must be at least 1")
	}
	return &Server{broker: b, opts: opts}, nil
}

// Register adds the service to a gRPC server.
func Register(g *grpc.Server, s *Server) {
	pb.RegisterBrokerServer(g, s)
}

// CreateTopic creates a topic.
func (s *Server) CreateTopic(_ context.Context, req *pb.CreateTopicRequest) (*pb.CreateTopicResponse, error) {
	if err := s.broker.CreateTopic(req.GetTopic(), int(req.GetPartitions())); err != nil {
		return nil, toStatus(err)
	}
	return &pb.CreateTopicResponse{}, nil
}

// Produce appends the messages of the request to one partition.
func (s *Server) Produce(_ context.Context, req *pb.ProduceRequest) (*pb.ProduceResponse, error) {
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
	part, base, err := s.broker.Produce(req.GetTopic(), partition, msgs)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.ProduceResponse{Partition: uint32(part), BaseOffset: base}, nil
}

// Fetch reads messages from one partition, starting at an offset.
func (s *Server) Fetch(_ context.Context, req *pb.FetchRequest) (*pb.FetchResponse, error) {
	if req.GetMaxMessages() == 0 {
		return nil, status.Error(codes.InvalidArgument, "max_messages must be greater than 0")
	}
	limit := s.opts.MaxFetchMessages
	if n := int(req.GetMaxMessages()); n < limit {
		limit = n
	}

	msgs, err := s.broker.Fetch(req.GetTopic(), int(req.GetPartition()), req.GetOffset(), limit)
	if err != nil {
		return nil, toStatus(err)
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
	return &pb.FetchResponse{Messages: out}, nil
}

// CommitOffset records the position of a consumer group in a partition.
func (s *Server) CommitOffset(_ context.Context, req *pb.CommitOffsetRequest) (*pb.CommitOffsetResponse, error) {
	if err := s.broker.CommitOffset(req.GetGroup(), req.GetTopic(), int(req.GetPartition()), req.GetOffset()); err != nil {
		return nil, toStatus(err)
	}
	return &pb.CommitOffsetResponse{}, nil
}

// FetchOffset returns the committed offset of a consumer group and its lag.
func (s *Server) FetchOffset(_ context.Context, req *pb.FetchOffsetRequest) (*pb.FetchOffsetResponse, error) {
	info, err := s.broker.GroupOffset(req.GetGroup(), req.GetTopic(), int(req.GetPartition()))
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.FetchOffsetResponse{
		Committed: info.Committed,
		Offset:    info.Offset,
		EndOffset: info.End,
		Lag:       info.Lag(),
	}, nil
}

// toStatus maps broker and storage errors to gRPC status codes.
func toStatus(err error) error {
	switch {
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
