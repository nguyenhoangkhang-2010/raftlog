package raft

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// --- helpers ----------------------------------------------------------------

// newLeaderNode returns node 1 of a cluster of the given size, already elected
// leader. If log is not nil, the node starts with that log.
func newLeaderNode(t *testing.T, size int, log LogStore, mutate func(*Config)) *Node {
	t.Helper()
	st := &memStorage{}
	cfg := testConfig(1, clusterIDs(size), st, 1)
	if log != nil {
		cfg.Log = log
		if lastTerm, _ := log.Term(log.LastIndex()); st.hs.Term < lastTerm {
			st.hs.Term = lastTerm
		}
	}
	if mutate != nil {
		mutate(&cfg)
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for n.Status().Role == Follower {
		if err := n.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	n.Messages()
	term := n.Status().Term
	for p := NodeID(2); p <= NodeID(size) && n.Status().Role != Leader; p++ {
		if err := n.Step(Message{Type: MsgRequestVoteResp, From: p, To: 1, Term: term, Granted: true}); err != nil {
			t.Fatal(err)
		}
	}
	if n.Status().Role != Leader {
		t.Fatal("node did not become leader")
	}
	return n
}

// appendTo returns the Append message addressed to node id.
func appendTo(t *testing.T, msgs []Message, id NodeID) Message {
	t.Helper()
	for _, m := range msgs {
		if m.To == id && m.Type == MsgAppend {
			return m
		}
	}
	t.Fatalf("no Append message for node %d in %+v", id, msgs)
	return Message{}
}

func appendResp(from NodeID, term uint64, success bool, match, hint uint64) Message {
	return Message{Type: MsgAppendResp, From: from, To: 1, Term: term, Success: success, MatchIndex: match, HintIndex: hint}
}

func indexes(entries []Entry) []uint64 {
	out := make([]uint64, len(entries))
	for i, e := range entries {
		out[i] = e.Index
	}
	return out
}

// logTerms lists the terms of all entries in a log.
func logTerms(t *testing.T, l LogStore) []uint64 {
	t.Helper()
	var out []uint64
	for i := uint64(1); i <= l.LastIndex(); i++ {
		term, err := l.Term(i)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, term)
	}
	return out
}

func logWithSizedEntries(terms []uint64, size int) *MemoryLog {
	l := NewMemoryLog()
	for i, term := range terms {
		l.entries = append(l.entries, Entry{Index: uint64(i + 1), Term: term, Data: bytes.Repeat([]byte("x"), size)})
	}
	return l
}

// followerNode returns node 2 of a 3-node cluster holding the given log.
func followerNode(t *testing.T, log LogStore) *Node {
	t.Helper()
	return newTestNode(t, 2, 3, &memStorage{}, log)
}

func appendMsg(term, prevIndex, prevTerm, commit uint64, entries ...Entry) Message {
	return Message{Type: MsgAppend, From: 1, To: 2, Term: term, PrevLogIndex: prevIndex, PrevLogTerm: prevTerm, LeaderCommit: commit, Entries: entries}
}

func entry(index, term uint64) Entry {
	return Entry{Index: index, Term: term, Data: []byte(fmt.Sprintf("e%d.%d", index, term))}
}

// --- proposals and the leader -----------------------------------------------

func TestProposeOnANonLeaderIsRefused(t *testing.T) {
	n := newTestNode(t, 2, 3, &memStorage{}, nil)

	_, _, err := n.Propose([]byte("x"))
	var nl *NotLeaderError
	if !errors.As(err, &nl) || !errors.Is(err, ErrNotLeader) || nl.Leader != None {
		t.Fatalf("err = %v, want NotLeaderError with unknown leader", err)
	}

	// Once it follows a leader, the error says who.
	if err := n.Step(appendMsg(1, 0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	_, _, err = n.Propose([]byte("x"))
	if !errors.As(err, &nl) || nl.Leader != 1 {
		t.Fatalf("err = %v, want NotLeaderError pointing at node 1", err)
	}
	if n.log.LastIndex() != 0 {
		t.Fatal("a refused proposal reached the log")
	}
}

func TestNewLeaderAppendsANoopOfItsOwnTerm(t *testing.T) {
	n := newLeaderNode(t, 3, logWithTerms(1, 1), nil)
	st := n.Status()
	if st.Term != 2 || st.LastIndex != 3 {
		t.Fatalf("status = %+v, want term 2 and a log of 3 entries", st)
	}
	noop, _ := n.log.Entries(3, 4)
	if noop[0].Type != EntryNoop || noop[0].Term != 2 || len(noop[0].Data) != 0 {
		t.Fatalf("last entry = %+v, want a no-op of term 2", noop[0])
	}

	msgs := n.Messages()
	for _, peer := range []NodeID{2, 3} {
		m := appendTo(t, msgs, peer)
		if m.PrevLogIndex != 2 || m.PrevLogTerm != 1 || !reflect.DeepEqual(indexes(m.Entries), []uint64{3}) {
			t.Fatalf("append to %d = prev (%d,%d) entries %v; want prev (2,1) and the no-op", peer, m.PrevLogIndex, m.PrevLogTerm, indexes(m.Entries))
		}
	}
}

func TestSingleNodeClusterCommitsAtOnce(t *testing.T) {
	c := newCluster(t, 1, 1)
	if c.runUntil(10*testElectionTicks, c.hasLeader) < 0 {
		t.Fatal("no leader")
	}
	if _, _, err := c.propose(1, "a"); err != nil {
		t.Fatal(err)
	}
	if got := c.appliedData(1); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("applied %v, want [a]", got)
	}
	if st := c.status(1); st.CommitIndex != 2 { // the no-op and "a"
		t.Fatalf("commit index = %d, want 2", st.CommitIndex)
	}
}

// Section 5.4.2 / Figure 8: a leader must not count replicas to commit an
// entry of an older term, because such an entry can still be overwritten.
func TestLeaderCommitsOnlyEntriesOfItsOwnTerm(t *testing.T) {
	n := newLeaderNode(t, 3, logWithTerms(1, 1, 1), nil)
	n.Messages()

	// Node 2 now stores the three old entries: a majority has them, but they
	// belong to term 1 and the leader is in term 2.
	if err := n.Step(appendResp(2, 2, true, 3, 0)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status().CommitIndex; got != 0 {
		t.Fatalf("commit index = %d: an old-term entry was committed by counting replicas", got)
	}

	// Once the no-op of the current term is on a majority, everything before it
	// is committed with it.
	if err := n.Step(appendResp(2, 2, true, 4, 0)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status().CommitIndex; got != 4 {
		t.Fatalf("commit index = %d, want 4", got)
	}
	committed, err := n.Committed()
	if err != nil || len(committed) != 4 {
		t.Fatalf("Committed = %d entries, err %v; want 4", len(committed), err)
	}
	if again, _ := n.Committed(); len(again) != 0 {
		t.Fatal("Committed returned the same entries twice")
	}
}

func TestLeaderBacksUpUsingTheHint(t *testing.T) {
	n := newLeaderNode(t, 3, logWithTerms(1, 1, 1), nil)
	n.Messages()

	// Node 2 says its log only matches up to index 1.
	if err := n.Step(appendResp(2, 2, false, 0, 1)); err != nil {
		t.Fatal(err)
	}
	m := appendTo(t, n.Messages(), 2)
	if m.PrevLogIndex != 1 || !reflect.DeepEqual(indexes(m.Entries), []uint64{2, 3, 4}) {
		t.Fatalf("retry = prev %d entries %v; want prev 1 and entries 2..4", m.PrevLogIndex, indexes(m.Entries))
	}

	// A hint that points forward must not make the leader loop: it still backs up.
	if err := n.Step(appendResp(2, 2, false, 0, 99)); err != nil {
		t.Fatal(err)
	}
	m = appendTo(t, n.Messages(), 2)
	if m.PrevLogIndex != 0 {
		t.Fatalf("after a useless hint prev = %d, want 0", m.PrevLogIndex)
	}
}

func TestRejectThatChangesNothingIsNotRetriedAtOnce(t *testing.T) {
	n := newLeaderNode(t, 3, logWithTerms(1, 1, 1), nil)
	n.Messages()
	// Node 2 is known to have indexes 1..3, so the leader will never back up
	// past index 3; a reject cannot change what it sends.
	if err := n.Step(appendResp(2, 2, true, 3, 0)); err != nil {
		t.Fatal(err)
	}
	n.Messages()

	if err := n.Step(appendResp(2, 2, false, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if msgs := n.Messages(); len(msgs) != 0 {
		t.Fatalf("an unproductive reject triggered an immediate resend: %+v", msgs)
	}
	// The heartbeat still retries later.
	for i := 0; i < testHeartbeatTicks; i++ {
		_ = n.Tick()
	}
	if len(n.Messages()) == 0 {
		t.Fatal("heartbeat did not retry")
	}
}

func TestAppendBatchLimits(t *testing.T) {
	t.Run("entries", func(t *testing.T) {
		n := newLeaderNode(t, 3, logWithTerms(1, 1, 1, 1, 1), func(c *Config) { c.MaxBatchEntries = 2 })
		n.Messages()
		_ = n.Step(appendResp(2, 2, false, 0, 0)) // follower has nothing
		m := appendTo(t, n.Messages(), 2)
		if !reflect.DeepEqual(indexes(m.Entries), []uint64{1, 2}) {
			t.Fatalf("first batch = %v, want [1 2]", indexes(m.Entries))
		}
		// The next batch follows as soon as the first is acknowledged.
		_ = n.Step(appendResp(2, 2, true, 2, 0))
		m = appendTo(t, n.Messages(), 2)
		if m.PrevLogIndex != 2 || !reflect.DeepEqual(indexes(m.Entries), []uint64{3, 4}) {
			t.Fatalf("second batch = prev %d entries %v; want prev 2 and [3 4]", m.PrevLogIndex, indexes(m.Entries))
		}
	})

	t.Run("bytes", func(t *testing.T) {
		log := logWithSizedEntries([]uint64{1, 1, 1}, 6)
		n := newLeaderNode(t, 3, log, func(c *Config) { c.MaxBatchBytes = 10 })
		n.Messages()
		_ = n.Step(appendResp(2, 2, false, 0, 0))
		m := appendTo(t, n.Messages(), 2)
		if !reflect.DeepEqual(indexes(m.Entries), []uint64{1}) {
			t.Fatalf("batch = %v, want just [1] (two entries would be 12 bytes)", indexes(m.Entries))
		}
	})

	t.Run("an oversized entry still goes out", func(t *testing.T) {
		log := logWithSizedEntries([]uint64{1}, 100)
		n := newLeaderNode(t, 3, log, func(c *Config) { c.MaxBatchBytes = 10 })
		n.Messages()
		_ = n.Step(appendResp(2, 2, false, 0, 0))
		m := appendTo(t, n.Messages(), 2)
		if len(m.Entries) != 1 {
			t.Fatalf("batch = %v, want the single large entry", indexes(m.Entries))
		}
	})
}

func TestProposeFailsCleanlyWhenTheLogCannotBeWritten(t *testing.T) {
	log := &failingLog{LogStore: NewMemoryLog()}
	n := newLeaderNode(t, 3, log, nil)
	n.Messages()
	before := n.Status().LastIndex

	log.failAppend = errDiskFull
	if _, _, err := n.Propose([]byte("x")); !errors.Is(err, errDiskFull) {
		t.Fatalf("err = %v, want errDiskFull", err)
	}
	if len(n.Messages()) != 0 || n.Status().LastIndex != before {
		t.Fatal("a proposal that failed to persist was replicated")
	}
}

func TestNodeRefusesALogFromTheFuture(t *testing.T) {
	cfg := testConfig(1, clusterIDs(3), &memStorage{hs: HardState{Term: 3}}, 1)
	cfg.Log = logWithTerms(1, 5)
	_, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "term") {
		t.Fatalf("err = %v, want an error about the log being ahead of the saved term", err)
	}
}

// --- the follower side of AppendEntries -------------------------------------

func TestFollowerConsistencyCheck(t *testing.T) {
	// The follower's log has terms [1 1 2 2 2].
	cases := []struct {
		name                string
		prevIndex, prevTerm uint64
		wantSuccess         bool
		wantMatch, wantHint uint64
	}{
		{"prev is the last entry", 5, 2, true, 5, 0},
		{"prev is in the middle", 3, 2, true, 3, 0},
		{"prev is the empty prefix", 0, 0, true, 0, 0},
		{"prev is beyond the log", 7, 2, false, 0, 5},
		{"prev has the wrong term", 5, 9, false, 0, 2}, // backs up over the whole run of term 2
		// Index 2 has term 1 and belongs to a run of term 1 that starts at index 1,
		// so the whole run is in doubt and the leader should resend from index 1.
		{"prev in the middle, wrong term", 2, 2, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := followerNode(t, logWithTerms(1, 1, 2, 2, 2))
			if err := n.Step(appendMsg(2, c.prevIndex, c.prevTerm, 0)); err != nil {
				t.Fatal(err)
			}
			r := only(t, n.Messages())
			if r.Type != MsgAppendResp || r.To != 1 || r.Success != c.wantSuccess || r.MatchIndex != c.wantMatch || r.HintIndex != c.wantHint {
				t.Fatalf("reply = %+v; want success=%v match=%d hint=%d", r, c.wantSuccess, c.wantMatch, c.wantHint)
			}
			if n.Status().Leader != 1 {
				t.Fatal("a valid leader's message was not remembered as such")
			}
		})
	}
}

func TestFollowerStoresNewEntriesAndReportsTheMatch(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 1))
	if err := n.Step(appendMsg(1, 2, 1, 0, entry(3, 1), entry(4, 1))); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Success || r.MatchIndex != 4 {
		t.Fatalf("reply = %+v, want success with match 4", r)
	}
	if !reflect.DeepEqual(logTerms(t, n.log), []uint64{1, 1, 1, 1}) {
		t.Fatalf("log terms = %v", logTerms(t, n.log))
	}
}

func TestFollowerReplacesAConflictingSuffix(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 1, 2, 2))

	// The new leader (term 3) agrees up to index 2 and has other entries after it.
	if err := n.Step(appendMsg(3, 2, 1, 0, entry(3, 3), entry(4, 3), entry(5, 3))); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Success || r.MatchIndex != 5 {
		t.Fatalf("reply = %+v", r)
	}
	if !reflect.DeepEqual(logTerms(t, n.log), []uint64{1, 1, 3, 3, 3}) {
		t.Fatalf("log terms = %v, want [1 1 3 3 3]", logTerms(t, n.log))
	}

	// An older, shorter copy of the same message arrives late. It must not cut
	// the log back: the entries it carries are already there.
	if err := n.Step(appendMsg(3, 2, 1, 0, entry(3, 3))); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Success {
		t.Fatalf("late duplicate rejected: %+v", r)
	}
	if n.log.LastIndex() != 5 {
		t.Fatalf("a late duplicate shortened the log to %d entries", n.log.LastIndex())
	}
}

