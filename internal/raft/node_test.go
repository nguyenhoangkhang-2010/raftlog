package raft

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// --- unit tests on a single node --------------------------------------------

func requestVote(from, to NodeID, term, lastIndex, lastTerm uint64) Message {
	return Message{Type: MsgRequestVote, From: from, To: to, Term: term, LastLogIndex: lastIndex, LastLogTerm: lastTerm}
}

func heartbeat(from, to NodeID, term uint64) Message {
	return Message{Type: MsgAppend, From: from, To: to, Term: term}
}

// only returns the single message in msgs.
func only(t *testing.T, msgs []Message) Message {
	t.Helper()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(msgs), msgs)
	}
	return msgs[0]
}

func TestConfigValidation(t *testing.T) {
	good := testConfig(1, []NodeID{1, 2, 3}, &memStorage{}, 1)
	if _, err := New(good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]func(c *Config){
		"zero id":               func(c *Config) { c.ID = None },
		"id not in cluster":     func(c *Config) { c.ID = 9 },
		"duplicate member":      func(c *Config) { c.Cluster = []NodeID{1, 2, 2} },
		"zero member":           func(c *Config) { c.Cluster = []NodeID{0, 1, 2} },
		"no heartbeat":          func(c *Config) { c.HeartbeatTicks = 0 },
		"election <= heartbeat": func(c *Config) { c.ElectionTicks = c.HeartbeatTicks },
		"missing storage":       func(c *Config) { c.Storage = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(1, []NodeID{1, 2, 3}, &memStorage{}, 1)
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestStartsAsFollowerAtTermZero(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{}, nil)
	st := n.Status()
	if st.Role != Follower || st.Term != 0 || st.Leader != None || st.VotedFor != None {
		t.Fatalf("status = %+v", st)
	}
}

func TestElectionTimeoutIsRandomizedInRange(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{}, nil)
	seen := make(map[int]bool)
	for i := 0; i < 500; i++ {
		n.resetElectionTimer()
		got := n.randomizedTimeout
		if got < testElectionTicks || got >= 2*testElectionTicks {
			t.Fatalf("timeout %d outside [%d, %d)", got, testElectionTicks, 2*testElectionTicks)
		}
		seen[got] = true
	}
	if len(seen) < testElectionTicks/2 {
		t.Fatalf("only %d distinct timeouts in 500 draws: not random enough", len(seen))
	}
}

func TestFollowerCampaignsAfterTimeout(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{}, logWithTerms(1, 1, 1, 1, 1, 2, 2))
	for i := 0; i < 2*testElectionTicks && n.Status().Role == Follower; i++ {
		if err := n.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	st := n.Status()
	// The log ends in term 2, so the node was in term 2 and campaigns in term 3.
	if st.Role != Candidate || st.Term != 3 || st.VotedFor != 1 {
		t.Fatalf("status = %+v, want candidate in term 3 voting for itself", st)
	}
	msgs := n.Messages()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want one RequestVote per peer", len(msgs))
	}
	for i, m := range msgs {
		want := NodeID(i + 2)
		if m.Type != MsgRequestVote || m.To != want || m.Term != 3 || m.From != 1 || m.LastLogIndex != 7 || m.LastLogTerm != 2 {
			t.Fatalf("message %d = %+v", i, m)
		}
	}
	if len(n.Messages()) != 0 {
		t.Fatal("Messages did not clear the queue")
	}
}

func TestCandidateWinsWithMajority(t *testing.T) {
	n := newTestNode(t, 1, 5, &memStorage{}, nil)
	for n.Status().Role == Follower {
		_ = n.Tick()
	}
	n.Messages()
	term := n.Status().Term

	grant := func(from NodeID) Message {
		return Message{Type: MsgRequestVoteResp, From: from, To: 1, Term: term, Granted: true}
	}
	if err := n.Step(grant(2)); err != nil {
		t.Fatal(err)
	}
	if n.Status().Role != Candidate {
		t.Fatal("2 of 5 votes must not be enough")
	}
	if err := n.Step(grant(3)); err != nil {
		t.Fatal(err)
	}
	st := n.Status()
	if st.Role != Leader || st.Leader != 1 {
		t.Fatalf("3 of 5 votes should win: %+v", st)
	}
	// Winning is announced at once.
	msgs := n.Messages()
	if len(msgs) != 4 || msgs[0].Type != MsgAppend {
		t.Fatalf("expected a heartbeat to each of 4 peers, got %+v", msgs)
	}
}

