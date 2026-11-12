package broker

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// unstamped is a Produce whose messages carry no timestamp: every node refuses it.
func unstamped(topic string, partition uint32) []byte {
	return command(&pb.Command{Command: &pb.Command_Produce{Produce: &pb.ProduceCommand{
		Topic: topic, Partition: partition, Messages: []*pb.StoredMessage{{Value: []byte("u")}},
	}}})
}

// scenario is a list of entries, with the gaps in the numbering that no-op entries of the
// leader leave in a real log.
type scenario []raft.Entry

// randomScenario builds commands of every kind, with many refusals among them.
func randomScenario(seed int64, n int) scenario {
	rng := rand.New(rand.NewSource(seed))
	var out scenario
	index := uint64(0)
	add := func(data []byte) {
		index += 1 + uint64(boolToInt(rng.Intn(6) == 0)) // sometimes a no-op takes an index
		out = append(out, entryAt(index, data))
	}
	add(createCmd("a", 2))
	add(createCmd("b", 1))
	for i := 0; i < n; i++ {
		switch r := rng.Intn(20); {
		case r < 11:
			vals := make([]string, 1+rng.Intn(3))
			for j := range vals {
				vals[j] = fmt.Sprintf("v%d-%d", i, j)
			}
			topic := []string{"a", "a", "b", "c"}[rng.Intn(4)]
			add(produceCmd(topic, uint32(rng.Intn(2)), vals...)) // partition 1 of "b" does not exist
		case r < 13:
			add(produceCmd("nope", 0, "x")) // unknown topic
		case r < 14:
			add(produceCmd("a", 0)) // empty batch
		case r < 15:
			add(unstamped("a", 0))
		case r < 17:
			add(commitCmd("g", "a", uint32(rng.Intn(2)), uint64(rng.Intn(3))))
		case r < 18:
			add(createCmd("c", 1)) // sometimes already there
		case r < 19:
			add([]byte{0xff, 0xff, 0xff}) // does not decode
		default:
			add(command(&pb.Command{})) // decodes, but carries no command
		}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s scenario) last() uint64 { return s[len(s)-1].Index }

// applyOneByOne gives the entries to the machine through Apply.
func applyOneByOne(m *Machine, entries []raft.Entry) []any {
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = m.Apply(e)
	}
	return out
}

// applyChunked gives the entries to ApplyBatch in pieces of random length.
func applyChunked(m *Machine, entries []raft.Entry, rng *rand.Rand) []any {
	var out []any
	for len(entries) > 0 {
		n := 1 + rng.Intn(12)
		if n > len(entries) {
			n = len(entries)
		}
		out = append(out, m.ApplyBatch(entries[:n])...)
		entries = entries[n:]
	}
	return out
}

func describeResults(rs []any) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = fmt.Sprintf("%+v", r)
	}
	return out
}

