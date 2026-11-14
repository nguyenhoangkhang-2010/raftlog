package server

import (
	"context"
	"testing"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
)

type fakeRaft struct{ st raft.Status }

func (f fakeRaft) Status() raft.Status { return f.st }

func TestNodeStatusStandalone(t *testing.T) {
	srv, err := New(nil, Options{MaxFetchMessages: 1})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.NodeStatus(context.Background(), &pb.NodeStatusRequest{})
	if err != nil || resp.RaftEnabled {
		t.Fatalf("resp = %+v, err = %v; want raft disabled", resp, err)
	}
}

func TestNodeStatusReportsRaftState(t *testing.T) {
	srv, err := New(nil, Options{
		MaxFetchMessages: 1,
		Raft:             fakeRaft{raft.Status{ID: 2, Role: raft.Candidate, Term: 7, Leader: raft.None, CommitIndex: 4, LastIndex: 6}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.NodeStatus(context.Background(), &pb.NodeStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.RaftEnabled || resp.NodeId != 2 || resp.Role != "candidate" || resp.Term != 7 || resp.LeaderId != 0 || resp.CommitIndex != 4 || resp.LastIndex != 6 {
		t.Fatalf("resp = %+v", resp)
	}

	srv.opts.Raft = fakeRaft{raft.Status{ID: 2, Role: raft.Follower, Term: 8, Leader: 3}}
	resp, _ = srv.NodeStatus(context.Background(), &pb.NodeStatusRequest{})
	if resp.Role != "follower" || resp.LeaderId != 3 || resp.Term != 8 {
		t.Fatalf("resp = %+v", resp)
	}
}
