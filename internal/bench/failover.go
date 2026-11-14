package bench

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// Cluster is what the failover benchmark needs to do to the nodes: stop one at once,
// as a crash would, and start it again with the data it had.
type Cluster interface {
	Kill(id int) error
	Start(id int) error
}

// FailoverClient is the part of the client that the failover benchmark uses.
type FailoverClient interface {
	Producer
	Fetcher
	NodeStatus(ctx context.Context, addr string) (client.NodeStatus, error)
}

// FailoverConfig says what to measure.
type FailoverConfig struct {
	Topic string `json:"topic"`
	// Addrs are the nodes' addresses in node ID order: Addrs[0] is node 1.
	Addrs  []string `json:"addrs"`
	Rounds int      `json:"rounds"`
	// Steady is how long a writer runs before each kill, and after the first
	// acknowledged write following it, so that the figures are taken on a cluster
	// that was working.
	Steady time.Duration `json:"steady_ns"`
	// SettleTimeout bounds the wait for the cluster to agree on a leader again after a
	// node was started.
	SettleTimeout time.Duration `json:"settle_timeout_ns"`
	// RequestTimeout bounds one write. It must be longer than a failover.
	RequestTimeout time.Duration `json:"request_timeout_ns"`
	// PollEvery is how often the nodes are asked who the leader is.
	PollEvery time.Duration `json:"poll_every_ns"`
}

// Validate checks that every field is usable.
func (c FailoverConfig) Validate() error {
	switch {
	case c.Topic == "" || len(c.Addrs) < 3:
		return errors.New("bench: a failover needs a topic and at least three nodes")
	case c.Rounds < 1:
		return errors.New("bench: rounds must be at least 1")
	case c.Steady <= 0 || c.SettleTimeout <= 0 || c.RequestTimeout <= 0 || c.PollEvery <= 0:
		return errors.New("bench: steady, settle timeout, request timeout and poll interval must be positive")
	}
	return nil
}

// FailoverRound is one kill of the leader.
type FailoverRound struct {
	Killed    int `json:"killed_leader"`
	NewLeader int `json:"new_leader"`
	// LeaderVisible is the time from the kill until a surviving node reported itself as
	// leader. It is measured by polling, so it is accurate to about PollEvery.
	LeaderVisible time.Duration `json:"leader_visible_ns"`
	// FirstWrite is the time from the kill until the first write was acknowledged, as the
	// client saw it. It includes waiting out the election and the client's retries.
	FirstWrite time.Duration `json:"first_write_ns"`
}

// FailoverResult is what the failover benchmark measured.
type FailoverResult struct {
	Config FailoverConfig  `json:"config"`
	Rounds []FailoverRound `json:"rounds"`

	LeaderVisible Summary `json:"leader_visible"`
	FirstWrite    Summary `json:"first_write"`

	// Acked counts writes the cluster acknowledged during the whole run, Failed those
	// that returned an error.
	Acked  uint64 `json:"acked"`
	Failed uint64 `json:"failed"`
	// Missing counts acknowledged writes that are not in the log at the end: it must be
	// zero. Duplicates counts values stored more than once, which a retry can cause.
	Missing    int `json:"missing_acknowledged"`
	Duplicates int `json:"duplicates"`
	// Nodes has the result of reading the log of each node, after giving a node that is
	// behind time to catch up. Missing and Duplicates are the worst of them.
	Nodes []NodeCheck `json:"nodes"`
	// MissingSeqs are the first numbers of the acknowledged writes that are missing, and
	// LastAckedSeq is the highest number acknowledged: if the missing ones are all near
	// it, the log was read before a node had caught up.
	MissingSeqs  []uint64 `json:"missing_first_seqs,omitempty"`
	LastAckedSeq uint64   `json:"last_acked_seq"`
}

type attempt struct {
	seq   uint64
	end   time.Time
	acked bool
}

// writer sends one numbered value at a time, back to back, to partition 0, and keeps
// when each was acknowledged.
type writer struct {
	runID string
	p     Producer
	topic string
	rto   time.Duration

	mu       sync.Mutex
	attempts []attempt
	lastErr  string
	cancel   context.CancelFunc
	done     chan struct{}
}

func (w *writer) value(seq uint64) []byte {
	return []byte(w.runID + "-" + strconv.FormatUint(seq, 10))
}

