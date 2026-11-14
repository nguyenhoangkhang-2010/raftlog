package raft

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
)

// --- the node: ProposeBatch -------------------------------------------------------

func (c *cluster) proposeBatch(id NodeID, data ...string) (first, term uint64, err error) {
	c.t.Helper()
	raw := make([][]byte, len(data))
	for i, d := range data {
		raw[i] = []byte(d)
	}
	first, term, err = c.nodes[id].ProposeBatch(raw)
	c.collect(id)
	c.deliver()
	return first, term, err
}

func TestProposeBatchAppendsConsecutiveEntriesAndEveryNodeAppliesThemInOrder(t *testing.T) {
	c := newCluster(t, 3, 11)
	c.runUntil(500, c.hasLeader)
	leader := c.leaders()[0]
	before := c.nodes[leader].log.LastIndex()

	first, term, err := c.proposeBatch(leader, "a", "b", "c", "d")
	if err != nil {
		t.Fatal(err)
	}
	if first != before+1 || term != c.status(leader).Term {
		t.Fatalf("first index %d term %d, want %d and %d", first, term, before+1, c.status(leader).Term)
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		e, err := c.nodes[leader].log.Entries(first+uint64(i), first+uint64(i)+1)
		if err != nil || len(e) != 1 || string(e[0].Data) != want || e[0].Term != term || e[0].Index != first+uint64(i) {
			t.Fatalf("entry %d = %+v (%v), want %q in term %d", first+uint64(i), e, err, want, term)
		}
	}
	c.runUntil(500, c.converged)
	for _, id := range c.ids {
		if got := c.appliedData(id); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
			t.Fatalf("node %d applied %v", id, got)
		}
	}
}

func TestProposeBatchOnAFollowerOrWithNothingChangesNothing(t *testing.T) {
	c := newCluster(t, 3, 12)
	c.runUntil(500, c.hasLeader)
	leader := c.leaders()[0]
	var follower NodeID
	for _, id := range c.ids {
		if id != leader {
			follower = id
		}
	}
	last := c.nodes[follower].log.LastIndex()
	_, _, err := c.nodes[follower].ProposeBatch([][]byte{[]byte("x"), []byte("y")})
	var nl *NotLeaderError
	if !errors.As(err, &nl) || nl.Leader != leader {
		t.Fatalf("a follower answered %v, want NotLeaderError naming %d", err, leader)
	}
	if c.nodes[follower].log.LastIndex() != last {
		t.Fatal("a refused batch changed the follower's log")
	}
	if _, _, err := c.nodes[leader].ProposeBatch(nil); err == nil {
		t.Fatal("an empty batch was accepted")
	}
}

func TestAFailedBatchAppendsNothing(t *testing.T) {
	st := &memStorage{}
	log := &failingLog{LogStore: NewMemoryLog()}
	n := newTestNode(t, 1, 1, st, log)
	for i := 0; i < 5*testElectionTicks && n.Status().Role != Leader; i++ {
		if err := n.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if n.Status().Role != Leader {
		t.Fatal("a one-node cluster did not elect itself")
	}
	last := log.LastIndex()
	log.failAppend = errors.New("disk full")
	if _, _, err := n.ProposeBatch([][]byte{[]byte("a"), []byte("b")}); err == nil {
		t.Fatal("a batch that could not be written was reported as proposed")
	}
	if log.LastIndex() != last {
		t.Fatalf("the log grew from %d to %d though the append failed", last, log.LastIndex())
	}
}

// --- the runner --------------------------------------------------------------------

// slowLog counts the appends, and can hold the next one until released.
type slowLog struct {
	LogStore
	mu      sync.Mutex
	calls   int
	entries int
	gate    chan struct{} // if set, the next Append waits for it to be closed, once
}

func (l *slowLog) Append(es []Entry) error {
	l.mu.Lock()
	l.calls++
	l.entries += len(es)
	g := l.gate
	l.gate = nil
	l.mu.Unlock()
	if g != nil {
		<-g
	}
	return l.LogStore.Append(es)
}

func (l *slowLog) hold() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gate = make(chan struct{})
	return l.gate
}

func (l *slowLog) counts() (calls, entries int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls, l.entries
}

type nullTransport struct{}

func (nullTransport) Send(context.Context, Message) error { return nil }

