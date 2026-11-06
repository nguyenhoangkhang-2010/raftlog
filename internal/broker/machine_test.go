package broker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// machineEnv is a broker with its state machine in a directory that can be
// closed and reopened, like a process that restarts.
type machineEnv struct {
	t   *testing.T
	dir string
	b   *Broker
	m   *Machine
}

func newMachineEnv(t *testing.T) *machineEnv {
	t.Helper()
	e := &machineEnv{t: t, dir: t.TempDir()}
	e.open()
	return e
}

func (e *machineEnv) open() {
	e.t.Helper()
	b, err := Open(filepath.Join(e.dir, "data"), Options{MaxPartitions: 8})
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := NewMachine(b, filepath.Join(e.dir, "machine"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.b, e.m = b, m
	e.t.Cleanup(func() { _ = b.Close() })
}

// restart closes everything and reopens it from disk.
func (e *machineEnv) restart() {
	e.t.Helper()
	if err := e.b.Close(); err != nil {
		e.t.Fatal(err)
	}
	e.open()
}

func command(c *pb.Command) []byte {
	data, err := proto.Marshal(c)
	if err != nil {
		panic(err)
	}
	return data
}

func createCmd(topic string, partitions uint32) []byte {
	return command(&pb.Command{Command: &pb.Command_CreateTopic{CreateTopic: &pb.CreateTopicCommand{Topic: topic, Partitions: partitions}}})
}

func produceCmd(topic string, partition uint32, vals ...string) []byte {
	msgs := make([]*pb.StoredMessage, len(vals))
	for i, v := range vals {
		msgs[i] = &pb.StoredMessage{Key: []byte("k"), Value: []byte(v), TimestampMs: 1000 + int64(i)}
	}
	return command(&pb.Command{Command: &pb.Command_Produce{Produce: &pb.ProduceCommand{Topic: topic, Partition: partition, Messages: msgs}}})
}

func commitCmd(group, topic string, partition uint32, offset uint64) []byte {
	return command(&pb.Command{Command: &pb.Command_CommitOffset{CommitOffset: &pb.CommitOffsetCommand{Group: group, Topic: topic, Partition: partition, Offset: offset}}})
}

func entryAt(index uint64, data []byte) raft.Entry {
	return raft.Entry{Index: index, Term: 1, Type: raft.EntryNormal, Data: data}
}

// apply runs a command through the machine and returns its Result.
func (e *machineEnv) apply(index uint64, data []byte) Result {
	e.t.Helper()
	res, ok := e.m.Apply(entryAt(index, data)).(Result)
	if !ok {
		e.t.Fatalf("Apply(%d) did not return a Result", index)
	}
	return res
}

func (e *machineEnv) values(topic string, partition int) []string {
	e.t.Helper()
	end, err := e.b.nextOffset(topic, partition)
	if err != nil {
		e.t.Fatal(err)
	}
	if end == 0 {
		return nil
	}
	msgs, err := e.b.Fetch(topic, partition, 0, int(end))
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m.Value)
	}
	return out
}

func TestMachineAppliesCommandsInOrder(t *testing.T) {
	e := newMachineEnv(t)

	if res := e.apply(1, createCmd("events", 2)); res.Err != nil {
		t.Fatal(res.Err)
	}
	res := e.apply(2, produceCmd("events", 1, "a", "b", "c"))
	if res.Err != nil || res.Partition != 1 || res.BaseOffset != 0 {
		t.Fatalf("first produce = %+v", res)
	}
	res = e.apply(3, produceCmd("events", 1, "d", "e"))
	if res.Err != nil || res.BaseOffset != 3 {
		t.Fatalf("second produce = %+v, want base offset 3", res)
	}
	if got := e.values("events", 1); !reflect.DeepEqual(got, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("partition 1 holds %v", got)
	}
	if got := e.values("events", 0); len(got) != 0 {
		t.Fatalf("partition 0 holds %v", got)
	}
	// The timestamps chosen by the leader are stored as they are.
	if msgs, _ := e.b.Fetch("events", 1, 0, 1); msgs[0].Timestamp != 1000 {
		t.Fatalf("timestamp = %d, want the one in the command", msgs[0].Timestamp)
	}

	if res := e.apply(4, commitCmd("g1", "events", 1, 4)); res.Err != nil {
		t.Fatal(res.Err)
	}
	info, err := e.m.GroupOffset("g1", "events", 1)
	if err != nil || !info.Committed || info.Offset != 4 || info.End != 5 {
		t.Fatalf("group offset = %+v, %v", info, err)
	}
	if e.m.AppliedIndex() != 4 {
		t.Fatalf("AppliedIndex = %d, want 4", e.m.AppliedIndex())
	}
}