func (w *writer) start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		for seq := uint64(0); ctx.Err() == nil; seq++ {
			reqCtx, cancel := context.WithTimeout(ctx, w.rto)
			_, err := w.p.Produce(reqCtx, w.topic, []client.Message{{Value: w.value(seq)}}, client.ToPartition(0))
			cancel()
			if ctx.Err() != nil {
				return // cut off by the end of the run: neither acknowledged nor failed
			}
			a := attempt{seq: seq, end: time.Now(), acked: err == nil}
			w.mu.Lock()
			if err != nil {
				w.lastErr = err.Error()
			}
			w.attempts = append(w.attempts, a)
			w.mu.Unlock()
			if err != nil {
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	}()
}

func (w *writer) stop() {
	w.cancel()
	<-w.done
}

// firstAckAfter returns when the first write acknowledged after t finished.
func (w *writer) firstAckAfter(t time.Time) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range w.attempts {
		if a.acked && a.end.After(t) {
			return a.end, true
		}
	}
	return time.Time{}, false
}

func (w *writer) hasAckBefore(t time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range w.attempts {
		if a.acked && !a.end.After(t) {
			return true
		}
	}
	return false
}

// RunFailover kills the leader Rounds times while a writer sends numbered values, and
// measures how long it takes for a new leader to be visible and for a write to be
// acknowledged again. After each kill the node is started again and the cluster is
// given time to settle. At the end it reads the topic back and checks that every
// acknowledged value is there.
//
// The moment of the kill is taken after Kill has returned, when the process is gone, so
// the measured times leave out the time Kill itself took (milliseconds); nothing the
// dead leader acknowledged can arrive later.
func RunFailover(ctx context.Context, c FailoverClient, cl Cluster, readers []Fetcher, cfg FailoverConfig) (FailoverResult, error) {
	if err := cfg.Validate(); err != nil {
		return FailoverResult{}, err
	}
	res := FailoverResult{Config: cfg}
	w := &writer{
		runID: strconv.FormatInt(time.Now().UnixNano(), 36),
		p:     c, topic: cfg.Topic, rto: cfg.RequestTimeout,
	}
	w.start(ctx)
	stopped := false
	defer func() {
		if !stopped {
			w.stop()
		}
	}()

	for round := 1; round <= cfg.Rounds; round++ {
		leader, err := waitSettled(ctx, c, cfg.Addrs, nil, cfg.SettleTimeout, cfg.PollEvery)
		if err != nil {
			return res, fmt.Errorf("round %d, before the kill: %w", round, err)
		}
		if err := sleep(ctx, cfg.Steady); err != nil {
			return res, err
		}
		if !w.hasAckBefore(time.Now()) {
			return res, fmt.Errorf("round %d: no write was acknowledged before the kill (last error: %s)", round, w.lastError())
		}

		if err := cl.Kill(leader); err != nil {
			return res, fmt.Errorf("round %d: kill node %d: %w", round, leader, err)
		}
		killed := time.Now()

		r := FailoverRound{Killed: leader}
		newLeader, seen, err := waitNewLeader(ctx, c, cfg.Addrs, leader, cfg.SettleTimeout, cfg.PollEvery)
		if err != nil {
			return res, fmt.Errorf("round %d, after the kill: %w", round, err)
		}
		r.NewLeader, r.LeaderVisible = newLeader, seen.Sub(killed)

		deadline := time.Now().Add(cfg.SettleTimeout)
		for {
			if at, ok := w.firstAckAfter(killed); ok {
				r.FirstWrite = at.Sub(killed)
				break
			}
			if time.Now().After(deadline) {
				return res, fmt.Errorf("round %d: no write was acknowledged within %s of the kill (last error: %s)", round, cfg.SettleTimeout, w.lastError())
			}
			if err := sleep(ctx, cfg.PollEvery); err != nil {
				return res, err
			}
		}
		res.Rounds = append(res.Rounds, r)

		if err := sleep(ctx, cfg.Steady); err != nil { // writes go on with the new leader
			return res, err
		}
		if err := cl.Start(leader); err != nil {
			return res, fmt.Errorf("round %d: start node %d: %w", round, leader, err)
		}
		if _, err := waitSettled(ctx, c, cfg.Addrs, nil, cfg.SettleTimeout, cfg.PollEvery); err != nil {
			return res, fmt.Errorf("round %d, after the node came back: %w", round, err)
		}
	}

	w.stop()
	stopped = true
	if len(readers) == 0 {
		readers = []Fetcher{c}
	}
	if err := res.check(ctx, readers, w, cfg.SettleTimeout); err != nil {
		return res, err
	}
	var vis, first []time.Duration
	for _, r := range res.Rounds {
		vis = append(vis, r.LeaderVisible)
		first = append(first, r.FirstWrite)
	}
	res.LeaderVisible, res.FirstWrite = Summarize(vis), Summarize(first)
	return res, nil
}

