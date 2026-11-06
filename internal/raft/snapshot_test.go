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

func snapData(index uint64) []byte { return []byte(fmt.Sprintf("state@%d", index)) }

// appliedFollowerOn returns node 2 of a 3-node cluster whose log is log, with
// every entry committed and handed to the state machine, so that it can be
// snapshotted. It also returns the snapshot store.
func appliedFollowerOn(t *testing.T, log LogStore, trailing uint64) (*Node, *MemorySnapshotStore) {
	t.Helper()
	st := &memStorage{}
	if lastTerm, _ := log.Term(log.LastIndex()); st.hs.Term < lastTerm {
		st.hs.Term = lastTerm
	}
	snaps := NewMemorySnapshotStore()
	cfg := testConfig(2, clusterIDs(3), st, 2)
	cfg.Log = log
	cfg.Snapshots = snaps
	cfg.SnapshotTrailing = trailing
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	last := log.LastIndex()
	lastTerm, _ := log.Term(last)
	if err := n.Step(appendMsg(st.hs.Term, last, lastTerm, last)); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	if _, err := n.Committed(); err != nil {
		t.Fatal(err)
	}
	return n, snaps
}

func snapshotMsg(term uint64, s Snapshot) Message {
	return Message{Type: MsgSnapshot, From: 1, To: 2, Term: term, Snapshot: &s}
}

func baseOf(l LogStore) [2]uint64 {
	i, term := l.Base()
	return [2]uint64{i, term}
}

// leaderWithSnapshot returns node 1, leader of term 2, whose log holds six
// entries (five old ones and its no-op) that are committed, applied and folded
// into a snapshot at index 6. Node 2 has everything; node 3 has nothing.
func leaderWithSnapshot(t *testing.T) *Node {
	t.Helper()
	n := newLeaderNode(t, 3, logWithTerms(1, 1, 1, 1, 1), nil)
	n.Messages()
	if err := n.Step(appendResp(2, 2, true, 6, 0)); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	if got, err := n.Committed(); err != nil || len(got) != 6 {
		t.Fatalf("Committed = %d entries, err %v; want 6", len(got), err)
	}
	if err := n.Compact(6, snapData(6)); err != nil {
		t.Fatal(err)
	}
	return n
}

func ofType(msgs []Message, typ MessageType, to NodeID) []Message {
	var out []Message
	for _, m := range msgs {
		if m.Type == typ && m.To == to {
			out = append(out, m)
		}
	}
	return out
}

// --- Compact ----------------------------------------------------------------

func TestCompactFoldsAppliedEntriesIntoASnapshot(t *testing.T) {
	log := logWithTerms(1, 1, 2, 2, 2)
	n, snaps := appliedFollowerOn(t, log, 0)

	if err := n.Compact(4, snapData(4)); err != nil {
		t.Fatal(err)
	}

	st := n.Status()
	if st.SnapshotIndex != 4 || st.LastIndex != 5 || st.CommitIndex != 5 {
		t.Fatalf("status = %+v", st)
	}
	if got := baseOf(log); got != [2]uint64{4, 2} {
		t.Fatalf("log base = %v, want [4 2]", got)
	}
	if _, err := log.Term(3); !errors.Is(err, ErrCompacted) {
		t.Fatalf("Term(3) err = %v, want ErrCompacted", err)
	}
	if rest, err := log.Entries(5, 6); err != nil || len(rest) != 1 {
		t.Fatalf("the entry after the snapshot is gone: %v, %v", rest, err)
	}
	saved, ok, _ := snaps.Load()
	if !ok || saved.Index != 4 || saved.Term != 2 || !bytes.Equal(saved.Data, snapData(4)) {
		t.Fatalf("saved snapshot = %+v, %v", saved, ok)
	}
}

func TestCompactKeepsTrailingEntries(t *testing.T) {
	log := logWithTerms(1, 1, 2, 2, 2, 2)
	n, _ := appliedFollowerOn(t, log, 2)

	if err := n.Compact(5, snapData(5)); err != nil {
		t.Fatal(err)
	}
	if n.Status().SnapshotIndex != 5 {
		t.Fatalf("snapshot index = %d, want 5", n.Status().SnapshotIndex)
	}
	// The log is cut two entries below the snapshot, not at it.
	if got := baseOf(log); got != [2]uint64{3, 2} {
		t.Fatalf("log base = %v, want [3 2]", got)
	}
	if _, err := log.Entries(4, 7); err != nil {
		t.Fatalf("trailing entries are missing: %v", err)
	}
}

