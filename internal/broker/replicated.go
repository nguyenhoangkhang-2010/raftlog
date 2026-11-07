package broker

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// Replicated is the broker of a Raft cluster. Every change (topic creation,
// produce, offset commit) goes through the Raft log: it is accepted only by the
// leader, and it returns once a majority of nodes has stored it and this node
// has applied it. Reads are served from this node's applied state.
//
// What a reader sees: only data that is committed. A follower may be a little
// behind the leader. A leader that was just deposed and does not know it yet
// may also serve stale data; reads do not go through the log.
type Replicated struct {
	m      *Machine
	runner *raft.Runner
	now    func() time.Time
}

// NewReplicated returns the replicated broker for a machine and the runner that
// feeds it. The runner must have been created with raft.WithStateMachine(m).
func NewReplicated(m *Machine, runner *raft.Runner) *Replicated {
	return &Replicated{m: m, runner: runner, now: time.Now}
}

// propose replicates a command and waits for its outcome.
func (s *Replicated) propose(ctx context.Context, cmd *pb.Command) (Result, error) {
	data, err := proto.Marshal(cmd)
	if err != nil {
		return Result{}, fmt.Errorf("encode command: %w", err)
	}
	_, v, err := s.runner.ProposeResult(ctx, data)
	if err != nil {
		return Result{}, err
	}
	res, ok := v.(Result)
	if !ok {
		return Result{}, fmt.Errorf("broker: the state machine answered with %T, not a Result", v)
	}
	return res, nil
}

// CreateTopic creates a topic on every node.
func (s *Replicated) CreateTopic(ctx context.Context, name string, partitions int) error {
	// Refuse what could never succeed before putting it in the log.
	if !validTopicName(name) {
		return ErrInvalidTopicName
	}
	if limit := s.m.b.opts.MaxPartitions; partitions < 1 || partitions > limit {
		return fmt.Errorf("%w: %d (allowed 1..%d)", ErrInvalidPartitions, partitions, limit)
	}
	res, err := s.propose(ctx, &pb.Command{Command: &pb.Command_CreateTopic{
		CreateTopic: &pb.CreateTopicCommand{Topic: name, Partitions: uint32(partitions)},
	}})
	if err != nil {
		return err
	}
	return res.Err
}

// Produce appends messages to one partition of a topic, on every node.
//
// The leader chooses the partition and fills in missing timestamps here, before
// the command is replicated, so that all nodes apply the very same command.
// If partition is AnyPartition it is chosen from the key of the first message.
func (s *Replicated) Produce(ctx context.Context, name string, partition int, msgs []storage.Message) (int, uint64, error) {
	if len(msgs) == 0 {
		return 0, 0, ErrEmptyBatch
	}
	for _, m := range msgs {
		if err := storage.ValidateMessage(m); err != nil {
			return 0, 0, err
		}
	}
	t, err := s.m.b.lookup(name)
	if err != nil {
		return 0, 0, err
	}
	idx := partition
	if idx == AnyPartition {
		idx = t.choose(msgs[0].Key)
	} else if idx < 0 || idx >= len(t.parts) {
		return 0, 0, ErrInvalidPartition
	}

	stamp := s.now().UnixMilli()
	cmd := &pb.ProduceCommand{Topic: name, Partition: uint32(idx), Messages: make([]*pb.StoredMessage, len(msgs))}
	for i, m := range msgs {
		ts := m.Timestamp
		if ts == 0 {
			ts = stamp
		}
		cmd.Messages[i] = &pb.StoredMessage{Key: m.Key, Value: m.Value, TimestampMs: ts}
	}

	res, err := s.propose(ctx, &pb.Command{Command: &pb.Command_Produce{Produce: cmd}})
	if err != nil {
		return 0, 0, err
	}
	if res.Err != nil {
		return 0, 0, res.Err
	}
	return res.Partition, res.BaseOffset, nil
}

// CommitOffset records the position of a consumer group, on every node.
func (s *Replicated) CommitOffset(ctx context.Context, group, name string, partition int, offset uint64) error {
	if !validGroupName(group) {
		return ErrInvalidGroup
	}
	if partition < 0 {
		return ErrInvalidPartition
	}
	res, err := s.propose(ctx, &pb.Command{Command: &pb.Command_CommitOffset{
		CommitOffset: &pb.CommitOffsetCommand{Group: group, Topic: name, Partition: uint32(partition), Offset: offset},
	}})
	if err != nil {
		return err
	}
	return res.Err
}

// Fetch reads messages from a partition of this node.
func (s *Replicated) Fetch(name string, partition int, offset uint64, limit int) ([]storage.Message, error) {
	return s.m.Fetch(name, partition, offset, limit)
}

// Watch returns a channel that is closed when new data may be readable on this
// node; see Broker.Watch.
func (s *Replicated) Watch(name string, partition int) (<-chan struct{}, error) {
	return s.m.Watch(name, partition)
}

// GroupOffset returns the position of a consumer group as this node knows it.
func (s *Replicated) GroupOffset(group, name string, partition int) (OffsetInfo, error) {
	return s.m.GroupOffset(group, name, partition)
}
