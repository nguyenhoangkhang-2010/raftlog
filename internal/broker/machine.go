package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

const (
	journalName = "applied.json"
	// dumpChunk is how many messages are read from a partition at a time when
	// the state is dumped for a snapshot.
	dumpChunk = 1024
)

// errUnreadableCommand is the outcome of a log entry that does not decode. Every
// node reaches it alike, so applying it as a no-op keeps them in step.
var errUnreadableCommand = errors.New("broker: unreadable command")

// errMissingTimestamp is the outcome of a produce command whose messages the
// leader did not stamp; see the check in produce.
var errMissingTimestamp = errors.New("broker: produce command without timestamps")

// Result is what applying a command produced. It is returned to the node that
// proposed the command.
type Result struct {
	// Partition and BaseOffset describe where a Produce put its messages.
	Partition  int
	BaseOffset uint64
	// Err is the outcome for a command that was valid to replicate but could not
	// be carried out, such as a Produce to a topic that does not exist. Every
	// node reaches the same Err for the same command.
	Err error
}

// journalRecord tells, after a restart, how far the state machine got.
//
// The broker keeps its data in its own durable files, but Raft replays the log
// after a restart. Without a record of what was already done, those entries
// would be applied twice.
type journalRecord struct {
	// Index is the last log entry applied, or, for a Produce, the last one begun.
	Index uint64 `json:"index"`
	// For a Produce that was begun: where it appends and how many messages it
	// carries. A crash can leave it half done; whether it finished is read off
	// the partition (its next offset against Start+Count).
	Topic     string `json:"topic,omitempty"`
	Partition uint32 `json:"partition,omitempty"`
	Start     uint64 `json:"start,omitempty"`
	Count     uint32 `json:"count,omitempty"`
}

// Machine is the broker as a replicated state machine. Raft hands it the
// committed commands; applying them in order on every node leaves every node
// with the same topics, messages, offsets and consumer group positions.
//
// It implements raft.StateMachine. Apply, Snapshot and Restore are called by one
// goroutine; Fetch and GroupOffset may be called concurrently with them.
type Machine struct {
	b    *Broker
	path string
	rec  journalRecord

	mu sync.RWMutex // Restore takes it exclusively; reads share it

	// hook is called at named points during Apply. Tests use it to crash the
	// state machine at a chosen moment.
	hook func(point string)
}

var _ raft.StateMachine = (*Machine)(nil)

// NewMachine wraps b as a state machine and keeps its journal in dir.
func NewMachine(b *Broker, dir string) (*Machine, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create machine dir: %w", err)
	}
	m := &Machine{b: b, path: filepath.Join(dir, journalName)}
	_ = os.Remove(m.path + ".tmp")

	data, err := os.ReadFile(m.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read journal: %w", err)
	default:
		if err := json.Unmarshal(data, &m.rec); err != nil {
			return nil, fmt.Errorf("parse journal %s: %w", m.path, err)
		}
	}
	return m, nil
}

// AppliedIndex returns the index of the last log entry applied.
func (m *Machine) AppliedIndex() uint64 { return m.rec.Index }

