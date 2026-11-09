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
	// Snapshots persists the latest snapshot. Optional: nil means an in-memory
	// store that is lost on restart.
	Snapshots SnapshotStore
	// SnapshotTrailing is how many entries below a snapshot stay in the log.
	// A follower that is only slightly behind can then be served from the log
	// instead of receiving the whole snapshot.
	SnapshotTrailing uint64
	// MaxBatchEntries and MaxBatchBytes limit how much one Append message
	// carries. Zero means the default. A single entry larger than MaxBatchBytes
	// is still sent on its own.
	MaxBatchEntries int
	MaxBatchBytes   int
	// PreVote makes a node whose election timer ran out first ask the others
	// whether they would vote for it, without changing its term. Only when a
	// majority says yes does it start a real election. A node that merely cannot
	// hear the leader (it was restarted, or the network between them is broken)
	// then no longer forces an election on a healthy cluster by raising the term.
	// Nodes that have heard from a leader within an election timeout refuse.
	// Every node of a cluster must use the same setting: a node without it does not
	// understand the new messages.
	PreVote bool
	// CheckQuorum makes a leader step down when it has not heard from a majority of
	// the cluster for an election timeout, so a leader that is cut off stops
	// believing it leads. With it, a leader also ignores vote requests from nodes
	// that would depose it while it is in contact with a majority.
	CheckQuorum bool
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

// Node is the Raft state machine: leader election, log replication and
// snapshots.
//
// It does no I/O besides its Storage, LogStore and SnapshotStore and never
// looks at a clock. The caller drives it: Tick() advances logical time, Step()
// delivers a received message, Propose() submits a command on the leader,
// Messages() returns what the node wants to send, and Committed() returns the
// entries that are ready to be applied to the state machine. TakeRestore()
// returns a snapshot the state machine must load before it applies anything
// else, and Compact() folds applied entries into a snapshot. A Node is not safe
// for concurrent use; Runner wraps it with a lock for real deployments.
type Node struct {
	id                            NodeID
	peers                         []NodeID // every other member, sorted
	quorum                        int
	electionTicks, heartbeatTicks int
	maxBatchEntries               int
	maxBatchBytes                 int
	snapshotTrailing              uint64
	storage                       Storage
	log                           LogStore
	snaps                         SnapshotStore
	rnd                           *rand.Rand
	logger                        *slog.Logger

	hs     HardState
	role   Role
	leader NodeID
	votes  map[NodeID]bool // candidate only: who answered, and whether they granted

	preVote, checkQuorum bool
	// While preVoting, a follower is asking whether it would win an election. It
	// is still a follower, in its own term; preVotes records who has answered.
	preVoting bool
	preVotes  map[NodeID]bool
	// Leader only, with checkQuorum: the followers heard from since the last check.
	active map[NodeID]bool

	// Volatile state of the log (section 5.3). Without a snapshot both start at
	// 0 after a restart and are rebuilt from the leader's messages; with one
	// they start at the snapshot's index.
	commitIndex uint64 // highest index known to be committed
	lastApplied uint64 // highest index already handed to the state machine

	// The latest snapshot, kept in memory so it can be sent to followers. Index
	// 0 means there is none. restore is a snapshot the state machine has not
	// loaded yet (see TakeRestore).
	snapshot Snapshot
	restore  *Snapshot

	// Leader only: for each follower, the next index to send and the highest
	// index known to be replicated on it. snapshotPause counts the ticks during
	// which a snapshot that was just sent is given time to arrive.
	nextIndex     map[NodeID]uint64
	matchIndex    map[NodeID]uint64
	snapshotPause map[NodeID]int

	electionElapsed   int
	randomizedTimeout int
	heartbeatElapsed  int

	// Counters for observation only; the protocol never reads them.
	campaigns     uint64 // elections this node started
	wins          uint64 // elections it won
	preVoteRounds uint64 // pre-vote rounds it started
	quorumLost    uint64 // times it stepped down as leader for lack of contact with a majority

	outbox []Message
}

