package raft

import (
	"bytes"
	"errors"
	"math/rand"
	"sort"
	"testing"
)

// Timing used by the tests. These are test parameters, not protocol constants.
const (
	testElectionTicks  = 10
	testHeartbeatTicks = 2
)

// memStorage is an in-memory Storage that can be told to fail.
type memStorage struct {
	hs       HardState
	saves    int
	failSave error
}

func (s *memStorage) Load() (HardState, error) { return s.hs, nil }

func (s *memStorage) Save(hs HardState) error {
	if s.failSave != nil {
		return s.failSave
	}
	s.hs = hs
	s.saves++
	return nil
}

// failingLog wraps a LogStore and fails writes on demand.
type failingLog struct {
	LogStore
	failAppend   error
	failTruncate error
}

func (l *failingLog) Append(entries []Entry) error {
	if l.failAppend != nil {
		return l.failAppend
	}
	return l.LogStore.Append(entries)
}

func (l *failingLog) TruncateSuffix(from uint64) error {
	if l.failTruncate != nil {
		return l.failTruncate
	}
	return l.LogStore.TruncateSuffix(from)
}

// logWithTerms returns a log whose entry i+1 has term terms[i].
func logWithTerms(terms ...uint64) *MemoryLog {
	l := NewMemoryLog()
	for i, t := range terms {
		l.entries = append(l.entries, Entry{Index: uint64(i + 1), Term: t, Data: []byte{byte(i)}})
	}
	return l
}

func testConfig(id NodeID, cluster []NodeID, st Storage, seed int64) Config {
	return Config{
		ID:             id,
		Cluster:        cluster,
		ElectionTicks:  testElectionTicks,
		HeartbeatTicks: testHeartbeatTicks,
		Storage:        st,
		Rand:           rand.New(rand.NewSource(seed)),
	}
}

