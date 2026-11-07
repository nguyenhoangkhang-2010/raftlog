package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/waitingroom"
)

// leaderOf returns the id of the node that currently says it is the leader, or 0.
func (c *cluster) leaderOf() int {
	for id := 1; id <= c.n; id++ {
		if !c.running(id) {
			continue
		}
		cl, closeConn := dial(c.t, c.node(id).addr)
		st := nodeStatus(cl)
		closeConn()
		if st != nil && st.Role == "leader" {
			return id
		}
	}
	return 0
}

// decodeGate returns the queue offset a gate record admits up to.
func decodeGate(r client.Record) (uint64, bool) {
	var e struct {
		UpTo uint64 `json:"upto"`
	}
	if json.Unmarshal(r.Value, &e) != nil {
		return 0, false
	}
	return e.UpTo, true
}

// readAll reads a whole one-partition topic through the cluster client.
func readAll(t *testing.T, cl *client.Client, topic string) []client.Record {
	t.Helper()
	var out []client.Record
	for offset := uint64(0); ; {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		recs, err := cl.Fetch(ctx, topic, 0, offset, 500)
		cancel()
		if err != nil {
			t.Fatalf("read %s from %d: %v", topic, offset, err)
		}
		if len(recs) == 0 {
			return out
		}
		out = append(out, recs...)
		offset = recs[len(recs)-1].Offset + 1
	}
}

// The waiting room is a real application of the cluster: its whole state is two
// topics. This test runs it on three real nodes, kills the leader while people
// are joining, and then checks the line against the raw logs.
func TestWaitingRoomKeepsItsOrderWhenTheLeaderDies(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	const people = 120

	c := newCluster(t, 3)
	for id := 1; id <= 3; id++ {
		c.mustStart(id)
	}
	addrs := make([]string, 3)
	for i, n := range c.nodes {
		addrs[i] = n.addr
	}
	cl, err := client.New(addrs, client.WithRetryPause(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	room, err := waitingroom.New(cl, waitingroom.Config{
		QueueTopic: waitingroom.DefaultQueueTopic, GateTopic: waitingroom.DefaultGateTopic,
		PollWait: 3 * time.Second, RetryPause: 50 * time.Millisecond, FetchLimit: 200, RequestTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	for {
		err := room.Setup(setupCtx)
		if err == nil {
			break
		}
		if setupCtx.Err() != nil {
			t.Fatalf("the cluster never accepted the topics: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	setupCancel()

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); _ = room.Run(runCtx) }()
	go func() { defer bg.Done(); _ = room.RunController(runCtx, 100*time.Millisecond, 5) }()

	type result struct {
		joined    waitingroom.Status
		retries   int
		increased bool // the position got worse while waiting
		err       error
	}
	results := make([]result, people)
	var joined atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < people; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ticket := fmt.Sprintf("student-%03d", i)
			r := &results[i]
			// Arrivals are spread out so that the kill lands in the middle of them.
			time.Sleep(time.Duration(i) * 25 * time.Millisecond)

			deadline := time.Now().Add(90 * time.Second)
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				st, err := room.Join(ctx, ticket, ticket)
				cancel()
				if err == nil {
					r.joined = st
					break
				}
				// What a browser does: press the button again, with the same ticket.
				r.retries++
				if time.Now().After(deadline) {
					r.err = fmt.Errorf("never joined: %w", err)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			joined.Add(1)

			last := r.joined.Position
			for time.Now().Before(deadline) {
				st := room.Status(ticket)
				if st.State == waitingroom.StateAdmitted {
					return
				}
				if st.State == waitingroom.StateWaiting {
					if st.Position > last {
						r.increased = true
					}
					last = st.Position
				}
				time.Sleep(20 * time.Millisecond)
			}
			r.err = fmt.Errorf("never admitted; last status %+v", room.Status(ticket))
		}(i)
	}

	// Kill the leader once a third of the people are in the line.
	for joined.Load() < people/3 {
		time.Sleep(10 * time.Millisecond)
	}
	victim := c.leaderOf()
	if victim == 0 {
		t.Fatal("no leader to kill")
	}
	t.Logf("killing the leader, node %d, with %d of %d people in the line", victim, joined.Load(), people)
	c.kill(victim)
	wg.Wait()
	stop()
	bg.Wait()

	retries := 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("student-%03d: %v", i, r.err)
		}
		if r.increased {
			t.Errorf("student-%03d: the position got worse while waiting", i)
		}
		retries += r.retries
	}
	t.Logf("%d people, %d join attempts had to be repeated because of the failover", people, retries)
	if t.Failed() {
		return
	}

	// The truth is in the logs. Read them again from the surviving nodes and check
	// every promise the room made.
	queue := readAll(t, cl, waitingroom.DefaultQueueTopic)
	gate := readAll(t, cl, waitingroom.DefaultGateTopic)
	fresh := waitingroom.NewState()
	for _, r := range queue {
		fresh.ApplyQueue(r)
	}
	for _, r := range gate {
		fresh.ApplyGate(r)
	}

	tickets := fresh.Tickets()
	if len(tickets) != people {
		t.Fatalf("the queue log holds %d distinct tickets, want %d (a join was lost, or one appeared from nowhere)", len(tickets), people)
	}
	place := map[string]uint64{}
	for _, tk := range tickets {
		place[tk.ID] = tk.Offset
	}
	for i, r := range results {
		id := fmt.Sprintf("student-%03d", i)
		off, ok := place[id]
		if !ok {
			t.Errorf("%s was told it had a place, but its join is not in the log", id)
			continue
		}
		if r.joined.Offset != off {
			t.Errorf("%s was told place offset %d, the log says %d", id, r.joined.Offset, off)
		}
	}
	if st := fresh.Stats(); st.Admitted != people || st.Waiting != 0 {
		t.Errorf("rebuilt from the logs: %+v, want everyone admitted", st)
	}
	// What the running room says is what the logs say.
	live := room.State()
	for _, tk := range tickets {
		a, b := live.Status(tk.ID), fresh.Status(tk.ID)
		if a.State != b.State || a.Offset != b.Offset {
			t.Errorf("%s: the running room says %+v, the logs say %+v", tk.ID, a, b)
		}
	}
	// Admissions never went beyond the queue, and the gate never moved back in the
	// effective sense: the admitted tickets are exactly a prefix of the queue.
	var queueEnd uint64
	if n := len(queue); n > 0 {
		queueEnd = queue[n-1].Offset + 1
	}
	for _, g := range gate {
		if ev, ok := decodeGate(g); ok && ev > queueEnd {
			t.Errorf("gate record %d admits up to %d but the queue ends at %d", g.Offset, ev, queueEnd)
		}
	}
	admitted := fresh.Admitted()
	for i, tk := range admitted {
		if tk.ID != tickets[i].ID {
			t.Fatalf("admitted #%d is %s, but the queue has %s there: the gate did not follow the order", i, tk.ID, tickets[i].ID)
		}
	}
}
