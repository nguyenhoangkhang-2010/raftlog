package raft

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

var typeToPB = map[MessageType]pb.RaftMessageType{
	MsgRequestVote:     pb.RaftMessageType_RAFT_MESSAGE_TYPE_REQUEST_VOTE,
	MsgRequestVoteResp: pb.RaftMessageType_RAFT_MESSAGE_TYPE_REQUEST_VOTE_RESP,
	MsgAppend:          pb.RaftMessageType_RAFT_MESSAGE_TYPE_APPEND_ENTRIES,
	MsgAppendResp:      pb.RaftMessageType_RAFT_MESSAGE_TYPE_APPEND_ENTRIES_RESP,
}

var entryTypeToPB = map[EntryType]pb.LogEntryType{
	EntryNormal: pb.LogEntryType_LOG_ENTRY_TYPE_NORMAL,
	EntryNoop:   pb.LogEntryType_LOG_ENTRY_TYPE_NOOP,
}

var entryTypeFromPB = func() map[pb.LogEntryType]EntryType {
	m := make(map[pb.LogEntryType]EntryType, len(entryTypeToPB))
	for k, v := range entryTypeToPB {
		m[v] = k
	}
	return m
}()

var typeFromPB = func() map[pb.RaftMessageType]MessageType {
	m := make(map[pb.RaftMessageType]MessageType, len(typeToPB))
	for k, v := range typeToPB {
		m[v] = k
	}
	return m
}()

func messageToPB(m Message) (*pb.RaftMessage, error) {
	t, ok := typeToPB[m.Type]
	if !ok {
		return nil, fmt.Errorf("raft: cannot send message of type %v", m.Type)
	}
	entries := make([]*pb.LogEntry, len(m.Entries))
	for i, e := range m.Entries {
		et, ok := entryTypeToPB[e.Type]
		if !ok {
			return nil, fmt.Errorf("raft: cannot send entry %d of type %d", e.Index, e.Type)
		}
		entries[i] = &pb.LogEntry{Index: e.Index, Term: e.Term, Type: et, Data: e.Data}
	}
	return &pb.RaftMessage{
		Type:         t,
		From:         uint64(m.From),
		To:           uint64(m.To),
		Term:         m.Term,
		LastLogIndex: m.LastLogIndex,
		LastLogTerm:  m.LastLogTerm,
		Granted:      m.Granted,
		Success:      m.Success,
		PrevLogIndex: m.PrevLogIndex,
		PrevLogTerm:  m.PrevLogTerm,
		Entries:      entries,
		LeaderCommit: m.LeaderCommit,
		MatchIndex:   m.MatchIndex,
		HintIndex:    m.HintIndex,
	}, nil
}

func messageFromPB(p *pb.RaftMessage) (Message, error) {
	t, ok := typeFromPB[p.GetType()]
	if !ok {
		return Message{}, fmt.Errorf("unknown message type %v", p.GetType())
	}
	var entries []Entry
	for _, e := range p.GetEntries() {
		et, ok := entryTypeFromPB[e.GetType()]
		if !ok {
			return Message{}, fmt.Errorf("unknown entry type %v", e.GetType())
		}
		entries = append(entries, Entry{Index: e.GetIndex(), Term: e.GetTerm(), Type: et, Data: e.GetData()})
	}
	return Message{
		Type:         t,
		From:         NodeID(p.GetFrom()),
		To:           NodeID(p.GetTo()),
		Term:         p.GetTerm(),
		LastLogIndex: p.GetLastLogIndex(),
		LastLogTerm:  p.GetLastLogTerm(),
		Granted:      p.GetGranted(),
		Success:      p.GetSuccess(),
		PrevLogIndex: p.GetPrevLogIndex(),
		PrevLogTerm:  p.GetPrevLogTerm(),
		Entries:      entries,
		LeaderCommit: p.GetLeaderCommit(),
		MatchIndex:   p.GetMatchIndex(),
		HintIndex:    p.GetHintIndex(),
	}, nil
}

// GRPCTransport sends Raft messages to peers over gRPC. Connections are opened
// lazily, on the first message to each peer.
//
// Limitation: the connections are not encrypted or authenticated yet.
type GRPCTransport struct {
	addrs map[NodeID]string

	mu     sync.Mutex
	conns  map[NodeID]*grpc.ClientConn
	closed bool
}

// NewGRPCTransport returns a transport that reaches each node at addrs[id].
func NewGRPCTransport(addrs map[NodeID]string) *GRPCTransport {
	copied := make(map[NodeID]string, len(addrs))
	for id, a := range addrs {
		copied[id] = a
	}
	return &GRPCTransport{addrs: copied, conns: make(map[NodeID]*grpc.ClientConn)}
}

func (t *GRPCTransport) conn(id NodeID) (*grpc.ClientConn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("raft: transport closed")
	}
	if c, ok := t.conns[id]; ok {
		return c, nil
	}
	addr, ok := t.addrs[id]
	if !ok {
		return nil, fmt.Errorf("raft: no address for node %d", id)
	}
	c, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("raft: connect to node %d at %s: %w", id, addr, err)
	}
	t.conns[id] = c
	return c, nil
}

// Send implements Transport.
func (t *GRPCTransport) Send(ctx context.Context, m Message) error {
	p, err := messageToPB(m)
	if err != nil {
		return err
	}
	c, err := t.conn(m.To)
	if err != nil {
		return err
	}
	_, err = pb.NewRaftClient(c).Send(ctx, p)
	return err
}

// Close closes all connections.
func (t *GRPCTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	var errs []error
	for _, c := range t.conns {
		errs = append(errs, c.Close())
	}
	t.conns = nil
	return errors.Join(errs...)
}

// GRPCService receives Raft messages from peers and gives them to a Runner.
type GRPCService struct {
	pb.UnimplementedRaftServer
	runner *Runner
}

// RegisterGRPC adds the Raft service to a gRPC server.
func RegisterGRPC(g *grpc.Server, r *Runner) {
	pb.RegisterRaftServer(g, &GRPCService{runner: r})
}

// Send implements the Raft gRPC service.
func (s *GRPCService) Send(_ context.Context, in *pb.RaftMessage) (*pb.SendResponse, error) {
	m, err := messageFromPB(in)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.runner.Receive(m); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.SendResponse{}, nil
}