func TestDeniedAndDuplicateVotesDoNotCount(t *testing.T) {
	n := newTestNode(t, 1, 5, &memStorage{}, nil)
	for n.Status().Role == Follower {
		_ = n.Tick()
	}
	term := n.Status().Term
	step := func(from NodeID, granted bool) {
		t.Helper()
		if err := n.Step(Message{Type: MsgRequestVoteResp, From: from, To: 1, Term: term, Granted: granted}); err != nil {
			t.Fatal(err)
		}
	}
	step(2, true)
	step(2, true) // the same voter again must not count twice
	step(3, false)
	step(4, false)
	if n.Status().Role != Candidate {
		t.Fatalf("role = %v, want candidate", n.Status().Role)
	}
}

func TestSingleNodeClusterElectsItself(t *testing.T) {
	n := newTestNode(t, 1, 1, &memStorage{}, nil)
	for i := 0; i < 2*testElectionTicks && n.Status().Role != Leader; i++ {
		_ = n.Tick()
	}
	if st := n.Status(); st.Role != Leader || st.Term != 1 {
		t.Fatalf("status = %+v", st)
	}
}

func TestVoteGrantedOncePerTerm(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{}, nil)

	if err := n.Step(requestVote(2, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); r.Type != MsgRequestVoteResp || !r.Granted || r.Term != 1 || r.To != 2 {
		t.Fatalf("first request: %+v", r)
	}

	// A second candidate in the same term is refused.
	_ = n.Step(requestVote(3, 1, 1, 0, 0))
	if r := only(t, n.Messages()); r.Granted {
		t.Fatalf("voted twice in one term: %+v", r)
	}

	// A retransmission from the candidate we already voted for is fine.
	_ = n.Step(requestVote(2, 1, 1, 0, 0))
	if r := only(t, n.Messages()); !r.Granted {
		t.Fatalf("retransmitted request refused: %+v", r)
	}

	// In a newer term the node may vote again.
	_ = n.Step(requestVote(3, 1, 2, 0, 0))
	if r := only(t, n.Messages()); !r.Granted || r.Term != 2 {
		t.Fatalf("new term: %+v", r)
	}
}

func TestVotingChecksTheCandidateLogIsUpToDate(t *testing.T) {
	// The voter's log ends at index 5, term 3.
	cases := []struct {
		name                string
		lastIndex, lastTerm uint64
		want                bool
	}{
		{"older last term, longer log", 9, 2, false},
		{"same last term, shorter log", 4, 3, false},
		{"same last term, same length", 5, 3, true},
		{"same last term, longer log", 8, 3, true},
		{"newer last term, shorter log", 1, 4, true},
		{"empty log", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := newTestNode(t, 1, 3, &memStorage{}, logWithTerms(1, 1, 2, 3, 3))
			// The voter's log ends in term 3, so it is in term 3 and the request
			// must come from a later term to be considered at all.
			_ = n.Step(requestVote(2, 1, 4, c.lastIndex, c.lastTerm))
			if r := only(t, n.Messages()); r.Granted != c.want {
				t.Fatalf("granted = %v, want %v", r.Granted, c.want)
			}
		})
	}
}

func TestStaleTermIsRejectedAndTold(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{hs: HardState{Term: 5}}, nil)

	_ = n.Step(requestVote(2, 1, 3, 0, 0))
	if r := only(t, n.Messages()); r.Type != MsgRequestVoteResp || r.Granted || r.Term != 5 {
		t.Fatalf("stale RequestVote answer: %+v", r)
	}
	_ = n.Step(heartbeat(2, 1, 3))
	if r := only(t, n.Messages()); r.Type != MsgAppendResp || r.Success || r.Term != 5 {
		t.Fatalf("stale heartbeat answer: %+v", r)
	}
	// A stale reply is simply dropped.
	_ = n.Step(Message{Type: MsgRequestVoteResp, From: 2, To: 1, Term: 3, Granted: true})
	if len(n.Messages()) != 0 || n.Status().Term != 5 {
		t.Fatalf("stale reply had an effect: %+v", n.Status())
	}
}