func TestRefusedCommandsChangeNothingAndAreTheSameOnEveryNode(t *testing.T) {
	big := string(make([]byte, 65*1024*1024)) // over the size a record can have
	cases := []struct {
		name string
		cmd  []byte
		want error
	}{
		{"produce to a missing topic", produceCmd("nope", 0, "x"), ErrTopicNotFound},
		{"produce to a missing partition", produceCmd("events", 5, "x"), ErrInvalidPartition},
		{"produce nothing", produceCmd("events", 0), ErrEmptyBatch},
		{"produce an oversized message", produceCmd("events", 0, "fine", big), storage.ErrRecordTooLarge},
		{"produce messages without timestamps", command(&pb.Command{Command: &pb.Command_Produce{Produce: &pb.ProduceCommand{
			Topic: "events", Messages: []*pb.StoredMessage{{Value: []byte("x")}},
		}}}), errMissingTimestamp},
		{"create a topic twice", createCmd("events", 1), ErrTopicExists},
		{"create with a bad name", createCmd("a/b", 1), ErrInvalidTopicName},
		{"create with too many partitions", createCmd("wide", 99), ErrInvalidPartitions},
		{"commit beyond the end", commitCmd("g1", "events", 0, 7), ErrOffsetBeyondEnd},
		{"commit for a bad group", commitCmd("bad group", "events", 0, 0), ErrInvalidGroup},
		{"commit to a missing topic", commitCmd("g1", "nope", 0, 0), ErrTopicNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newMachineEnv(t)
			e.apply(1, createCmd("events", 1))
			before, _ := e.m.Snapshot()

			res := e.apply(2, c.cmd)
			if !errors.Is(res.Err, c.want) {
				t.Fatalf("Err = %v, want %v", res.Err, c.want)
			}
			after, _ := e.m.Snapshot()
			// Only the position in the log moved; the data did not.
			var b, a pb.StateSnapshot
			_ = proto.Unmarshal(before, &b)
			_ = proto.Unmarshal(after, &a)
			b.AppliedIndex, a.AppliedIndex = 0, 0
			if !proto.Equal(&b, &a) {
				t.Fatal("a refused command changed the data")
			}
			if e.m.AppliedIndex() != 2 {
				t.Fatalf("AppliedIndex = %d: a refused command must still count as applied", e.m.AppliedIndex())
			}
		})
	}
}

func TestAProduceWithoutTimestampsIsRefusedBecauseNodesWouldDisagree(t *testing.T) {
	e := newMachineEnv(t)
	e.apply(1, createCmd("events", 1))
	unstamped := command(&pb.Command{Command: &pb.Command_Produce{Produce: &pb.ProduceCommand{
		Topic:    "events",
		Messages: []*pb.StoredMessage{{Key: []byte("k"), Value: []byte("v"), TimestampMs: 5}, {Value: []byte("no timestamp")}},
	}}})

	res := e.apply(2, unstamped)
	if !errors.Is(res.Err, errMissingTimestamp) {
		t.Fatalf("Err = %v, want errMissingTimestamp", res.Err)
	}
	if got := e.values("events", 0); len(got) != 0 {
		t.Fatalf("a refused batch was partly written: %v", got)
	}
}

func TestAnUnreadableCommandIsSkippedAndTheNextOneWorks(t *testing.T) {
	e := newMachineEnv(t)
	res := e.apply(1, []byte{0xff, 0xff, 0xff, 0xff, 0xff})
	if !errors.Is(res.Err, errUnreadableCommand) {
		t.Fatalf("Err = %v, want errUnreadableCommand", res.Err)
	}
	if res := e.apply(2, createCmd("events", 1)); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := e.apply(3, command(&pb.Command{})); !errors.Is(res.Err, errUnreadableCommand) { // no command inside
		t.Fatalf("an empty command: Err = %v", res.Err)
	}
}

