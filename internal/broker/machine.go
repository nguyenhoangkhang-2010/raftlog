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
	// Index is the last log entry applied, or, for a group of Produce commands, the
	// last one of the group that was begun.
	Index uint64 `json:"index"`

	// For a group of Produce commands that was begun: First is the index of its first
	// entry, and Parts says, for every partition the group appends to, where the group's
	// messages start and end. A crash can leave the group half done; how far it got is
	// read off the partitions (their next offset against Start and End), and the entries
	// First..Index are applied again after the restart, skipping what is already there.
	First uint64        `json:"first,omitempty"`
	Parts []journalPart `json:"parts,omitempty"`

	// The journal of a single Produce, as an earlier version wrote it. It is only read:
	// NewMachine turns it into a group of one.
	Topic     string `json:"topic,omitempty"`
	Partition uint32 `json:"partition,omitempty"`
	Start     uint64 `json:"start,omitempty"`
	Count     uint32 `json:"count,omitempty"`
}

// journalPart is where one group of Produce commands appends in one partition: the
// messages it adds occupy offsets Start up to, not including, End.
type journalPart struct {
	Topic     string `json:"topic"`
	Partition uint32 `json:"partition"`
	Start     uint64 `json:"start"`
	End       uint64 `json:"end"`
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

	// While the entries of a group that was begun before a restart are applied again
	// (see replayEntry): where the next message of each partition of the group goes, and
	// the partitions of the group, which are synced when it is complete. Nil at all other times.
	replayNext    map[partKey]uint64
	replayTouched map[partKey]*partition

	// hook is called at named points during Apply. Tests use it to crash the
	// state machine at a chosen moment.
	hook func(point string)
}

var (
	_ raft.StateMachine      = (*Machine)(nil)
	_ raft.BatchStateMachine = (*Machine)(nil)
)

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
		if m.rec.Count > 0 && len(m.rec.Parts) == 0 {
			// A journal written for one Produce: the same thing as a group of one.
			m.rec = journalRecord{
				Index: m.rec.Index, First: m.rec.Index,
				Parts: []journalPart{{Topic: m.rec.Topic, Partition: m.rec.Partition, Start: m.rec.Start, End: m.rec.Start + uint64(m.rec.Count)}},
			}
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

// pendingProduce reports whether the journal records a group of Produce commands that
// began but whose messages are not all in the partitions yet.
func (m *Machine) pendingProduce() bool {
	for _, jp := range m.rec.Parts {
		next, err := m.b.nextOffset(jp.Topic, int(jp.Partition))
		if err != nil || next < jp.End {
			return true
		}
	}
	return false
}

// applied reports whether entry index was fully applied before a restart: its work is
// done and the journal has moved past it.
func (m *Machine) applied(index uint64) bool {
	if len(m.rec.Parts) > 0 {
		return index < m.rec.First
	}
	return index <= m.rec.Index
}

// begun reports whether entry index belongs to the group of Produce commands that the
// journal says was begun, and is applied again to finish it.
func (m *Machine) begun(index uint64) bool {
	return len(m.rec.Parts) > 0 && index >= m.rec.First && index <= m.rec.Index
}

// Apply implements raft.StateMachine.
func (m *Machine) Apply(e raft.Entry) any { return m.ApplyBatch([]raft.Entry{e})[0] }

// A group of Produce commands ends when it holds this many commands or this many bytes.
const (
	applyRunMaxEntries = 256
	applyRunMaxBytes   = 4 << 20
)

// ApplyBatch implements raft.BatchStateMachine: it applies the entries in order, and
// consecutive Produce commands as one group, so that the journal is written once and
// each partition is synced once for all of them, not once for each. The result for each
// entry is what Apply would have returned, and the state afterwards is the same.
func (m *Machine) ApplyBatch(entries []raft.Entry) []any {
	results := make([]any, len(entries))
	var run []plannedProduce
	runBytes := 0
	flush := func() {
		if len(run) > 0 {
			m.applyProduceRun(run, results)
			run, runBytes = run[:0], 0
		}
	}
	for i, e := range entries {
		switch {
		case m.applied(e.Index):
			continue // done before a restart
		case m.begun(e.Index):
			flush()
			results[i] = m.replayEntry(e)
			continue
		}

		var cmd pb.Command
		if err := proto.Unmarshal(e.Data, &cmd); err != nil {
			flush()
			m.record(e.Index)
			results[i] = Result{Err: fmt.Errorf("%w: %v", errUnreadableCommand, err)}
			continue
		}
		switch c := cmd.Command.(type) {
		case *pb.Command_Produce:
			run = append(run, plannedProduce{pos: i, index: e.Index, cmd: c.Produce})
			runBytes += len(e.Data)
			if len(run) >= applyRunMaxEntries || runBytes >= applyRunMaxBytes {
				flush()
			}
		case *pb.Command_CreateTopic:
			flush()
			results[i] = m.createTopic(e.Index, c.CreateTopic)
		case *pb.Command_CommitOffset:
			flush()
			results[i] = m.commitOffset(e.Index, c.CommitOffset)
		default:
			flush()
			m.record(e.Index)
			results[i] = Result{Err: errUnreadableCommand}
		}
	}
	flush()
	return results
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

// partKey names a partition.
type partKey struct {
	topic     string
	partition uint32
}

// plannedProduce is a Produce command that waits to be applied with its neighbours.
type plannedProduce struct {
	pos   int // where its result goes in the slice ApplyBatch returns
	index uint64
	cmd   *pb.ProduceCommand
}

// checkProduce returns the partition a Produce command appends to, or the reason the
// command is refused. A refusal is the same on every node, and changes nothing.
func (m *Machine) checkProduce(c *pb.ProduceCommand) (*partition, error) {
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
		return nil, err
	}
	return t.parts[c.GetPartition()], nil
}

// applyProduceRun applies consecutive Produce commands as one group: it writes one
// journal record for all of them, appends their messages, and syncs each partition once.
// The commands are applied in order, so the offsets are the ones they would get one by
// one, and a command that is refused changes nothing, as when applied alone. Each
// command's outcome goes to results at its position.
func (m *Machine) applyProduceRun(run []plannedProduce, results []any) {
	type target struct {
		key        partKey
		p          *partition
		start, end uint64
	}
	var targets []*target
	byKey := map[partKey]*target{}
	owner := make([]*partition, len(run)) // the partition of each command; nil if refused
	for i, pr := range run {
		p, err := m.checkProduce(pr.cmd)
		if err != nil {
			results[pr.pos] = Result{Err: err}
			continue
		}
		owner[i] = p
		k := partKey{pr.cmd.GetTopic(), pr.cmd.GetPartition()}
		tg := byKey[k]
		if tg == nil {
			start := p.log.NextOffset()
			tg = &target{key: k, p: p, start: start, end: start}
			byKey[k] = tg
			targets = append(targets, tg)
		}
		results[pr.pos] = Result{Partition: int(k.partition), BaseOffset: tg.end}
		tg.end += uint64(len(pr.cmd.GetMessages()))
	}

	first, last := run[0].index, run[len(run)-1].index
	if len(targets) == 0 {
		m.record(last) // nothing was valid: only the position moves
		return
	}
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i].key, targets[j].key
		if a.topic != b.topic {
			return a.topic < b.topic
		}
		return a.partition < b.partition
	})
	for _, tg := range targets {
		tg.p.mu.Lock()
		defer tg.p.mu.Unlock()
	}

	// Write down what is about to happen before doing it, so that a crash in the
	// middle can be finished after the restart instead of repeated.
	parts := make([]journalPart, len(targets))
	for i, tg := range targets {
		parts[i] = journalPart{Topic: tg.key.topic, Partition: tg.key.partition, Start: tg.start, End: tg.end}
	}
	m.writeJournal(journalRecord{Index: last, First: first, Parts: parts})
	m.at("produce:intent")

	for i, pr := range run {
		if owner[i] == nil {
			continue
		}
		for j, sm := range pr.cmd.GetMessages() {
			if _, err := owner[i].log.Append(storage.Message{Key: sm.GetKey(), Value: sm.GetValue(), Timestamp: sm.GetTimestampMs()}); err != nil {
				m.fatal("append", err) // sizes were checked above, so this is the disk
			}
			m.at(fmt.Sprintf("produce:appended:%d", j+1))
		}
	}
	for _, tg := range targets {
		if err := tg.p.log.Sync(); err != nil {
			m.fatal("sync partition", err)
		}
	}
	for _, tg := range targets {
		tg.p.signal() // readers waiting for data on this partition
	}
}