func TestHeartbeatMakesFollowerAndResetsTimer(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{}, nil)
	for i := 0; i < testElectionTicks-1; i++ {
		_ = n.Tick() // almost time out
	}
	if err := n.Step(heartbeat(2, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); r.Type != MsgAppendResp || !r.Success || r.To != 2 {
		t.Fatalf("heartbeat answer: %+v", r)
	}
	st := n.Status()
	if st.Role != Follower || st.Term != 1 || st.Leader != 2 {
		t.Fatalf("status = %+v", st)
	}
	// The timer restarted, so the node survives a full base timeout.
	for i := 0; i < testElectionTicks; i++ {
		_ = n.Tick()
	}
	if n.Status().Role != Follower {
		t.Fatal("follower campaigned although a leader is alive")
	}
}

func TestCandidateStepsDownOnHeartbeatInSameTerm(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{}, nil)
	for n.Status().Role == Follower {
		_ = n.Tick()
	}
	n.Messages()
	term := n.Status().Term

	_ = n.Step(heartbeat(2, 1, term))
	st := n.Status()
	if st.Role != Follower || st.Leader != 2 || st.Term != term {
		t.Fatalf("status = %+v", st)
	}
}

func TestLeaderStepsDownOnHigherTermReply(t *testing.T) {
	c := newCluster(t, 3, 1)
	if c.runUntil(10*testElectionTicks, c.hasLeader) < 0 {
		t.Fatal("no leader")
	}
	leader := c.leaders()[0]
	term := c.status(leader).Term

	other := NodeID(1)
	if other == leader {
		other = 2
	}
	// A reply from a node that lives in a later term.
	if err := c.nodes[leader].Step(Message{Type: MsgAppendResp, From: other, To: leader, Term: term + 3}); err != nil {
		t.Fatal(err)
	}
	st := c.status(leader)
	if st.Role != Follower || st.Term != term+3 || st.VotedFor != None {
		t.Fatalf("status = %+v", st)
	}
}

func TestIgnoresMessagesFromStrangersAndForOthers(t *testing.T) {
	n := newTestNode(t, 1, 3, &memStorage{}, nil)
	_ = n.Step(requestVote(9, 1, 7, 0, 0)) // 9 is not a member
	_ = n.Step(requestVote(2, 3, 7, 0, 0)) // addressed to node 3
	_ = n.Step(requestVote(1, 1, 7, 0, 0)) // from itself
	if len(n.Messages()) != 0 || n.Status().Term != 0 {
		t.Fatalf("ignored messages had an effect: %+v", n.Status())
	}
}

// --- persistence ------------------------------------------------------------

func TestVoteSurvivesRestart(t *testing.T) {
	st := &memStorage{}
	n := newTestNode(t, 1, 3, st, nil)
	_ = n.Step(requestVote(2, 1, 4, 0, 0))
	if r := only(t, n.Messages()); !r.Granted {
		t.Fatal("vote not granted")
	}
	if st.hs != (HardState{Term: 4, VotedFor: 2}) {
		t.Fatalf("persisted state = %+v", st.hs)
	}

	// Crash and restart: a new node on the same storage.
	n2 := newTestNode(t, 1, 3, st, nil)
	if got := n2.Status(); got.Term != 4 || got.VotedFor != 2 {
		t.Fatalf("restored status = %+v", got)
	}
	_ = n2.Step(requestVote(3, 1, 4, 0, 0))
	if r := only(t, n2.Messages()); r.Granted {
		t.Fatal("voted for a second candidate in the same term after restarting")
	}
	_ = n2.Step(requestVote(2, 1, 4, 0, 0))
	if r := only(t, n2.Messages()); !r.Granted {
		t.Fatal("refused the candidate it had already voted for")
	}
}

