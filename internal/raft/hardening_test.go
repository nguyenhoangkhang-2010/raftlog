package raft

import (
	"errors"
	"sort"
	"testing"
)

// Tests of pre-vote and check-quorum: first on a single node, then on simulated
// clusters. The simulation delivers messages at once and in order and watches
// election safety, log matching and state machine safety all the time.

func hardenedNode(t *testing.T, id NodeID, size int, st *memStorage, preVote, checkQuorum bool) *Node {
	t.Helper()
	cfg := testConfig(id, clusterIDs(size), st, int64(id))
	cfg.PreVote, cfg.CheckQuorum = preVote, checkQuorum
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

// tickToTimeout ticks until the node says something (it times out), at most twice
// the base timeout, and returns what it sent.
func tickToTimeout(t *testing.T, n *Node) []Message {
	t.Helper()
	for i := 0; i < 2*testElectionTicks; i++ {
		if err := n.Tick(); err != nil {
			t.Fatal(err)
		}
		if msgs := n.Messages(); len(msgs) > 0 {
			return msgs
		}
	}
	t.Fatal("the node never timed out")
	return nil
}

func preVote(from, to NodeID, term, lastIndex, lastTerm uint64) Message {
	return Message{Type: MsgPreVote, From: from, To: to, Term: term, LastLogIndex: lastIndex, LastLogTerm: lastTerm}
}

func preVoteResp(from, to NodeID, term uint64, granted bool) Message {
	return Message{Type: MsgPreVoteResp, From: from, To: to, Term: term, Granted: granted}
}

// --- the node that asks ---------------------------------------------------------

func TestWithPreVoteATimeoutAsksFirstAndChangesNothing(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 4}}
	n := hardenedNode(t, 1, 3, st, true, false)
	msgs := tickToTimeout(t, n)

	if len(msgs) != 2 {
		t.Fatalf("%d messages, want one pre-vote per peer: %+v", len(msgs), msgs)
	}
	for i, m := range msgs {
		if m.Type != MsgPreVote || m.To != NodeID(i+2) || m.From != 1 || m.Term != 5 {
			t.Fatalf("message %d = %+v, want a PreVote for term 5 (one above the node's term 4)", i, m)
		}
	}
	got := n.Status()
	if got.Role != Follower || got.Term != 4 || got.VotedFor != None || got.Leader != None {
		t.Fatalf("status = %+v: asking must change neither role, term nor vote", got)
	}
	if st.saves != 0 {
		t.Fatalf("the node saved its state %d times while only asking", st.saves)
	}
	if n.Stats().PreVotes != 1 || n.Stats().Campaigns != 0 {
		t.Fatalf("stats = %+v", n.Stats())
	}
}

func TestAMajorityOfPreVotesStartsTheRealElection(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 4}}
	n := hardenedNode(t, 1, 3, st, true, false)
	tickToTimeout(t, n)

	// One yes plus its own is a majority of three.
	if err := n.Step(preVoteResp(2, 1, 5, true)); err != nil {
		t.Fatal(err)
	}
	got := n.Status()
	if got.Role != Candidate || got.Term != 5 || got.VotedFor != 1 {
		t.Fatalf("status = %+v, want a candidate in term 5 voting for itself", got)
	}
	msgs := n.Messages()
	if len(msgs) != 2 || msgs[0].Type != MsgRequestVote || msgs[0].Term != 5 || msgs[1].Type != MsgRequestVote {
		t.Fatalf("messages = %+v, want a RequestVote for term 5 to each peer", msgs)
	}
	// Another yes, now late, changes nothing.
	if err := n.Step(preVoteResp(3, 1, 5, true)); err != nil {
		t.Fatal(err)
	}
	if n.Status().Role != Candidate || len(n.Messages()) != 0 {
		t.Fatal("a late pre-vote answer disturbed the election")
	}
}

