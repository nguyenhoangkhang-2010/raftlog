package raft

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sort"
	"time"
)

// Config configures a Node.
type Config struct {
	// ID is this node's identifier; it must be non-zero and part of Cluster.
	ID NodeID
	// Cluster lists every member of the cluster, including this node.
	Cluster []NodeID
	// ElectionTicks is the base election timeout in ticks. A node waits a random
	// number of ticks in [ElectionTicks, 2*ElectionTicks) before it starts an
	// election; the randomness is what resolves split votes.
	ElectionTicks int
	// HeartbeatTicks is how often a leader sends heartbeats, in ticks. It must be
	// smaller than ElectionTicks, or followers would time out between heartbeats.
	HeartbeatTicks int
	// Storage persists the term and the vote.
	Storage Storage
	// Log reports the last log entry. Optional: nil means an empty log.
	Log LogView
	// Rand chooses the randomized election timeouts. Optional: nil means a
	// generator seeded from the clock and the node ID, so nodes differ. Tests
	// pass a seeded generator to make runs reproducible.
	Rand *rand.Rand
	// Logger receives role changes. Optional.
	Logger *slog.Logger
}

func (c *Config) validate() error {
	var errs []error
	if c.ID == None {
		errs = append(errs, errors.New("raft: node ID must not be zero"))
	}
	seen := make(map[NodeID]bool)
	hasSelf := false
	for _, id := range c.Cluster {
		switch {
		case id == None:
			errs = append(errs, errors.New("raft: cluster member ID must not be zero"))
		case seen[id]:
			errs = append(errs, fmt.Errorf("raft: duplicate cluster member %d", id))
		}
		seen[id] = true
		if id == c.ID {
			hasSelf = true
		}
	}
	if !hasSelf {
		errs = append(errs, fmt.Errorf("raft: node %d is not in the cluster %v", c.ID, c.Cluster))
	}
	if c.HeartbeatTicks < 1 {
		errs = append(errs, errors.New("raft: HeartbeatTicks must be at least 1"))
	}
	if c.ElectionTicks <= c.HeartbeatTicks {
		errs = append(errs, errors.New("raft: ElectionTicks must be greater than HeartbeatTicks"))
	}
	if c.Storage == nil {
		errs = append(errs, errors.New("raft: Storage is required"))
	}
	return errors.Join(errs...)
}

// Node is the Raft state machine for leader election.
//
// It does no I/O besides its Storage and never looks at a clock. The caller
// drives it: Tick() advances logical time, Step() delivers a received message,
// and Messages() returns what the node wants to send. A Node is not safe for
// concurrent use; Runner wraps it with a lock for real deployments.
type Node struct {
	id                            NodeID
	peers                         []NodeID // every other member, sorted
	quorum                        int
	electionTicks, heartbeatTicks int
	storage                       Storage
	log                           LogView
	rnd                           *rand.Rand
	logger                        *slog.Logger

	hs     HardState
	role   Role
	leader NodeID
	votes  map[NodeID]bool // candidate only: who answered, and whether they granted

	electionElapsed   int
	randomizedTimeout int
	heartbeatElapsed  int

	outbox []Message
}

// New creates a node and restores its persistent state from cfg.Storage.
func New(cfg Config) (*Node, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	hs, err := cfg.Storage.Load()
	if err != nil {
		return nil, fmt.Errorf("raft: load state: %w", err)
	}

	n := &Node{
		id:             cfg.ID,
		quorum:         len(cfg.Cluster)/2 + 1,
		electionTicks:  cfg.ElectionTicks,
		heartbeatTicks: cfg.HeartbeatTicks,
		storage:        cfg.Storage,
		log:            cfg.Log,
		rnd:            cfg.Rand,
		logger:         cfg.Logger,
		hs:             hs,
		role:           Follower,
	}
	for _, id := range cfg.Cluster {
		if id != cfg.ID {
			n.peers = append(n.peers, id)
		}
	}
	sort.Slice(n.peers, func(i, j int) bool { return n.peers[i] < n.peers[j] })
	if n.rnd == nil {
		n.rnd = rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(cfg.ID)<<32))
	}
	if n.logger == nil {
		n.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	n.resetElectionTimer()
	return n, nil
}