func TestCampaignPersistsTermAndSelfVote(t *testing.T) {
	st := &memStorage{}
	n := newTestNode(t, 2, 3, st, nil)
	for n.Status().Role == Follower {
		_ = n.Tick()
	}
	if st.hs != (HardState{Term: 1, VotedFor: 2}) {
		t.Fatalf("persisted state = %+v", st.hs)
	}
}

func TestNothingIsSentIfTheStateCannotBeSaved(t *testing.T) {
	st := &memStorage{failSave: errDiskFull}
	n := newTestNode(t, 1, 3, st, nil)

	// Granting a vote needs a durable write first.
	err := n.Step(requestVote(2, 1, 1, 0, 0))
	if !errors.Is(err, errDiskFull) {
		t.Fatalf("Step error = %v, want errDiskFull", err)
	}
	if len(n.Messages()) != 0 {
		t.Fatal("a vote was sent although it could not be saved")
	}
	if got := n.Status(); got.Term != 0 || got.VotedFor != None {
		t.Fatalf("state changed despite the failed save: %+v", got)
	}

	// The same for starting an election.
	var tickErr error
	for i := 0; i < 2*testElectionTicks && tickErr == nil; i++ {
		tickErr = n.Tick()
	}
	if !errors.Is(tickErr, errDiskFull) {
		t.Fatalf("Tick error = %v, want errDiskFull", tickErr)
	}
	if len(n.Messages()) != 0 || n.Status().Role != Follower {
		t.Fatalf("campaign proceeded without saving: %+v", n.Status())
	}
}

// --- whole-cluster behaviour ------------------------------------------------

func TestClusterElectsExactlyOneLeader(t *testing.T) {
	for _, size := range []int{1, 3, 5} {
		c := newCluster(t, size, 42)
		if c.runUntil(10*testElectionTicks, c.settled) < 0 {
			t.Fatalf("size %d: cluster did not settle on a leader", size)
		}
	}
}

func TestLeaderKeepsLeadershipWhileHeartbeatsFlow(t *testing.T) {
	c := newCluster(t, 3, 7)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	leader, term := c.leaders()[0], c.status(c.leaders()[0]).Term

	c.run(50 * testElectionTicks)

	if ls := c.leaders(); len(ls) != 1 || ls[0] != leader || c.status(leader).Term != term {
		t.Fatalf("leadership changed without a fault: leaders %v, term %d -> %d", ls, term, c.status(leader).Term)
	}
}

func TestIsolatedLeaderIsReplacedAndStepsDownAfterHealing(t *testing.T) {
	c := newCluster(t, 3, 3)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	old := c.leaders()[0]
	oldTerm := c.status(old).Term

	c.isolate(old)
	newLeader := func() NodeID {
		for _, id := range c.leaders() {
			if id != old && c.status(id).Term > oldTerm {
				return id
			}
		}
		return None
	}
	if c.runUntil(10*testElectionTicks, func() bool { return newLeader() != None }) < 0 {
		t.Fatal("the two connected nodes did not elect a new leader")
	}
	// The isolated node cannot know it was replaced.
	if st := c.status(old); st.Role != Leader || st.Term != oldTerm {
		t.Fatalf("isolated old leader: %+v", st)
	}

	c.heal()
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("cluster did not settle after healing")
	}
	if st := c.status(old); st.Role != Follower || st.Term <= oldTerm {
		t.Fatalf("old leader did not step down: %+v", st)
	}
}

func TestMinorityPartitionCannotElectALeader(t *testing.T) {
	c := newCluster(t, 5, 11)
	c.split(1, 2) // {1,2} versus {3,4,5}

	c.run(30 * testElectionTicks)

	for _, id := range []NodeID{1, 2} {
		if st := c.status(id); st.Role == Leader {
			t.Fatalf("node %d won an election without a majority", id)
		}
	}
	ls := c.leaders()
	if len(ls) != 1 || ls[0] < 3 {
		t.Fatalf("leaders = %v, want exactly one among 3,4,5", ls)
	}
	// The minority keeps campaigning, so its term runs ahead.
	if c.status(1).Term <= c.status(ls[0]).Term {
		t.Logf("note: minority term %d did not run ahead of %d", c.status(1).Term, c.status(ls[0]).Term)
	}

	c.heal()
	if c.runUntil(20*testElectionTicks, c.settled) < 0 {
		t.Fatal("cluster did not settle after the partition healed")
	}
}