func TestPreVoteAnswersForOtherRoundsAreIgnored(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 4}}
	n := hardenedNode(t, 1, 3, st, true, false)
	tickToTimeout(t, n)
	for _, term := range []uint64{4, 6, 9} { // the round asked about term 5
		if err := n.Step(preVoteResp(2, 1, term, true)); err != nil {
			t.Fatal(err)
		}
		if got := n.Status(); got.Role != Follower || got.Term != 4 {
			t.Fatalf("a yes for term %d changed the node: %+v", term, got)
		}
	}
	// A node that is not asking ignores a yes altogether.
	idle := hardenedNode(t, 1, 3, &memStorage{hs: HardState{Term: 4}}, true, false)
	if err := idle.Step(preVoteResp(2, 1, 5, true)); err != nil {
		t.Fatal(err)
	}
	if idle.Status().Role != Follower || len(idle.Messages()) != 0 {
		t.Fatal("an unasked-for yes started an election")
	}
}

func TestRefusedPreVotesLeaveTheNodeAFollowerWhichTriesAgain(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 4}}
	n := hardenedNode(t, 1, 3, st, true, false)
	tickToTimeout(t, n)
	for _, from := range []NodeID{2, 3} {
		if err := n.Step(preVoteResp(from, 1, 4, false)); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.Status(); got.Role != Follower || got.Term != 4 || st.saves != 0 {
		t.Fatalf("status %+v, saves %d after two refusals", got, st.saves)
	}
	// After another timeout it asks again, still without raising its term.
	msgs := tickToTimeout(t, n)
	if msgs[0].Type != MsgPreVote || msgs[0].Term != 5 || n.Stats().PreVotes != 2 || n.Status().Term != 4 {
		t.Fatalf("second round: %+v, stats %+v, status %+v", msgs, n.Stats(), n.Status())
	}
}

func TestARefusalFromANewerTermIsLearnedFrom(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 4}}
	n := hardenedNode(t, 1, 3, st, true, false)
	tickToTimeout(t, n)
	if err := n.Step(preVoteResp(2, 1, 9, false)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status(); got.Role != Follower || got.Term != 9 || got.VotedFor != None {
		t.Fatalf("status = %+v: the refuser is in term 9, so this node was out of date", got)
	}
	// It is not asking any more: a yes for its old round does nothing.
	if err := n.Step(preVoteResp(3, 1, 5, true)); err != nil || n.Status().Term != 9 {
		t.Fatalf("a late yes after learning of a newer term: %v, %+v", err, n.Status())
	}
}

func TestAOneNodeClusterElectsItselfAtOnceEvenWithPreVote(t *testing.T) {
	n := hardenedNode(t, 1, 1, &memStorage{}, true, true)
	for i := 0; i < 2*testElectionTicks && n.Status().Role != Leader; i++ {
		_ = n.Tick()
	}
	if got := n.Status(); got.Role != Leader || got.Term != 1 {
		t.Fatalf("status = %+v", got)
	}
	// And with check-quorum it never steps down: it is its own majority.
	for i := 0; i < 5*testElectionTicks; i++ {
		_ = n.Tick()
	}
	if n.Status().Role != Leader || n.Stats().QuorumLost != 0 {
		t.Fatalf("a one-node leader stepped down: %+v, %+v", n.Status(), n.Stats())
	}
}

// --- the node that answers ------------------------------------------------------