func TestFollowerNeverOverwritesCommittedEntries(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 1, 2))
	if err := n.Step(appendMsg(2, 3, 2, 3)); err != nil { // leader commit = 3
		t.Fatal(err)
	}
	n.Messages()
	if n.Status().CommitIndex != 3 {
		t.Fatal("setup: commit index not advanced")
	}

	// A message that would replace committed entry 2 is a protocol violation.
	err := n.Step(appendMsg(3, 1, 1, 3, entry(2, 3)))
	if err == nil {
		t.Fatal("a committed entry was overwritten without an error")
	}
	if !reflect.DeepEqual(logTerms(t, n.log), []uint64{1, 1, 2}) {
		t.Fatalf("log terms = %v: the committed log was modified", logTerms(t, n.log))
	}
	if len(n.Messages()) != 0 {
		t.Fatal("the node acknowledged a message it rejected")
	}
}

func TestFollowerCommitsOnlyWhatTheLeaderVerified(t *testing.T) {
	// The follower holds [1 1 1] but the leader only vouched for index 1: the
	// rest may differ from the leader's log, so it must not be committed yet.
	n := followerNode(t, logWithTerms(1, 1, 1))
	if err := n.Step(appendMsg(1, 1, 1, 10)); err != nil { // LeaderCommit far ahead
		t.Fatal(err)
	}
	if got := n.Status().CommitIndex; got != 1 {
		t.Fatalf("commit index = %d, want 1 (the last verified entry)", got)
	}
	// Learning more advances it.
	if err := n.Step(appendMsg(1, 3, 1, 10)); err != nil {
		t.Fatal(err)
	}
	if got := n.Status().CommitIndex; got != 3 {
		t.Fatalf("commit index = %d, want 3", got)
	}
}