func TestCompactRefusesIndexesNotYetApplied(t *testing.T) {
	log := logWithTerms(1, 1, 1)
	n := newTestNode(t, 2, 3, &memStorage{}, log) // nothing committed or applied
	err := n.Compact(2, snapData(2))
	if err == nil {
		t.Fatal("snapshotted entries that were never applied")
	}
	if n.Status().SnapshotIndex != 0 || baseOf(log) != [2]uint64{0, 0} {
		t.Fatal("a refused Compact changed something")
	}
	if _, ok, _ := n.snaps.Load(); ok {
		t.Fatal("a refused Compact saved a snapshot")
	}
}

func TestCompactIgnoresSnapshotsOlderThanTheCurrentOne(t *testing.T) {
	log := logWithTerms(1, 1, 2, 2, 2)
	n, snaps := appliedFollowerOn(t, log, 0)
	if err := n.Compact(4, snapData(4)); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []uint64{4, 3, 1} {
		if err := n.Compact(idx, []byte("older")); err != nil {
			t.Fatalf("Compact(%d): %v", idx, err)
		}
	}
	if saved, _, _ := snaps.Load(); saved.Index != 4 || !bytes.Equal(saved.Data, snapData(4)) {
		t.Fatalf("an older snapshot replaced the newer one: %+v", saved)
	}
}

func TestSnapshotIsSavedBeforeTheLogIsCut(t *testing.T) {
	t.Run("log cannot be cut", func(t *testing.T) {
		// The snapshot is durable first. If cutting the log fails, nothing is
		// lost: the log is just longer than it needs to be.
		log := &failingLog{LogStore: logWithTerms(1, 1, 2, 2)}
		n, snaps := appliedFollowerOn(t, log, 0)

		log.failCompact = errDiskFull
		if err := n.Compact(3, snapData(3)); !errors.Is(err, errDiskFull) {
			t.Fatalf("err = %v, want errDiskFull", err)
		}
		if saved, ok, _ := snaps.Load(); !ok || saved.Index != 3 {
			t.Fatalf("the snapshot was not saved before the log was cut: %+v, %v", saved, ok)
		}
		if baseOf(log) != [2]uint64{0, 0} {
			t.Fatal("the log was cut although cutting failed")
		}

		// A restart on the same files works and resumes from the snapshot.
		st := &memStorage{hs: HardState{Term: 2}}
		cfg := testConfig(2, clusterIDs(3), st, 2)
		cfg.Log, cfg.Snapshots = log, snaps
		n2, err := New(cfg)
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if got := n2.Status(); got.SnapshotIndex != 3 || got.CommitIndex != 3 {
			t.Fatalf("status after restart = %+v", got)
		}
	})

	t.Run("snapshot cannot be saved", func(t *testing.T) {
		log := logWithTerms(1, 1, 2, 2)
		st := &memStorage{hs: HardState{Term: 2}}
		failing := &failingSnapshots{SnapshotStore: NewMemorySnapshotStore(), failSave: errDiskFull}
		cfg := testConfig(2, clusterIDs(3), st, 2)
		cfg.Log, cfg.Snapshots = log, failing
		n, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = n.Step(appendMsg(2, 4, 2, 4))
		n.Messages()
		_, _ = n.Committed()

		if err := n.Compact(3, snapData(3)); !errors.Is(err, errDiskFull) {
			t.Fatalf("err = %v, want errDiskFull", err)
		}
		if baseOf(log) != [2]uint64{0, 0} || n.Status().SnapshotIndex != 0 {
			t.Fatal("the log was cut although the snapshot is not durable")
		}
	})
}

// --- recovery at startup ----------------------------------------------------