// New creates a node and restores its persistent state from cfg.Storage,
// cfg.Snapshots and cfg.Log.
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
	if cfg.Snapshots == nil {
		cfg.Snapshots = NewMemorySnapshotStore()
	}

	n := &Node{
		id:               cfg.ID,
		quorum:           len(cfg.Cluster)/2 + 1,
		electionTicks:    cfg.ElectionTicks,
		heartbeatTicks:   cfg.HeartbeatTicks,
		maxBatchEntries:  cfg.MaxBatchEntries,
		maxBatchBytes:    cfg.MaxBatchBytes,
		snapshotTrailing: cfg.SnapshotTrailing,
		storage:          cfg.Storage,
		log:              cfg.Log,
		snaps:            cfg.Snapshots,
		preVote:          cfg.PreVote,
		checkQuorum:      cfg.CheckQuorum,
		rnd:              cfg.Rand,
		logger:           cfg.Logger,
		hs:               hs,
		role:             Follower,
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

	if err := n.recoverSnapshot(); err != nil {
		return nil, err
	}

	// A log entry from a term newer than the node's own means the term file was
	// lost or the log came from somewhere else. Starting would break safety.
	lastTerm, err := n.log.Term(n.log.LastIndex())
	if err != nil {
		return nil, fmt.Errorf("raft: read last log entry: %w", err)
	}
	if lastTerm > hs.Term {
		return nil, fmt.Errorf("raft: log ends in term %d but the saved term is only %d", lastTerm, hs.Term)
	}
	n.resetElectionTimer()
	return n, nil
}

// recoverSnapshot loads the snapshot and makes the log agree with it. The
// state machine starts from the snapshot, so everything up to its index counts
// as committed and applied.
func (n *Node) recoverSnapshot() error {
	snap, ok, err := n.snaps.Load()
	if err != nil {
		return fmt.Errorf("raft: load snapshot: %w", err)
	}
	baseIndex, _ := n.log.Base()
	if !ok {
		if baseIndex > 0 {
			return fmt.Errorf("raft: the log was compacted up to index %d but there is no snapshot", baseIndex)
		}
		return nil
	}

	switch {
	case snap.Index > n.log.LastIndex():
		// The snapshot was saved, but the log was not yet cut down to it (a crash
		// in between, or a snapshot received from the leader). The snapshot is
		// ahead of everything the log holds, so it replaces the log.
		if err := n.log.CompactTo(snap.Index, snap.Term); err != nil {
			return fmt.Errorf("raft: align the log with the snapshot: %w", err)
		}
	case baseIndex > snap.Index:
		return fmt.Errorf("raft: the log was compacted up to index %d, past the snapshot at index %d", baseIndex, snap.Index)
	default:
		if term, err := n.log.Term(snap.Index); err != nil || term != snap.Term {
			return fmt.Errorf("raft: the snapshot (index %d, term %d) disagrees with the log", snap.Index, snap.Term)
		}
	}

	n.snapshot = snap
	n.commitIndex, n.lastApplied = snap.Index, snap.Index
	restore := copySnapshot(snap)
	n.restore = &restore
	return nil
}

// Status returns a snapshot of the node.
func (n *Node) Status() Status {
	return Status{
		ID:            n.id,
		Role:          n.role,
		Term:          n.hs.Term,
		Leader:        n.leader,
		VotedFor:      n.hs.VotedFor,
		CommitIndex:   n.commitIndex,
		LastIndex:     n.log.LastIndex(),
		SnapshotIndex: n.snapshot.Index,
	}
}

// FollowerProgress is how far a leader knows a follower has got.
type FollowerProgress struct {
	ID NodeID
	// Match is the highest log index known to be stored on the follower.
	Match uint64
	// Next is the next index the leader will send it.
	Next uint64
}