func TestMalformedAppendIsDropped(t *testing.T) {
	n := followerNode(t, logWithTerms(1))
	// The entries do not continue from PrevLogIndex.
	if err := n.Step(appendMsg(1, 1, 1, 0, entry(5, 1))); err != nil {
		t.Fatal(err)
	}
	if len(n.Messages()) != 0 || n.log.LastIndex() != 1 {
		t.Fatalf("a malformed message had an effect: lastIndex %d", n.log.LastIndex())
	}
}

func TestFollowerDoesNotAcknowledgeEntriesItCouldNotStore(t *testing.T) {
	log := &failingLog{LogStore: logWithTerms(1), failAppend: errDiskFull}
	n := followerNode(t, log)

	err := n.Step(appendMsg(1, 1, 1, 5, entry(2, 1)))
	if !errors.Is(err, errDiskFull) {
		t.Fatalf("err = %v, want errDiskFull", err)
	}
	if len(n.Messages()) != 0 {
		t.Fatal("success was reported for entries that were not stored")
	}
	if n.Status().CommitIndex != 0 {
		t.Fatal("the commit index moved although the entries were not stored")
	}
}

func TestAppendsFromAStaleLeaderAreRejected(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 2, 3))
	if err := n.Step(appendMsg(2, 3, 3, 0, entry(4, 2))); err != nil { // term 2 < 3
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); r.Success || r.Term != 3 {
		t.Fatalf("reply = %+v, want a rejection carrying term 3", r)
	}
	if n.log.LastIndex() != 3 {
		t.Fatal("a stale leader modified the log")
	}
}