// oneNodeRunner starts a cluster of one, which elects itself and commits at once.
func oneNodeRunner(t *testing.T, log LogStore, sm StateMachine, every uint64) *Runner {
	t.Helper()
	cfg := Config{ID: 1, Cluster: []NodeID{1}, ElectionTicks: testElectionTicks, HeartbeatTicks: testHeartbeatTicks, Storage: &memStorage{}, Log: log}
	var opts []RunnerOption
	opts = append(opts, WithStateMachine(sm))
	if every > 0 {
		snaps, err := NewFileSnapshotStore(filepath.Join(t.TempDir(), "snap.bin"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Snapshots = snaps
		opts = append(opts, WithSnapshotEvery(every))
	}
	r, err := NewRunner(cfg, nullTransport{}, realTick, opts...)
	if err != nil {
		t.Fatal(err)
	}
	r.Start()
	t.Cleanup(r.Stop)
	waitFor(t, "the node to elect itself", func() bool { return r.Status().Role == Leader })
	return r
}

// proposeConcurrently proposes every command from its own goroutine and returns the
// results in the order of the commands.
func proposeConcurrently(t *testing.T, r *Runner, cmds []string) []any {
	t.Helper()
	out, _ := proposeConcurrentlyAt(t, r, cmds)
	return out
}

// proposeConcurrentlyAt is proposeConcurrently that also returns the log index each
// command was given.
func proposeConcurrentlyAt(t *testing.T, r *Runner, cmds []string) ([]any, []uint64) {
	t.Helper()
	out := make([]any, len(cmds))
	at := make([]uint64, len(cmds))
	errs := make([]error, len(cmds))
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), waitForState)
			defer cancel()
			at[i], out[i], errs[i] = r.ProposeResult(ctx, []byte(c))
		}(i, c)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("proposal %d (%s): %v", i, cmds[i], err)
		}
	}
	return out, at
}

func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("c%03d", i)
	}
	return out
}

func TestProposalsThatQueueWhileTheLogIsBusyAreAppendedTogether(t *testing.T) {
	log := &slowLog{LogStore: NewMemoryLog()}
	kv := &kvMachine{}
	r := oneNodeRunner(t, log, kv, 0)
	callsBefore, entriesBefore := log.counts()

	// The first proposal is held in the middle of its append; the others queue behind it.
	gate := log.hold()
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release) // runs before the runner is stopped, so a failure cannot leave it stuck
	const n = 40
	cmds := numbered(n)
	type outcome struct {
		results []any
		at      []uint64
	}
	done := make(chan outcome, 1)
	go func() {
		res, at := proposeConcurrentlyAt(t, r, cmds)
		done <- outcome{res, at}
	}()
	// The first group is held in its append. It holds however many proposals had arrived
	// when it was taken, at least one; everything else waits in the queue.
	waitFor(t, "the rest of the proposals to queue", func() bool {
		calls, entries := log.counts()
		return calls-callsBefore == 1 && r.queued() == n-(entries-entriesBefore)
	})
	release()
	finished := <-done
	results := finished.results

	// The index each caller was given is where its command is in the log.
	for i, c := range cmds {
		e, err := log.LogStore.Entries(finished.at[i], finished.at[i]+1)
		if err != nil || len(e) != 1 || string(e[0].Data) != c {
			t.Fatalf("command %s was told it is at index %d, where the log has %+v (%v)", c, finished.at[i], e, err)
		}
	}

	calls, entries := log.counts()
	if calls-callsBefore != 2 || entries-entriesBefore != n {
		t.Fatalf("%d proposals took %d appends of %d entries, want 2 appends (the held group, and one for all that queued behind it)",
			n, calls-callsBefore, entries-entriesBefore)
	}
	// Every caller got its own outcome: the state machine numbers the commands it applies.
	var counts []int
	for _, v := range results {
		counts = append(counts, v.(int))
	}
	sort.Ints(counts)
	for i, c := range counts {
		if c != i+1 {
			t.Fatalf("outcomes %v: not the numbers 1..%d, each once", counts, n)
		}
	}
	got := kv.commands()
	sort.Strings(got)
	if !reflect.DeepEqual(got, cmds) {
		t.Fatalf("the state machine applied %v", got)
	}
}

func TestAProposalWhoseCallerGaveUpWhileItWasQueuedDoesNotBlockTheOthers(t *testing.T) {
	log := &slowLog{LogStore: NewMemoryLog()}
	kv := &kvMachine{}
	r := oneNodeRunner(t, log, kv, 0)

	release := log.hold()
	first := make(chan error, 1)
	go func() {
		_, _, err := r.ProposeResult(context.Background(), []byte("first"))
		first <- err
	}()
	waitFor(t, "the first append to start", func() bool { c, _ := log.counts(); return c >= 2 })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := r.ProposeResult(ctx, []byte("impatient"))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("a proposal whose context ended while it waited returned %v after %v", err, time.Since(start))
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// The runner still works.
	if _, err := r.Propose(context.Background(), []byte("after")); err != nil {
		t.Fatal(err)
	}
	// The impatient command was queued before its caller left, so it is applied as well:
	// like any proposal whose caller gives up, it may still commit.
	waitFor(t, "the queued command to be applied", func() bool { return len(kv.commands()) == 3 })
}