func TestTwoMachinesGiveTheSameStateForTheSameCommands(t *testing.T) {
	cmds := [][]byte{
		createCmd("a", 2), createCmd("b", 1),
		produceCmd("a", 0, "1", "2"), produceCmd("a", 1, "3"), produceCmd("b", 0, "4", "5", "6"),
		commitCmd("g1", "a", 0, 2), commitCmd("g2", "b", 0, 1),
		produceCmd("nope", 0, "x"), // refused on both
		commitCmd("g1", "a", 0, 1),
	}
	one, two := newMachineEnv(t), newMachineEnv(t)
	for i, c := range cmds {
		one.apply(uint64(i+1), c)
		two.apply(uint64(i+1), c)
	}
	s1, err1 := one.m.Snapshot()
	s2, err2 := two.m.Snapshot()
	if err1 != nil || err2 != nil || !bytes.Equal(s1, s2) {
		t.Fatalf("the machines diverged (errors %v, %v)", err1, err2)
	}
}

// --- restarts -----------------------------------------------------------------

func TestEntriesAppliedBeforeARestartAreNotAppliedAgain(t *testing.T) {
	e := newMachineEnv(t)
	cmds := [][]byte{createCmd("events", 1), produceCmd("events", 0, "a", "b"), commitCmd("g1", "events", 0, 1), produceCmd("events", 0, "c")}
	for i, c := range cmds {
		e.apply(uint64(i+1), c)
	}
	e.restart()

	// Raft replays the whole log to the state machine after a restart.
	for i, c := range cmds {
		e.m.Apply(entryAt(uint64(i+1), c))
	}
	if got := e.values("events", 0); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("after the replay the partition holds %v, want [a b c]", got)
	}
	if info, _ := e.m.GroupOffset("g1", "events", 0); info.Offset != 1 {
		t.Fatalf("offset = %d, want 1", info.Offset)
	}

	// New entries still apply.
	if res := e.apply(5, produceCmd("events", 0, "d")); res.Err != nil || res.BaseOffset != 3 {
		t.Fatalf("produce after the replay = %+v", res)
	}
}

// crash is the panic the test hook raises to stop the machine mid-command.
type crash struct{ point string }

// applyUntilCrash applies the entry and reports whether the hook stopped it.
func (e *machineEnv) applyUntilCrash(index uint64, data []byte, point string) (crashed bool) {
	e.t.Helper()
	e.m.hook = func(p string) {
		if p == point {
			panic(crash{p})
		}
	}
	defer func() {
		e.m.hook = nil
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	e.m.Apply(entryAt(index, data))
	return false
}

func TestACrashInTheMiddleOfAProduceIsFinishedAfterTheRestart(t *testing.T) {
	// Raft redelivers the entry after the restart. Whatever part of the batch was
	// already written must neither be lost nor written a second time.
	points := []string{"produce:intent", "produce:appended:1", "produce:appended:2", "produce:appended:3", "produce:appended:4"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			e := newMachineEnv(t)
			e.apply(1, createCmd("events", 1))
			e.apply(2, produceCmd("events", 0, "x")) // something already there
			batch := produceCmd("events", 0, "a", "b", "c", "d")

			crashed := e.applyUntilCrash(3, batch, point)
			if point != "produce:appended:4" && !crashed {
				t.Fatal("the hook did not fire")
			}
			e.restart()

			// The replay after the restart: every entry again, in order.
			var res Result
			for i, c := range [][]byte{createCmd("events", 1), produceCmd("events", 0, "x"), batch} {
				if r, ok := e.m.Apply(entryAt(uint64(i+1), c)).(Result); ok {
					res = r
				}
			}
			// An entry that had been written completely before the crash is just
			// skipped, so there is nothing to answer; nobody waits after a restart.
			// One that was only partly written is finished, and answers as usual.
			if point != "produce:appended:4" && (res.Err != nil || res.BaseOffset != 1) {
				t.Fatalf("the resumed produce answered %+v, want base offset 1", res)
			}
			want := []string{"x", "a", "b", "c", "d"}
			if got := e.values("events", 0); !reflect.DeepEqual(got, want) {
				t.Fatalf("partition holds %v, want %v", got, want)
			}
			// And the machine carries on normally.
			if res := e.apply(4, produceCmd("events", 0, "e")); res.BaseOffset != 5 {
				t.Fatalf("next produce got base offset %d, want 5", res.BaseOffset)
			}
		})
	}
}