func TestNoLeaderWithoutAMajority(t *testing.T) {
	c := newCluster(t, 5, 5)
	c.crash(3)
	c.crash(4)
	c.crash(5)
	c.run(30 * testElectionTicks)
	if ls := c.leaders(); len(ls) != 0 {
		t.Fatalf("leaders %v elected with only 2 of 5 nodes up", ls)
	}

	// One node comes back: 3 of 5 is a majority.
	c.restartNode(3)
	if c.runUntil(30*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader after a majority was restored")
	}
}

func TestRestartedNodeKeepsItsTermAndVote(t *testing.T) {
	c := newCluster(t, 3, 9)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	leader := c.leaders()[0]
	before := c.status(leader)

	c.crash(leader)
	c.restartNode(leader)

	after := c.status(leader)
	if after.Term != before.Term || after.VotedFor != before.VotedFor || after.Role != Follower {
		t.Fatalf("restart lost state: before %+v, after %+v", before, after)
	}
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("cluster did not settle after the leader restarted")
	}
}

// The election always finishes, even when several nodes time out together.
// This also measures how long it takes, in ticks, over many random seeds.
func TestElectionTimeAcrossSeeds(t *testing.T) {
	const runs = 300
	var ticks []int
	for seed := int64(1); seed <= runs; seed++ {
		c := newCluster(t, 5, seed)
		n := c.runUntil(20*testElectionTicks, c.settled)
		if n < 0 {
			t.Fatalf("seed %d: no settled leader within %d ticks", seed, 20*testElectionTicks)
		}
		ticks = append(ticks, n)
	}
	sort.Ints(ticks)
	t.Logf("ticks to a settled leader over %d seeds (5 nodes, election timeout %d-%d ticks): min %d, median %d, p99 %d, max %d",
		runs, testElectionTicks, 2*testElectionTicks-1, ticks[0], ticks[runs/2], ticks[runs*99/100], ticks[runs-1])
}

// Randomly cut links and crash nodes, checking election safety at every step.
// checkSafety runs after every delivered message, so one violation fails the test.
func TestElectionSafetyUnderRandomFaults(t *testing.T) {
	electionSafetyUnderRandomFaults(t, newCluster)
}

func TestElectionSafetyUnderRandomFaultsWithPreVoteAndCheckQuorum(t *testing.T) {
	electionSafetyUnderRandomFaults(t, newHardenedCluster)
}

func electionSafetyUnderRandomFaults(t *testing.T, mk func(*testing.T, int, int64) *cluster) {
	const seeds = 60
	for seed := int64(1); seed <= seeds; seed++ {
		c := mk(t, 5, seed)
		rng := rand.New(rand.NewSource(seed))

		for step := 0; step < 40; step++ {
			switch rng.Intn(6) {
			case 0:
				c.isolate(NodeID(1 + rng.Intn(5)))
			case 1:
				c.split(NodeID(1+rng.Intn(5)), NodeID(1+rng.Intn(5)))
			case 2:
				c.heal()
			case 3:
				c.crash(NodeID(1 + rng.Intn(5)))
			case 4:
				id := NodeID(1 + rng.Intn(5))
				if c.down[id] {
					c.restartNode(id)
				}
			}
			c.run(1 + rng.Intn(2*testElectionTicks))
		}

		// Repair everything: the cluster must converge.
		c.heal()
		for _, id := range c.ids {
			if c.down[id] {
				c.restartNode(id)
			}
		}
		if c.runUntil(30*testElectionTicks, c.settled) < 0 {
			t.Fatalf("seed %d: cluster did not converge after all faults were repaired", seed)
		}
	}
}

func TestStatusStrings(t *testing.T) {
	if Leader.String() != "leader" || Candidate.String() != "candidate" || Follower.String() != "follower" {
		t.Fatal("role names changed")
	}
	if !strings.Contains(Role(9).String(), "9") || !strings.Contains(MessageType(9).String(), "9") {
		t.Fatal("unknown values must still be printable")
	}
}