func TestRestartResumesFromTheSnapshot(t *testing.T) {
	log := logWithTerms(1, 1, 2, 2, 2)
	n, snaps := appliedFollowerOn(t, log, 0)
	if err := n.Compact(4, snapData(4)); err != nil {
		t.Fatal(err)
	}

	st := &memStorage{hs: HardState{Term: 2}}
	cfg := testConfig(2, clusterIDs(3), st, 2)
	cfg.Log, cfg.Snapshots = log, snaps
	n2, err := New(cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}

	if got := n2.Status(); got.CommitIndex != 4 || got.SnapshotIndex != 4 {
		t.Fatalf("status = %+v, want commit and snapshot at 4", got)
	}
	// The state machine must load the snapshot, exactly once.
	restore := n2.TakeRestore()
	if restore == nil || restore.Index != 4 || !bytes.Equal(restore.Data, snapData(4)) {
		t.Fatalf("TakeRestore = %+v", restore)
	}
	if n2.TakeRestore() != nil {
		t.Fatal("the snapshot was handed out twice")
	}
	// The entries it covers are not replayed.
	if got, _ := n2.Committed(); len(got) != 0 {
		t.Fatalf("Committed replayed %d entries that the snapshot covers", len(got))
	}
}

func TestRecoveryCutsTheLogDownToASnapshotThatIsAheadOfIt(t *testing.T) {
	log := logWithTerms(1, 1)
	snaps := NewMemorySnapshotStore()
	_ = snaps.Save(Snapshot{Index: 5, Term: 2, Data: snapData(5)})
	cfg := testConfig(2, clusterIDs(3), &memStorage{hs: HardState{Term: 2}}, 2)
	cfg.Log, cfg.Snapshots = log, snaps

	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if baseOf(log) != [2]uint64{5, 2} || log.LastIndex() != 5 {
		t.Fatalf("log base %v, last %d; want the log replaced by the snapshot at 5", baseOf(log), log.LastIndex())
	}
	if got := n.Status(); got.CommitIndex != 5 || got.LastIndex != 5 {
		t.Fatalf("status = %+v", got)
	}
}

func TestRecoveryRefusesInconsistentFiles(t *testing.T) {
	cases := map[string]struct {
		log  func() *MemoryLog
		snap *Snapshot
		want string
	}{
		"log compacted past the snapshot": {
			log: func() *MemoryLog {
				l := logWithTerms(1, 1, 1, 1, 1, 1, 1, 1, 1, 1)
				_ = l.CompactTo(8, 1)
				return l
			},
			snap: &Snapshot{Index: 5, Term: 1},
			want: "past the snapshot",
		},
		"log compacted but no snapshot": {
			log: func() *MemoryLog {
				l := logWithTerms(1, 1, 1, 1)
				_ = l.CompactTo(3, 1)
				return l
			},
			want: "no snapshot",
		},
		"snapshot term disagrees with the log": {
			log:  func() *MemoryLog { return logWithTerms(1, 1, 1, 1) },
			snap: &Snapshot{Index: 3, Term: 2},
			want: "disagrees",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			snaps := NewMemorySnapshotStore()
			if c.snap != nil {
				_ = snaps.Save(*c.snap)
			}
			cfg := testConfig(2, clusterIDs(3), &memStorage{hs: HardState{Term: 5}}, 2)
			cfg.Log, cfg.Snapshots = c.log(), snaps
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, c.want)
			}
		})
	}
}

// --- the follower side of InstallSnapshot -----------------------------------

func TestFollowerInstallsASnapshotAheadOfItsLog(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 1))
	snap := Snapshot{Index: 10, Term: 2, Data: snapData(10)}

	if err := n.Step(snapshotMsg(2, snap)); err != nil {
		t.Fatal(err)
	}
	r := only(t, n.Messages())
	if r.Type != MsgSnapshotResp || !r.Success || r.MatchIndex != 10 || r.To != 1 || r.Term != 2 {
		t.Fatalf("reply = %+v", r)
	}
	st := n.Status()
	if st.CommitIndex != 10 || st.SnapshotIndex != 10 || st.LastIndex != 10 || st.Leader != 1 {
		t.Fatalf("status = %+v", st)
	}
	if baseOf(n.log) != [2]uint64{10, 2} {
		t.Fatalf("log base = %v", baseOf(n.log))
	}
	if saved, ok, _ := n.snaps.Load(); !ok || saved.Index != 10 || !bytes.Equal(saved.Data, snap.Data) {
		t.Fatalf("snapshot was not saved: %+v", saved)
	}
	// The state machine is told to load it, once, and nothing is replayed.
	if got := n.TakeRestore(); got == nil || got.Index != 10 || !bytes.Equal(got.Data, snap.Data) {
		t.Fatalf("TakeRestore = %+v", got)
	}
	if n.TakeRestore() != nil {
		t.Fatal("restore handed out twice")
	}
	if got, _ := n.Committed(); len(got) != 0 {
		t.Fatalf("Committed returned %d entries after a snapshot", len(got))
	}

	// The leader carries on right after the snapshot.
	if err := n.Step(appendMsg(2, 10, 2, 11, entry(11, 2))); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Success || r.MatchIndex != 11 {
		t.Fatalf("append after the snapshot: %+v", r)
	}
	if got, _ := n.Committed(); len(got) != 1 || got[0].Index != 11 {
		t.Fatalf("Committed = %v, want just entry 11", got)
	}
}

