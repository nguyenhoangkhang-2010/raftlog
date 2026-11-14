package console

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// Prober asks one node for its status. *client.Client implements it.
type Prober interface {
	NodeStatus(ctx context.Context, addr string) (client.NodeStatus, error)
}

// NodeView is what the console knows about one node.
type NodeView struct {
	ID          int    `json:"id"`
	Addr        string `json:"addr"`
	MetricsAddr string `json:"metrics_addr,omitempty"`
	// Running is whether the process is alive, when the console runs the nodes;
	// for a cluster it only watches, it is the same as Reachable.
	Running   bool   `json:"running"`
	Reachable bool   `json:"reachable"`
	Role      string `json:"role,omitempty"`
	Term      uint64 `json:"term,omitempty"`
	LeaderID  uint64 `json:"leader_id,omitempty"`
	Commit    uint64 `json:"commit_index,omitempty"`
	Last      uint64 `json:"last_index,omitempty"`
	Snapshot  uint64 `json:"snapshot_index,omitempty"`
	// Behind is how many log entries the node is behind the leader's log; absent
	// on the leader, on a node that cannot be reached, and when there is no leader.
	Behind *uint64 `json:"behind,omitempty"`
}

// Summary says whether the cluster can work.
type Summary struct {
	Total int `json:"total"`
	Up    int `json:"up"`
	// Quorum is how many nodes a majority is.
	Quorum    int    `json:"quorum"`
	LeaderID  int    `json:"leader_id"` // 0 when there is none
	Term      uint64 `json:"term"`
	CanCommit bool   `json:"can_commit"` // a leader exists and a majority is up
}

// Event is something that happened, for the timeline.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // "leader", "node", "action"
	Text string    `json:"text"`
}

const maxEvents = 60

// Observer polls the nodes and keeps their latest state and a timeline of changes.
type Observer struct {
	prober  Prober
	nodes   []NodeSpec
	running func(id int) (managed, alive bool)
	timeout time.Duration
	now     func() time.Time

	mu       sync.Mutex
	views    []NodeView
	events   []Event
	lastSeen map[int]bool // reachability at the previous poll
	everUp   map[int]bool // nodes that have answered at least once
	leader   int          // leader at the previous poll; 0 for none
	polled   bool
}

// NewObserver returns an observer for the nodes. running tells whether a process
// the console runs is alive; it may be nil when the console only watches. timeout
// bounds the question put to each node.
func NewObserver(p Prober, nodes []NodeSpec, running func(id int) (managed, alive bool), timeout time.Duration) *Observer {
	if running == nil {
		running = func(int) (bool, bool) { return false, false }
	}
	return &Observer{prober: p, nodes: nodes, running: running, timeout: timeout, now: time.Now, lastSeen: map[int]bool{}, everUp: map[int]bool{}}
}

// Record adds an event to the timeline, for what the console itself did.
func (o *Observer) Record(kind, text string) {
	o.mu.Lock()
	o.add(kind, text)
	o.mu.Unlock()
}

func (o *Observer) add(kind, text string) {
	o.events = append(o.events, Event{At: o.now(), Kind: kind, Text: text})
	if len(o.events) > maxEvents {
		o.events = o.events[len(o.events)-maxEvents:]
	}
}

// Poll asks every node for its status, all at once, and updates the state and the
// timeline.
func (o *Observer) Poll(ctx context.Context) {
	views := make([]NodeView, len(o.nodes))
	var wg sync.WaitGroup
	for i, n := range o.nodes {
		wg.Add(1)
		go func(i int, n NodeSpec) {
			defer wg.Done()
			v := NodeView{ID: n.ID, Addr: n.Addr, MetricsAddr: n.MetricsAddr}
			qctx, cancel := context.WithTimeout(ctx, o.timeout)
			st, err := o.prober.NodeStatus(qctx, n.Addr)
			cancel()
			if err == nil && st.RaftEnabled {
				v.Reachable = true
				v.Role, v.Term, v.LeaderID = st.Role, st.Term, st.LeaderID
				v.Commit, v.Last, v.Snapshot = st.CommitIndex, st.LastIndex, st.SnapshotIndex
			}
			managed, alive := o.running(n.ID)
			if managed {
				v.Running = alive
			} else {
				v.Running = v.Reachable
			}
			views[i] = v
		}(i, n)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return // the console is stopping; what was read is not worth keeping
	}

	// The leader is the reachable node that says so in the highest term: an old
	// leader that has not noticed yet may still say it, in a lower one.
	leaderIdx := -1
	for i, v := range views {
		if v.Reachable && v.Role == "leader" && (leaderIdx < 0 || v.Term > views[leaderIdx].Term) {
			leaderIdx = i
		}
	}
	if leaderIdx >= 0 {
		for i := range views {
			if i != leaderIdx && views[i].Reachable && views[leaderIdx].Last >= views[i].Last {
				b := views[leaderIdx].Last - views[i].Last
				views[i].Behind = &b
			}
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.views = views
	for _, v := range views {
		was, known := o.lastSeen[v.ID]
		switch {
		case o.polled && known && was && !v.Reachable:
			o.add("node", fmt.Sprintf("node %d stopped answering", v.ID))
		case o.polled && known && !was && v.Reachable && o.everUp[v.ID]:
			o.add("node", fmt.Sprintf("node %d is answering again (%s, term %d)", v.ID, v.Role, v.Term))
		case o.polled && known && !was && v.Reachable:
			o.add("node", fmt.Sprintf("node %d is up (%s, term %d)", v.ID, v.Role, v.Term))
		}
		o.lastSeen[v.ID] = v.Reachable
		if v.Reachable {
			o.everUp[v.ID] = true
		}
	}
	leader := 0
	if leaderIdx >= 0 {
		leader = views[leaderIdx].ID
	}
	if o.polled && leader != o.leader {
		switch {
		case leader != 0:
			o.add("leader", fmt.Sprintf("node %d became leader in term %d", leader, views[leaderIdx].Term))
		default:
			o.add("leader", "the cluster has no leader")
		}
	}
	o.leader = leader
	o.polled = true
}

// Run polls every interval until ctx ends.
func (o *Observer) Run(ctx context.Context, interval time.Duration) {
	o.Poll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.Poll(ctx)
		}
	}
}

// Snapshot returns the latest state of the nodes, the summary and the timeline,
// newest event last.
func (o *Observer) Snapshot() ([]NodeView, Summary, []Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	views := append([]NodeView(nil), o.views...)
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	events := append([]Event(nil), o.events...)

	s := Summary{Total: len(o.nodes), Quorum: len(o.nodes)/2 + 1}
	var best *NodeView
	for i := range views {
		v := &views[i]
		if v.Reachable {
			s.Up++
		}
		if v.Reachable && v.Role == "leader" && (best == nil || v.Term > best.Term) {
			best = v
		}
	}
	if best != nil {
		s.LeaderID, s.Term = best.ID, best.Term
	}
	s.CanCommit = best != nil && s.Up >= s.Quorum
	return views, s, events
}
