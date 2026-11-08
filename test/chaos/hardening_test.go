package chaos

import (
	"testing"
	"time"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

// These tests run real raftlogd processes, with the network between them cut by the
// test's own proxies. Their Raft timings are short (an election timeout of 100 to
// 200 ms), so a few seconds of isolation is dozens of election timeouts.

// statuses returns the status of every node that answers, by node ID.
func statuses(t *testing.T, c *cluster) map[int]*pb.NodeStatusResponse {
	t.Helper()
	out := map[int]*pb.NodeStatusResponse{}
	for id := 1; id <= c.n; id++ {
		if !c.running(id) {
			continue
		}
		cl, closeConn := dial(t, c.node(id).addr)
		if st := nodeStatus(cl); st != nil {
			out[id] = st
		}
		closeConn()
	}
	return out
}

// waitForSettledLeader waits until exactly one running node is the leader and all
// the others follow it in the same term, and returns the leader and its term.
func waitForSettledLeader(t *testing.T, c *cluster, except map[int]bool) (int, uint64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		sts := statuses(t, c)
		leader, leaders := 0, 0
		for id, st := range sts {
			if st.Role == "leader" {
				leader, leaders = id, leaders+1
			}
		}
		if leaders == 1 {
			ok := true
			for id, st := range sts {
				if except[id] || id == leader {
					continue
				}
				if st.Role != "follower" || st.Term != sts[leader].Term || int(st.LeaderId) != leader {
					ok = false
				}
			}
			if ok {
				return leader, sts[leader].Term
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no settled leader within 30 seconds")
	return 0, 0
}

// isolatedFollowerScenario cuts one follower off for a few seconds while writes go
// on, then reconnects it, and reports the terms at each point.
type isolationResult struct {
	leader                int
	termBefore, termAlone uint64 // the cluster's term, and the cut-off node's own
	leaderAfter           int
	termAfter             uint64
	followerTermAfterHeal uint64
	committedWhileCutOff  bool
}

func runIsolation(t *testing.T, extra ...string) isolationResult {
	t.Helper()
	c := newCluster(t, 3, extra...)
	for id := 1; id <= 3; id++ {
		c.mustStart(id)
	}
	w := newWorkload(t, c, "hardening", 1)
	t.Cleanup(w.close)
	w.createTopic(30 * time.Second)

	var r isolationResult
	r.leader, r.termBefore = waitForSettledLeader(t, c, nil)
	victim := 1
	if r.leader == victim {
		victim = 2
	}

	c.isolate(victim)
	time.Sleep(6 * time.Second) // dozens of election timeouts
	sts := statuses(t, c)
	if st := sts[victim]; st != nil {
		r.termAlone = st.Term
	}
	// The other two are a majority: the cluster still works without the victim.
	r.committedWhileCutOff = canWrite(w, 15*time.Second)

	c.heal()
	time.Sleep(3 * time.Second)
	r.leaderAfter, r.termAfter = waitForSettledLeader(t, c, nil)
	if st := statuses(t, c)[victim]; st != nil {
		r.followerTermAfterHeal = st.Term
	}
	return r
}

func TestAnIsolatedNodeDoesNotDisturbTheClusterWhenItComesBack(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	r := runIsolation(t) // pre-vote and check-quorum are on by default
	t.Logf("leader %d term %d; the cut-off node's own term while alone: %d; after it came back: leader %d term %d",
		r.leader, r.termBefore, r.termAlone, r.leaderAfter, r.termAfter)
	if !r.committedWhileCutOff {
		t.Fatal("the two connected nodes could not commit a write")
	}
	if r.termAlone != r.termBefore {
		t.Fatalf("the cut-off node's term went from %d to %d while it was alone: it ran elections that nobody could join", r.termBefore, r.termAlone)
	}
	if r.leaderAfter != r.leader || r.termAfter != r.termBefore || r.followerTermAfterHeal != r.termBefore {
		t.Fatalf("after the node came back the cluster is leader %d, term %d (node's term %d), want leader %d, term %d: the return caused an election",
			r.leaderAfter, r.termAfter, r.followerTermAfterHeal, r.leader, r.termBefore)
	}
}

// The same scenario with the hardening turned off, to show what it is worth.
func TestWithoutHardeningTheReturnOfAnIsolatedNodeCausesAnElection(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	r := runIsolation(t, "-pre-vote=false", "-check-quorum=false")
	t.Logf("without hardening: leader %d term %d; the cut-off node's term while alone: %d; after it came back: leader %d term %d",
		r.leader, r.termBefore, r.termAlone, r.leaderAfter, r.termAfter)
	if r.termAlone <= r.termBefore {
		t.Fatalf("the cut-off node's term stayed at %d: the control no longer shows the problem", r.termAlone)
	}
	if r.termAfter <= r.termBefore {
		t.Fatalf("the cluster stayed in term %d: the control no longer shows the problem", r.termAfter)
	}
}

func TestACutOffLeaderStopsLeadingWithinAFewElectionTimeouts(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	c := newCluster(t, 3)
	for id := 1; id <= 3; id++ {
		c.mustStart(id)
	}
	old, term := waitForSettledLeader(t, c, nil)

	c.isolate(old)
	deadline := time.Now().Add(5 * time.Second)
	for {
		sts := statuses(t, c)
		if st := sts[old]; st != nil && st.Role != "leader" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cut-off leader still thinks it leads after 5 s (status %+v)", statuses(t, c)[old])
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The majority has a leader in a newer term.
	newLeader, newTerm := waitForSettledLeader(t, c, map[int]bool{old: true})
	if newLeader == old || newTerm <= term {
		t.Fatalf("new leader %d in term %d, want another node in a term above %d", newLeader, newTerm, term)
	}

	c.heal()
	l, tm := waitForSettledLeader(t, c, nil)
	if l != newLeader || tm != newTerm {
		t.Fatalf("after healing: leader %d term %d, want the new leader %d in term %d to stay", l, tm, newLeader, newTerm)
	}
}

// canWrite tries to store one message, asking every node in turn (only the leader
// accepts it), until one does or the time is up.
func canWrite(w *workload, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for id := 1; id <= w.c.n; id++ {
			if !w.c.running(id) {
				continue
			}
			ctx, cancel := shortCtx(2 * time.Second)
			_, err := w.clients[id].Produce(ctx, &pb.ProduceRequest{Topic: w.topic, Messages: []*pb.Message{{Value: []byte("probe")}}})
			cancel()
			if err == nil {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