func TestSnapshotKeepsTheEntriesAfterItWhenTheLogAgrees(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 1, 2, 2, 2, 2))
	if err := n.Step(snapshotMsg(3, Snapshot{Index: 4, Term: 2, Data: snapData(4)})); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	if baseOf(n.log) != [2]uint64{4, 2} || n.log.LastIndex() != 6 {
		t.Fatalf("base %v, last %d; want base [4 2] and the entries 5 and 6 kept", baseOf(n.log), n.log.LastIndex())
	}
	if kept, err := n.log.Entries(5, 7); err != nil || len(kept) != 2 {
		t.Fatalf("kept = %v, %v", kept, err)
	}
}

func TestSnapshotDiscardsALogThatDisagreesWithIt(t *testing.T) {
	// Entry 4 of the log has term 1, but the snapshot says index 4 has term 2:
	// this log belongs to another history and must go entirely.
	n := followerNode(t, logWithTerms(1, 1, 1, 1, 1, 1))
	if err := n.Step(snapshotMsg(3, Snapshot{Index: 4, Term: 2, Data: snapData(4)})); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	if baseOf(n.log) != [2]uint64{4, 2} || n.log.LastIndex() != 4 {
		t.Fatalf("base %v, last %d; want an empty log based at [4 2]", baseOf(n.log), n.log.LastIndex())
	}
}

func TestSnapshotBehindOurCommitIsAcknowledgedWithoutChange(t *testing.T) {
	log := logWithTerms(1, 1, 1, 1, 1)
	n, snaps := appliedFollowerOn(t, log, 0)

	if err := n.Step(snapshotMsg(1, Snapshot{Index: 3, Term: 1, Data: snapData(3)})); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); r.Type != MsgSnapshotResp || !r.Success || r.MatchIndex != 3 {
		t.Fatalf("reply = %+v", r)
	}
	if _, ok, _ := snaps.Load(); ok || n.TakeRestore() != nil || log.LastIndex() != 5 || baseOf(log) != [2]uint64{0, 0} {
		t.Fatal("an old snapshot changed the node")
	}
}

func TestSnapshotFromAStaleLeaderIsRejected(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 2, 3))
	if err := n.Step(snapshotMsg(2, Snapshot{Index: 9, Term: 2, Data: snapData(9)})); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); r.Type != MsgSnapshotResp || r.Success || r.Term != 3 {
		t.Fatalf("reply = %+v, want a rejection carrying term 3", r)
	}
	if n.TakeRestore() != nil || n.log.LastIndex() != 3 {
		t.Fatal("a stale leader's snapshot was installed")
	}
}

