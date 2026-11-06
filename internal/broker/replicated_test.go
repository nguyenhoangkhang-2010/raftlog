package broker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

type nopTransport struct{}

func (nopTransport) Send(context.Context, raft.Message) error { return nil }

// replicatedEnv is the broker of a one-node cluster: the node is its own
// majority, so it commits at once, and everything else is the real thing.
type replicatedEnv struct {
	t      *testing.T
	b      *Broker
	m      *Machine
	runner *raft.Runner
	s      *Replicated
}

func newReplicatedEnv(t *testing.T, snapshotEvery uint64) *replicatedEnv {
	t.Helper()
	dir := t.TempDir()
	b, err := Open(filepath.Join(dir, "data"), Options{MaxPartitions: 4})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMachine(b, filepath.Join(dir, "machine"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := raft.NewFileStorage(filepath.Join(dir, "raft", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := raft.OpenFileLog(filepath.Join(dir, "raft", "log.wal"))
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := raft.NewFileSnapshotStore(filepath.Join(dir, "raft", "snapshot.bin"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := raft.NewRunner(raft.Config{
		ID: 1, Cluster: []raft.NodeID{1}, ElectionTicks: 5, HeartbeatTicks: 1,
		Storage: store, Log: log, Snapshots: snaps,
	}, nopTransport{}, 2*time.Millisecond, raft.WithStateMachine(m), raft.WithSnapshotEvery(snapshotEvery))
	if err != nil {
		t.Fatal(err)
	}
	runner.Start()
	t.Cleanup(func() {
		runner.Stop()
		_ = log.Close()
		_ = b.Close()
	})

	deadline := time.Now().Add(10 * time.Second)
	for runner.Status().Role != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("the single node never became leader")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return &replicatedEnv{t: t, b: b, m: m, runner: runner, s: NewReplicated(m, runner)}
}

func ctx5() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func TestReplicatedBrokerCreatesProducesAndReads(t *testing.T) {
	e := newReplicatedEnv(t, 0)
	ctx, cancel := ctx5()
	defer cancel()

	if err := e.s.CreateTopic(ctx, "events", 2); err != nil {
		t.Fatal(err)
	}
	part, base, err := e.s.Produce(ctx, "events", 1, []storage.Message{msg("k", "a"), msg("k", "b")})
	if err != nil || part != 1 || base != 0 {
		t.Fatalf("Produce = (%d, %d, %v)", part, base, err)
	}
	_, base, err = e.s.Produce(ctx, "events", 1, []storage.Message{msg("k", "c")})
	if err != nil || base != 2 {
		t.Fatalf("second Produce base = %d, err %v; want 2", base, err)
	}
	got, err := e.s.Fetch("events", 1, 0, 10)
	if err != nil || len(got) != 3 || string(got[2].Value) != "c" {
		t.Fatalf("Fetch = %v, %v", got, err)
	}
	// The leader filled in the timestamps before replicating.
	if got[0].Timestamp == 0 {
		t.Fatal("a message was stored without a timestamp")
	}

	if err := e.s.CommitOffset(ctx, "g1", "events", 1, 2); err != nil {
		t.Fatal(err)
	}
	info, err := e.s.GroupOffset("g1", "events", 1)
	if err != nil || !info.Committed || info.Offset != 2 || info.End != 3 || info.Lag() != 1 {
		t.Fatalf("GroupOffset = %+v, %v", info, err)
	}
}

func TestReplicatedBrokerRefusesBadRequestsBeforeReplicatingThem(t *testing.T) {
	e := newReplicatedEnv(t, 0)
	ctx, cancel := ctx5()
	defer cancel()
	if err := e.s.CreateTopic(ctx, "events", 2); err != nil {
		t.Fatal(err)
	}
	before := e.runner.Status().LastIndex

	big := storage.Message{Value: make([]byte, 65*1024*1024)}
	checks := []struct {
		name string
		err  error
		want error
	}{
		{"bad topic name", e.s.CreateTopic(ctx, "a/b", 1), ErrInvalidTopicName},
		{"zero partitions", e.s.CreateTopic(ctx, "x", 0), ErrInvalidPartitions},
		{"too many partitions", e.s.CreateTopic(ctx, "x", 99), ErrInvalidPartitions},
		{"duplicate topic", e.s.CreateTopic(ctx, "events", 2), ErrTopicExists},
		{"bad group", e.s.CommitOffset(ctx, "bad group", "events", 0, 0), ErrInvalidGroup},
		{"negative partition", e.s.CommitOffset(ctx, "g", "events", -1, 0), ErrInvalidPartition},
		{"commit beyond the end", e.s.CommitOffset(ctx, "g", "events", 0, 5), ErrOffsetBeyondEnd},
	}
	for _, c := range checks {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, c.err, c.want)
		}
	}

	produce := func(topic string, partition int, msgs ...storage.Message) error {
		_, _, err := e.s.Produce(ctx, topic, partition, msgs)
		return err
	}
	for name, c := range map[string]struct {
		err  error
		want error
	}{
		"no messages":       {produce("events", 0), ErrEmptyBatch},
		"missing topic":     {produce("nope", 0, msg("k", "v")), ErrTopicNotFound},
		"missing partition": {produce("events", 7, msg("k", "v")), ErrInvalidPartition},
		"oversized message": {produce("events", 0, big), storage.ErrRecordTooLarge},
	} {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, c.err, c.want)
		}
	}

	// Only the three refusals that need the state machine's view of the data
	// (duplicate topic, bad group is checked early, commit beyond end) may have
	// reached the log; the purely invalid ones must not have.
	grew := e.runner.Status().LastIndex - before
	if grew > 2 {
		t.Fatalf("the log grew by %d entries; invalid requests should be refused before they are replicated", grew)
	}
}

func TestReplicatedBrokerChoosesPartitionsItself(t *testing.T) {
	e := newReplicatedEnv(t, 0)
	ctx, cancel := ctx5()
	defer cancel()
	if err := e.s.CreateTopic(ctx, "events", 3); err != nil {
		t.Fatal(err)
	}

	// The same key always lands in the same partition.
	first, _, err := e.s.Produce(ctx, "events", AnyPartition, []storage.Message{msg("user-9", "x")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		p, _, err := e.s.Produce(ctx, "events", AnyPartition, []storage.Message{msg("user-9", "x")})
		if err != nil || p != first {
			t.Fatalf("partition %d (err %v), want %d", p, err, first)
		}
	}
	// Messages without a key are spread over all partitions.
	seen := map[int]bool{}
	for i := 0; i < 9; i++ {
		p, _, err := e.s.Produce(ctx, "events", AnyPartition, []storage.Message{msg("", "x")})
		if err != nil {
			t.Fatal(err)
		}
		seen[p] = true
	}
	if len(seen) != 3 {
		t.Fatalf("keyless messages reached %d of 3 partitions", len(seen))
	}
}

func TestConcurrentProducersGetDistinctContiguousBatches(t *testing.T) {
	e := newReplicatedEnv(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.s.CreateTopic(ctx, "events", 1); err != nil {
		t.Fatal(err)
	}

	const producers, batches, size = 6, 10, 4
	var mu sync.Mutex
	var bases []uint64
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for b := 0; b < batches; b++ {
				msgs := make([]storage.Message, size)
				for i := range msgs {
					msgs[i] = msg("k", fmt.Sprintf("p%d-b%d-m%d", p, b, i))
				}
				_, base, err := e.s.Produce(ctx, "events", 0, msgs)
				if err != nil {
					t.Errorf("Produce: %v", err)
					return
				}
				mu.Lock()
				bases = append(bases, base)
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()

	// Every batch got its own range of offsets: sorted, they are 0, 4, 8, ...
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })
	for i, base := range bases {
		if base != uint64(i*size) {
			t.Fatalf("batch %d starts at offset %d, want %d: batches overlap or leave gaps", i, base, i*size)
		}
	}
	all, err := e.s.Fetch("events", 0, 0, producers*batches*size)
	if err != nil || len(all) != producers*batches*size {
		t.Fatalf("Fetch: %d messages, err %v", len(all), err)
	}
	// And the messages of a batch are adjacent and in order.
	for i := 0; i < len(all); i += size {
		var prefix string
		for j := 0; j < size; j++ {
			v := string(all[i+j].Value)
			if want := fmt.Sprintf("m%d", j); v[len(v)-len(want):] != want {
				t.Fatalf("offset %d holds %q, want a message ending in %q", i+j, v, want)
			}
			if j == 0 {
				prefix = v[:len(v)-2]
			} else if v[:len(v)-2] != prefix {
				t.Fatalf("a batch is interleaved with another at offset %d", i+j)
			}
		}
	}
}

func TestReplicatedBrokerSnapshotsItsOwnState(t *testing.T) {
	e := newReplicatedEnv(t, 5) // snapshot after every 5 applied entries
	ctx, cancel := ctx5()
	defer cancel()
	if err := e.s.CreateTopic(ctx, "events", 1); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := 0; i < 20; i++ {
		v := fmt.Sprintf("m%02d", i)
		want = append(want, v)
		if _, _, err := e.s.Produce(ctx, "events", 0, []storage.Message{msg("k", v)}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.runner.Status().SnapshotIndex == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no snapshot was taken")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Taking a snapshot must not disturb the live data.
	got, err := e.s.Fetch("events", 0, 0, 100)
	if err != nil || len(got) != len(want) {
		t.Fatalf("Fetch after snapshots: %d messages, err %v", len(got), err)
	}
	values := make([]string, len(got))
	for i, m := range got {
		values[i] = string(m.Value)
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("data changed while snapshotting: %v", values)
	}
}

func TestReplicatedBrokerAfterTheNodeStops(t *testing.T) {
	e := newReplicatedEnv(t, 0)
	ctx, cancel := ctx5()
	defer cancel()
	if err := e.s.CreateTopic(ctx, "events", 1); err != nil {
		t.Fatal(err)
	}
	e.runner.Stop()

	// Nothing can commit any more, and the caller learns that promptly.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	_, _, err := e.s.Produce(stopCtx, "events", 0, []storage.Message{msg("k", "v")})
	if err == nil {
		t.Fatal("a produce succeeded on a stopped node")
	}
}