// --- whole-cluster behaviour ------------------------------------------------

func TestEntriesReplicateAndApplyEverywhere(t *testing.T) {
	c := newCluster(t, 3, 21)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	for _, d := range []string{"a", "b", "c"} {
		if _, ok := c.proposeToAnyLeader(d); !ok {
			t.Fatal("no leader to propose to")
		}
	}
	if c.runUntil(5*testElectionTicks, c.converged) < 0 {
		t.Fatal("cluster did not converge")
	}
	for _, id := range c.ids {
		if got := c.appliedData(id); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
			t.Fatalf("node %d applied %v, want [a b c]", id, got)
		}
		if st := c.status(id); st.CommitIndex != 4 { // the leader's no-op plus three commands
			t.Fatalf("node %d commit index = %d, want 4", id, st.CommitIndex)
		}
	}
}

func TestNothingCommitsWithoutAMajority(t *testing.T) {
	c := newCluster(t, 3, 5)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	leader := c.leaders()[0]
	c.isolate(leader)

	index, _, err := c.propose(leader, "lonely")
	if err != nil {
		t.Fatal(err)
	}
	c.run(3 * testElectionTicks)

	if got := c.status(leader).CommitIndex; got >= index {
		t.Fatalf("leader committed index %d alone", index)
	}
	for _, id := range c.ids {
		if got := c.appliedData(id); len(got) != 0 {
			t.Fatalf("node %d applied %v without a majority", id, got)
		}
	}
}