func TestSnapshotThatCannotBeStoredIsNotAcknowledged(t *testing.T) {
	build := func(log LogStore, snaps SnapshotStore) *Node {
		cfg := testConfig(2, clusterIDs(3), &memStorage{hs: HardState{Term: 1}}, 2)
		cfg.Log, cfg.Snapshots = log, snaps
		n, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, n := range map[string]*Node{
		"snapshot store fails": build(NewMemoryLog(), &failingSnapshots{SnapshotStore: NewMemorySnapshotStore(), failSave: errDiskFull}),
		"log fails":            build(&failingLog{LogStore: NewMemoryLog(), failCompact: errDiskFull}, NewMemorySnapshotStore()),
	} {
		t.Run(name, func(t *testing.T) {
			err := n.Step(snapshotMsg(1, Snapshot{Index: 5, Term: 1, Data: snapData(5)}))
			if !errors.Is(err, errDiskFull) {
				t.Fatalf("err = %v, want errDiskFull", err)
			}
			if len(n.Messages()) != 0 {
				t.Fatal("success was reported for a snapshot that was not stored")
			}
			if st := n.Status(); st.CommitIndex != 0 || st.SnapshotIndex != 0 {
				t.Fatalf("status = %+v: the node moved although the snapshot was not stored", st)
			}
			if n.TakeRestore() != nil {
				t.Fatal("the state machine would load a snapshot that was not stored")
			}
		})
	}
}

func TestAppendThatStartsBelowOurSnapshotIsTrimmed(t *testing.T) {
	n := followerNode(t, logWithTerms(1, 1))
	if err := n.Step(snapshotMsg(2, Snapshot{Index: 10, Term: 2, Data: snapData(10)})); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	n.TakeRestore()

	// The leader still thinks we are behind index 8: entries 9 and 10 are
	// inside our snapshot, 11 and 12 are new.
	if err := n.Step(appendMsg(2, 8, 2, 12, entry(9, 2), entry(10, 2), entry(11, 2), entry(12, 2))); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Success || r.MatchIndex != 12 {
		t.Fatalf("reply = %+v", r)
	}
	if n.log.LastIndex() != 12 {
		t.Fatalf("LastIndex = %d, want 12", n.log.LastIndex())
	}
	if got, _ := n.Committed(); len(got) != 2 || got[0].Index != 11 || got[1].Index != 12 {
		t.Fatalf("Committed = %v, want entries 11 and 12", got)
	}

	// Entries that are all inside the snapshot are acknowledged and ignored.
	if err := n.Step(appendMsg(2, 3, 1, 12, entry(4, 1), entry(5, 1))); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Success || r.MatchIndex != 5 {
		t.Fatalf("reply = %+v", r)
	}
	if n.log.LastIndex() != 12 {
		t.Fatal("old entries changed the log")
	}
}

func TestSnapshotMessageWithoutASnapshotIsDropped(t *testing.T) {
	n := followerNode(t, logWithTerms(1))
	if err := n.Step(Message{Type: MsgSnapshot, From: 1, To: 2, Term: 1}); err != nil {
		t.Fatal(err)
	}
	if len(n.Messages()) != 0 || n.TakeRestore() != nil {
		t.Fatal("an empty snapshot message had an effect")
	}
}

func TestSnapshotFromANewerTermMakesUsFollowItsSender(t *testing.T) {
	n := newTestNode(t, 2, 3, &memStorage{}, nil)
	if err := n.Step(snapshotMsg(5, Snapshot{Index: 3, Term: 5, Data: snapData(3)})); err != nil {
		t.Fatal(err)
	}
	if st := n.Status(); st.Term != 5 || st.Leader != 1 || st.Role != Follower {
		t.Fatalf("status = %+v", st)
	}
}

// --- the leader side --------------------------------------------------------

func TestLeaderSendsASnapshotInsteadOfEntriesItNoLongerHas(t *testing.T) {
	n := leaderWithSnapshot(t)
	for i := 0; i < testHeartbeatTicks; i++ {
		_ = n.Tick()
	}
	msgs := n.Messages()

	// Node 3 has nothing, and the log no longer starts at index 1.
	snaps := ofType(msgs, MsgSnapshot, 3)
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshot messages for node 3, want 1: %+v", len(snaps), msgs)
	}
	if s := snaps[0].Snapshot; s == nil || s.Index != 6 || s.Term != 2 || !bytes.Equal(s.Data, snapData(6)) {
		t.Fatalf("snapshot = %+v", s)
	}
	// Node 2 is up to date and keeps getting ordinary heartbeats.
	if hb := ofType(msgs, MsgAppend, 2); len(hb) != 1 || len(hb[0].Entries) != 0 || hb[0].PrevLogIndex != 6 {
		t.Fatalf("heartbeat to node 2 = %+v", hb)
	}
}

