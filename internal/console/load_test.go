package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// fakeLog is an in-memory topic that can misbehave on request.
type fakeLog struct {
	mu      sync.Mutex
	created bool
	parts   [][]string
	calls   int
	next    int

	failFirst int           // the first N produces fail
	delay     time.Duration // every produce takes this long
	dropEvery int           // acknowledge every Nth message but do not store it: a lost write
	dupEvery  int           // store every Nth message twice
	hideLast  int           // while hideReads > 0, a fetch does not show the last N records
	hideReads int
}

func newFakeLog(partitions int) *fakeLog { return &fakeLog{parts: make([][]string, partitions)} }

func (f *fakeLog) CreateTopic(context.Context, string, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created {
		return client.ErrAlreadyExists
	}
	f.created = true
	return nil
}

func (f *fakeLog) Produce(ctx context.Context, _ string, msgs []client.Message, _ ...client.ProduceOption) (client.ProduceResult, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	delay := f.delay
	fail := n <= f.failFirst
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return client.ProduceResult{}, ctx.Err()
		}
	}
	if fail {
		return client.ProduceResult{}, client.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.next % len(f.parts)
	f.next++
	if f.dropEvery > 0 && n%f.dropEvery == 0 {
		return client.ProduceResult{Partition: p, Offset: uint64(len(f.parts[p]))}, nil // acknowledged, never stored
	}
	off := uint64(len(f.parts[p]))
	f.parts[p] = append(f.parts[p], string(msgs[0].Value))
	if f.dupEvery > 0 && n%f.dupEvery == 0 {
		f.parts[p] = append(f.parts[p], string(msgs[0].Value))
	}
	return client.ProduceResult{Partition: p, Offset: off}, nil
}

func (f *fakeLog) Fetch(_ context.Context, _ string, p int, offset uint64, limit int, _ ...client.FetchOption) ([]client.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p >= len(f.parts) {
		return nil, client.ErrInvalidArgument
	}
	vals := f.parts[p]
	if f.hideReads > 0 {
		if p == len(f.parts)-1 {
			f.hideReads-- // count one lagging read per round over all partitions
		}
		cut := len(vals) - f.hideLast
		if cut < 0 {
			cut = 0
		}
		vals = vals[:cut]
	}
	if offset > uint64(len(vals)) {
		return nil, client.ErrOffsetOutOfRange
	}
	var out []client.Record
	for i := int(offset); i < len(vals) && len(out) < limit; i++ {
		out = append(out, client.Record{Offset: uint64(i), Value: []byte(vals[i])})
	}
	return out, nil
}

func (f *fakeLog) stored() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.parts {
		n += len(p)
	}
	return n
}

func loadCfg() LoadConfig {
	return LoadConfig{Topic: "demo", Partitions: 2, Workers: 4, RequestTimeout: 2 * time.Second, MaxRate: 1000, MaxTracked: 10_000, Window: 5 * time.Second}
}