func (m *Machine) writeJournal(rec journalRecord) {
	data, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	tmp := m.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		m.fatal("write journal", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		m.fatal("write journal", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		m.fatal("sync journal", err)
	}
	if err := f.Close(); err != nil {
		m.fatal("close journal", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		m.fatal("commit journal", err)
	}
	m.rec = rec
}

// fatal stops the process. The state machine cannot skip an entry it failed to
// apply: the node would silently diverge from the others. Crashing is safe,
// because after a restart the journal lets Apply finish what was begun.
func (m *Machine) fatal(what string, err error) {
	panic(fmt.Errorf("broker state machine: %s: %w", what, err))
}

func (m *Machine) at(point string) {
	if m.hook != nil {
		m.hook(point)
	}
}

// pendingProduce reports whether the journal records a Produce that began but
// whose messages are not all in the partition yet.
func (m *Machine) pendingProduce() bool {
	if m.rec.Count == 0 {
		return false
	}
	next, err := m.b.nextOffset(m.rec.Topic, int(m.rec.Partition))
	return err != nil || next < m.rec.Start+uint64(m.rec.Count)
}

// Apply implements raft.StateMachine.
func (m *Machine) Apply(e raft.Entry) any {
	if e.Index < m.rec.Index {
		return nil // applied before a restart
	}
	if e.Index == m.rec.Index && !m.pendingProduce() {
		return nil // also applied before the restart
	}

	var cmd pb.Command
	if err := proto.Unmarshal(e.Data, &cmd); err != nil {
		m.record(e.Index)
		return Result{Err: fmt.Errorf("%w: %v", errUnreadableCommand, err)}
	}
	switch c := cmd.Command.(type) {
	case *pb.Command_CreateTopic:
		return m.createTopic(e.Index, c.CreateTopic)
	case *pb.Command_Produce:
		return m.produce(e.Index, c.Produce)
	case *pb.Command_CommitOffset:
		return m.commitOffset(e.Index, c.CommitOffset)
	default:
		m.record(e.Index)
		return Result{Err: errUnreadableCommand}
	}
}

// record marks entry index as applied.
func (m *Machine) record(index uint64) { m.writeJournal(journalRecord{Index: index}) }

// isLogical reports whether err is one of the broker's defined refusals, which
// all nodes reach alike, as opposed to a failure of this machine's disk.
func isLogical(err error) bool {
	for _, target := range []error{
		ErrTopicExists, ErrTopicNotFound, ErrInvalidTopicName, ErrInvalidPartitions,
		ErrInvalidPartition, ErrEmptyBatch, ErrInvalidGroup, ErrOffsetBeyondEnd,
		storage.ErrRecordTooLarge,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (m *Machine) createTopic(index uint64, c *pb.CreateTopicCommand) Result {
	err := m.b.CreateTopic(c.GetTopic(), int(c.GetPartitions()))
	if errors.Is(err, ErrTopicExists) {
		if _, lookupErr := m.b.lookup(c.GetTopic()); lookupErr != nil {
			// A directory is there but no topic: an earlier attempt that crashed
			// half way. Clear it and create the topic for real.
			_ = os.RemoveAll(m.b.topicDir(c.GetTopic()))
			err = m.b.CreateTopic(c.GetTopic(), int(c.GetPartitions()))
		}
	}
	if err != nil && !isLogical(err) {
		m.fatal("create topic", err)
	}
	m.at("create-topic:done")
	m.record(index)
	return Result{Err: err}
}

func (m *Machine) commitOffset(index uint64, c *pb.CommitOffsetCommand) Result {
	err := m.b.CommitOffset(c.GetGroup(), c.GetTopic(), int(c.GetPartition()), c.GetOffset())
	if err != nil && !isLogical(err) {
		m.fatal("commit offset", err)
	}
	if err == nil {
		if serr := m.b.offsets.log.Sync(); serr != nil {
			m.fatal("sync offsets", serr)
		}
	}
	m.at("commit-offset:done")
	m.record(index)
	return Result{Err: err}
}

func (m *Machine) produce(index uint64, c *pb.ProduceCommand) Result {
	t, err := m.b.lookup(c.GetTopic())
	if err == nil && int(c.GetPartition()) >= len(t.parts) {
		err = ErrInvalidPartition
	}
	if err == nil && len(c.GetMessages()) == 0 {
		err = ErrEmptyBatch
	}
	// A batch is applied whole or not at all, so refuse it before writing anything.
	for _, sm := range c.GetMessages() {
		if err == nil {
			err = storage.ValidateMessage(storage.Message{Key: sm.GetKey(), Value: sm.GetValue()})
		}
		// The leader stamps the messages before replicating. Without that, every
		// node would take the time of its own clock when it applies the command,
		// and the nodes would store different timestamps for the same message.
		if err == nil && sm.GetTimestampMs() == 0 {
			err = errMissingTimestamp
		}
	}
	if err != nil {
		m.record(index)
		return Result{Err: err}
	}

	p := t.parts[c.GetPartition()]
	p.mu.Lock()
	defer p.mu.Unlock()

	// Write down what is about to happen before doing it, so that a crash in the
	// middle can be finished after the restart instead of repeated.
	start := p.log.NextOffset()
	if index == m.rec.Index {
		if m.rec.Topic != c.GetTopic() || m.rec.Partition != c.GetPartition() {
			m.fatal("resume produce", fmt.Errorf("journal says %s/%d, entry %d says %s/%d",
				m.rec.Topic, m.rec.Partition, index, c.GetTopic(), c.GetPartition()))
		}
		start = m.rec.Start
	} else {
		m.writeJournal(journalRecord{
			Index:     index,
			Topic:     c.GetTopic(),
			Partition: c.GetPartition(),
			Start:     start,
			Count:     uint32(len(c.GetMessages())),
		})
	}
	m.at("produce:intent")

	done := p.log.NextOffset() - start // messages an interrupted attempt already wrote
	if done > uint64(len(c.GetMessages())) {
		m.fatal("resume produce", fmt.Errorf("partition holds %d messages of a batch of %d", done, len(c.GetMessages())))
	}
	for i, sm := range c.GetMessages()[done:] {
		_, err := p.log.Append(storage.Message{Key: sm.GetKey(), Value: sm.GetValue(), Timestamp: sm.GetTimestampMs()})
		if err != nil {
			m.fatal("append", err) // sizes were checked above, so this is the disk
		}
		m.at(fmt.Sprintf("produce:appended:%d", uint64(i)+done+1))
	}
	if err := p.log.Sync(); err != nil {
		m.fatal("sync partition", err)
	}
	p.signal() // readers waiting for data on this partition
	return Result{Partition: int(c.GetPartition()), BaseOffset: start}
}

// Snapshot implements raft.StateMachine: the complete state, as a protobuf.
func (m *Machine) Snapshot() ([]byte, error) {
	state := &pb.StateSnapshot{AppliedIndex: m.rec.Index}

	m.b.mu.RLock()
	names := make([]string, 0, len(m.b.topics))
	for name := range m.b.topics {
		names = append(names, name)
	}
	m.b.mu.RUnlock()
	sort.Strings(names)

	for _, name := range names {
		t, err := m.b.lookup(name)
		if err != nil {
			return nil, err
		}
		ts := &pb.TopicState{Name: name}
		for _, p := range t.parts {
			ps := &pb.PartitionState{}
			end := p.log.NextOffset()
			for next := uint64(0); next < end; {
				msgs, err := p.log.Read(next, dumpChunk)
				if err != nil {
					return nil, fmt.Errorf("dump %s: %w", name, err)
				}
				if len(msgs) == 0 {
					break
				}
				for _, msg := range msgs {
					ps.Messages = append(ps.Messages, &pb.StoredMessage{Key: msg.Key, Value: msg.Value, TimestampMs: msg.Timestamp})
				}
				next = msgs[len(msgs)-1].Offset + 1
			}
			ts.Partitions = append(ts.Partitions, ps)
		}
		state.Topics = append(state.Topics, ts)
	}

	offsets := m.b.offsets.all()
	keys := make([]offsetKey, 0, len(offsets))
	for k := range offsets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.topic != b.topic {
			return a.topic < b.topic
		}
		return a.partition < b.partition
	})
	for _, k := range keys {
		state.Offsets = append(state.Offsets, &pb.GroupOffset{Group: k.group, Topic: k.topic, Partition: uint32(k.partition), Offset: offsets[k]})
	}
	return proto.Marshal(state)
}

// Restore implements raft.StateMachine: replace the whole state with a snapshot.
//
// It does nothing if this node already has everything the snapshot holds, which
// is the usual case when a node restarts with an existing snapshot.
func (m *Machine) Restore(data []byte) error {
	var state pb.StateSnapshot
	if err := proto.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	if m.rec.Index >= state.GetAppliedIndex() && !m.pendingProduce() {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.b.reset(); err != nil {
		return fmt.Errorf("clear the broker state: %w", err)
	}
	for _, ts := range state.GetTopics() {
		if err := m.b.CreateTopic(ts.GetName(), len(ts.GetPartitions())); err != nil {
			return fmt.Errorf("restore topic %q: %w", ts.GetName(), err)
		}
		t, err := m.b.lookup(ts.GetName())
		if err != nil {
			return err
		}
		for i, ps := range ts.GetPartitions() {
			for _, sm := range ps.GetMessages() {
				if _, err := t.parts[i].log.Append(storage.Message{Key: sm.GetKey(), Value: sm.GetValue(), Timestamp: sm.GetTimestampMs()}); err != nil {
					return fmt.Errorf("restore %q/%d: %w", ts.GetName(), i, err)
				}
			}
			if err := t.parts[i].log.Sync(); err != nil {
				return fmt.Errorf("sync %q/%d: %w", ts.GetName(), i, err)
			}
		}
	}
	for _, o := range state.GetOffsets() {
		if err := m.b.offsets.commit(offsetKey{group: o.GetGroup(), topic: o.GetTopic(), partition: int(o.GetPartition())}, o.GetOffset()); err != nil {
			return fmt.Errorf("restore offsets: %w", err)
		}
	}
	if err := m.b.offsets.log.Sync(); err != nil {
		return fmt.Errorf("sync offsets: %w", err)
	}

	// Only now is the state complete. If the process dies before this line, the
	// journal still shows the old position and the restore is simply redone.
	m.writeJournal(journalRecord{Index: state.GetAppliedIndex()})
	return nil
}

// Fetch reads messages from a partition. It sees only applied (committed) data.
func (m *Machine) Fetch(name string, partition int, offset uint64, limit int) ([]storage.Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.b.Fetch(name, partition, offset, limit)
}

// Watch is Broker.Watch for the applied data of this node.
func (m *Machine) Watch(name string, partition int) (<-chan struct{}, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.b.Watch(name, partition)
}

// GroupOffset returns the position of a consumer group.
func (m *Machine) GroupOffset(group, name string, partition int) (OffsetInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.b.GroupOffset(group, name, partition)
}

// Describe is Broker.Describe for the applied data of this node.
func (m *Machine) Describe() []TopicDescription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.b.Describe()
}

// Groups is Broker.Groups for the applied data of this node.
func (m *Machine) Groups() []GroupDescription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.b.Groups()
}
