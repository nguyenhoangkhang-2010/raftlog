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

// Defaults for the batching limits of Config.
const (
	defaultMaxBatchEntries = 64
	defaultMaxBatchBytes   = 1 << 20
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
	// Log persists the replicated log. Optional: nil means an in-memory log
	// that is lost on restart.
	Log LogStore
	// MaxBatchEntries and MaxBatchBytes limit how much one Append message
	// carries. Zero means the default. A single entry larger than MaxBatchBytes
	// is still sent on its own.
	MaxBatchEntries int
	MaxBatchBytes   int
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
	if c.MaxBatchEntries < 0 || c.MaxBatchBytes < 0 {
		errs = append(errs, errors.New("raft: batch limits must not be negative"))
	}
	return errors.Join(errs...)
}

// Node is the Raft state machine: leader election and log replication.
//
// It does no I/O besides its Storage and LogStore and never looks at a clock.
// The caller drives it: Tick() advances logical time, Step() delivers a
// received message, Propose() submits a command on the leader, Messages()
// returns what the node wants to send, and Committed() returns the entries
// that are ready to be applied to the state machine. A Node is not safe for
// concurrent use; Runner wraps it with a lock for real deployments.
type Node struct {
	id                            NodeID
	peers                         []NodeID // every other member, sorted
	quorum                        int
	electionTicks, heartbeatTicks int
	maxBatchEntries               int
	maxBatchBytes                 int
	storage                       Storage
	log                           LogStore
	rnd                           *rand.Rand
	logger                        *slog.Logger

	hs     HardState
	role   Role
	leader NodeID
	votes  map[NodeID]bool // candidate only: who answered, and whether they granted

	// Volatile state of the log (section 5.3). After a restart both start at 0
	// and are rebuilt from the leader's messages.
	commitIndex uint64 // highest index known to be committed
	lastApplied uint64 // highest index already returned by Committed()

	// Leader only: for each follower, the next index to send and the highest
	// index known to be replicated on it.
	nextIndex  map[NodeID]uint64
	matchIndex map[NodeID]uint64

	electionElapsed   int
	randomizedTimeout int
	heartbeatElapsed  int

	outbox []Message
}

