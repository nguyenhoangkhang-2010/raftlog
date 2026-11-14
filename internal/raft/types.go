package raft

import (
	"errors"
	"fmt"
)

// NodeID identifies a member of the cluster. Zero is reserved to mean "none".
type NodeID uint64

// None is the absence of a node (no leader, no vote).
const None NodeID = 0

// Role is the state of a node in the Raft protocol.
type Role int

const (
	Follower Role = iota
	Candidate
	Leader
)

func (r Role) String() string {
	switch r {
	case Follower:
		return "follower"
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	default:
		return fmt.Sprintf("role(%d)", int(r))
	}
}

// EntryType tells the state machine what to do with a log entry.
type EntryType int

const (
	// EntryNormal carries a command proposed by a client.
	EntryNormal EntryType = iota
	// EntryNoop is appended by a new leader. Committing it also commits every
	// entry from earlier terms before it, and it carries no command.
	EntryNoop
)

// Entry is one element of the replicated log. Indexes start at 1.
type Entry struct {
	Index uint64
	Term  uint64
	Type  EntryType
	Data  []byte
}

// Snapshot is the state of the state machine after applying every entry up to
// and including Index, whose term was Term. It replaces those log entries.
type Snapshot struct {
	Index uint64
	Term  uint64
	Data  []byte
}

// MessageType is the kind of a Message.
type MessageType int

const (
	MsgRequestVote MessageType = iota + 1
	MsgRequestVoteResp
	// MsgAppend is the AppendEntries RPC. Without entries it is a heartbeat.
	MsgAppend
	MsgAppendResp
	// MsgSnapshot is the InstallSnapshot RPC: the leader no longer has the
	// entries a follower needs, so it sends the state they were folded into.
	MsgSnapshot
	MsgSnapshotResp
	// MsgPreVote asks whether the sender would win an election in the term it would
	// start, without anyone changing their term or vote. See Config.PreVote.
	MsgPreVote
	MsgPreVoteResp
)

func (t MessageType) String() string {
	switch t {
	case MsgRequestVote:
		return "RequestVote"
	case MsgRequestVoteResp:
		return "RequestVoteResp"
	case MsgAppend:
		return "Append"
	case MsgAppendResp:
		return "AppendResp"
	case MsgSnapshot:
		return "Snapshot"
	case MsgSnapshotResp:
		return "SnapshotResp"
	case MsgPreVote:
		return "PreVote"
	case MsgPreVoteResp:
		return "PreVoteResp"
	default:
		return fmt.Sprintf("MessageType(%d)", int(t))
	}
}

// Message is the only thing nodes exchange. Raft RPCs are modelled as one-way
// messages; a reply is a separate message in the opposite direction. That keeps
// the protocol core free of networking and easy to test deterministically.
type Message struct {
	Type MessageType
	From NodeID
	To   NodeID
	// Term is the sender's current term.
	Term uint64

	// RequestVote: the candidate's last log entry.
	LastLogIndex uint64
	LastLogTerm  uint64

	// RequestVoteResp: whether the vote was granted.
	Granted bool

	// Append: the entry that immediately precedes Entries in the leader's log,
	// the entries to store, and the leader's commit index.
	PrevLogIndex uint64
	PrevLogTerm  uint64
	Entries      []Entry
	LeaderCommit uint64

	// AppendResp and SnapshotResp: Success says whether the follower's log
	// matched at PrevLogIndex (or accepted the snapshot). On success MatchIndex
	// is the last index now known to match the leader's log; on failure
	// HintIndex is the last index the leader can assume matches, which lets it
	// back up faster than one entry at a time.
	Success    bool
	MatchIndex uint64
	HintIndex  uint64

	// Snapshot (InstallSnapshot): the complete snapshot.
	Snapshot *Snapshot
}

// HardState is the part of the state that Raft requires to survive a restart.
// Forgetting it can make a node vote twice in one term and break safety.
type HardState struct {
	Term     uint64
	VotedFor NodeID
}

// Storage persists the HardState. Save must not return before the data is
// durable; the node sends no message that depends on a state until it is saved.
type Storage interface {
	// Load returns the saved state, or the zero HardState if none was saved yet.
	Load() (HardState, error)
	Save(HardState) error
}

// SnapshotStore persists the latest snapshot. Only the newest one is kept.
type SnapshotStore interface {
	// Load returns the saved snapshot, or false if there is none.
	Load() (Snapshot, bool, error)
	// Save makes snap the stored snapshot. It must be durable, and an old
	// snapshot must survive until the new one is completely written.
	Save(Snapshot) error
}

// Status is a snapshot of a node.
type Status struct {
	ID       NodeID
	Role     Role
	Term     uint64
	Leader   NodeID // None if unknown
	VotedFor NodeID
	// CommitIndex is the highest log index known to be committed.
	CommitIndex uint64
	// LastIndex is the index of the last entry in the node's log.
	LastIndex uint64
	// SnapshotIndex is the last log index covered by the latest snapshot, or 0.
	SnapshotIndex uint64
}

// ErrNotLeader is returned when a proposal reaches a node that is not the leader.
var ErrNotLeader = errors.New("raft: not the leader")

// NotLeaderError is a proposal rejected by a node that is not the leader. Leader
// is the node it believes is the leader, or None if it does not know.
type NotLeaderError struct{ Leader NodeID }

func (e *NotLeaderError) Error() string {
	if e.Leader == None {
		return "raft: not the leader (leader unknown)"
	}
	return fmt.Sprintf("raft: not the leader (leader is node %d)", e.Leader)
}

// Unwrap lets errors.Is(err, ErrNotLeader) work.
func (e *NotLeaderError) Unwrap() error { return ErrNotLeader }