// batchKV is a state machine that applies several entries in one call, and can hold the
// first call until released so that the next entries queue up behind it.
type batchKV struct {
	kvMachine
	mu      sync.Mutex
	sizes   []int
	gate    chan struct{}
	snapped []int // how many commands had been applied at each snapshot
}

func (m *batchKV) ApplyBatch(es []Entry) []any {
	m.mu.Lock()
	m.sizes = append(m.sizes, len(es))
	g := m.gate
	m.gate = nil
	m.mu.Unlock()
	if g != nil {
		<-g
	}
	out := make([]any, len(es))
	for i, e := range es {
		m.kvMachine.Apply(e)
		out[i] = "answer to " + string(e.Data)
	}
	return out
}

func (m *batchKV) Snapshot() ([]byte, error) {
	m.mu.Lock()
	m.snapped = append(m.snapped, len(m.kvMachine.commands()))
	m.mu.Unlock()
	return m.kvMachine.Snapshot()
}

func (m *batchKV) batchSizes() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.sizes...)
}

func TestACommittedBacklogIsGivenToABatchStateMachineTogetherAndEachCallerGetsItsOwnAnswer(t *testing.T) {
	log := &slowLog{LogStore: NewMemoryLog()}
	sm := &batchKV{}
	r := oneNodeRunner(t, log, sm, 0)

	const n = 30
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release) // runs before the runner is stopped, so a failure cannot leave it stuck
	sm.mu.Lock()
	sm.gate = gate
	sm.mu.Unlock()

	cmds := numbered(n)
	done := make(chan []any, 1)
	go func() { done <- proposeConcurrently(t, r, cmds) }()
	// The first call is held. It got however many entries had been committed when it
	// started, at least one; the rest are committed and wait for it.
	waitFor(t, "the backlog to build up", func() bool {
		sizes := sm.batchSizes()
		return len(sizes) == 1 && r.Observe().ApplyBacklog == n-sizes[0]
	})
	first := sm.batchSizes()[0]
	release()
	results := <-done

	want := []int{first}
	if first < n {
		want = append(want, n-first) // everything that had queued, in one call
	}
	if sizes := sm.batchSizes(); !reflect.DeepEqual(sizes, want) {
		t.Fatalf("the state machine was called with batches of %v, want %v", sizes, want)
	}
	if n-first < 2 && first < 2 {
		t.Fatal("no call had more than one entry: the test did not exercise batching")
	}
	for i, v := range results {
		if v != "answer to "+cmds[i] {
			t.Fatalf("caller %d got %v: the answers were not matched to their commands", i, v)
		}
	}
}

func TestSnapshotsFallAtTheSameEntriesWhetherOrNotTheStateMachineAppliesInBatches(t *testing.T) {
	run := func(sm StateMachine, hold func()) {
		log := &slowLog{LogStore: NewMemoryLog()}
		r := oneNodeRunner(t, log, sm, 10)
		hold()
		proposeConcurrently(t, r, numbered(47))
		waitFor(t, "the snapshots", func() bool { return r.Status().SnapshotIndex >= 40 })
	}

	plain := &recordingKV{}
	run(plain, func() {})

	batched := &batchKV{}
	run(batched, func() {
		batched.mu.Lock()
		batched.gate = make(chan struct{})
		g := batched.gate
		batched.mu.Unlock()
		go func() { time.Sleep(200 * time.Millisecond); close(g) }() // let a backlog build
	})

	batched.mu.Lock()
	got := append([]int(nil), batched.snapped...)
	batched.mu.Unlock()
	plain.mu.Lock()
	want := append([]int(nil), plain.snapped...)
	plain.mu.Unlock()

	if len(want) < 4 || !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshots were taken after %v commands when applied in batches, and %v one by one", got, want)
	}
	big := false
	for _, s := range batched.batchSizes() {
		big = big || s > 1
	}
	if !big {
		t.Fatalf("no batch had more than one entry (%v): the test did not exercise batching", batched.batchSizes())
	}
}

// recordingKV is kvMachine that notes how many commands it held at each snapshot.
type recordingKV struct {
	kvMachine
	mu      sync.Mutex
	snapped []int
}

func (m *recordingKV) Snapshot() ([]byte, error) {
	m.mu.Lock()
	m.snapped = append(m.snapped, len(m.kvMachine.commands()))
	m.mu.Unlock()
	return m.kvMachine.Snapshot()
}