// Status returns a snapshot of the node.
func (n *Node) Status() Status {
	return Status{ID: n.id, Role: n.role, Term: n.hs.Term, Leader: n.leader, VotedFor: n.hs.VotedFor}
}

// Messages returns the messages queued for sending and clears the queue.
func (n *Node) Messages() []Message {
	out := n.outbox
	n.outbox = nil
	return out
}

// Tick advances logical time by one tick.
//
// A non-nil error means the node could not persist its state. The node then
// stays as it was and sends nothing that depends on the failed update.
func (n *Node) Tick() error {
	if n.role == Leader {
		n.heartbeatElapsed++
		if n.heartbeatElapsed >= n.heartbeatTicks {
			n.heartbeatElapsed = 0
			n.broadcastHeartbeat()
		}
		return nil
	}
	n.electionElapsed++
	if n.electionElapsed >= n.randomizedTimeout {
		return n.campaign()
	}
	return nil
}

// Step handles a message received from another node.
func (n *Node) Step(m Message) error {
	if m.To != n.id || m.From == n.id || !n.isMember(m.From) {
		return nil // not for us, or from a stranger: ignore
	}

	switch {
	case m.Term > n.hs.Term:
		// A newer term exists: this node is out of date, whatever its role.
		var leader NodeID
		if m.Type == MsgHeartbeat {
			leader = m.From
		}
		if err := n.becomeFollower(m.Term, leader); err != nil {
			return err
		}
	case m.Term < n.hs.Term:
		// A stale sender. Tell requests how things are; drop stale replies.
		switch m.Type {
		case MsgRequestVote:
			n.send(Message{Type: MsgRequestVoteResp, To: m.From, Granted: false})
		case MsgHeartbeat:
			n.send(Message{Type: MsgHeartbeatResp, To: m.From, Success: false})
		}
		return nil
	}

	// From here on m.Term == n.hs.Term.
	switch m.Type {
	case MsgRequestVote:
		return n.handleRequestVote(m)
	case MsgRequestVoteResp:
		n.handleRequestVoteResp(m)
	case MsgHeartbeat:
		n.handleHeartbeat(m)
	case MsgHeartbeatResp:
		// Nothing to do until log replication tracks follower progress.
	}
	return nil
}

func (n *Node) isMember(id NodeID) bool {
	for _, p := range n.peers {
		if p == id {
			return true
		}
	}
	return false
}

// send queues a message; the node fills in From and the current Term.
func (n *Node) send(m Message) {
	m.From = n.id
	m.Term = n.hs.Term
	n.outbox = append(n.outbox, m)
}

// setHardState persists hs first and only then adopts it, so a failed write
// leaves the node unchanged.
func (n *Node) setHardState(hs HardState) error {
	if hs == n.hs {
		return nil
	}
	if err := n.storage.Save(hs); err != nil {
		return fmt.Errorf("raft: save state: %w", err)
	}
	n.hs = hs
	return nil
}

func (n *Node) resetElectionTimer() {
	n.electionElapsed = 0
	n.randomizedTimeout = n.electionTicks + n.rnd.Intn(n.electionTicks)
}

// becomeFollower adopts term (forgetting the vote if the term is new) and
// follows leader, which may be None.
func (n *Node) becomeFollower(term uint64, leader NodeID) error {
	hs := n.hs
	if term > hs.Term {
		hs = HardState{Term: term, VotedFor: None}
	}
	if err := n.setHardState(hs); err != nil {
		return err
	}
	if n.role != Follower {
		n.logger.Info("became follower", "node", n.id, "term", n.hs.Term, "was", n.role.String())
	}
	n.role = Follower
	n.leader = leader
	n.votes = nil
	n.resetElectionTimer()
	return nil
}