func mustSnapshot(t *testing.T, m *Machine) []byte {
	t.Helper()
	s, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestApplyBatchLeavesTheSameStateAndAnswersAsApplyingOneByOne(t *testing.T) {
	seeds := int64(40) // each seed is 80 commands applied twice, each with its own journal write
	if testing.Short() {
		seeds = 6
	}
	for seed := int64(1); seed <= seeds; seed++ {
		sc := randomScenario(seed, 80)
		one, many := newMachineEnv(t), newMachineEnv(t)
		want := applyOneByOne(one.m, sc)
		got := applyChunked(many.m, sc, rand.New(rand.NewSource(seed*7)))

		if !reflect.DeepEqual(describeResults(want), describeResults(got)) {
			t.Fatalf("seed %d: the answers differ\n one by one: %v\n batched:    %v", seed, describeResults(want), describeResults(got))
		}
		if one.m.AppliedIndex() != many.m.AppliedIndex() {
			t.Fatalf("seed %d: applied index %d against %d", seed, one.m.AppliedIndex(), many.m.AppliedIndex())
		}
		if string(mustSnapshot(t, one.m)) != string(mustSnapshot(t, many.m)) {
			t.Fatalf("seed %d: the state differs", seed)
		}
	}
}

// groupScenario is a topic of two partitions and then a run of Produce commands to both,
// with a refused one in the middle: one group for the machine.
func groupScenario() (setup, group scenario) {
	setup = scenario{entryAt(1, createCmd("t", 2))}
	cmds := [][]byte{
		produceCmd("t", 0, "a1", "a2"),
		produceCmd("t", 1, "b1"),
		produceCmd("t", 0, "a3"),
		produceCmd("nope", 0, "x"), // refused
		produceCmd("t", 1, "b2", "b3"),
		produceCmd("t", 0, "a4", "a5", "a6"),
	}
	for i, c := range cmds {
		group = append(group, entryAt(uint64(i+2), c))
	}
	return setup, group
}

func TestAGroupOfProduceCommandsIsOneJournalWriteAndEachPartitionIsSyncedOnce(t *testing.T) {
	setup, group := groupScenario()
	e := newMachineEnv(t)
	e.m.ApplyBatch(setup)
	intents := 0
	e.m.hook = func(p string) {
		if p == "produce:intent" {
			intents++
		}
	}
	res := e.m.ApplyBatch(group)
	if intents != 1 {
		t.Fatalf("%d journal records for one group of %d commands, want 1", intents, len(group))
	}
	// The offsets are the ones the commands get one by one.
	wantBase := []uint64{0, 0, 2, 0, 1, 3}
	for i, r := range res {
		got, _ := r.(Result)
		if i == 3 {
			if got.Err == nil {
				t.Fatalf("the refused command answered %+v", got)
			}
			continue
		}
		if got.Err != nil || got.BaseOffset != wantBase[i] {
			t.Fatalf("command %d answered %+v, want base offset %d", i, got, wantBase[i])
		}
	}
	if got := e.values("t", 0); !reflect.DeepEqual(got, []string{"a1", "a2", "a3", "a4", "a5", "a6"}) {
		t.Fatalf("partition 0 holds %v", got)
	}
	if got := e.values("t", 1); !reflect.DeepEqual(got, []string{"b1", "b2", "b3"}) {
		t.Fatalf("partition 1 holds %v", got)
	}
}

// crashAt stops ApplyBatch at the n-th point of the group: 0 is the journal record, and
// k is the k-th message appended.
func crashAt(e *machineEnv, entries []raft.Entry, n int) (crashed bool) {
	count := -1
	e.m.hook = func(p string) {
		switch {
		case p == "produce:intent":
			count = 0
		case strings.HasPrefix(p, "produce:appended:"):
			count++
		default:
			return
		}
		if count == n {
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
	e.m.ApplyBatch(entries)
	return false
}

func TestAGroupInterruptedAtAnyPointIsFinishedAfterTheRestartHoweverTheEntriesAreRedelivered(t *testing.T) {
	setup, group := groupScenario()
	all := append(append(scenario{}, setup...), group...)
	totalMessages := 9 // appended by the valid commands of the group

	// What a machine that was never interrupted ends with.
	ref := newMachineEnv(t)
	applyOneByOne(ref.m, all)
	want := string(mustSnapshot(t, ref.m))

	for n := 0; n <= totalMessages; n++ {
		for _, mode := range []string{"one by one", "chunked", "prefix first"} {
			t.Run(fmt.Sprintf("crash %d/%s", n, mode), func(t *testing.T) {
				e := newMachineEnv(t)
				e.m.ApplyBatch(setup)
				crashed := crashAt(e, group, n)
				if n < totalMessages && !crashed {
					t.Fatal("the hook did not fire")
				}
				e.restart()

				// Raft hands the whole log over again after a restart.
				switch mode {
				case "one by one":
					applyOneByOne(e.m, all)
				case "chunked":
					applyChunked(e.m, all, rand.New(rand.NewSource(int64(n))))
				case "prefix first":
					// A second restart before the group has been applied again in full.
					applyOneByOne(e.m, all[:3])
					e.restart()
					applyChunked(e.m, all, rand.New(rand.NewSource(int64(n))))
				}
				if got := string(mustSnapshot(t, e.m)); got != want {
					t.Fatalf("after the restart the state differs from an uninterrupted machine: partition 0 %v, partition 1 %v",
						e.values("t", 0), e.values("t", 1))
				}
				// It carries on as usual.
				next := entryAt(group.last()+1, produceCmd("t", 0, "z"))
				if r, _ := e.m.Apply(next).(Result); r.Err != nil || r.BaseOffset != 6 {
					t.Fatalf("the next produce answered %+v, want base offset 6", r)
				}
			})
		}
	}
}

func TestAnUnfinishedGroupIsReplacedByASnapshotInsteadOfBeingSkipped(t *testing.T) {
	setup, group := groupScenario()

	// A node that is ahead of nothing: build the snapshot on a healthy machine.
	healthy := newMachineEnv(t)
	healthy.m.ApplyBatch(setup)
	healthy.m.ApplyBatch(group)
	snap := mustSnapshot(t, healthy.m)

	e := newMachineEnv(t)
	e.m.ApplyBatch(setup)
	if !crashAt(e, group, 3) {
		t.Fatal("the hook did not fire")
	}
	e.restart()
	if !e.m.pendingProduce() {
		t.Fatal("the journal does not show a group in progress")
	}
	if err := e.m.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if got := string(mustSnapshot(t, e.m)); got != string(snap) {
		t.Fatalf("restoring over an unfinished group left %v and %v", e.values("t", 0), e.values("t", 1))
	}
	if e.m.pendingProduce() {
		t.Fatal("the group is still pending after the restore")
	}
}

func TestAJournalWrittenForOneProduceByAnEarlierVersionIsStillFinished(t *testing.T) {
	e := newMachineEnv(t)
	e.apply(1, createCmd("events", 1))
	e.apply(2, produceCmd("events", 0, "x"))
	// Entry 3, "a".."d", was begun: the old journal said where it started and how many
	// messages it had, and the process died after two of them.
	tp, err := e.b.lookup("events")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"a", "b"} {
		if _, err := tp.parts[0].log.Append(storage.Message{Key: []byte("k"), Value: []byte(v), Timestamp: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	old := `{"index":3,"topic":"events","partition":0,"start":1,"count":4}`
	if err := os.WriteFile(filepath.Join(e.dir, "machine", journalName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	e.restart()

	batch := produceCmd("events", 0, "a", "b", "c", "d")
	for i, c := range [][]byte{createCmd("events", 1), produceCmd("events", 0, "x"), batch} {
		e.m.Apply(entryAt(uint64(i+1), c))
	}
	if got := e.values("events", 0); !reflect.DeepEqual(got, []string{"x", "a", "b", "c", "d"}) {
		t.Fatalf("partition holds %v, want [x a b c d]", got)
	}
}

func TestTheGroupSizeIsBounded(t *testing.T) {
	e := newMachineEnv(t)
	e.m.Apply(entryAt(1, createCmd("t", 1)))
	var entries []raft.Entry
	for i := 0; i < 517; i++ { // two full groups of 256 and a few more
		entries = append(entries, entryAt(uint64(i+2), produceCmd("t", 0, fmt.Sprint(i))))
	}
	intents := 0
	e.m.hook = func(p string) {
		if p == "produce:intent" {
			intents++
		}
	}
	e.m.ApplyBatch(entries)
	if intents != 3 {
		t.Fatalf("%d groups for %d commands, want 3 (groups are limited to 256 commands)", intents, len(entries))
	}
	if got := e.values("t", 0); len(got) != len(entries) || got[0] != "0" || got[len(got)-1] != fmt.Sprint(len(entries)-1) {
		t.Fatalf("partition holds %d messages, first %q", len(got), got[0])
	}
}