// newTestNode builds a node of the cluster {1, 2, ..., size}. If the log is not
// nil, the stored term is raised to the log's last term, as it would be on a
// real node (a node never has a log entry from a term it has not reached).
func newTestNode(t *testing.T, id NodeID, size int, st *memStorage, log LogStore) *Node {
	t.Helper()
	cfg := testConfig(id, clusterIDs(size), st, int64(id))
	if log != nil {
		cfg.Log = log
		if lastTerm, _ := log.Term(log.LastIndex()); st.hs.Term < lastTerm {
			st.hs.Term = lastTerm
		}
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

func clusterIDs(size int) []NodeID {
	ids := make([]NodeID, size)
	for i := range ids {
		ids[i] = NodeID(i + 1)
	}
	return ids
}

// cluster is a simulated network of nodes driven by logical ticks. Delivery is
// instantaneous and in order; links can be cut, and nodes can crash and restart
// (keeping their storage and log, as a real process would).
//
// It also watches the safety properties of Raft while a test runs:
//   - election safety: at most one leader per term;
//   - log matching: two logs that agree on an entry agree on everything before it;
//   - state machine safety: no two nodes ever apply different entries at the
//     same index.
//
// Everything is deterministic for a given seed.
type cluster struct {
	t       *testing.T
	ids     []NodeID
	nodes   map[NodeID]*Node
	stores  map[NodeID]*memStorage
	logs    map[NodeID]*MemoryLog
	down    map[NodeID]bool
	blocked map[[2]NodeID]bool
	queue   []Message
	seed    int64
	restart int64 // makes the RNG of a restarted node differ from before

	leaderOfTerm map[uint64]NodeID
	applied      map[NodeID][]Entry // what each node's state machine has applied
	committed    map[uint64]Entry   // first entry seen committed at each index
}

func newCluster(t *testing.T, size int, seed int64) *cluster {
	t.Helper()
	c := &cluster{
		t:            t,
		ids:          clusterIDs(size),
		nodes:        make(map[NodeID]*Node),
		stores:       make(map[NodeID]*memStorage),
		logs:         make(map[NodeID]*MemoryLog),
		down:         make(map[NodeID]bool),
		blocked:      make(map[[2]NodeID]bool),
		seed:         seed,
		leaderOfTerm: make(map[uint64]NodeID),
		applied:      make(map[NodeID][]Entry),
		committed:    make(map[uint64]Entry),
	}
	for _, id := range c.ids {
		c.stores[id] = &memStorage{}
		c.logs[id] = NewMemoryLog()
		c.start(id)
	}
	return c
}

// start (re)creates the node from its storage and log, like a process restart.
// The state machine starts empty and is rebuilt from the replayed log.
func (c *cluster) start(id NodeID) {
	c.t.Helper()
	cfg := testConfig(id, c.ids, c.stores[id], c.seed*1000+int64(id)+c.restart)
	cfg.Log = c.logs[id]
	n, err := New(cfg)
	if err != nil {
		c.t.Fatalf("New(%d): %v", id, err)
	}
	c.nodes[id] = n
	c.down[id] = false
	c.applied[id] = nil
}

func (c *cluster) crash(id NodeID) { c.down[id] = true }

func (c *cluster) restartNode(id NodeID) {
	c.restart++
	c.start(id)
}

// isolate cuts every link to and from id.
func (c *cluster) isolate(id NodeID) {
	for _, other := range c.ids {
		if other != id {
			c.blocked[[2]NodeID{id, other}] = true
			c.blocked[[2]NodeID{other, id}] = true
		}
	}
}

// cut drops every message sent from one node to another (one direction only).
func (c *cluster) cut(from, to NodeID) { c.blocked[[2]NodeID{from, to}] = true }

// split cuts every link between the nodes in group and all the others.
func (c *cluster) split(group ...NodeID) {
	in := make(map[NodeID]bool)
	for _, id := range group {
		in[id] = true
	}
	for _, a := range c.ids {
		for _, b := range c.ids {
			if in[a] != in[b] {
				c.blocked[[2]NodeID{a, b}] = true
			}
		}
	}
}

func (c *cluster) heal() { c.blocked = make(map[[2]NodeID]bool) }

// collect moves what a node produced into the network and the state machine.
func (c *cluster) collect(id NodeID) {
	c.t.Helper()
	c.queue = append(c.queue, c.nodes[id].Messages()...)

	entries, err := c.nodes[id].Committed()
	if err != nil {
		c.t.Fatalf("node %d Committed: %v", id, err)
	}
	for _, e := range entries {
		have := c.applied[id]
		if e.Index != uint64(len(have))+1 {
			c.t.Fatalf("node %d applied index %d after %d entries: not in order", id, e.Index, len(have))
		}
		c.applied[id] = append(have, e)

		if first, ok := c.committed[e.Index]; ok {
			if first.Term != e.Term || first.Type != e.Type || !bytes.Equal(first.Data, e.Data) {
				c.t.Fatalf("state machine safety violated at index %d: node %d applied (term %d, %q), another node applied (term %d, %q)",
					e.Index, id, e.Term, e.Data, first.Term, first.Data)
			}
		} else {
			c.committed[e.Index] = e
		}
	}
}

// tick advances every running node by one tick, then delivers all messages.
func (c *cluster) tick() {
	c.t.Helper()
	for _, id := range c.ids {
		if c.down[id] {
			continue
		}
		if err := c.nodes[id].Tick(); err != nil {
			c.t.Fatalf("node %d Tick: %v", id, err)
		}
		c.collect(id)
	}
	c.deliver()
	c.checkLogMatching()
}

func (c *cluster) deliver() {
	c.t.Helper()
	for steps := 0; len(c.queue) > 0; steps++ {
		if steps > 100_000 {
			c.t.Fatal("message storm: the queue never drained")
		}
		m := c.queue[0]
		c.queue = c.queue[1:]
		if c.down[m.To] || c.blocked[[2]NodeID{m.From, m.To}] {
			continue
		}
		if err := c.nodes[m.To].Step(m); err != nil {
			c.t.Fatalf("node %d Step(%v from %d): %v", m.To, m.Type, m.From, err)
		}
		c.collect(m.To)
		c.checkSafety()
	}
	c.checkSafety()
}

// propose submits data to node id and delivers the resulting messages.
func (c *cluster) propose(id NodeID, data string) (index, term uint64, err error) {
	c.t.Helper()
	index, term, err = c.nodes[id].Propose([]byte(data))
	c.collect(id)
	c.deliver()
	return index, term, err
}

// proposeToAnyLeader proposes to the first running node that believes it is
// leader and reports whether there was one.
func (c *cluster) proposeToAnyLeader(data string) (NodeID, bool) {
	c.t.Helper()
	ls := c.leaders()
	if len(ls) == 0 {
		return None, false
	}
	if _, _, err := c.propose(ls[0], data); err != nil {
		c.t.Fatalf("Propose on leader %d: %v", ls[0], err)
	}
	return ls[0], true
}

// checkSafety enforces election safety: at most one leader per term, ever.
func (c *cluster) checkSafety() {
	c.t.Helper()
	for _, id := range c.ids {
		if c.down[id] {
			continue
		}
		st := c.nodes[id].Status()
		if st.Role != Leader {
			continue
		}
		if prev, ok := c.leaderOfTerm[st.Term]; ok && prev != id {
			c.t.Fatalf("election safety violated: nodes %d and %d are both leader in term %d", prev, id, st.Term)
		}
		c.leaderOfTerm[st.Term] = id
	}
}

// checkLogMatching enforces the log matching property for every pair of logs.
func (c *cluster) checkLogMatching() {
	c.t.Helper()
	for i, a := range c.ids {
		for _, b := range c.ids[i+1:] {
			la, lb := c.logs[a].entries, c.logs[b].entries
			// Find the highest index at which both logs have the same term...
			for k := min(len(la), len(lb)); k >= 1; k-- {
				if la[k-1].Term != lb[k-1].Term {
					continue
				}
				// ...and everything up to it must be identical.
				for j := 1; j <= k; j++ {
					x, y := la[j-1], lb[j-1]
					if x.Term != y.Term || x.Type != y.Type || !bytes.Equal(x.Data, y.Data) {
						c.t.Fatalf("log matching violated: nodes %d and %d agree on index %d (term %d) but differ at index %d",
							a, b, k, la[k-1].Term, j)
					}
				}
				break
			}
		}
	}
}

func (c *cluster) status(id NodeID) Status { return c.nodes[id].Status() }

// leaders returns the running nodes that currently believe they are leader.
func (c *cluster) leaders() []NodeID {
	var out []NodeID
	for _, id := range c.ids {
		if !c.down[id] && c.nodes[id].Status().Role == Leader {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// runUntil ticks until cond is true and returns the number of ticks used, or
// -1 if cond is still false after limit ticks.
func (c *cluster) runUntil(limit int, cond func() bool) int {
	c.t.Helper()
	for i := 0; i <= limit; i++ {
		if cond() {
			return i
		}
		c.tick()
	}
	return -1
}

func (c *cluster) run(ticks int) {
	c.t.Helper()
	for i := 0; i < ticks; i++ {
		c.tick()
	}
}

// settled reports whether exactly one running node is leader and every other
// running node follows it in the same term.
func (c *cluster) settled() bool {
	ls := c.leaders()
	if len(ls) != 1 {
		return false
	}
	leader := c.nodes[ls[0]].Status()
	for _, id := range c.ids {
		if c.down[id] || id == ls[0] {
			continue
		}
		st := c.nodes[id].Status()
		if st.Role != Follower || st.Leader != leader.ID || st.Term != leader.Term {
			return false
		}
	}
	return true
}

// converged reports whether the cluster is settled and every running node has
// the same log, with everything committed and applied.
func (c *cluster) converged() bool {
	if !c.settled() {
		return false
	}
	var want Status
	first := true
	for _, id := range c.ids {
		if c.down[id] {
			continue
		}
		st := c.nodes[id].Status()
		if st.CommitIndex != st.LastIndex || uint64(len(c.applied[id])) != st.CommitIndex {
			return false
		}
		if first {
			want, first = st, false
		} else if st.LastIndex != want.LastIndex {
			return false
		}
	}
	return true
}

func (c *cluster) hasLeader() bool { return len(c.leaders()) > 0 }

// appliedData returns the commands (not no-ops) a node has applied, in order.
func (c *cluster) appliedData(id NodeID) []string {
	var out []string
	for _, e := range c.applied[id] {
		if e.Type == EntryNormal {
			out = append(out, string(e.Data))
		}
	}
	return out
}

var errDiskFull = errors.New("disk full")