func (w *writer) lastError() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastErr == "" {
		return "none"
	}
	return w.lastErr
}

// NodeCheck is what one node's log held when the writes had stopped.
type NodeCheck struct {
	Node int `json:"node"`
	// Missing counts acknowledged writes this node still did not hold after waiting up to
	// the settle timeout. It must be zero.
	Missing int `json:"missing_acknowledged"`
	// CaughtUpAfter is how long after the last write this node took to hold every
	// acknowledged write; zero if it already did. A node that was restarted applies the
	// entries it missed one by one, so it may be behind for a while.
	CaughtUpAfter time.Duration `json:"caught_up_after_ns"`
	Duplicates    int           `json:"duplicates"`
}

// check reads the topic from every node and counts the acknowledged values each lacks.
// A read shows only what a node has applied, and a node that was just restarted applies
// the entries it missed one at a time, so a node that lacks the newest writes is given
// time to catch up: only a write still absent when the time is up is reported as lost.
func (res *FailoverResult) check(ctx context.Context, readers []Fetcher, w *writer, patience time.Duration) error {
	w.mu.Lock()
	attempts := append([]attempt(nil), w.attempts...)
	w.mu.Unlock()
	for _, a := range attempts {
		if !a.acked {
			res.Failed++
			continue
		}
		res.Acked++
		if a.seq > res.LastAckedSeq {
			res.LastAckedSeq = a.seq
		}
	}

	// The nodes are read at the same time, so that each one's catch-up time is its own.
	stoppedAt := time.Now()
	type outcome struct {
		nc      NodeCheck
		missing []uint64
		err     error
	}
	outs := make([]outcome, len(readers))
	var wg sync.WaitGroup
	for i, f := range readers {
		wg.Add(1)
		go func(i int, f Fetcher) {
			defer wg.Done()
			outs[i].nc, outs[i].missing, outs[i].err = checkNode(ctx, f, w, attempts, i+1, stoppedAt, patience)
		}(i, f)
	}
	wg.Wait()
	for _, o := range outs {
		if o.err != nil {
			return fmt.Errorf("reading the topic from node %d: %w", o.nc.Node, o.err)
		}
		res.Nodes = append(res.Nodes, o.nc)
		res.MissingSeqs = append(res.MissingSeqs, o.missing...)
		if len(res.MissingSeqs) > 10 {
			res.MissingSeqs = res.MissingSeqs[:10]
		}
		if o.nc.Missing > res.Missing {
			res.Missing = o.nc.Missing
		}
		if o.nc.Duplicates > res.Duplicates {
			res.Duplicates = o.nc.Duplicates
		}
	}
	return nil
}

// checkNode reads one node's log until it holds every acknowledged write or the patience
// is spent. It returns the first few missing numbers if some are still absent.
func checkNode(ctx context.Context, f Fetcher, w *writer, attempts []attempt, node int, stoppedAt time.Time, patience time.Duration) (NodeCheck, []uint64, error) {
	nc := NodeCheck{Node: node}
	nodeStart := time.Now()
	for {
		stored, err := readAll(ctx, f, w)
		var missing []uint64
		if err == nil {
			for _, a := range attempts {
				if a.acked && stored[string(w.value(a.seq))] == 0 {
					missing = append(missing, a.seq)
				}
			}
		}
		if err == nil && len(missing) == 0 {
			for _, n := range stored {
				if n > 1 {
					nc.Duplicates++
				}
			}
			if d := time.Since(stoppedAt); d > time.Second {
				nc.CaughtUpAfter = d
			}
			return nc, nil, nil
		}
		if time.Since(nodeStart) > patience {
			if err != nil {
				return nc, nil, err
			}
			nc.Missing = len(missing)
			if len(missing) > 10 {
				missing = missing[:10]
			}
			return nc, missing, nil
		}
		if err := sleep(ctx, 250*time.Millisecond); err != nil {
			return nc, nil, err
		}
	}
}