func TestACrashAfterATopicOrOffsetWasChangedButNotRecordedIsHarmless(t *testing.T) {
	for _, point := range []string{"create-topic:done", "commit-offset:done"} {
		t.Run(point, func(t *testing.T) {
			e := newMachineEnv(t)
			e.apply(1, createCmd("events", 1))
			e.apply(2, produceCmd("events", 0, "a", "b", "c"))

			cmd := createCmd("other", 2)
			if point == "commit-offset:done" {
				cmd = commitCmd("g1", "events", 0, 2)
			}
			if !e.applyUntilCrash(3, cmd, point) {
				t.Fatal("the hook did not fire")
			}
			e.restart()

			for i, c := range [][]byte{createCmd("events", 1), produceCmd("events", 0, "a", "b", "c"), cmd} {
				e.m.Apply(entryAt(uint64(i+1), c))
			}
			if got := e.values("events", 0); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
				t.Fatalf("partition holds %v", got)
			}
			if point == "create-topic:done" {
				if n, err := e.b.nextOffset("other", 1); err != nil || n != 0 {
					t.Fatalf("topic 'other' is missing or wrong after the replay: %d, %v", n, err)
				}
			} else if info, _ := e.m.GroupOffset("g1", "events", 0); !info.Committed || info.Offset != 2 {
				t.Fatalf("offset after the replay = %+v", info)
			}
		})
	}
}