func mustLoad(t *testing.T, f LogClient, mutate func(*LoadConfig)) *Load {
	t.Helper()
	cfg := loadCfg()
	if mutate != nil {
		mutate(&cfg)
	}
	l, err := NewLoad(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Stop)
	return l
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runFor writes at the rate for about d, stops, and waits until nothing is in flight.
func runFor(t *testing.T, l *Load, rate int, d time.Duration) {
	t.Helper()
	if err := l.Start(rate); err != nil {
		t.Fatal(err)
	}
	time.Sleep(d)
	l.Stop()
	settled := func() bool {
		s := l.Stats()
		return s.Sent == s.Acked+s.Failed
	}
	waitFor(t, "the writes in flight to finish", settled)
	// A worker that had just taken a task when Stop came may still start it; give
	// it the moment it needs, then check again.
	time.Sleep(100 * time.Millisecond)
	waitFor(t, "the writes in flight to finish, again", settled)
}

func TestTheLoadConfigurationMustBeComplete(t *testing.T) {
	bad := []func(*LoadConfig){
		func(c *LoadConfig) { c.Topic = "" }, func(c *LoadConfig) { c.Partitions = 0 }, func(c *LoadConfig) { c.Workers = 0 },
		func(c *LoadConfig) { c.RequestTimeout = 0 }, func(c *LoadConfig) { c.MaxRate = 0 }, func(c *LoadConfig) { c.MaxTracked = 0 },
		func(c *LoadConfig) { c.Window = 0 },
	}
	for i, mutate := range bad {
		cfg := loadCfg()
		mutate(&cfg)
		if _, err := NewLoad(newFakeLog(2), cfg); err == nil {
			t.Errorf("case %d: accepted", i)
		}
	}
}

func TestTheLoadWritesAtTheChosenRateAndCountsWhatTheClusterConfirmed(t *testing.T) {
	f := newFakeLog(2)
	l := mustLoad(t, f, nil)
	runFor(t, l, 200, 700*time.Millisecond)

	s := l.Stats()
	// 200/s for 0.7 s is 140; allow for the ticker and the machine.
	if s.Acked < 60 || s.Acked > 160 || s.Failed != 0 || s.Sent != s.Acked || s.Running || s.Tracked != int(s.Acked) {
		t.Fatalf("stats = %+v", s)
	}
	if f.stored() != int(s.Acked) {
		t.Fatalf("%d messages stored for %d acknowledged", f.stored(), s.Acked)
	}
	if !f.created {
		t.Fatal("the topic was not created")
	}
}

func TestTheRateIsBounded(t *testing.T) {
	l := mustLoad(t, newFakeLog(2), func(c *LoadConfig) { c.MaxRate = 50 })
	for _, rate := range []int{0, -3, 51} {
		if err := l.Start(rate); err == nil {
			t.Errorf("rate %d accepted", rate)
		}
	}
	if err := l.Start(50); err != nil {
		t.Fatalf("the highest allowed rate: %v", err)
	}
	if err := l.Start(10); err == nil {
		t.Error("a second start while running was accepted")
	}
	l.Stop()
	l.Stop() // stopping twice is harmless
}

func TestFailedWritesAreCountedAndExplained(t *testing.T) {
	f := newFakeLog(2)
	f.failFirst = 5
	l := mustLoad(t, f, nil)
	runFor(t, l, 200, 600*time.Millisecond)
	s := l.Stats()
	if s.Failed != 5 || s.Acked == 0 || s.Sent != s.Acked+s.Failed {
		t.Fatalf("stats = %+v, want exactly the first 5 writes failed", s)
	}
	if !strings.Contains(s.LastError, "unavailable") {
		t.Fatalf("last error = %q", s.LastError)
	}
}

func TestVerifyFindsEveryAcknowledgedWriteWhenNothingWasLost(t *testing.T) {
	f := newFakeLog(2)
	l := mustLoad(t, f, nil)
	runFor(t, l, 300, 500*time.Millisecond)
	s := l.Stats()

	v, err := l.Verify(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if v.Missing != 0 || v.Acked != int(s.Acked) || v.Stored != int(s.Acked) || v.Duplicates != 0 || len(v.MissingExamples) != 0 {
		t.Fatalf("verification = %+v for %d acknowledged writes", v, s.Acked)
	}
}

func TestVerifyCatchesAnAcknowledgedWriteThatWasLost(t *testing.T) {
	f := newFakeLog(2)
	f.dropEvery = 10 // every tenth acknowledged write is not stored
	l := mustLoad(t, f, nil)
	runFor(t, l, 300, 500*time.Millisecond)
	s := l.Stats()

	start := time.Now()
	v, err := l.Verify(context.Background(), 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	want := int(s.Acked) / 10
	if v.Missing < want-1 || v.Missing > want+1 || v.Missing == 0 {
		t.Fatalf("missing = %d, want about %d of %d", v.Missing, want, s.Acked)
	}
	if len(v.MissingExamples) == 0 || len(v.MissingExamples) > 5 {
		t.Fatalf("examples = %v", v.MissingExamples)
	}
	// It looked again for a while before giving up on them.
	if time.Since(start) < 300*time.Millisecond {
		t.Fatalf("Verify gave up after %v, before its patience ran out", time.Since(start))
	}
}

func TestVerifyIsPatientWithANodeThatIsBehind(t *testing.T) {
	f := newFakeLog(2)
	l := mustLoad(t, f, nil)
	runFor(t, l, 200, 400*time.Millisecond)

	f.mu.Lock()
	f.hideLast, f.hideReads = 3, 2 // the first two reads miss the newest records
	f.mu.Unlock()
	v, err := l.Verify(context.Background(), 5*time.Second)
	if err != nil || v.Missing != 0 {
		t.Fatalf("verification = %+v, %v: a lagging node showed the records a moment later", v, err)
	}
}

func TestStoringAWriteTwiceIsADuplicateNotALoss(t *testing.T) {
	f := newFakeLog(2)
	f.dupEvery = 7
	l := mustLoad(t, f, nil)
	runFor(t, l, 300, 400*time.Millisecond)
	v, err := l.Verify(context.Background(), time.Second)
	if err != nil || v.Missing != 0 || v.Duplicates == 0 || v.Stored != int(l.Stats().Acked)+v.Duplicates {
		t.Fatalf("verification = %+v, %v", v, err)
	}
}

func TestWritesDueWhileEveryWorkerIsBusyAreCountedAsNotSent(t *testing.T) {
	f := newFakeLog(2)
	f.delay = 150 * time.Millisecond
	l := mustLoad(t, f, func(c *LoadConfig) { c.Workers = 1 })
	if err := l.Start(500); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "writes to be dropped", func() bool { return l.Stats().Dropped > 0 })
	l.Stop()
	s := l.Stats()
	if s.Sent > 5 || s.Dropped < 5 {
		t.Fatalf("stats = %+v: a single slow writer cannot send 500 a second", s)
	}
}

func TestLatencyAndThroughputAreMeasured(t *testing.T) {
	f := newFakeLog(2)
	f.delay = 30 * time.Millisecond
	l := mustLoad(t, f, nil)
	if err := l.Start(100); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "some acknowledged writes", func() bool { return l.Stats().Acked >= 20 })
	s := l.Stats()
	l.Stop()
	if s.Throughput <= 0 || s.P50Ms < 25 || s.P99Ms < s.P50Ms || s.P99Ms > 2000 {
		t.Fatalf("stats = %+v: every write took at least 30 ms", s)
	}
}

func TestOnlyTheFirstWritesAreRememberedForVerification(t *testing.T) {
	f := newFakeLog(2)
	l := mustLoad(t, f, func(c *LoadConfig) { c.MaxTracked = 10 })
	runFor(t, l, 300, 400*time.Millisecond)
	s := l.Stats()
	if s.Acked <= 10 {
		t.Skip("too few writes happened to reach the limit")
	}
	v, err := l.Verify(context.Background(), time.Second)
	if err != nil || v.Acked != 10 || v.Untracked != s.Acked-10 || v.Missing != 0 {
		t.Fatalf("verification = %+v, %v; stats %+v", v, err, s)
	}
}

func TestResetForgetsARunButNotWhileItGoesOn(t *testing.T) {
	f := newFakeLog(2)
	l := mustLoad(t, f, nil)
	if err := l.Start(100); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a write", func() bool { return l.Stats().Acked > 0 })
	l.Reset() // refused: it is running
	if l.Stats().Acked == 0 {
		t.Fatal("Reset wiped a run that was going on")
	}
	l.Stop()
	waitFor(t, "writes in flight", func() bool { s := l.Stats(); return s.Sent == s.Acked+s.Failed })
	l.Reset()
	if s := l.Stats(); s.Acked != 0 || s.Sent != 0 || s.Tracked != 0 || s.LastError != "" {
		t.Fatalf("after Reset: %+v", s)
	}
	if v, err := l.Verify(context.Background(), 100*time.Millisecond); err != nil || v.Acked != 0 {
		t.Fatalf("verification after a reset: %+v, %v", v, err)
	}
}

func TestAFailingReadIsAnErrorNotAVerdict(t *testing.T) {
	var calls atomic.Int32
	f := &errFetchLog{fakeLog: newFakeLog(2), calls: &calls}
	l := mustLoad(t, f, nil)
	runFor(t, l, 100, 200*time.Millisecond)
	if _, err := l.Verify(context.Background(), 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "read partition") {
		t.Fatalf("err = %v, want a read error rather than a verdict", err)
	}
	if calls.Load() == 0 {
		t.Fatal("it never tried to read")
	}
}

type errFetchLog struct {
	*fakeLog
	calls *atomic.Int32
}

func (e *errFetchLog) Fetch(context.Context, string, int, uint64, int, ...client.FetchOption) ([]client.Record, error) {
	e.calls.Add(1)
	return nil, errors.New("the node is down")
}

func TestTheValuesOfOneRunDoNotCollideWithTheNext(t *testing.T) {
	f := newFakeLog(2)
	l := mustLoad(t, f, nil)
	runFor(t, l, 200, 300*time.Millisecond)
	first := l.Stats().Acked
	time.Sleep(5 * time.Millisecond) // a different start time gives a different run ID
	runFor(t, l, 200, 300*time.Millisecond)
	seen := map[string]bool{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.parts {
		for _, v := range p {
			if seen[v] {
				t.Fatalf("the value %q was written twice", v)
			}
			seen[v] = true
		}
	}
	if uint64(len(seen)) != first+(l.Stats().Acked-first) {
		t.Fatalf("%d distinct values for %d acknowledged writes", len(seen), l.Stats().Acked)
	}
	_ = fmt.Sprint(first)
}