// campaign starts an election: new term, vote for self, ask everyone else.
func (n *Node) campaign() error {
	if err := n.setHardState(HardState{Term: n.hs.Term + 1, VotedFor: n.id}); err != nil {
		return err
	}
	n.role = Candidate
	n.leader = None
	n.votes = map[NodeID]bool{n.id: true}
	n.resetElectionTimer()
	n.logger.Info("became candidate", "node", n.id, "term", n.hs.Term)

	if n.countVotes() >= n.quorum {
		n.becomeLeader() // a one-node cluster wins at once
		return nil
	}
	var lastIndex, lastTerm uint64
	if n.log != nil {
		lastIndex, lastTerm = n.log.LastIndexTerm()
	}
	for _, p := range n.peers {
		n.send(Message{Type: MsgRequestVote, To: p, LastLogIndex: lastIndex, LastLogTerm: lastTerm})
	}
	return nil
}

func (n *Node) countVotes() int {
	granted := 0
	for _, g := range n.votes {
		if g {
			granted++
		}
	}
	return granted
}

func (n *Node) becomeLeader() {
	n.role = Leader
	n.leader = n.id
	n.votes = nil
	n.heartbeatElapsed = 0
	n.logger.Info("became leader", "node", n.id, "term", n.hs.Term)
	n.broadcastHeartbeat() // assert leadership immediately
}

func (n *Node) broadcastHeartbeat() {
	for _, p := range n.peers {
		n.send(Message{Type: MsgHeartbeat, To: p})
	}
}

// logUpToDate implements the voting rule of section 5.4.1: the candidate's log
// must be at least as up to date as ours. A later last term wins; with equal
// terms the longer log wins.
func (n *Node) logUpToDate(lastIndex, lastTerm uint64) bool {
	var myIndex, myTerm uint64
	if n.log != nil {
		myIndex, myTerm = n.log.LastIndexTerm()
	}
	return lastTerm > myTerm || (lastTerm == myTerm && lastIndex >= myIndex)
}

func (n *Node) handleRequestVote(m Message) error {
	canVote := n.hs.VotedFor == None || n.hs.VotedFor == m.From
	if !canVote || !n.logUpToDate(m.LastLogIndex, m.LastLogTerm) {
		n.send(Message{Type: MsgRequestVoteResp, To: m.From, Granted: false})
		return nil
	}
	// The vote must be durable before the candidate hears about it, otherwise a
	// crash could let this node vote again in the same term.
	if err := n.setHardState(HardState{Term: n.hs.Term, VotedFor: m.From}); err != nil {
		return err
	}
	n.resetElectionTimer() // do not start our own election right after granting
	n.send(Message{Type: MsgRequestVoteResp, To: m.From, Granted: true})
	return nil
}

func (n *Node) handleRequestVoteResp(m Message) {
	if n.role != Candidate {
		return
	}
	n.votes[m.From] = m.Granted
	if n.countVotes() >= n.quorum {
		n.becomeLeader()
	}
}

func (n *Node) handleHeartbeat(m Message) {
	if n.role == Leader {
		// Two leaders in one term would break election safety; it cannot happen
		// while every node votes at most once per term.
		n.logger.Error("received a heartbeat from another leader in my term", "node", n.id, "term", n.hs.Term, "from", m.From)
		return
	}
	if n.role == Candidate {
		// A leader was elected in this term: stop campaigning.
		n.logger.Info("became follower", "node", n.id, "term", n.hs.Term, "was", n.role.String())
		n.role = Follower
		n.votes = nil
	}
	n.leader = m.From
	n.resetElectionTimer()
	n.send(Message{Type: MsgHeartbeatResp, To: m.From, Success: true})
}
