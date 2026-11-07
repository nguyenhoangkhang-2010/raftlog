package waitingroom

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

func TestNewValidatesItsInput(t *testing.T) {
	good := testConfig()
	if _, err := New(nil, good); err == nil {
		t.Error("accepted a nil log")
	}
	log := newClient(t, startBroker(t))
	for name, mutate := range map[string]func(*Config){
		"same topic twice": func(c *Config) { c.GateTopic = c.QueueTopic },
		"empty topic":      func(c *Config) { c.QueueTopic = "" },
		"no poll wait":     func(c *Config) { c.PollWait = 0 },
		"no retry pause":   func(c *Config) { c.RetryPause = 0 },
		"no fetch limit":   func(c *Config) { c.FetchLimit = 0 },
		"no timeout":       func(c *Config) { c.RequestTimeout = 0 },
	} {
		c := good
		mutate(&c)
		if _, err := New(log, c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestJoinGivesPlacesInTheOrderOfArrival(t *testing.T) {
	r := startRoom(t, newClient(t, startBroker(t)), testConfig())
	for i := 0; i < 8; i++ {
		st, err := r.Join(bg(t), fmt.Sprintf("t%d", i), fmt.Sprintf("student %d", i))
		if err != nil {
			t.Fatal(err)
		}
		if st.State != StateWaiting || st.Position != i+1 || st.User != fmt.Sprintf("student %d", i) {
			t.Fatalf("join %d: %+v, want waiting at position %d", i, st, i+1)
		}
	}
}

func TestJoiningAgainKeepsThePlaceAndWritesNothing(t *testing.T) {
	log := newClient(t, startBroker(t))
	r := startRoom(t, log, testConfig())
	first, err := r.Join(bg(t), "a", "Ann")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(bg(t), "b", "Bob"); err != nil {
		t.Fatal(err)
	}
	again, err := r.Join(bg(t), "a", "A different label")
	if err != nil {
		t.Fatal(err)
	}
	if again.Position != first.Position || again.Offset != first.Offset || again.User != "Ann" {
		t.Fatalf("second join = %+v, want the first place %+v", again, first)
	}
	pos, err := log.GroupPosition(bg(t), "probe", DefaultQueueTopic, 0)
	if err != nil || pos.End != 2 {
		t.Fatalf("the queue log holds %d events (err %v), want 2", pos.End, err)
	}
}

func TestManyJoinsOfTheSameTicketTakeOnePlace(t *testing.T) {
	// A browser that retries, or a double click: several requests race. The log
	// may hold more than one event, but the room has one ticket.
	r := startRoom(t, newClient(t, startBroker(t)), testConfig())
	var wg sync.WaitGroup
	results := make([]Status, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := r.Join(bg(t), "same", "x")
			if err != nil {
				t.Error(err)
			}
			results[i] = st
		}(i)
	}
	wg.Wait()
	if s := r.State().Stats(); s.Total != 1 {
		t.Fatalf("Total = %d, want 1", s.Total)
	}
	for i, st := range results {
		if st.Position != 1 || st.Offset != results[0].Offset {
			t.Fatalf("request %d got %+v, want the same place as request 0 (%+v)", i, st, results[0])
		}
	}
}

func TestJoinRefusesBadInput(t *testing.T) {
	r := startRoom(t, newClient(t, startBroker(t)), testConfig())
	if _, err := r.Join(bg(t), "has space", "x"); !errors.Is(err, ErrInvalidTicket) {
		t.Errorf("bad ticket: err = %v", err)
	}
	if _, err := r.Join(bg(t), "ok", "two\nlines"); !errors.Is(err, ErrInvalidUser) {
		t.Errorf("bad user: err = %v", err)
	}
	if s := r.State().Stats(); s.Total != 0 || s.QueueEnd != 0 {
		t.Errorf("a refused join left a trace: %+v", s)
	}
}

func TestAdmitLetsInTheFirstOnesAndEveryoneElseMovesUp(t *testing.T) {
	r := startRoom(t, newClient(t, startBroker(t)), testConfig())
	for i := 0; i < 6; i++ {
		if _, err := r.Join(bg(t), fmt.Sprintf("t%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	n, err := r.Admit(bg(t), 2)
	if err != nil || n != 2 {
		t.Fatalf("Admit(2) = %d, %v", n, err)
	}
	for i, want := range []string{StateAdmitted, StateAdmitted, StateWaiting, StateWaiting} {
		if got := r.Status(fmt.Sprintf("t%d", i)); got.State != want {
			t.Fatalf("t%d: %+v, want %s", i, got, want)
		}
	}
	if st := r.Status("t2"); st.Position != 1 {
		t.Fatalf("t2: %+v, want position 1", st)
	}
	if st := r.Status("t5"); st.Position != 4 {
		t.Fatalf("t5: %+v, want position 4", st)
	}

	// A batch larger than the queue admits only who is there.
	n, err = r.Admit(bg(t), 100)
	if err != nil || n != 4 {
		t.Fatalf("Admit(100) = %d, %v; want 4", n, err)
	}
	// Nobody waiting: nothing is written.
	end := r.State().Stats().GateEnd
	if n, err := r.Admit(bg(t), 5); err != nil || n != 0 {
		t.Fatalf("Admit on an empty queue = %d, %v", n, err)
	}
	if got := r.State().Stats().GateEnd; got != end {
		t.Fatalf("an admission of nobody wrote to the gate log (%d -> %d)", end, got)
	}
}

func TestLateArrivalsWaitBehindThoseAlreadyAdmitted(t *testing.T) {
	r := startRoom(t, newClient(t, startBroker(t)), testConfig())
	for i := 0; i < 3; i++ {
		if _, err := r.Join(bg(t), fmt.Sprintf("early%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Admit(bg(t), 3); err != nil {
		t.Fatal(err)
	}
	st, err := r.Join(bg(t), "late", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateWaiting || st.Position != 1 {
		t.Fatalf("a ticket that joins after the gate moved: %+v, want waiting at position 1", st)
	}
}

func TestTheControllerAdmitsEveryoneInOrder(t *testing.T) {
	cfg := testConfig()
	r := startRoom(t, newClient(t, startBroker(t)), cfg)

	const people = 25
	for i := 0; i < people; i++ {
		if _, err := r.Join(bg(t), fmt.Sprintf("t%02d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	ctx := bg(t)
	go func() { _ = r.RunController(ctx, 10*time.Millisecond, 4) }()

	eventually(t, "everybody admitted", func() bool { return r.State().Stats().Admitted == people })
	// The gate admits a prefix of the queue, so the admitted tickets are exactly
	// the joined ones, in the order they joined.
	for i, tk := range r.State().Admitted() {
		if want := fmt.Sprintf("t%02d", i); tk.ID != want {
			t.Fatalf("admitted #%d is %s, want %s", i, tk.ID, want)
		}
	}
	// With several admissions seen, the rate is measured.
	if st := r.State().Stats(); st.AdmitRate == nil || *st.AdmitRate <= 0 {
		t.Fatalf("no admission rate was measured: %+v", st)
	}
}

func TestARoomRebuiltFromTheLogsIsTheSameRoom(t *testing.T) {
	// A restarted or second web process holds nothing and reads the logs.
	log := newClient(t, startBroker(t))
	a := startRoom(t, log, testConfig())
	for i := 0; i < 10; i++ {
		if _, err := a.Join(bg(t), fmt.Sprintf("t%d", i), fmt.Sprintf("u%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Admit(bg(t), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Admit(bg(t), 3); err != nil {
		t.Fatal(err)
	}

	b := startRoom(t, log, testConfig())
	eventually(t, "the second room to read both logs", func() bool {
		sa, sb := a.State().Stats(), b.State().Stats()
		return sb.QueueEnd == sa.QueueEnd && sb.GateEnd == sa.GateEnd
	})
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("t%d", i)
		sa, sb := a.Status(id), b.Status(id)
		if sa.State != sb.State || sa.Position != sb.Position || sa.Offset != sb.Offset || sa.User != sb.User {
			t.Fatalf("%s: first room says %+v, rebuilt room says %+v", id, sa, sb)
		}
	}
	if got := b.State().Stats().Admitted; got != 7 {
		t.Fatalf("rebuilt room has %d admitted, want 7", got)
	}
}

func TestAControllerThatRestartsDoesNotAdmitAgain(t *testing.T) {
	log := newClient(t, startBroker(t))
	a := startRoom(t, log, testConfig())
	for i := 0; i < 10; i++ {
		if _, err := a.Join(bg(t), fmt.Sprintf("t%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Admit(bg(t), 4); err != nil {
		t.Fatal(err)
	}

	// A new process takes over: it must read the 4 admissions before it acts.
	b := startRoom(t, log, testConfig())
	if err := b.WaitCaughtUp(bg(t)); err != nil {
		t.Fatal(err)
	}
	if got := b.State().Stats().Admitted; got != 4 {
		t.Fatalf("after catching up the new process sees %d admitted, want 4", got)
	}
	if n, err := b.Admit(bg(t), 2); err != nil || n != 2 {
		t.Fatalf("Admit(2) = %d, %v", n, err)
	}
	eventually(t, "both rooms to agree", func() bool { return a.State().Stats().Admitted == 6 && b.State().Stats().Admitted == 6 })
}

// slowGate delays the reads of the gate log, as a node that is catching up would.
type slowGate struct {
	Log
	topic string
	delay time.Duration
}

func (s slowGate) Fetch(ctx context.Context, topic string, p int, off uint64, limit int, opts ...client.FetchOption) ([]client.Record, error) {
	if topic == s.topic {
		time.Sleep(s.delay)
	}
	return s.Log.Fetch(ctx, topic, p, off, limit, opts...)
}

func TestAControllerReadsTheHistoryBeforeItActs(t *testing.T) {
	log := newClient(t, startBroker(t))
	a := startRoom(t, log, testConfig())
	for i := 0; i < 6; i++ {
		if _, err := a.Join(bg(t), fmt.Sprintf("t%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Admit(bg(t), 6); err != nil {
		t.Fatal(err)
	}
	before, err := log.GroupPosition(bg(t), "probe", DefaultGateTopic, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A new process whose view of the gate arrives late. The queue it reads at
	// once, so for a moment it sees six people waiting who are in fact admitted.
	cfg := testConfig()
	b, err := New(slowGate{Log: log, topic: cfg.GateTopic, delay: 400 * time.Millisecond}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg(t))
	defer cancel()
	go func() { _ = b.Run(ctx) }()
	ctrlDone := make(chan error, 1)
	go func() { ctrlDone <- b.RunController(ctx, 10*time.Millisecond, 3) }()

	eventually(t, "the new process to read the whole history", func() bool {
		s := b.State().Stats()
		return s.Admitted == 6 && s.QueueEnd == 6
	})
	time.Sleep(100 * time.Millisecond) // give a controller that did not wait time to act
	after, err := log.GroupPosition(bg(t), "probe", DefaultGateTopic, 0)
	if err != nil {
		t.Fatal(err)
	}
	if after.End != before.End {
		t.Fatalf("the gate log grew from %d to %d records: the controller acted on a room it had not finished reading", before.End, after.End)
	}
	cancel()
	<-ctrlDone
}

// countingLog counts the reads of the log.
type countingLog struct {
	Log
	fetches atomic.Int64
}

func (c *countingLog) Fetch(ctx context.Context, topic string, p int, off uint64, limit int, opts ...client.FetchOption) ([]client.Record, error) {
	c.fetches.Add(1)
	return c.Log.Fetch(ctx, topic, p, off, limit, opts...)
}

func TestAnIdleRoomWaitsInsteadOfPolling(t *testing.T) {
	counting := &countingLog{Log: newClient(t, startBroker(t))}
	r := startRoom(t, counting, testConfig())
	for i := 0; i < 3; i++ {
		if _, err := r.Join(bg(t), fmt.Sprintf("t%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // let the reads settle into their waits
	before := counting.fetches.Load()
	time.Sleep(500 * time.Millisecond)
	if n := counting.fetches.Load() - before; n > 4 {
		t.Fatalf("an idle room read the log %d times in half a second: it should wait for news, not ask again and again", n)
	}
}

func TestTheRoomRidesThroughAnOutageOfTheLog(t *testing.T) {
	flaky := &flakyLog{Log: newClient(t, startBroker(t))}
	r := startRoom(t, flaky, testConfig())
	if _, err := r.Join(bg(t), "before", ""); err != nil {
		t.Fatal(err)
	}

	// Writes fail: the join is refused, loudly, and leaves nothing behind.
	flaky.failWrites.Store(true)
	if _, err := r.Join(bg(t), "during", ""); err == nil {
		t.Fatal("a join succeeded while the log refused writes")
	}
	if st := r.Status("during"); st.State != StateUnknown {
		t.Fatalf("a failed join left %+v", st)
	}
	flaky.failWrites.Store(false)

	// Reads fail for a while; meanwhile another process writes. When reads come
	// back the room catches up without losing or repeating anything.
	flaky.failReads.Store(true)
	other := startRoom(t, flaky.Log, testConfig())
	for i := 0; i < 5; i++ {
		if _, err := other.Join(bg(t), fmt.Sprintf("other%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if r.State().Stats().Total != 1 {
		t.Fatal("the room changed although it could not read")
	}
	flaky.failReads.Store(false)
	eventually(t, "the room to catch up", func() bool { return r.State().Stats().Total == 6 })
	if st := r.Status("other4"); st.Position != 6 {
		t.Fatalf("other4: %+v, want position 6", st)
	}
}

func TestSetupIsRepeatable(t *testing.T) {
	log := newClient(t, startBroker(t))
	r, err := New(log, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.Setup(bg(t)); err != nil {
			t.Fatalf("Setup #%d: %v", i+1, err)
		}
	}
}

func TestTheControllerNeedsSaneSettings(t *testing.T) {
	r, err := New(newClient(t, startBroker(t)), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunController(bg(t), 0, 1); err == nil {
		t.Error("accepted a zero interval")
	}
	if err := r.RunController(bg(t), time.Second, 0); err == nil {
		t.Error("accepted a batch of zero")
	}
}