// replayEntry applies an entry of the group that the journal says was begun and may not
// have finished, after a restart. Raft hands the entries over again, one by one, from
// the first of the group. For each, the messages that are already in the partition are
// not appended again: the partition's next offset says how many there are, counted from
// where the group began. After the last entry of the group the partitions are synced
// and the group is complete.
func (m *Machine) replayEntry(e raft.Entry) any {
	var cmd pb.Command
	if err := proto.Unmarshal(e.Data, &cmd); err != nil {
		m.fatal("replay", fmt.Errorf("entry %d, inside a begun group, does not decode: %w", e.Index, err))
	}
	c, ok := cmd.Command.(*pb.Command_Produce)
	if !ok {
		m.fatal("replay", fmt.Errorf("entry %d is inside a begun group of Produce commands but is not one", e.Index))
	}
	if m.replayNext == nil {
		m.replayNext = make(map[partKey]uint64, len(m.rec.Parts))
		m.replayTouched = map[partKey]*partition{}
		for _, jp := range m.rec.Parts {
			m.replayNext[partKey{jp.Topic, jp.Partition}] = jp.Start
		}
	}

	var res any
	p, err := m.checkProduce(c.Produce)
	if err != nil {
		res = Result{Err: err}
	} else {
		k := partKey{c.Produce.GetTopic(), c.Produce.GetPartition()}
		start, known := m.replayNext[k]
		if !known {
			m.fatal("replay", fmt.Errorf("entry %d appends to %s/%d, which the journal's group does not", e.Index, k.topic, k.partition))
		}
		msgs := c.Produce.GetMessages()
		have := p.log.NextOffset()
		if have < start {
			m.fatal("replay", fmt.Errorf("partition %s/%d holds %d messages, fewer than the %d where the group began", k.topic, k.partition, have, start))
		}
		done := min(have-start, uint64(len(msgs)))
		if done < uint64(len(msgs)) {
			p.mu.Lock()
			for _, sm := range msgs[done:] {
				if _, err := p.log.Append(storage.Message{Key: sm.GetKey(), Value: sm.GetValue(), Timestamp: sm.GetTimestampMs()}); err != nil {
					p.mu.Unlock()
					m.fatal("append", err)
				}
			}
			p.mu.Unlock()
		}
		m.replayNext[k] = start + uint64(len(msgs))
		m.replayTouched[k] = p // synced at the end even if nothing was appended: a message
		// written before the crash may not have reached the disk
		res = Result{Partition: int(k.partition), BaseOffset: start}
	}

	if e.Index == m.rec.Index { // the last entry of the group
		for _, p := range m.replayTouched {
			if err := p.log.Sync(); err != nil {
				m.fatal("sync partition", err)
			}
			p.signal()
		}
		m.replayNext, m.replayTouched = nil, nil
	}
	return res
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

	m.replayNext, m.replayTouched = nil, nil // whatever was being finished is replaced
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