func TestAPreVoteIsAnsweredWithoutChangingTheAnswerer(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 5}}
	n := hardenedNode(t, 1, 3, st, true, false)
	for i := 0; i < 3; i++ {
		_ = n.Tick()
	}
	elapsed := n.electionElapsed

	if err := n.Step(preVote(2, 1, 6, 0, 0)); err != nil {
		t.Fatal(err)
	}
	r := only(t, n.Messages())
	if r.Type != MsgPreVoteResp || !r.Granted || r.To != 2 || r.Term != 6 {
		t.Fatalf("answer = %+v, want a yes carrying the asked-about term 6", r)
	}
	if got := n.Status(); got.Term != 5 || got.VotedFor != None || st.saves != 0 {
		t.Fatalf("status %+v, saves %d: a pre-vote must not change the answerer", got, st.saves)
	}
	if n.electionElapsed != elapsed {
		t.Fatalf("a pre-vote reset the election timer (%d -> %d)", elapsed, n.electionElapsed)
	}
	// It can say yes to several askers: it has not voted for anyone.
	if err := n.Step(preVote(3, 1, 6, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Granted {
		t.Fatalf("second asker: %+v", r)
	}
}

func TestAPreVoteIsRefusedWhenTheAskerWouldNotWin(t *testing.T) {
	cases := []struct {
		name  string
		ask   Message
		term  uint64
		terms []uint64 // the answerer's log
	}{
		{"the asker's term is not newer", preVote(2, 1, 5, 0, 0), 5, nil},
		{"the asker's term is older", preVote(2, 1, 4, 0, 0), 5, nil},
		{"the asker's log is behind", preVote(2, 1, 6, 1, 2), 5, []uint64{2, 2, 3}},
		{"the asker's log is shorter in the same term", preVote(2, 1, 6, 2, 3), 5, []uint64{3, 3, 3}},
	}
	for _, c := range cases {
		st := &memStorage{hs: HardState{Term: c.term}}
		cfg := testConfig(1, clusterIDs(3), st, 1)
		cfg.PreVote = true
		if c.terms != nil {
			cfg.Log = logWithTerms(c.terms...)
		}
		n, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := n.Step(c.ask); err != nil {
			t.Fatal(err)
		}
		r := only(t, n.Messages())
		if r.Type != MsgPreVoteResp || r.Granted || r.Term != n.Status().Term {
			t.Errorf("%s: answer %+v, want a refusal carrying the answerer's own term", c.name, r)
		}
		if n.Status().Term != c.term || n.Status().VotedFor != None {
			t.Errorf("%s: the refusal changed the node: %+v", c.name, n.Status())
		}
	}
}

func TestANodeThatHeardFromALeaderRefusesPreVotesUntilItsTimeoutPasses(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 5}}
	n := hardenedNode(t, 1, 3, st, true, false)
	if err := n.Step(heartbeat(2, 1, 5)); err != nil {
		t.Fatal(err)
	}
	n.Messages()

	ask := func() Message {
		t.Helper()
		if err := n.Step(preVote(3, 1, 6, 0, 0)); err != nil {
			t.Fatal(err)
		}
		msgs := n.Messages()
		return msgs[len(msgs)-1]
	}
	if r := ask(); r.Type != MsgPreVoteResp || r.Granted {
		t.Fatalf("right after a heartbeat: %+v, want a refusal: the leader is alive", r)
	}
	for i := 0; i < testElectionTicks-1; i++ {
		_ = n.Tick()
	}
	n.Messages()
	if r := ask(); r.Granted {
		t.Fatalf("just before the timeout: %+v, want a refusal", r)
	}
	for i := 0; i < testElectionTicks; i++ {
		_ = n.Tick()
	}
	n.Messages()
	if r := ask(); r.Type != MsgPreVoteResp || !r.Granted {
		t.Fatalf("a whole timeout without the leader: %+v, want a yes", r)
	}
}

func TestALeaderThatChecksItsQuorumRefusesPreVotes(t *testing.T) {
	// A leader in contact with a majority has no reason to help anyone depose it.
	n := leaderOf3(t, false, true)
	if err := n.Step(preVote(2, 1, n.Status().Term+1, 99, 99)); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); r.Granted {
		t.Fatalf("answer = %+v, want a refusal", r)
	}
}