// New creates a node and restores its persistent state from cfg.Storage and
// cfg.Log.
func New(cfg Config) (*Node, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	hs, err := cfg.Storage.Load()
	if err != nil {
		return nil, fmt.Errorf("raft: load state: %w", err)
	}
	if cfg.Log == nil {
		cfg.Log = NewMemoryLog()
	}
	// A log entry from a term newer than the node's own means the term file was
	// lost or the log came from somewhere else. Starting would break safety.
	lastTerm, err := cfg.Log.Term(cfg.Log.LastIndex())
	if err != nil {
		return nil, fmt.Errorf("raft: read last log entry: %w", err)
	}
	if lastTerm > hs.Term {
		return nil, fmt.Errorf("raft: log ends in term %d but the saved term is only %d", lastTerm, hs.Term)
	}

	n := &Node{
		id:              cfg.ID,
		quorum:          len(cfg.Cluster)/2 + 1,
		electionTicks:   cfg.ElectionTicks,
		heartbeatTicks:  cfg.HeartbeatTicks,
		maxBatchEntries: cfg.MaxBatchEntries,
		maxBatchBytes:   cfg.MaxBatchBytes,
		storage:         cfg.Storage,
		log:             cfg.Log,
		rnd:             cfg.Rand,
		logger:          cfg.Logger,
		hs:              hs,
		role:            Follower,
	}
	if n.maxBatchEntries == 0 {
		n.maxBatchEntries = defaultMaxBatchEntries
	}
	if n.maxBatchBytes == 0 {
		n.maxBatchBytes = defaultMaxBatchBytes
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
	return Status{
		ID:          n.id,
		Role:        n.role,
		Term:        n.hs.Term,
		Leader:      n.leader,
		VotedFor:    n.hs.VotedFor,
		CommitIndex: n.commitIndex,
		LastIndex:   n.log.LastIndex(),
	}
}

// Messages returns the messages queued for sending and clears the queue.
func (n *Node) Messages() []Message {
	out := n.outbox
	n.outbox = nil
	return out
}

// Committed returns the entries that became committed since the last call, in
// log order, and marks them as handed over. The caller applies them to the
// state machine. After a restart the node starts again from index 1, so a state
// machine that outlives the process must skip entries it already applied.
func (n *Node) Committed() ([]Entry, error) {
	if n.commitIndex <= n.lastApplied {
		return nil, nil
	}
	entries, err := n.log.Entries(n.lastApplied+1, n.commitIndex+1)
	if err != nil {
		return nil, fmt.Errorf("raft: read committed entries: %w", err)
	}
	n.lastApplied = n.commitIndex
	return entries, nil
}

// Propose appends a command to the log. Only the leader accepts it; any other
// node returns a *NotLeaderError. The entry is not committed when Propose
// returns: it counts as committed once Committed() delivers it. If leadership
// changes first, the entry may be overwritten, so a caller must compare the
// term of the delivered entry with the term returned here.
func (n *Node) Propose(data []byte) (index, term uint64, err error) {
	if n.role != Leader {
		return 0, 0, &NotLeaderError{Leader: n.leader}
	}
	e := Entry{Index: n.log.LastIndex() + 1, Term: n.hs.Term, Type: EntryNormal, Data: data}
	if err := n.log.Append([]Entry{e}); err != nil {
		return 0, 0, fmt.Errorf("raft: append proposal: %w", err)
	}
	n.maybeCommit() // a one-node cluster commits at once
	n.broadcastAppend()
	return e.Index, e.Term, nil
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
			n.broadcastAppend()
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
		if m.Type == MsgAppend {
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
		case MsgAppend:
			n.send(Message{Type: MsgAppendResp, To: m.From, Success: false})
		}
		return nil
	}

	// From here on m.Term == n.hs.Term.
	switch m.Type {
	case MsgRequestVote:
		return n.handleRequestVote(m)
	case MsgRequestVoteResp:
		return n.handleRequestVoteResp(m)
	case MsgAppend:
		return n.handleAppend(m)
	case MsgAppendResp:
		n.handleAppendResp(m)
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

// lastIndexTerm returns the index and term of the last log entry.
func (n *Node) lastIndexTerm() (index, term uint64) {
	index = n.log.LastIndex()
	term, _ = n.log.Term(index) // index <= LastIndex, so this cannot fail
	return index, term
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
	n.nextIndex, n.matchIndex = nil, nil
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
		return n.becomeLeader() // a one-node cluster wins at once
	}
	lastIndex, lastTerm := n.lastIndexTerm()
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

// becomeLeader starts the node's leadership. The new leader first appends a
// no-op entry of its own term: committing it commits every earlier entry, and
// it is the only way to learn which earlier entries are committed, because a
// leader never counts replicas for entries of previous terms (section 5.4.2).
func (n *Node) becomeLeader() error {
	noop := Entry{Index: n.log.LastIndex() + 1, Term: n.hs.Term, Type: EntryNoop}
	if err := n.log.Append([]Entry{noop}); err != nil {
		return fmt.Errorf("raft: append no-op: %w", err)
	}

	n.role = Leader
	n.leader = n.id
	n.votes = nil
	n.heartbeatElapsed = 0
	n.nextIndex = make(map[NodeID]uint64, len(n.peers))
	n.matchIndex = make(map[NodeID]uint64, len(n.peers))
	for _, p := range n.peers {
		n.nextIndex[p] = noop.Index // optimistic: assume followers are up to date
		n.matchIndex[p] = 0
	}
	n.logger.Info("became leader", "node", n.id, "term", n.hs.Term)

	n.maybeCommit()
	n.broadcastAppend() // assert leadership immediately
	return nil
}

// broadcastAppend sends every follower the entries it is missing, or just a
// heartbeat if it is up to date.
func (n *Node) broadcastAppend() {
	for _, p := range n.peers {
		n.sendAppend(p)
	}
}

func (n *Node) sendAppend(to NodeID) {
	next := n.nextIndex[to]
	prev := next - 1
	prevTerm, err := n.log.Term(prev)
	if err != nil {
		n.logger.Error("cannot read the log to replicate", "node", n.id, "to", to, "index", prev, "err", err)
		return
	}

	var entries []Entry
	if last := n.log.LastIndex(); next <= last {
		hi := min(last+1, next+uint64(n.maxBatchEntries))
		entries, err = n.log.Entries(next, hi)
		if err != nil {
			n.logger.Error("cannot read the log to replicate", "node", n.id, "to", to, "from", next, "err", err)
			return
		}
		entries = limitBytes(entries, n.maxBatchBytes)
	}
	n.send(Message{
		Type:         MsgAppend,
		To:           to,
		PrevLogIndex: prev,
		PrevLogTerm:  prevTerm,
		Entries:      entries,
		LeaderCommit: n.commitIndex,
	})
}

// limitBytes keeps the longest prefix of entries whose data fits in limit bytes.
// It always keeps at least one entry, so an oversized entry cannot get stuck.
func limitBytes(entries []Entry, limit int) []Entry {
	total := 0
	for i, e := range entries {
		total += len(e.Data)
		if i > 0 && total > limit {
			return entries[:i]
		}
	}
	return entries
}

// maybeCommit advances commitIndex to the highest index that a majority has
// stored and that belongs to the current term. It reports whether it moved.
func (n *Node) maybeCommit() bool {
	for idx := n.log.LastIndex(); idx > n.commitIndex; idx-- {
		term, _ := n.log.Term(idx)
		if term != n.hs.Term {
			// Terms never decrease along the log, so everything below is older
			// still. Those entries become committed only together with a
			// current-term entry above them.
			return false
		}
		replicas := 1 // this node
		for _, p := range n.peers {
			if n.matchIndex[p] >= idx {
				replicas++
			}
		}
		if replicas >= n.quorum {
			n.commitIndex = idx
			return true
		}
	}
	return false
}

// logUpToDate implements the voting rule of section 5.4.1: the candidate's log
// must be at least as up to date as ours. A later last term wins; with equal
// terms the longer log wins.
func (n *Node) logUpToDate(lastIndex, lastTerm uint64) bool {
	myIndex, myTerm := n.lastIndexTerm()
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

func (n *Node) handleRequestVoteResp(m Message) error {
	if n.role != Candidate {
		return nil
	}
	n.votes[m.From] = m.Granted
	if n.countVotes() >= n.quorum {
		return n.becomeLeader()
	}
	return nil
}

// handleAppend is the follower side of AppendEntries (Figure 2).
func (n *Node) handleAppend(m Message) error {
	if n.role == Leader {
		// Two leaders in one term would break election safety; it cannot happen
		// while every node votes at most once per term.
		n.logger.Error("received an append from another leader in my term", "node", n.id, "term", n.hs.Term, "from", m.From)
		return nil
	}
	if n.role == Candidate {
		// A leader was elected in this term: stop campaigning.
		n.logger.Info("became follower", "node", n.id, "term", n.hs.Term, "was", n.role.String())
		n.role = Follower
		n.votes = nil
	}
	n.leader = m.From
	n.resetElectionTimer()

	for i, e := range m.Entries {
		if e.Index != m.PrevLogIndex+1+uint64(i) {
			n.logger.Warn("dropping a malformed append", "node", n.id, "from", m.From)
			return nil
		}
	}

	// Consistency check: our log must contain the entry just before the new ones.
	if hint, ok := n.matchesPrev(m.PrevLogIndex, m.PrevLogTerm); !ok {
		n.send(Message{Type: MsgAppendResp, To: m.From, Success: false, HintIndex: hint})
		return nil
	}

	if err := n.storeEntries(m.Entries); err != nil {
		return err
	}

	lastNew := m.PrevLogIndex + uint64(len(m.Entries))
	if newCommit := min(m.LeaderCommit, lastNew); newCommit > n.commitIndex {
		n.commitIndex = newCommit
	}
	n.send(Message{Type: MsgAppendResp, To: m.From, Success: true, MatchIndex: lastNew})
	return nil
}

// matchesPrev reports whether the log has an entry at index with the given
// term. If not, hint is the last index the leader may assume to match.
func (n *Node) matchesPrev(index, term uint64) (hint uint64, ok bool) {
	last := n.log.LastIndex()
	if index > last {
		return last, false
	}
	have, _ := n.log.Term(index)
	if have == term {
		return 0, true
	}
	// Conflict: skip back over the whole conflicting term, so the leader does not
	// have to probe one entry at a time.
	i := index
	for i > n.commitIndex+1 {
		prev, _ := n.log.Term(i - 1)
		if prev != have {
			break
		}
		i--
	}
	return i - 1, false
}

// storeEntries adds the leader's entries to the log. Entries we already have
// are skipped; at the first entry that conflicts with ours (same index,
// different term) our entry and everything after it are replaced.
func (n *Node) storeEntries(entries []Entry) error {
	for i, e := range entries {
		if e.Index <= n.log.LastIndex() {
			have, _ := n.log.Term(e.Index)
			if have == e.Term {
				continue // already stored (a duplicate or reordered message)
			}
			if e.Index <= n.commitIndex {
				// Committed entries are never replaced; reaching this means the
				// protocol was violated somewhere.
				return fmt.Errorf("raft: leader sent entry %d (term %d) that conflicts with committed entry (term %d)", e.Index, e.Term, have)
			}
			if err := n.log.TruncateSuffix(e.Index); err != nil {
				return fmt.Errorf("raft: truncate conflicting entries: %w", err)
			}
		}
		if err := n.log.Append(entries[i:]); err != nil {
			return fmt.Errorf("raft: append entries: %w", err)
		}
		return nil
	}
	return nil
}

// handleAppendResp is the leader side of AppendEntries replies.
func (n *Node) handleAppendResp(m Message) {
	if n.role != Leader {
		return
	}
	if m.Success {
		if m.MatchIndex > n.matchIndex[m.From] {
			n.matchIndex[m.From] = m.MatchIndex
		}
		if next := n.matchIndex[m.From] + 1; next > n.nextIndex[m.From] {
			n.nextIndex[m.From] = next
		}
		if n.maybeCommit() {
			n.broadcastAppend() // tell the followers about the new commit index
			return
		}
		if n.nextIndex[m.From] <= n.log.LastIndex() {
			n.sendAppend(m.From) // more to send: do not wait for the next heartbeat
		}
		return
	}

	// The follower's log does not match at PrevLogIndex: back up and retry.
	cur := n.nextIndex[m.From]
	next := m.HintIndex + 1
	if next >= cur {
		next = cur - 1 // always move back, or a lying hint could loop forever
	}
	if floor := n.matchIndex[m.From] + 1; next < floor {
		next = floor // never go back past what is known to match
	}
	if next < 1 {
		next = 1
	}
	n.nextIndex[m.From] = next
	// Retry at once only if that changes something. Otherwise the follower would
	// answer the same message the same way, forever; the next heartbeat retries.
	if next != cur {
		n.sendAppend(m.From)
	}
}