// Stats are counters and, on a leader, the progress of every follower. They exist
// to be looked at: nothing in the protocol depends on them.
type Stats struct {
	// Applied is the highest index handed to the state machine.
	Applied uint64
	// Campaigns counts the elections this node started, Wins those it won.
	Campaigns, Wins uint64
	// PreVotes counts the pre-vote rounds it started, and QuorumLost the times it
	// stepped down as leader for lack of contact with a majority.
	PreVotes, QuorumLost uint64
	// Followers is empty unless the node is the leader; sorted by node ID.
	Followers []FollowerProgress
}

// Stats returns the counters of the node.
func (n *Node) Stats() Stats {
	s := Stats{Applied: n.lastApplied, Campaigns: n.campaigns, Wins: n.wins, PreVotes: n.preVoteRounds, QuorumLost: n.quorumLost}
	if n.role == Leader {
		for _, p := range n.peers {
			s.Followers = append(s.Followers, FollowerProgress{ID: p, Match: n.matchIndex[p], Next: n.nextIndex[p]})
		}
		sort.Slice(s.Followers, func(i, j int) bool { return s.Followers[i].ID < s.Followers[j].ID })
	}
	return s
}

// Messages returns the messages queued for sending and clears the queue.
func (n *Node) Messages() []Message {
	out := n.outbox
	n.outbox = nil
	return out
}

// TakeRestore returns a snapshot that the state machine must load, or nil. It
// happens at startup if a snapshot exists, and when a leader sends this node a
// snapshot because the entries it was missing no longer exist. The state
// machine must load it before applying any entry that Committed() returns
// afterwards, and it replaces all state it had.
func (n *Node) TakeRestore() *Snapshot {
	s := n.restore
	n.restore = nil
	return s
}

// Committed returns the entries that became committed since the last call, in
// log order, and marks them as handed over. The caller applies them to the
// state machine. Without a snapshot, a restarted node starts again from
// index 1, so a state machine that outlives the process must skip entries it
// already applied.
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

// Compact folds the applied entries up to index into a snapshot. data is the
// state machine's state after applying entry index, and index must already
// have been handed out by Committed().
//
// The snapshot is saved first and the log is cut down afterwards, so a crash
// in between loses nothing. The log keeps SnapshotTrailing entries below
// index. Compact does nothing if a snapshot at or beyond index exists.
func (n *Node) Compact(index uint64, data []byte) error {
	if index <= n.snapshot.Index {
		return nil
	}
	if index > n.lastApplied {
		return fmt.Errorf("raft: cannot snapshot index %d: only %d entries were handed to the state machine", index, n.lastApplied)
	}
	term, err := n.log.Term(index)
	if err != nil {
		return fmt.Errorf("raft: snapshot index %d: %w", index, err)
	}

	snap := Snapshot{Index: index, Term: term, Data: append([]byte(nil), data...)}
	if err := n.snaps.Save(snap); err != nil {
		return fmt.Errorf("raft: save snapshot: %w", err)
	}
	n.snapshot = snap

	// Keep a tail of the log so that a follower that is just a little behind
	// can still be served from it.
	if index <= n.snapshotTrailing {
		return nil
	}
	cut := index - n.snapshotTrailing
	if baseIndex, _ := n.log.Base(); cut <= baseIndex {
		return nil
	}
	cutTerm, err := n.log.Term(cut)
	if err != nil {
		return fmt.Errorf("raft: compact the log: %w", err)
	}
	if err := n.log.CompactTo(cut, cutTerm); err != nil {
		return fmt.Errorf("raft: compact the log: %w", err)
	}
	return nil
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
		for p, left := range n.snapshotPause {
			if left > 0 {
				n.snapshotPause[p] = left - 1
			}
		}
		n.heartbeatElapsed++
		if n.heartbeatElapsed >= n.heartbeatTicks {
			n.heartbeatElapsed = 0
			n.broadcastAppend()
		}
		if n.checkQuorum {
			n.electionElapsed++
			if n.electionElapsed >= n.electionTicks {
				n.electionElapsed = 0
				if len(n.active)+1 < n.quorum {
					// Not even a majority answered in a whole election timeout: this
					// leader may be the one on the wrong side of a broken network, and
					// the others may already have chosen another. Stop leading.
					n.quorumLost++
					n.logger.Info("stepping down: no contact with a majority", "node", n.id, "term", n.hs.Term, "heard_from", len(n.active)+1)
					return n.becomeFollower(n.hs.Term, None)
				}
				n.active = make(map[NodeID]bool, len(n.peers))
			}
		}
		return nil
	}
	n.electionElapsed++
	if n.electionElapsed >= n.randomizedTimeout {
		if n.preVote && n.role == Follower && len(n.peers) > 0 {
			return n.startPreVote()
		}
		return n.campaign()
	}
	return nil
}