func TestLaggingFollowerCatchesUp(t *testing.T) {
	const entries = 200 // more than one batch of 64
	c := newCluster(t, 3, 8)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	leader := c.leaders()[0]
	var lagging NodeID
	for _, id := range c.ids {
		if id != leader {
			lagging = id
			break
		}
	}

	c.isolate(lagging)
	want := make([]string, entries)
	for i := range want {
		want[i] = fmt.Sprintf("entry-%03d", i)
		if _, _, err := c.propose(leader, want[i]); err != nil {
			t.Fatal(err)
		}
	}
	c.run(2 * testHeartbeatTicks)
	if got := len(c.appliedData(lagging)); got != 0 {
		t.Fatalf("the isolated node applied %d entries", got)
	}

	c.heal()
	if c.runUntil(20*testElectionTicks, c.converged) < 0 {
		t.Fatal("the lagging follower did not catch up")
	}
	if got := c.appliedData(lagging); !reflect.DeepEqual(got, want) {
		t.Fatalf("lagging node applied %d entries, want %d (or the order differs)", len(got), entries)
	}
}

func TestDeposedLeadersUncommittedEntriesAreOverwritten(t *testing.T) {
	c := newCluster(t, 3, 4)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	old := c.leaders()[0]
	if _, _, err := c.propose(old, "committed-before"); err != nil {
		t.Fatal(err)
	}
	c.run(2 * testHeartbeatTicks)

	// The old leader is cut off and keeps accepting commands that cannot commit.
	c.isolate(old)
	for _, d := range []string{"lost-1", "lost-2"} {
		if _, _, err := c.propose(old, d); err != nil {
			t.Fatal(err)
		}
	}

	// The others elect a new leader that commits different commands.
	var next NodeID
	if c.runUntil(10*testElectionTicks, func() bool {
		for _, id := range c.leaders() {
			if id != old {
				next = id
				return true
			}
		}
		return false
	}) < 0 {
		t.Fatal("no new leader")
	}
	for _, d := range []string{"kept-1", "kept-2"} {
		if _, _, err := c.propose(next, d); err != nil {
			t.Fatal(err)
		}
	}

	c.heal()
	if c.runUntil(20*testElectionTicks, c.converged) < 0 {
		t.Fatal("cluster did not converge after healing")
	}
	want := []string{"committed-before", "kept-1", "kept-2"}
	for _, id := range c.ids {
		if got := c.appliedData(id); !reflect.DeepEqual(got, want) {
			t.Fatalf("node %d applied %v, want %v", id, got, want)
		}
	}
}

