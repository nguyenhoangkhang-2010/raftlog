package raft

import "fmt"

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

// MessageType is the kind of a Message.
type MessageType int

const (
	MsgRequestVote MessageType = iota + 1
	MsgRequestVoteResp
	// MsgHeartbeat is an AppendEntries RPC without entries. Log replication
	// will extend it in a later branch.
	MsgHeartbeat
	MsgHeartbeatResp
)

func (t MessageType) String() string {
	switch t {
	case MsgRequestVote:
		return "RequestVote"
	case MsgRequestVoteResp:
		return "RequestVoteResp"
	case MsgHeartbeat:
		return "Heartbeat"
	case MsgHeartbeatResp:
		return "HeartbeatResp"
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
	// HeartbeatResp: false if the heartbeat came from a stale leader.
	Success bool
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

// LogView exposes the last entry of the replicated log, which voting needs
// ("is the candidate's log at least as up to date as mine?"). Until log
// replication exists, the log is empty and both values are zero.
type LogView interface {
	LastIndexTerm() (index, term uint64)
}

// Status is a snapshot of a node.
type Status struct {
	ID       NodeID
	Role     Role
	Term     uint64
	Leader   NodeID // None if unknown
	VotedFor NodeID
}