// Step handles a message received from another node.
func (n *Node) Step(m Message) error {
	if m.To != n.id || m.From == n.id || !n.isMember(m.From) {
		return nil // not for us, or from a stranger: ignore
	}

	// A pre-vote is a question about a term that nobody has entered yet, so it is
	// answered without the usual rules about terms, which would make the receiver
	// adopt the asker's term.
	switch m.Type {
	case MsgPreVote:
		n.handlePreVote(m)
		return nil
	case MsgPreVoteResp:
		if m.Granted {
			return n.handlePreVoteGranted(m)
		}
		// A refusal carries the refuser's own term, handled like any message below.
	case MsgRequestVote:
		// A node that has heard from a leader within an election timeout does not
		// take a vote request from a higher term seriously and does not even adopt
		// the term (section 4.2.3 of the thesis): the cluster has a leader, and the
		// asker is out of touch with it. A candidate that is not is told in the usual
		// way once the leader's contact times out.
		if m.Term > n.hs.Term && n.inLease() {
			return nil
		}
	}

	switch {
	case m.Term > n.hs.Term:
		// A newer term exists: this node is out of date, whatever its role.
		var leader NodeID
		if m.Type == MsgAppend || m.Type == MsgSnapshot {
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
		case MsgSnapshot:
			n.send(Message{Type: MsgSnapshotResp, To: m.From, Success: false})
		}
		return nil
	}

	// From here on m.Term == n.hs.Term.
	switch m.Type {
	case MsgPreVoteResp:
		return nil // a refusal that tells us nothing new
	case MsgRequestVote:
		return n.handleRequestVote(m)
	case MsgRequestVoteResp:
		return n.handleRequestVoteResp(m)
	case MsgAppend:
		return n.handleAppend(m)
	case MsgSnapshot:
		return n.handleSnapshot(m)
	case MsgAppendResp, MsgSnapshotResp:
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
	n.preVoting, n.preVotes, n.active = false, nil, nil
	n.nextIndex, n.matchIndex, n.snapshotPause = nil, nil, nil
	n.resetElectionTimer()
	return nil
}

// campaign starts an election: new term, vote for self, ask everyone else.
func (n *Node) campaign() error {
	if err := n.setHardState(HardState{Term: n.hs.Term + 1, VotedFor: n.id}); err != nil {
		return err
	}
	n.role = Candidate
	n.campaigns++
	n.preVoting, n.preVotes = false, nil
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

// startPreVote asks the others whether they would vote for this node in the term
// it would start, without changing its own term or vote. It stays a follower: only
// a majority of yeses turns it into a candidate (see handlePreVoteGranted). The
// node no longer counts the old leader as alive, since it just waited out the
// timeout for hearing from it.
func (n *Node) startPreVote() error {
	n.preVoting = true
	n.preVoteRounds++
	n.leader = None
	n.preVotes = map[NodeID]bool{n.id: true}
	n.resetElectionTimer() // if this round fails, try again after another timeout
	n.logger.Info("asking for pre-votes", "node", n.id, "term", n.hs.Term)

	lastIndex, lastTerm := n.lastIndexTerm()
	for _, p := range n.peers {
		n.sendAtTerm(Message{Type: MsgPreVote, To: p, LastLogIndex: lastIndex, LastLogTerm: lastTerm}, n.hs.Term+1)
	}
	return nil
}

// sendAtTerm queues a message that carries a term other than the node's own.
func (n *Node) sendAtTerm(m Message, term uint64) {
	m.From = n.id
	m.Term = term
	n.outbox = append(n.outbox, m)
}

// inLease reports whether this node counts the cluster as having a live leader: it
// is the leader and checks its quorum, or it heard from a leader within the base
// election timeout. A node in the lease refuses to help anyone start an election.
// It is only used when pre-vote or check-quorum is on.
func (n *Node) inLease() bool {
	if !n.preVote && !n.checkQuorum {
		return false
	}
	if n.role == Leader {
		return n.checkQuorum
	}
	return n.leader != None && n.electionElapsed < n.electionTicks
}

// handlePreVote answers a pre-vote. It changes nothing in this node: no term, no
// vote, no timer. The answer is yes when the asker's term is newer than ours, its
// log is at least as up to date as ours, and we do not know a live leader.
func (n *Node) handlePreVote(m Message) {
	if m.Term > n.hs.Term && n.logUpToDate(m.LastLogIndex, m.LastLogTerm) && !n.inLease() {
		n.sendAtTerm(Message{Type: MsgPreVoteResp, To: m.From, Granted: true}, m.Term)
		return
	}
	n.send(Message{Type: MsgPreVoteResp, To: m.From, Granted: false}) // carries our own term
}

// handlePreVoteGranted counts a yes. When a majority has said yes, the election is
// held for real.
func (n *Node) handlePreVoteGranted(m Message) error {
	// A yes is for the term we asked about: one more than ours. Anything else is an
	// answer to a round that is over.
	if !n.preVoting || n.role != Follower || m.Term != n.hs.Term+1 {
		return nil
	}
	n.preVotes[m.From] = true
	granted := 0
	for _, g := range n.preVotes {
		if g {
			granted++
		}
	}
	if granted < n.quorum {
		return nil
	}
	n.preVoting, n.preVotes = false, nil
	return n.campaign()
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
	n.wins++
	n.electionElapsed = 0
	n.preVoting, n.preVotes = false, nil
	n.active = make(map[NodeID]bool, len(n.peers))
	n.leader = n.id
	n.votes = nil
	n.heartbeatElapsed = 0
	n.nextIndex = make(map[NodeID]uint64, len(n.peers))
	n.matchIndex = make(map[NodeID]uint64, len(n.peers))
	n.snapshotPause = make(map[NodeID]int, len(n.peers))
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
	if next < n.log.FirstIndex() {
		// The entries this follower needs were folded into a snapshot.
		n.sendSnapshot(to)
		return
	}
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

// sendSnapshot sends the latest snapshot to a follower that is too far behind
// for the log. A snapshot can be large and take a while to install, so after
// sending one the leader waits an election timeout before sending it again and
// meanwhile sends plain heartbeats that keep the follower from campaigning.
func (n *Node) sendSnapshot(to NodeID) {
	baseIndex, baseTerm := n.log.Base()
	if n.snapshot.Index == 0 || n.snapshotPause[to] > 0 {
		n.send(Message{Type: MsgAppend, To: to, PrevLogIndex: baseIndex, PrevLogTerm: baseTerm, LeaderCommit: n.commitIndex})
		return
	}
	n.snapshotPause[to] = n.electionTicks
	snap := n.snapshot // the data is never modified, so it can be shared
	n.send(Message{Type: MsgSnapshot, To: to, Snapshot: &snap})
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

// acceptLeader records that m comes from the leader of the current term.
// It reports false if this node is itself the leader, which cannot happen
// while every node votes at most once per term.
func (n *Node) acceptLeader(m Message) bool {
	if n.role == Leader {
		n.logger.Error("received a message from another leader in my term", "node", n.id, "term", n.hs.Term, "from", m.From, "type", m.Type.String())
		return false
	}
	if n.role == Candidate {
		// A leader was elected in this term: stop campaigning.
		n.logger.Info("became follower", "node", n.id, "term", n.hs.Term, "was", n.role.String())
		n.role = Follower
		n.votes = nil
	}
	n.leader = m.From
	n.preVoting, n.preVotes = false, nil // a leader exists: nothing to ask about
	n.resetElectionTimer()
	return true
}

// handleAppend is the follower side of AppendEntries (Figure 2).
func (n *Node) handleAppend(m Message) error {
	if !n.acceptLeader(m) {
		return nil
	}

	for i, e := range m.Entries {
		if e.Index != m.PrevLogIndex+1+uint64(i) {
			n.logger.Warn("dropping a malformed append", "node", n.id, "from", m.From)
			return nil
		}
	}
	lastNew := m.PrevLogIndex + uint64(len(m.Entries))

	// If the leader starts below our snapshot, the first entries are committed
	// here already (a snapshot only ever covers committed entries). Skip them
	// and check consistency at the snapshot boundary instead.
	prevIndex, prevTerm, entries := m.PrevLogIndex, m.PrevLogTerm, m.Entries
	if baseIndex, baseTerm := n.log.Base(); prevIndex < baseIndex {
		skip := baseIndex - prevIndex
		if skip >= uint64(len(entries)) {
			entries = nil
		} else {
			entries = entries[skip:]
		}
		prevIndex, prevTerm = baseIndex, baseTerm
	}

	// Consistency check: our log must contain the entry just before the new ones.
	if hint, ok := n.matchesPrev(prevIndex, prevTerm); !ok {
		n.send(Message{Type: MsgAppendResp, To: m.From, Success: false, HintIndex: hint})
		return nil
	}

	if err := n.storeEntries(entries); err != nil {
		return err
	}

	if newCommit := min(m.LeaderCommit, lastNew); newCommit > n.commitIndex {
		n.commitIndex = newCommit
	}
	n.send(Message{Type: MsgAppendResp, To: m.From, Success: true, MatchIndex: lastNew})
	return nil
}

// handleSnapshot is the follower side of InstallSnapshot.
func (n *Node) handleSnapshot(m Message) error {
	if !n.acceptLeader(m) {
		return nil
	}
	s := m.Snapshot
	if s == nil || s.Index == 0 {
		n.logger.Warn("dropping a snapshot message without a snapshot", "node", n.id, "from", m.From)
		return nil
	}

	if s.Index <= n.commitIndex {
		// Everything the snapshot covers is committed here already, and committed
		// entries are the same on every node, so the logs match up to s.Index.
		n.send(Message{Type: MsgSnapshotResp, To: m.From, Success: true, MatchIndex: s.Index})
		return nil
	}

	// Save the snapshot first. If the log were cut first and the node crashed,
	// the entries would exist nowhere.
	snap := copySnapshot(*s)
	if err := n.snaps.Save(snap); err != nil {
		return fmt.Errorf("raft: save received snapshot: %w", err)
	}
	// Keep the entries after the snapshot if our log agrees with it, otherwise
	// drop the whole log (section 7, step 6 and 7).
	if err := n.log.CompactTo(snap.Index, snap.Term); err != nil {
		return fmt.Errorf("raft: apply received snapshot to the log: %w", err)
	}
	n.snapshot = snap
	n.commitIndex, n.lastApplied = snap.Index, snap.Index
	restore := copySnapshot(snap)
	n.restore = &restore

	n.send(Message{Type: MsgSnapshotResp, To: m.From, Success: true, MatchIndex: snap.Index})
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

// handleAppendResp is the leader side of AppendEntries and InstallSnapshot
// replies.
func (n *Node) handleAppendResp(m Message) {
	if n.role != Leader {
		return
	}
	if n.checkQuorum {
		n.active[m.From] = true // any answer in the current term shows the follower is in touch
	}
	if m.Type == MsgSnapshotResp {
		n.snapshotPause[m.From] = 0 // answered: no need to wait any longer
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

	cur := n.nextIndex[m.From]
	if cur < n.log.FirstIndex() {
		// This follower is served from the snapshot; a reject to a heartbeat
		// tells the leader nothing new.
		return
	}

	// The follower's log does not match at PrevLogIndex: back up and retry.
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
