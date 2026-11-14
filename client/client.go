package client

import (
	"context"
	"fmt"
	"math"
	"time"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

// Client talks to a raftlog cluster. It follows the leader, skips nodes that are
// down and waits out elections; see Router for the details and for the
// at-least-once delivery that follows from retrying.
//
// A Client is safe for concurrent use.
type Client struct {
	r *Router
}

// New returns a Client for the nodes at addrs (host:port). Give it every node
// of the cluster so that it still works when some are down. Nothing is
// connected until the first request.
func New(addrs []string, opts ...Option) (*Client, error) {
	r, err := NewRouter(addrs, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{r: r}, nil
}

// Close closes the connections. Requests after Close fail with ErrClosed.
func (c *Client) Close() error { return c.r.Close() }

// Message is what a producer sends.
type Message struct {
	// Key decides the partition when none is given: messages with the same key
	// always go to the same partition, in order. It may be empty.
	Key   []byte
	Value []byte
	// Timestamp is optional; the cluster stamps the message if it is zero.
	Timestamp time.Time
}

// Record is a stored message, with the position it has in its partition.
type Record struct {
	Offset    uint64
	Timestamp time.Time
	Key       []byte
	Value     []byte
}

// ProduceResult says where a batch was stored.
type ProduceResult struct {
	// Partition is the partition that holds the batch.
	Partition int
	// Offset is the offset of the first message; the others follow it.
	Offset uint64
}

// GroupPosition is where a consumer group stands in one partition.
type GroupPosition struct {
	// Committed is false if the group has not committed an offset yet; Offset is
	// then 0 and the group should start from the beginning.
	Committed bool
	// Offset is the committed offset: the next offset the group will read.
	Offset uint64
	// End is the offset the next message will get.
	End uint64
	// Lag is End - Offset: how many messages the group has not read.
	Lag uint64
}

// NodeStatus is the state of one node.
type NodeStatus struct {
	// RaftEnabled is false for a node that runs on its own, outside a cluster.
	RaftEnabled bool
	NodeID      uint64
	// Role is "leader", "follower" or "candidate".
	Role     string
	Term     uint64
	LeaderID uint64
	// CommitIndex and LastIndex are positions in the node's Raft log, and
	// SnapshotIndex the last one folded into a snapshot.
	CommitIndex   uint64
	LastIndex     uint64
	SnapshotIndex uint64
}

// ProduceOption customizes Produce.
type ProduceOption func(*produceConfig)

type produceConfig struct {
	partition *int
}

// ToPartition stores the batch in that partition instead of letting the cluster
// choose one from the key of the first message.
func ToPartition(p int) ProduceOption { return func(c *produceConfig) { c.partition = &p } }

// FetchOption customizes Fetch.
type FetchOption func(*fetchConfig)

type fetchConfig struct {
	wait time.Duration
}

// WaitUpTo makes a Fetch that finds nothing to read wait up to d for a message
// to arrive, and return as soon as one does. It returns none if d passes first.
// The cluster may shorten the wait to its own limit, so a caller must be ready
// for an early empty answer and ask again. Keep ctx's deadline longer than d, or
// the call fails with the deadline error instead of returning none.
func WaitUpTo(d time.Duration) FetchOption { return func(c *fetchConfig) { c.wait = d } }

// CreateTopic creates a topic with the given number of partitions. It fails with
// ErrAlreadyExists if the topic is there.
func (c *Client) CreateTopic(ctx context.Context, topic string, partitions int) error {
	if partitions < 1 {
		return fmt.Errorf("%w: a topic needs at least one partition", ErrInvalidArgument)
	}
	_, err := c.r.CreateTopic(ctx, &pb.CreateTopicRequest{Topic: topic, Partitions: uint32(partitions)})
	return translate(err)
}

// Produce appends the messages to one partition of the topic and returns where
// they were stored. They are stored in order and next to each other, or not at
// all. Without ToPartition, the partition is chosen from the key of the first
// message (and spread round robin when that key is empty).
//
// If Produce fails after the request was sent, the batch may or may not have
// been stored, and the client may have sent it more than once: an error does
// not prove the messages are absent. Make consumers tolerate a duplicate.
func (c *Client) Produce(ctx context.Context, topic string, msgs []Message, opts ...ProduceOption) (ProduceResult, error) {
	if len(msgs) == 0 {
		return ProduceResult{}, fmt.Errorf("%w: no messages", ErrInvalidArgument)
	}
	var cfg produceConfig
	for _, o := range opts {
		o(&cfg)
	}

	req := &pb.ProduceRequest{Topic: topic, Messages: make([]*pb.Message, len(msgs))}
	for i, m := range msgs {
		pm := &pb.Message{Key: m.Key, Value: m.Value}
		if !m.Timestamp.IsZero() {
			pm.TimestampMs = m.Timestamp.UnixMilli()
		}
		req.Messages[i] = pm
	}
	if cfg.partition != nil {
		if *cfg.partition < 0 {
			return ProduceResult{}, fmt.Errorf("%w: negative partition", ErrInvalidArgument)
		}
		p := uint32(*cfg.partition)
		req.Partition = &p
	}

	resp, err := c.r.Produce(ctx, req)
	if err != nil {
		return ProduceResult{}, translate(err)
	}
	return ProduceResult{Partition: int(resp.GetPartition()), Offset: resp.GetBaseOffset()}, nil
}

// Fetch reads up to limit messages from a partition, starting at offset. It
// returns none if offset is the end of the partition, and ErrOffsetOutOfRange if
// it is beyond it. The cluster may return fewer than limit, or cap limit itself.
//
// Any node can answer a Fetch; it shows only data that is committed, but a node
// that is behind may not show the newest messages yet.
//
// With WaitUpTo, a Fetch at the end of the partition waits for a message to
// arrive instead of returning none at once.
func (c *Client) Fetch(ctx context.Context, topic string, partition int, offset uint64, limit int, opts ...FetchOption) ([]Record, error) {
	if limit < 1 || partition < 0 {
		return nil, fmt.Errorf("%w: the limit must be positive and the partition not negative", ErrInvalidArgument)
	}
	var cfg fetchConfig
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.wait < 0 {
		return nil, fmt.Errorf("%w: the wait must not be negative", ErrInvalidArgument)
	}
	wait := cfg.wait.Milliseconds()
	if wait > math.MaxUint32 {
		wait = math.MaxUint32
	}
	resp, err := c.r.Fetch(ctx, &pb.FetchRequest{
		Topic: topic, Partition: uint32(partition), Offset: offset, MaxMessages: uint32(limit),
		MaxWaitMs: uint32(wait),
	})
	if err != nil {
		return nil, translate(err)
	}
	out := make([]Record, len(resp.GetMessages()))
	for i, m := range resp.GetMessages() {
		out[i] = Record{
			Offset:    m.GetOffset(),
			Timestamp: time.UnixMilli(m.GetTimestampMs()),
			Key:       m.GetKey(),
			Value:     m.GetValue(),
		}
	}
	return out, nil
}

// CommitOffset records that the group has read everything before offset in the
// partition, so that it can continue from there. Going back to an older offset
// is allowed (it replays messages); an offset beyond the end is not.
func (c *Client) CommitOffset(ctx context.Context, group, topic string, partition int, offset uint64) error {
	if partition < 0 {
		return fmt.Errorf("%w: negative partition", ErrInvalidArgument)
	}
	_, err := c.r.CommitOffset(ctx, &pb.CommitOffsetRequest{
		Group: group, Topic: topic, Partition: uint32(partition), Offset: offset,
	})
	return translate(err)
}

// GroupPosition returns where a consumer group stands in a partition.
func (c *Client) GroupPosition(ctx context.Context, group, topic string, partition int) (GroupPosition, error) {
	if partition < 0 {
		return GroupPosition{}, fmt.Errorf("%w: negative partition", ErrInvalidArgument)
	}
	resp, err := c.r.FetchOffset(ctx, &pb.FetchOffsetRequest{Group: group, Topic: topic, Partition: uint32(partition)})
	if err != nil {
		return GroupPosition{}, translate(err)
	}
	return GroupPosition{
		Committed: resp.GetCommitted(), Offset: resp.GetOffset(), End: resp.GetEndOffset(), Lag: resp.GetLag(),
	}, nil
}

// NodeStatus asks the node at addr, and no other, for its status. This is for
// monitoring: it does not follow the leader or skip a node that is down.
func (c *Client) NodeStatus(ctx context.Context, addr string) (NodeStatus, error) {
	resp, err := c.r.NodeStatusAt(ctx, addr, &pb.NodeStatusRequest{})
	if err != nil {
		return NodeStatus{}, translate(err)
	}
	return NodeStatus{
		RaftEnabled:   resp.GetRaftEnabled(),
		NodeID:        resp.GetNodeId(),
		Role:          resp.GetRole(),
		Term:          resp.GetTerm(),
		LeaderID:      resp.GetLeaderId(),
		CommitIndex:   resp.GetCommitIndex(),
		LastIndex:     resp.GetLastIndex(),
		SnapshotIndex: resp.GetSnapshotIndex(),
	}, nil
}

// TopicInfo describes a topic.
type TopicInfo struct {
	Name string
	// Ends[i] is the offset the next message of partition i will get, which is
	// also how many messages the partition holds.
	Ends []uint64
}

// Partitions returns the number of partitions.
func (t TopicInfo) Partitions() int { return len(t.Ends) }

// GroupPartition is the position of a consumer group in one partition.
type GroupPartition struct {
	Topic     string
	Partition int
	Offset    uint64 // the next offset the group will read
	End       uint64
	Lag       uint64
}

// GroupInfo is a consumer group and everywhere it has committed an offset.
type GroupInfo struct {
	Name       string
	Partitions []GroupPartition
}

// Topics lists the topics, sorted by name, with the end of each partition. It is
// answered by one node, which may be a little behind the leader.
func (c *Client) Topics(ctx context.Context) ([]TopicInfo, error) {
	resp, err := c.r.ListTopics(ctx, &pb.ListTopicsRequest{})
	if err != nil {
		return nil, translate(err)
	}
	out := make([]TopicInfo, len(resp.GetTopics()))
	for i, t := range resp.GetTopics() {
		out[i] = TopicInfo{Name: t.GetName(), Ends: append([]uint64(nil), t.GetEndOffsets()...)}
	}
	return out, nil
}

// Groups lists the consumer groups that have committed an offset, sorted by name,
// with their position and lag in each partition. It is answered by one node.
func (c *Client) Groups(ctx context.Context) ([]GroupInfo, error) {
	resp, err := c.r.ListGroups(ctx, &pb.ListGroupsRequest{})
	if err != nil {
		return nil, translate(err)
	}
	out := make([]GroupInfo, len(resp.GetGroups()))
	for i, g := range resp.GetGroups() {
		gi := GroupInfo{Name: g.GetGroup(), Partitions: make([]GroupPartition, len(g.GetPartitions()))}
		for j, p := range g.GetPartitions() {
			gi.Partitions[j] = GroupPartition{
				Topic: p.GetTopic(), Partition: int(p.GetPartition()), Offset: p.GetOffset(), End: p.GetEndOffset(), Lag: p.GetLag(),
			}
		}
		out[i] = gi
	}
	return out, nil
}