// readAll reads partition 0 to its end and counts how many times each value of this run
// is stored.
func readAll(ctx context.Context, f Fetcher, w *writer) (map[string]int, error) {
	stored := map[string]int{}
	var offset uint64
	for {
		reqCtx, cancel := context.WithTimeout(ctx, w.rto)
		recs, err := f.Fetch(reqCtx, w.topic, 0, offset, 1000)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("at offset %d: %w", offset, err)
		}
		if len(recs) == 0 {
			return stored, nil
		}
		for _, r := range recs {
			if v := string(r.Value); strings.HasPrefix(v, w.runID+"-") {
				stored[v]++
			}
		}
		offset = recs[len(recs)-1].Offset + 1
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// statuses asks every node for its status at the same time, so that a node that is down
// and answers only when the timeout ends does not delay seeing the others. A node that
// does not answer in time is left out of the map.
func statuses(ctx context.Context, c FailoverClient, addrs []string, timeout time.Duration) map[int]client.NodeStatus {
	out := map[int]client.NodeStatus{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func(id int, addr string) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if st, err := c.NodeStatus(sctx, addr); err == nil {
				mu.Lock()
				out[id] = st
				mu.Unlock()
			}
		}(i+1, a)
	}
	wg.Wait()
	return out
}

// waitSettled waits until every node answers, exactly one is the leader and all the
// others follow it in the same term. It returns the leader.
func waitSettled(ctx context.Context, c FailoverClient, addrs []string, except map[int]bool, timeout, poll time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		sts := statuses(ctx, c, addrs, poll+200*time.Millisecond)
		if leader, ok := settledLeader(sts, len(addrs), except); ok {
			return leader, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("the cluster did not settle within %s (status: %s)", timeout, describe(sts))
		}
		if err := sleep(ctx, poll); err != nil {
			return 0, err
		}
	}
}

func settledLeader(sts map[int]client.NodeStatus, n int, except map[int]bool) (int, bool) {
	leader, leaders := 0, 0
	for id, st := range sts {
		if st.Role == "leader" {
			leader, leaders = id, leaders+1
		}
	}
	if leaders != 1 {
		return 0, false
	}
	for id := 1; id <= n; id++ {
		if except[id] {
			continue
		}
		st, ok := sts[id]
		if !ok {
			return 0, false
		}
		if id != leader && (st.Role != "follower" || st.Term != sts[leader].Term || int(st.LeaderID) != leader) {
			return 0, false
		}
	}
	return leader, true
}

// waitNewLeader polls the nodes other than the dead one until one of them is the leader,
// and returns it with the time it was seen.
//
// Each surviving node is polled by its own goroutine and the time is taken when the
// answer arrives, so that a slow or unreachable node cannot delay noticing the others.
func waitNewLeader(ctx context.Context, c FailoverClient, addrs []string, dead int, timeout, poll time.Duration) (int, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type sighting struct {
		id int
		at time.Time
	}
	found := make(chan sighting, len(addrs))
	for i, a := range addrs {
		if i+1 == dead {
			continue
		}
		go func(id int, addr string) {
			for ctx.Err() == nil {
				sctx, cancelStatus := context.WithTimeout(ctx, poll+200*time.Millisecond)
				st, err := c.NodeStatus(sctx, addr)
				cancelStatus()
				if err == nil && st.Role == "leader" {
					found <- sighting{id, time.Now()}
					return
				}
				_ = sleep(ctx, poll)
			}
		}(i+1, a)
	}
	select {
	case s := <-found:
		return s.id, s.at, nil
	case <-ctx.Done():
		return 0, time.Time{}, fmt.Errorf("no new leader within %s", timeout)
	}
}

func describe(sts map[int]client.NodeStatus) string {
	ids := make([]int, 0, len(sts))
	for id := range sts {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("node %d %s term %d leader %d", id, sts[id].Role, sts[id].Term, sts[id].LeaderID))
	}
	if len(parts) == 0 {
		return "no node answered"
	}
	return strings.Join(parts, "; ")
}

// WaitForLeader waits until every node answers and they agree on one leader in one term,
// and returns the leader's ID. Run it before a measurement, so that the cluster is not
// still starting.
func WaitForLeader(ctx context.Context, c FailoverClient, addrs []string, timeout time.Duration) (int, error) {
	return waitSettled(ctx, c, addrs, nil, timeout, 50*time.Millisecond)
}