func TestACrashLeavingAHalfCreatedTopicDoesNotBlockItForever(t *testing.T) {
	e := newMachineEnv(t)
	// A topic directory without its metadata is what a crash during creation leaves.
	orphan := e.b.topicDir("events")
	if err := os.MkdirAll(filepath.Join(orphan, "partition-0000"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.restart()

	if res := e.apply(1, createCmd("events", 2)); res.Err != nil {
		t.Fatalf("could not create the topic over the leftover: %v", res.Err)
	}
	if res := e.apply(2, produceCmd("events", 1, "ok")); res.Err != nil {
		t.Fatal(res.Err)
	}
}

func TestACorruptJournalIsAnErrorNotAFreshStart(t *testing.T) {
	e := newMachineEnv(t)
	e.apply(1, createCmd("events", 1))
	if err := os.WriteFile(filepath.Join(e.dir, "machine", journalName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMachine(e.b, filepath.Join(e.dir, "machine")); err == nil {
		t.Fatal("a corrupt journal was treated as an empty one: every entry would be applied again")
	}
}

// --- snapshots ----------------------------------------------------------------

func populated(t *testing.T) *machineEnv {
	e := newMachineEnv(t)
	cmds := [][]byte{
		createCmd("a", 2), createCmd("b", 1),
		produceCmd("a", 0, "a0-1", "a0-2"), produceCmd("a", 1, "a1-1"), produceCmd("b", 0, "b0-1", "b0-2", "b0-3"),
		commitCmd("g1", "a", 0, 2), commitCmd("g2", "b", 0, 1),
	}
	for i, c := range cmds {
		e.apply(uint64(i+1), c)
	}
	return e
}

func TestSnapshotAndRestoreReproduceTheState(t *testing.T) {
	src := populated(t)
	data, err := src.m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	dst := newMachineEnv(t)
	if err := dst.m.Restore(data); err != nil {
		t.Fatal(err)
	}
	again, err := dst.m.Snapshot()
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("the restored state is not the one that was snapshotted (err %v)", err)
	}
	if got := dst.values("b", 0); !reflect.DeepEqual(got, []string{"b0-1", "b0-2", "b0-3"}) {
		t.Fatalf("restored partition holds %v", got)
	}
	if info, _ := dst.m.GroupOffset("g2", "b", 0); !info.Committed || info.Offset != 1 || info.End != 3 {
		t.Fatalf("restored offset = %+v", info)
	}
	if dst.m.AppliedIndex() != 7 {
		t.Fatalf("AppliedIndex = %d, want 7", dst.m.AppliedIndex())
	}

	// A restored machine continues where the snapshot left off, and survives a restart.
	if res := dst.apply(8, produceCmd("b", 0, "b0-4")); res.Err != nil || res.BaseOffset != 3 {
		t.Fatalf("produce after the restore = %+v", res)
	}
	dst.restart()
	if got := dst.values("b", 0); len(got) != 4 {
		t.Fatalf("after a restart the partition holds %v", got)
	}
}

func TestRestoreReplacesWhatTheNodeHadBefore(t *testing.T) {
	src := populated(t)
	data, _ := src.m.Snapshot()

	dst := newMachineEnv(t)
	dst.apply(1, createCmd("stale", 1))
	dst.apply(2, produceCmd("stale", 0, "old"))
	dst.apply(3, commitCmd("old-group", "stale", 0, 1))

	if err := dst.m.Restore(data); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.b.nextOffset("stale", 0); !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("a topic from before the restore survived: %v", err)
	}
	if info, _ := dst.m.GroupOffset("old-group", "a", 0); info.Committed {
		t.Fatal("an offset from before the restore survived")
	}
	if again, _ := dst.m.Snapshot(); !bytes.Equal(data, again) {
		t.Fatal("the state differs from the snapshot")
	}
}

func TestRestoreIsSkippedWhenTheNodeIsAlreadyAhead(t *testing.T) {
	e := populated(t) // applied up to index 7
	old := newMachineEnv(t)
	old.apply(1, createCmd("a", 2))
	data, _ := old.m.Snapshot() // a snapshot at index 1

	if err := e.m.Restore(data); err != nil {
		t.Fatal(err)
	}
	if got := e.values("a", 0); !reflect.DeepEqual(got, []string{"a0-1", "a0-2"}) {
		t.Fatalf("an older snapshot overwrote newer data: %v", got)
	}
	if e.m.AppliedIndex() != 7 {
		t.Fatalf("AppliedIndex = %d, want 7", e.m.AppliedIndex())
	}
}

func TestRestoreIsNotSkippedWhileAProduceIsUnfinished(t *testing.T) {
	// The snapshot covers an entry whose messages this node only half wrote
	// before a crash. Skipping the restore would leave that gap forever, since
	// Raft will not redeliver an entry the snapshot covers.
	src := newMachineEnv(t)
	src.apply(1, createCmd("events", 1))
	src.apply(2, produceCmd("events", 0, "a", "b", "c"))
	data, _ := src.m.Snapshot() // covers entry 2

	e := newMachineEnv(t)
	e.apply(1, createCmd("events", 1))
	if !e.applyUntilCrash(2, produceCmd("events", 0, "a", "b", "c"), "produce:appended:1") {
		t.Fatal("the hook did not fire")
	}
	e.restart()
	if !e.m.pendingProduce() {
		t.Fatal("setup: the journal should show an unfinished produce")
	}

	if err := e.m.Restore(data); err != nil {
		t.Fatal(err)
	}
	if got := e.values("events", 0); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("partition holds %v, want [a b c]", got)
	}
	if e.m.pendingProduce() {
		t.Fatal("still unfinished after the restore")
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	e := newMachineEnv(t)
	if err := e.m.Restore([]byte{0xff, 0xff, 0xff}); err == nil {
		t.Fatal("garbage was accepted as a snapshot")
	}
}

func TestSnapshotOfAnEmptyMachine(t *testing.T) {
	e := newMachineEnv(t)
	data, err := e.m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	other := newMachineEnv(t)
	if err := other.m.Restore(data); err != nil {
		t.Fatal(err)
	}
	if len(other.b.Topics()) != 0 {
		t.Fatalf("topics = %v", other.b.Topics())
	}
}

func TestSnapshotOfAPartitionLargerThanOneChunk(t *testing.T) {
	e := newMachineEnv(t)
	e.apply(1, createCmd("big", 1))
	total := dumpChunk*2 + 17
	vals := make([]string, total)
	for i := range vals {
		vals[i] = fmt.Sprintf("v%05d", i)
	}
	e.apply(2, produceCmd("big", 0, vals...))

	data, err := e.m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	other := newMachineEnv(t)
	if err := other.m.Restore(data); err != nil {
		t.Fatal(err)
	}
	if got := other.values("big", 0); !reflect.DeepEqual(got, vals) {
		t.Fatalf("restored %d messages, want %d (or the order differs)", len(got), total)
	}
}