func TestSnapshotIsNotResentWhileItIsOnItsWay(t *testing.T) {
	n := leaderWithSnapshot(t)
	for i := 0; i < testHeartbeatTicks; i++ {
		_ = n.Tick()
	}
	if len(ofType(n.Messages(), MsgSnapshot, 3)) != 1 {
		t.Fatal("setup: the first snapshot was not sent")
	}

	// During the wait the follower still receives heartbeats, so it does not
	// start an election, but not the (large) snapshot again.
	sent, heartbeats := 0, 0
	for tick := testHeartbeatTicks + 1; tick < testElectionTicks; tick++ {
		_ = n.Tick()
		msgs := n.Messages()
		sent += len(ofType(msgs, MsgSnapshot, 3))
		for _, m := range ofType(msgs, MsgAppend, 3) {
			if m.PrevLogIndex == 6 && len(m.Entries) == 0 {
				heartbeats++
			}
		}
	}
	if sent != 0 || heartbeats == 0 {
		t.Fatalf("snapshots resent %d times, heartbeats %d; want 0 and at least 1", sent, heartbeats)
	}

	// If no answer ever comes, the snapshot is sent again after the wait.
	resent := 0
	for i := 0; i < 2*testElectionTicks; i++ {
		_ = n.Tick()
		resent += len(ofType(n.Messages(), MsgSnapshot, 3))
	}
	if resent == 0 {
		t.Fatal("the snapshot was never retried")
	}
}