func TestAFollowerIgnoresARealVoteRequestWhileItHasALeader(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 5}}
	n := hardenedNode(t, 1, 3, st, true, false)
	if err := n.Step(heartbeat(2, 1, 5)); err != nil {
		t.Fatal(err)
	}
	n.Messages()

	if err := n.Step(requestVote(3, 1, 9, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if msgs := n.Messages(); len(msgs) != 0 {
		t.Fatalf("answered a vote request from a node that cannot hear the leader: %+v", msgs)
	}
	if got := n.Status(); got.Term != 5 || got.Leader != 2 || got.VotedFor != None || st.saves != 0 {
		t.Fatalf("status %+v, saves %d: the term must not be adopted", got, st.saves)
	}

	// Once the leader has been silent for a whole timeout, the request counts.
	for i := 0; i < testElectionTicks; i++ {
		_ = n.Tick()
	}
	n.Messages()
	if err := n.Step(requestVote(3, 1, 9, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status(); got.Term != 9 || got.VotedFor != 3 {
		t.Fatalf("status = %+v, want the vote granted in term 9", got)
	}
}

func TestWithoutHardeningAVoteRequestFromANewerTermIsAlwaysTaken(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 5}}
	n := hardenedNode(t, 1, 3, st, false, false)
	if err := n.Step(heartbeat(2, 1, 5)); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	if err := n.Step(requestVote(3, 1, 9, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status(); got.Term != 9 || got.VotedFor != 3 {
		t.Fatalf("status = %+v: the default behaviour must be unchanged", got)
	}
}

// --- check-quorum -----------------------------------------------------------------

// leaderOf3 returns node 1 of three as the leader of term 1, with its messages drained.
func leaderOf3(t *testing.T, pre, check bool) *Node {
	t.Helper()
	n := hardenedNode(t, 1, 3, &memStorage{}, false, check)
	tickToTimeout(t, n) // a plain campaign: PreVote is not what is under test here
	if err := n.Step(Message{Type: MsgRequestVoteResp, From: 2, To: 1, Term: n.Status().Term, Granted: true}); err != nil {
		t.Fatal(err)
	}
	if n.Status().Role != Leader {
		t.Fatalf("node 1 is %+v", n.Status())
	}
	n.Messages()
	n.preVote = pre
	return n
}

func appendAnswer(from NodeID, term uint64, success bool) Message {
	return Message{Type: MsgAppendResp, From: from, To: 1, Term: term, Success: success, MatchIndex: 1}
}

func TestALeaderThatHearsFromNoMajorityStepsDown(t *testing.T) {
	n := leaderOf3(t, false, true)
	term := n.Status().Term
	for i := 0; i < testElectionTicks-1; i++ {
		_ = n.Tick()
	}
	if n.Status().Role != Leader {
		t.Fatal("stepped down before a whole election timeout had passed")
	}
	_ = n.Tick()
	got := n.Status()
	if got.Role != Follower || got.Term != term || got.Leader != None {
		t.Fatalf("status = %+v, want a follower in the same term with no leader", got)
	}
	if n.Stats().QuorumLost != 1 {
		t.Fatalf("stats = %+v", n.Stats())
	}
	// It does not campaign at once: it waits for a timeout of its own.
	if _, _, err := n.Propose([]byte("x")); err == nil {
		t.Fatal("a node that stepped down accepted a proposal")
	} else {
		var nl *NotLeaderError
		if !errors.As(err, &nl) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestALeaderKeepsLeadingWhileAMajorityAnswers(t *testing.T) {
	n := leaderOf3(t, false, true)
	term := n.Status().Term
	// One follower answers in every election timeout: with the leader itself that is 2 of 3.
	for period := 0; period < 6; period++ {
		for i := 0; i < testElectionTicks; i++ {
			_ = n.Tick()
			if i == testElectionTicks/2 {
				// A refusal counts as much as an acceptance: the follower is in touch.
				if err := n.Step(appendAnswer(2, term, period%2 == 0)); err != nil {
					t.Fatal(err)
				}
			}
			n.Messages()
		}
		if n.Status().Role != Leader {
			t.Fatalf("stepped down in period %d although a follower answered", period)
		}
	}
	if n.Stats().QuorumLost != 0 {
		t.Fatalf("stats = %+v", n.Stats())
	}
}

func TestAnAnswerCountsOnlyForThePeriodItCameIn(t *testing.T) {
	n := leaderOf3(t, false, true)
	term := n.Status().Term
	// An answer in the first period keeps the leader through that period only.
	_ = n.Tick()
	if err := n.Step(appendAnswer(2, term, true)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < testElectionTicks-1; i++ {
		_ = n.Tick()
	}
	if n.Status().Role != Leader {
		t.Fatal("stepped down in the first period")
	}
	// Nobody answers in the second.
	for i := 0; i < testElectionTicks; i++ {
		_ = n.Tick()
	}
	if n.Status().Role == Leader {
		t.Fatal("an old answer kept the leader in power through a second period of silence")
	}
}

func TestAnswersFromAnOlderTermDoNotCount(t *testing.T) {
	n := leaderOf3(t, false, true)
	term := n.Status().Term
	for i := 0; i < testElectionTicks; i++ {
		_ = n.Tick()
		_ = n.Step(appendAnswer(2, term-1, true)) // a stale follower of an earlier leader
	}
	if n.Status().Role == Leader {
		t.Fatal("answers from an older term kept the leader in power")
	}
}

func TestWithoutCheckQuorumALeaderNeverStepsDownOnItsOwn(t *testing.T) {
	n := leaderOf3(t, false, false)
	for i := 0; i < 10*testElectionTicks; i++ {
		_ = n.Tick()
		n.Messages()
	}
	if n.Status().Role != Leader {
		t.Fatal("the default behaviour changed: a leader stepped down on its own")
	}
}

// --- whole clusters -------------------------------------------------------------

func settleOrFail(t *testing.T, c *cluster, what string) NodeID {
	t.Helper()
	if c.runUntil(20*testElectionTicks, c.settled) < 0 {
		t.Fatalf("%s: the cluster did not settle", what)
	}
	return c.leaders()[0]
}

// The scenario the cluster console showed: a node cut off from the leader for a
// while, then back. Without hardening it raises its term while alone and deposes
// a healthy leader when it returns. With it, nothing happens at all.
func TestANodeThatReturnsAfterBeingCutOffDoesNotDisturbAHealthyCluster(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		c := newHardenedCluster(t, 3, seed)
		leader := settleOrFail(t, c, "start")
		term := c.status(leader).Term
		var lonely NodeID
		for _, id := range c.ids {
			if id != leader {
				lonely = id
				break
			}
		}

		c.isolate(lonely)
		for i := 0; i < 4; i++ {
			if _, _, err := c.propose(leader, "while-away"); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
		}
		c.run(15 * testElectionTicks) // its timer fires many times
		if got := c.status(lonely).Term; got != term {
			t.Fatalf("seed %d: the cut-off node's term went from %d to %d: it ran elections nobody could join", seed, term, got)
		}

		c.heal()
		c.run(6 * testElectionTicks)
		if ls := c.leaders(); len(ls) != 1 || ls[0] != leader || c.status(leader).Term != term {
			t.Fatalf("seed %d: after the node returned the leader is %v in term %d, want node %d in term %d", seed, ls, c.status(leader).Term, leader, term)
		}
		if c.runUntil(10*testElectionTicks, c.converged) < 0 {
			t.Fatalf("seed %d: the returned node did not catch up", seed)
		}
		if st := c.status(lonely); st.Term != term || st.Role != Follower {
			t.Fatalf("seed %d: the returned node is %+v", seed, st)
		}
	}
}

// The same scenario without the hardening, to show what it prevents. If this ever
// stops failing to be quiet, the test above no longer proves anything.
func TestWithoutHardeningTheSameNodeDisturbsTheCluster(t *testing.T) {
	disturbed := 0
	for seed := int64(1); seed <= 25; seed++ {
		c := newCluster(t, 3, seed)
		leader := settleOrFail(t, c, "start")
		term := c.status(leader).Term
		var lonely NodeID
		for _, id := range c.ids {
			if id != leader {
				lonely = id
				break
			}
		}
		c.isolate(lonely)
		c.run(15 * testElectionTicks)
		if c.status(lonely).Term <= term {
			t.Fatalf("seed %d: without pre-vote the cut-off node should have raised its term", seed)
		}
		c.heal()
		c.run(6 * testElectionTicks)
		if now := c.status(c.leaders()[0]).Term; now > term {
			disturbed++
		}
	}
	if disturbed != 25 {
		t.Fatalf("the cluster was disturbed in %d of 25 runs, want all: the control no longer shows the problem", disturbed)
	}
}

func TestACutOffLeaderStopsLeadingAndTheMajorityElectsAnother(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		c := newHardenedCluster(t, 5, seed)
		old := settleOrFail(t, c, "start")
		term := c.status(old).Term

		c.isolate(old)
		c.run(3 * testElectionTicks)
		if got := c.status(old); got.Role == Leader {
			t.Fatalf("seed %d: the cut-off leader still thinks it leads in term %d", seed, got.Term)
		}
		if _, _, err := c.propose(old, "x"); err == nil {
			t.Fatalf("seed %d: the cut-off node accepted a proposal", seed)
		} else if !errors.Is(err, ErrNotLeader) {
			t.Fatalf("seed %d: err = %v", seed, err)
		}
		// The other four have a leader in a newer term.
		var others []NodeID
		for _, id := range c.ids {
			if id != old {
				others = append(others, id)
			}
		}
		found := false
		for _, id := range others {
			if st := c.status(id); st.Role == Leader && st.Term > term {
				found = true
			}
		}
		if !found {
			t.Fatalf("seed %d: the majority did not elect a new leader", seed)
		}

		c.heal()
		settleOrFail(t, c, "after healing")
	}
}

func TestWithoutCheckQuorumACutOffLeaderKeepsBelievingItLeads(t *testing.T) {
	c := newCluster(t, 5, 3)
	old := settleOrFail(t, c, "start")
	c.isolate(old)
	c.run(3 * testElectionTicks)
	if c.status(old).Role != Leader {
		t.Fatal("the control no longer shows an unhardened leader holding on")
	}
}

func TestHealingAPartitionStartsNoElection(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		c := newHardenedCluster(t, 5, seed)
		settleOrFail(t, c, "start")

		c.split(1, 2)
		c.run(8 * testElectionTicks)
		if ls := c.leaders(); len(ls) != 1 {
			t.Fatalf("seed %d: leaders during the partition: %v", seed, ls)
		}
		before := elections(c)

		c.heal()
		c.run(8 * testElectionTicks)
		if after := elections(c); after != before {
			t.Fatalf("seed %d: healing the partition started %d elections", seed, after-before)
		}
		settleOrFail(t, c, "after healing")
	}
}

// elections is the total number of elections started by every node, pre-vote
// rounds excluded: those change no term.
func elections(c *cluster) uint64 {
	var total uint64
	for _, id := range c.ids {
		total += c.nodes[id].Stats().Campaigns
	}
	return total
}

func TestAClusterWithHardeningStillElectsALeaderPromptlyAfterOneCrashes(t *testing.T) {
	const runs = 100
	var hardened, plain []int
	for seed := int64(1); seed <= runs; seed++ {
		for _, mk := range []struct {
			name string
			new  func(*testing.T, int, int64) *cluster
			into *[]int
		}{{"plain", newCluster, &plain}, {"hardened", newHardenedCluster, &hardened}} {
			c := mk.new(t, 5, seed)
			leader := settleOrFail(t, c, mk.name)
			c.crash(leader)
			n := c.runUntil(12*testElectionTicks, c.settled)
			if n < 0 {
				t.Fatalf("%s, seed %d: no new leader within %d ticks of the old one crashing", mk.name, seed, 12*testElectionTicks)
			}
			*mk.into = append(*mk.into, n)
		}
	}
	sort.Ints(hardened)
	sort.Ints(plain)
	t.Logf("ticks from a leader crashing to a settled new one over %d seeds (5 nodes, election timeout %d-%d ticks): plain median %d, p99 %d; with pre-vote and check-quorum median %d, p99 %d",
		runs, testElectionTicks, 2*testElectionTicks-1, plain[runs/2], plain[runs*99/100], hardened[runs/2], hardened[runs*99/100])
	if hardened[runs*99/100] > 6*testElectionTicks {
		t.Fatalf("with hardening the slowest 1%% of elections took %d ticks, more than %d", hardened[runs*99/100], 6*testElectionTicks)
	}
}

func TestPreVoteNeverCostsAnEntryThatWasCommitted(t *testing.T) {
	// A node whose log is behind cannot win a pre-vote, so it cannot become leader
	// and overwrite what a majority has stored.
	for seed := int64(1); seed <= 40; seed++ {
		c := newHardenedCluster(t, 5, seed)
		leader := settleOrFail(t, c, "start")
		var behind NodeID
		for _, id := range c.ids {
			if id != leader {
				behind = id
				break
			}
		}
		c.isolate(behind)
		for i := 0; i < 6; i++ {
			if _, _, err := c.propose(leader, "kept"); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
		}
		committed := c.status(leader).CommitIndex
		c.heal()
		c.crash(leader) // now the node that is behind could try to take over
		c.run(10 * testElectionTicks)
		for _, id := range c.leaders() {
			if got := c.status(id).LastIndex; got < committed {
				t.Fatalf("seed %d: node %d leads with a log of %d entries, but %d were committed", seed, id, got, committed)
			}
		}
	}
}

func TestInAFiveNodeClusterOneYesIsNotAMajority(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 4}}
	n := hardenedNode(t, 1, 5, st, true, false)
	tickToTimeout(t, n)
	// Itself and one other are two of five: not enough.
	if err := n.Step(preVoteResp(2, 1, 5, true)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status(); got.Role != Follower || got.Term != 4 {
		t.Fatalf("two of five started an election: %+v", got)
	}
	// A third makes three of five.
	if err := n.Step(preVoteResp(3, 1, 5, true)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status(); got.Role != Candidate || got.Term != 5 {
		t.Fatalf("three of five did not start the election: %+v", got)
	}
	// The same node answering twice is still one answer.
	n2 := hardenedNode(t, 1, 5, &memStorage{hs: HardState{Term: 4}}, true, false)
	tickToTimeout(t, n2)
	for i := 0; i < 3; i++ {
		if err := n2.Step(preVoteResp(2, 1, 5, true)); err != nil {
			t.Fatal(err)
		}
	}
	if n2.Status().Role != Follower {
		t.Fatal("one node's repeated yes was counted as a majority")
	}
}

func TestTheLeaderLeaseEndsExactlyAfterAnElectionTimeout(t *testing.T) {
	st := &memStorage{hs: HardState{Term: 5}}
	n := hardenedNode(t, 1, 3, st, true, false)
	if err := n.Step(heartbeat(2, 1, 5)); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	for i := 0; i < testElectionTicks-1; i++ {
		_ = n.Tick()
	}
	n.Messages()
	if err := n.Step(preVote(3, 1, 6, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); r.Granted {
		t.Fatalf("%d ticks after the heartbeat: %+v, want a refusal", testElectionTicks-1, r)
	}
	_ = n.Tick() // the whole base timeout has now passed
	n.Messages()
	if err := n.Step(preVote(3, 1, 6, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Granted {
		t.Fatalf("%d ticks after the heartbeat: %+v, want a yes", testElectionTicks, r)
	}
}

func TestALeaderWithoutCheckQuorumDoesNotClaimALease(t *testing.T) {
	// Pre-vote alone: the leader cannot tell whether it still has a majority, so it
	// answers a pre-vote on the merits, like anyone else.
	n := leaderOf3(t, true, false)
	if err := n.Step(preVote(2, 1, n.Status().Term+1, 99, 99)); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Granted {
		t.Fatalf("answer = %+v, want a yes", r)
	}
}

// The two new messages must survive the trip over gRPC, which is how real nodes
// exchange them.
func TestPreVoteMessagesSurviveTheWire(t *testing.T) {
	for _, in := range []Message{
		{Type: MsgPreVote, From: 2, To: 1, Term: 8, LastLogIndex: 40, LastLogTerm: 7},
		{Type: MsgPreVoteResp, From: 1, To: 2, Term: 8, Granted: true},
		{Type: MsgPreVoteResp, From: 1, To: 2, Term: 9, Granted: false},
	} {
		p, err := messageToPB(in)
		if err != nil {
			t.Fatalf("%v: %v", in.Type, err)
		}
		out, err := messageFromPB(p)
		if err != nil || out.Type != in.Type || out.Term != in.Term || out.Granted != in.Granted ||
			out.LastLogIndex != in.LastLogIndex || out.LastLogTerm != in.LastLogTerm || out.From != in.From || out.To != in.To {
			t.Fatalf("%v: round trip gave %+v, err %v; want %+v", in.Type, out, err, in)
		}
	}
	if MsgPreVote.String() != "PreVote" || MsgPreVoteResp.String() != "PreVoteResp" {
		t.Fatalf("names: %q %q", MsgPreVote, MsgPreVoteResp)
	}
}