// An entry that reached a majority but was never reported committed to its
// leader (the replies were lost) must be committed by the next leader, through
// the no-op of the new term.
func TestEntryOnAMajorityIsCommittedByTheNextLeader(t *testing.T) {
	c := newCluster(t, 3, 13)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	leader := c.leaders()[0]
	for _, id := range c.ids {
		if id != leader {
			c.cut(id, leader) // the leader hears nothing back
		}
	}

	index, _, err := c.propose(leader, "x")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range c.ids {
		if id != leader && c.status(id).LastIndex < index {
			t.Fatalf("node %d did not store the entry", id)
		}
	}
	if c.status(leader).CommitIndex >= index {
		t.Fatal("setup: the leader committed although it heard nothing back")
	}

	c.crash(leader)
	c.heal()
	if c.runUntil(20*testElectionTicks, c.converged) < 0 {
		t.Fatal("the remaining nodes did not converge")
	}
	for _, id := range c.ids {
		if id == leader {
			continue
		}
		if got := c.appliedData(id); !reflect.DeepEqual(got, []string{"x"}) {
			t.Fatalf("node %d applied %v, want [x]", id, got)
		}
	}
}

func TestEverythingSurvivesARestartOfAllNodes(t *testing.T) {
	c := newCluster(t, 3, 17)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	for _, d := range []string{"a", "b"} {
		c.proposeToAnyLeader(d)
	}
	if c.runUntil(5*testElectionTicks, c.converged) < 0 {
		t.Fatal("no convergence before the restart")
	}

	for _, id := range c.ids {
		c.crash(id)
	}
	for _, id := range c.ids {
		c.restartNode(id)
	}
	if c.runUntil(20*testElectionTicks, c.converged) < 0 {
		t.Fatal("no convergence after restarting every node")
	}
	// The state machine was rebuilt by replaying the log.
	for _, id := range c.ids {
		if got := c.appliedData(id); !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("node %d applied %v after the restart, want [a b]", id, got)
		}
	}
}

// Cut links, crash nodes and submit commands at random. The harness checks the
// safety properties after every message; at the end the cluster must converge
// and keep everything that was ever committed.
func TestReplicationUnderRandomFaults(t *testing.T) {
	const seeds = 300
	committedTotal := 0
	for seed := int64(1); seed <= seeds; seed++ {
		c := newCluster(t, 5, seed)
		rng := rand.New(rand.NewSource(seed))
		next := 0

		for step := 0; step < 100; step++ {
			switch rng.Intn(10) {
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
			default:
				// Submit to a random running node; non-leaders refuse, which is fine.
				id := NodeID(1 + rng.Intn(5))
				if !c.down[id] {
					if _, _, err := c.propose(id, fmt.Sprintf("s%d-c%d", seed, next)); err == nil {
						next++
					} else if !errors.Is(err, ErrNotLeader) {
						t.Fatalf("seed %d: unexpected error %v", seed, err)
					}
				}
			}
			c.run(1 + rng.Intn(testElectionTicks))
		}

		c.heal()
		for _, id := range c.ids {
			if c.down[id] {
				c.restartNode(id)
			}
		}
		if c.runUntil(40*testElectionTicks, c.converged) < 0 {
			t.Fatalf("seed %d: cluster did not converge after all faults were repaired", seed)
		}

		// Everything ever reported committed is still there, in every node, in order.
		for _, id := range c.ids {
			applied := c.applied[id]
			if len(applied) < len(c.committed) {
				t.Fatalf("seed %d: node %d applied %d entries but %d were committed at some point", seed, id, len(applied), len(c.committed))
			}
		}
		committedTotal += len(c.committed)
	}
	t.Logf("%d seeds, %d entries committed in total, no safety violation", seeds, committedTotal)
}