func TestRejectOfAHeartbeatDuringASnapshotChangesNothing(t *testing.T) {
	n := leaderWithSnapshot(t)
	for i := 0; i < testHeartbeatTicks; i++ {
		_ = n.Tick()
	}
	n.Messages()
	before := n.nextIndex[3]

	if err := n.Step(appendResp(3, 2, false, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if len(n.Messages()) != 0 || n.nextIndex[3] != before {
		t.Fatal("a reject while the follower is served from the snapshot changed the leader's plan")
	}
}

func TestAcknowledgedSnapshotResumesNormalReplication(t *testing.T) {
	n := leaderWithSnapshot(t)
	if _, _, err := n.Propose([]byte("after")); err != nil { // entry 7
		t.Fatal(err)
	}
	if len(ofType(n.Messages(), MsgSnapshot, 3)) != 1 {
		t.Fatal("setup: the snapshot was not sent")
	}

	if err := n.Step(Message{Type: MsgSnapshotResp, From: 3, To: 1, Term: 2, Success: true, MatchIndex: 6}); err != nil {
		t.Fatal(err)
	}
	next := ofType(n.Messages(), MsgAppend, 3)
	if len(next) != 1 || next[0].PrevLogIndex != 6 || next[0].PrevLogTerm != 2 || !reflect.DeepEqual(indexes(next[0].Entries), []uint64{7}) {
		t.Fatalf("after the acknowledgement the leader sent %+v, want entry 7 after prev 6", next)
	}
	if n.matchIndex[3] != 6 || n.nextIndex[3] != 7 {
		t.Fatalf("match %d, next %d; want 6 and 7", n.matchIndex[3], n.nextIndex[3])
	}
}

// --- whole clusters ---------------------------------------------------------

func TestLaggingFollowerIsBroughtUpToDateByASnapshot(t *testing.T) {
	const entries = 100
	c := newClusterWithSnapshots(t, 3, 31, 10, 0)
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
		want[i] = fmt.Sprintf("cmd-%03d", i)
		if _, _, err := c.propose(leader, want[i]); err != nil {
			t.Fatal(err)
		}
	}
	c.run(2 * testHeartbeatTicks)
	if c.logs[leader].baseIndex == 0 {
		t.Fatal("the leader never compacted its log, so this test proves nothing")
	}

	c.heal()
	if c.runUntil(20*testElectionTicks, c.converged) < 0 {
		t.Fatal("the lagging follower did not catch up")
	}
	if c.delivered[MsgSnapshot] == 0 {
		t.Fatal("the follower caught up without a snapshot although the log was compacted past it")
	}
	if got := c.appliedData(lagging); !reflect.DeepEqual(got, want) {
		t.Fatalf("lagging node applied %d commands, want %d (or the order differs)", len(got), entries)
	}
	if c.status(lagging).SnapshotIndex == 0 {
		t.Fatal("the lagging node has no snapshot")
	}
}

func TestTrailingEntriesSaveTheFollowerFromAFullSnapshot(t *testing.T) {
	// The log keeps 1000 entries below every snapshot, far more than the
	// follower misses, so plain log replication is enough.
	c := newClusterWithSnapshots(t, 3, 32, 10, 1000)
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
	for i := 0; i < 60; i++ {
		if _, _, err := c.propose(leader, fmt.Sprintf("cmd-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if c.status(leader).SnapshotIndex == 0 {
		t.Fatal("the leader took no snapshot, so this test proves nothing")
	}

	c.heal()
	if c.runUntil(20*testElectionTicks, c.converged) < 0 {
		t.Fatal("the follower did not catch up")
	}
	if c.delivered[MsgSnapshot] != 0 {
		t.Fatalf("%d snapshots were sent although the log still had every entry the follower needed", c.delivered[MsgSnapshot])
	}
	if got := len(c.appliedData(lagging)); got != 60 {
		t.Fatalf("lagging node applied %d commands, want 60", got)
	}
}

func TestEveryNodeRestartsFromItsSnapshot(t *testing.T) {
	c := newClusterWithSnapshots(t, 3, 33, 5, 0)
	if c.runUntil(10*testElectionTicks, c.settled) < 0 {
		t.Fatal("no leader")
	}
	want := make([]string, 40)
	for i := range want {
		want[i] = fmt.Sprintf("cmd-%03d", i)
		if _, ok := c.proposeToAnyLeader(want[i]); !ok {
			t.Fatal("no leader")
		}
	}
	if c.runUntil(5*testElectionTicks, c.converged) < 0 {
		t.Fatal("no convergence before the restart")
	}

	for _, id := range c.ids {
		c.crash(id)
	}
	for _, id := range c.ids {
		c.restartNode(id)
		// The state machine of a restarted node is rebuilt from its snapshot.
		if c.snapIdx[id] == 0 || len(c.applied[id]) == 0 {
			t.Fatalf("node %d restarted without loading its snapshot", id)
		}
	}
	if c.runUntil(20*testElectionTicks, c.converged) < 0 {
		t.Fatal("no convergence after the restart")
	}
	for _, id := range c.ids {
		if got := c.appliedData(id); !reflect.DeepEqual(got, want) {
			t.Fatalf("node %d holds %d commands after restarting, want %d", id, len(got), len(want))
		}
	}
}

// snapshotFaultScenario runs one random scenario of cut links, crashes,
// restarts and commands on a five-node cluster that snapshots often, and
// returns how many entries were committed and how many snapshots were sent.
func snapshotFaultScenario(t *testing.T, seed int64) (committed, snapshotsSent int) {
	every := uint64(3 + seed%6)
	trailing := uint64(seed%4) * 2
	c := newClusterWithSnapshots(t, 5, seed, every, trailing)
	rng := rand.New(rand.NewSource(seed))
	next := 0

	for step := 0; step < 120; step++ {
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
			if id := NodeID(1 + rng.Intn(5)); c.down[id] {
				c.restartNode(id)
			}
		default:
			if id := NodeID(1 + rng.Intn(5)); !c.down[id] {
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
		t.Fatalf("seed %d (snapshot every %d, trailing %d): no convergence after all faults were repaired", seed, every, trailing)
	}
	for _, id := range c.ids {
		if len(c.applied[id]) < len(c.committed) {
			t.Fatalf("seed %d: node %d holds %d entries but %d were committed at some point", seed, id, len(c.applied[id]), len(c.committed))
		}
	}
	return len(c.committed), c.delivered[MsgSnapshot]
}

func TestSnapshotsUnderRandomFaults(t *testing.T) {
	const seeds = 200
	committed, sent := 0, 0
	for seed := int64(1); seed <= seeds; seed++ {
		c, s := snapshotFaultScenario(t, seed)
		committed += c
		sent += s
	}
	if sent == 0 {
		t.Fatal("no snapshot was ever sent to a follower, so the scenarios do not exercise InstallSnapshot")
	}
	t.Logf("%d seeds, %d entries committed, %d snapshots installed on followers, no safety violation", seeds, committed, sent)
}
