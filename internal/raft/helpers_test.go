package raft

import (
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

// fakeLog is a LogView with a fixed last entry.
type fakeLog struct{ index, term uint64 }

func (l fakeLog) LastIndexTerm() (uint64, uint64) { return l.index, l.term }

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

// newTestNode builds a node of the cluster {1, 2, ..., size}.
func newTestNode(t *testing.T, id NodeID, size int, st *memStorage, log LogView) *Node {
	t.Helper()
	cfg := testConfig(id, clusterIDs(size), st, int64(id))
	cfg.Log = log
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
// instantaneous and in order; links can be cut, and nodes can crash and restart.
// Everything is deterministic for a given seed.
type cluster struct {
	t       *testing.T
	ids     []NodeID
	nodes   map[NodeID]*Node
	stores  map[NodeID]*memStorage
	down    map[NodeID]bool
	blocked map[[2]NodeID]bool
	queue   []Message
	seed    int64
	restart int64 // makes the RNG of a restarted node differ from before

	// leaderOfTerm remembers every leader ever seen, to check election safety.
	leaderOfTerm map[uint64]NodeID
}

func newCluster(t *testing.T, size int, seed int64) *cluster {
	t.Helper()
	c := &cluster{
		t:            t,
		ids:          clusterIDs(size),
		nodes:        make(map[NodeID]*Node),
		stores:       make(map[NodeID]*memStorage),
		down:         make(map[NodeID]bool),
		blocked:      make(map[[2]NodeID]bool),
		seed:         seed,
		leaderOfTerm: make(map[uint64]NodeID),
	}
	for _, id := range c.ids {
		c.stores[id] = &memStorage{}
		c.start(id)
	}
	return c
}

// start (re)creates the node from its storage, like a process restart.
func (c *cluster) start(id NodeID) {
	c.t.Helper()
	cfg := testConfig(id, c.ids, c.stores[id], c.seed*1000+int64(id)+c.restart)
	n, err := New(cfg)
	if err != nil {
		c.t.Fatalf("New(%d): %v", id, err)
	}
	c.nodes[id] = n
	c.down[id] = false
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

func (c *cluster) collect(id NodeID) {
	c.queue = append(c.queue, c.nodes[id].Messages()...)
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

func (c *cluster) hasLeader() bool { return len(c.leaders()) > 0 }

var errDiskFull = errors.New("disk full")
