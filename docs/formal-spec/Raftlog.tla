-------------------------------- MODULE Raftlog --------------------------------
(***************************************************************************)
(* The consensus protocol of raftlog (internal/raft), written as a TLA+     *)
(* specification, so that its safety properties can be checked by TLC over  *)
(* every interleaving of a small cluster instead of only over the           *)
(* interleavings that a test happened to try.                               *)
(*                                                                          *)
(* What is modelled: leader election with terms and a durable vote;          *)
(* replication with the consistency check of AppendEntries, truncation of a  *)
(* conflicting suffix, commit by a majority of the current term, a no-op     *)
(* entry that a new leader appends; a crash that keeps the term, the vote   *)
(* and the log and loses everything else; a network that may lose, delay,   *)
(* reorder and repeat any message.                                           *)
(*                                                                          *)
(* What is not: snapshots and log compaction, pre-vote and check-quorum     *)
(* (they only make elections rarer, ADR 10, and do not take away a safe      *)
(* behaviour), membership changes (there are none), timing (a timeout can    *)
(* fire at any moment), the broker above the log, and the disk (a write is   *)
(* either whole or not made).                                               *)
(*                                                                          *)
(* The structure follows the specification of Diego Ongaro's dissertation,   *)
(* with the messages kept as a set that only grows, as in Lamport's Paxos    *)
(* specifications: a message that was sent can be received at any later     *)
(* time, any number of times, or never, which is the whole of what an        *)
(* asynchronous network may do.                                              *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS Server,      \* the nodes of the cluster
          Value,       \* the values that clients may write
          Nil          \* "no vote", and the value of the no-op entry

CONSTANTS MaxTerm,     \* bounds, only to keep the checked space finite:
          MaxLogLen,   \*   the highest term and the longest log of any node,
          MaxMessages  \*   and how many different messages may have been sent in all

\* The mistakes that the checked properties must catch. Each is FALSE in the real model, and TRUE in a model
\* that is expected to fail (see the .cfg files).
CONSTANTS BugVoteTwice,          \* a node votes for a second candidate in the same term
          BugNoUpToDateCheck,    \* a node votes for a candidate whose log is behind its own
          BugCommitOldTerms      \* the leader counts replicas of an entry of an earlier term to commit it

VARIABLES
    currentTerm, state, votedFor,        \* of each node; the first and the last are on disk
    log,                                  \* of each node, on disk: a sequence of [term, value]
    commitIndex,                          \* of each node, in memory
    votesGranted, nextIndex, matchIndex,  \* of a candidate or a leader, in memory
    messages,                             \* every message ever sent
    leaders,                              \* history: the pairs <<term, node>> of every election won
    committed                             \* history: <<index, term, value, term of the leader that committed it>> of every commit

serverVars == <<currentTerm, state, votedFor>>
leaderVars == <<votesGranted, nextIndex, matchIndex>>
vars == <<serverVars, log, commitIndex, leaderVars, messages, leaders, committed>>

Follower == "follower"
Candidate == "candidate"
Leader == "leader"

Quorums == {Q \in SUBSET Server : Cardinality(Q) * 2 > Cardinality(Server)}

\* ---- helpers ------------------------------------------------------------------------------------------------

Min(a, b) == IF a < b THEN a ELSE b
Max(a, b) == IF a > b THEN a ELSE b

LastIndex(l) == Len(l)
LastTerm(l) == IF Len(l) = 0 THEN 0 ELSE l[Len(l)].term

\* The terms of a log are the terms of its entries: a candidate's log is "at least as up to date" as another by
\* the rule of the Raft paper, section 5.4.1.
UpToDate(lastTerm, lastIndex, l) ==
    \/ lastTerm > LastTerm(l)
    \/ /\ lastTerm = LastTerm(l)
       /\ lastIndex >= LastIndex(l)

Send(m) == messages' = messages \cup {m}

\* ---- initial state ------------------------------------------------------------------------------------------

Init ==
    /\ currentTerm = [i \in Server |-> 1]
    /\ state = [i \in Server |-> Follower]
    /\ votedFor = [i \in Server |-> Nil]
    /\ log = [i \in Server |-> <<>>]
    /\ commitIndex = [i \in Server |-> 0]
    /\ votesGranted = [i \in Server |-> {}]
    /\ nextIndex = [i \in Server |-> [j \in Server |-> 1]]
    /\ matchIndex = [i \in Server |-> [j \in Server |-> 0]]
    /\ messages = {}
    /\ leaders = {}
    /\ committed = {}

\* ---- a node crashes and starts again: the term, the vote and the log are on disk, the rest is lost -----------

Restart(i) ==
    /\ state' = [state EXCEPT ![i] = Follower]
    /\ votesGranted' = [votesGranted EXCEPT ![i] = {}]
    /\ nextIndex' = [nextIndex EXCEPT ![i] = [j \in Server |-> 1]]
    /\ matchIndex' = [matchIndex EXCEPT ![i] = [j \in Server |-> 0]]
    /\ commitIndex' = [commitIndex EXCEPT ![i] = 0]
    /\ UNCHANGED <<currentTerm, votedFor, log, messages, leaders, committed>>

\* ---- elections ----------------------------------------------------------------------------------------------

\* A node whose timer ran out becomes a candidate of the next term and votes for itself.
Timeout(i) ==
    /\ state[i] \in {Follower, Candidate}
    /\ currentTerm[i] < MaxTerm
    /\ state' = [state EXCEPT ![i] = Candidate]
    /\ currentTerm' = [currentTerm EXCEPT ![i] = currentTerm[i] + 1]
    /\ votedFor' = [votedFor EXCEPT ![i] = i]
    /\ votesGranted' = [votesGranted EXCEPT ![i] = {i}]
    /\ UNCHANGED <<log, commitIndex, nextIndex, matchIndex, messages, leaders, committed>>

RequestVote(i, j) ==
    /\ state[i] = Candidate
    /\ i /= j
    /\ Send([type |-> "RequestVote", term |-> currentTerm[i], lastTerm |-> LastTerm(log[i]),
             lastIndex |-> LastIndex(log[i]), source |-> i, dest |-> j])
    /\ UNCHANGED <<serverVars, log, commitIndex, leaderVars, leaders, committed>>

HandleRequestVote(m) ==
    LET i == m.dest
        grant == /\ m.term = currentTerm[i]
                 /\ \/ BugNoUpToDateCheck
                    \/ UpToDate(m.lastTerm, m.lastIndex, log[i])
                 /\ \/ BugVoteTwice
                    \/ votedFor[i] \in {Nil, m.source}
    IN /\ m \in messages
       /\ m.type = "RequestVote"
       /\ m.term <= currentTerm[i]
       /\ votedFor' = [votedFor EXCEPT ![i] = IF grant THEN m.source ELSE votedFor[i]]
       /\ Send([type |-> "VoteReply", term |-> currentTerm[i], granted |-> grant, source |-> i, dest |-> m.source])
       /\ UNCHANGED <<currentTerm, state, log, commitIndex, leaderVars, leaders, committed>>

HandleVoteReply(m) ==
    LET i == m.dest
    IN /\ m \in messages
       /\ m.type = "VoteReply"
       /\ m.term = currentTerm[i]
       /\ state[i] = Candidate
       /\ votesGranted' = [votesGranted EXCEPT ![i] = IF m.granted THEN votesGranted[i] \cup {m.source} ELSE votesGranted[i]]
       /\ UNCHANGED <<serverVars, log, commitIndex, nextIndex, matchIndex, messages, leaders, committed>>

\* A candidate with the votes of a majority becomes the leader of its term and appends a no-op entry of that term:
\* once the no-op is committed, so is everything before it.
BecomeLeader(i) ==
    /\ state[i] = Candidate
    /\ votesGranted[i] \in Quorums
    /\ state' = [state EXCEPT ![i] = Leader]
    /\ log' = [log EXCEPT ![i] = Append(log[i], [term |-> currentTerm[i], value |-> Nil])]
    /\ nextIndex' = [nextIndex EXCEPT ![i] = [j \in Server |-> Len(log[i]) + 1]]
    /\ matchIndex' = [matchIndex EXCEPT ![i] = [j \in Server |-> 0]]
    /\ leaders' = leaders \cup {<<currentTerm[i], i>>}
    /\ UNCHANGED <<currentTerm, votedFor, commitIndex, votesGranted, messages, committed>>

\* ---- a node sees a later term: it follows it ---------------------------------------------------------------

UpdateTerm(m) ==
    LET i == m.dest
    IN /\ m \in messages
       /\ m.term > currentTerm[i]
       /\ currentTerm' = [currentTerm EXCEPT ![i] = m.term]
       /\ state' = [state EXCEPT ![i] = Follower]
       /\ votedFor' = [votedFor EXCEPT ![i] = Nil]
       /\ UNCHANGED <<log, commitIndex, leaderVars, messages, leaders, committed>>

\* ---- replication --------------------------------------------------------------------------------------------

ClientRequest(i, v) ==
    /\ state[i] = Leader
    /\ log' = [log EXCEPT ![i] = Append(log[i], [term |-> currentTerm[i], value |-> v])]
    /\ UNCHANGED <<serverVars, commitIndex, leaderVars, messages, leaders, committed>>

AppendEntries(i, j) ==
    /\ state[i] = Leader
    /\ i /= j
    /\ LET prev == nextIndex[i][j] - 1
           prevTerm == IF prev = 0 THEN 0 ELSE log[i][prev].term
           entries == IF LastIndex(log[i]) >= nextIndex[i][j] THEN <<log[i][nextIndex[i][j]]>> ELSE <<>>
       IN Send([type |-> "AppendEntries", term |-> currentTerm[i], prev |-> prev, prevTerm |-> prevTerm,
                entries |-> entries, commit |-> Min(commitIndex[i], prev + Len(entries)), source |-> i, dest |-> j])
    /\ UNCHANGED <<serverVars, log, commitIndex, leaderVars, leaders, committed>>

\* The log of a node agrees with the leader at index prev.
LogOk(i, m) == \/ m.prev = 0
               \/ /\ m.prev >= 1
                  /\ m.prev <= Len(log[i])
                  /\ log[i][m.prev].term = m.prevTerm

\* A node of the same term that was a candidate sees a leader and goes back to being a follower.
ReturnToFollower(m) ==
    LET i == m.dest
    IN /\ m \in messages
       /\ m.type = "AppendEntries"
       /\ m.term = currentTerm[i]
       /\ state[i] = Candidate
       /\ state' = [state EXCEPT ![i] = Follower]
       /\ UNCHANGED <<currentTerm, votedFor, log, commitIndex, leaderVars, messages, leaders, committed>>

HandleAppendEntries(m) ==
    LET i == m.dest
    IN /\ m \in messages
       /\ m.type = "AppendEntries"
       /\ m.term <= currentTerm[i]
       /\ \/ \* too old, or the logs do not agree at prev: refuse
             /\ \/ m.term < currentTerm[i]
                \/ /\ m.term = currentTerm[i]
                   /\ state[i] = Follower
                   /\ ~LogOk(i, m)
             /\ Send([type |-> "AppendReply", term |-> currentTerm[i], success |-> FALSE, match |-> 0,
                      source |-> i, dest |-> m.source])
             /\ UNCHANGED <<serverVars, log, commitIndex, leaderVars, leaders, committed>>
          \/ \* the logs agree: store the entry, throwing away a different one that is in its place
             /\ m.term = currentTerm[i]
             /\ state[i] = Follower
             /\ LogOk(i, m)
             /\ LET at == m.prev + 1
                    newLog ==
                        IF m.entries = <<>> THEN log[i]
                        ELSE IF Len(log[i]) >= at /\ log[i][at].term = m.entries[1].term THEN log[i]
                        ELSE Append(SubSeq(log[i], 1, m.prev), m.entries[1])
                IN /\ log' = [log EXCEPT ![i] = newLog]
                   /\ commitIndex' = [commitIndex EXCEPT ![i] = Max(commitIndex[i], Min(m.commit, Len(newLog)))]
             /\ Send([type |-> "AppendReply", term |-> currentTerm[i], success |-> TRUE, match |-> m.prev + Len(m.entries),
                      source |-> i, dest |-> m.source])
             /\ UNCHANGED <<serverVars, leaderVars, leaders, committed>>

HandleAppendReply(m) ==
    LET i == m.dest
        j == m.source
    IN /\ m \in messages
       /\ m.type = "AppendReply"
       /\ m.term = currentTerm[i]
       /\ state[i] = Leader
       /\ IF m.success
          THEN /\ nextIndex' = [nextIndex EXCEPT ![i][j] = m.match + 1]
               /\ matchIndex' = [matchIndex EXCEPT ![i][j] = m.match]
          ELSE /\ nextIndex' = [nextIndex EXCEPT ![i][j] = Max(nextIndex[i][j] - 1, 1)]
               /\ UNCHANGED matchIndex
       /\ UNCHANGED <<serverVars, log, commitIndex, votesGranted, messages, leaders, committed>>

\* The leader commits the highest index that is on a majority, if the entry is of its own term (the rule of
\* section 5.4.2 of the paper; BugCommitOldTerms removes it).
AdvanceCommitIndex(i) ==
    /\ state[i] = Leader
    /\ LET agree(k) == {i} \cup {j \in Server : matchIndex[i][j] >= k}
           ok == {k \in 1..Len(log[i]) : /\ agree(k) \in Quorums
                                         /\ \/ BugCommitOldTerms
                                            \/ log[i][k].term = currentTerm[i]}
           new == IF ok = {} THEN commitIndex[i] ELSE Max(commitIndex[i], CHOOSE k \in ok : \A l \in ok : l <= k)
       IN /\ new > commitIndex[i]
          /\ commitIndex' = [commitIndex EXCEPT ![i] = new]
          /\ committed' = committed \cup {<<k, log[i][k].term, log[i][k].value, currentTerm[i]>> : k \in (commitIndex[i] + 1)..new}
    /\ UNCHANGED <<serverVars, log, leaderVars, messages, leaders>>

\* ---- a starting point for the case of figure 8 of the Raft paper ------------------------------------------------

\* The case that the rule of section 5.4.2 exists for needs five terms, which is more than the whole space can be
\* searched to in reasonable time from Init. This is a state that the protocol can reach by the steps described
\* below (it is written down by hand, not found by the checker): from it the case is a few steps away, so the
\* checker can search all of them, with and without the mistake.
\*
\* s1 won term 2 and appended its no-op entry, and stopped before it sent it. s3 won term 3 with the vote of s2 and
\* appended its own no-op, and stopped. Then s1, restarted, won term 4 with the vote of s2 and appended its no-op:
\* s1 leads, its log is the entries of terms 2 and 4, s2's log is empty, s3's log is the entry of term 3, and
\* nothing is committed. From here the leader sends the entry of term 2 to s2, which makes a majority hold it, and
\* the rule decides whether that is enough to commit it.
InitFigure8 ==
    LET s == CHOOSE f \in [1..3 -> Server] : \A x, y \in 1..3 : x /= y => f[x] /= f[y]
        entry(t) == [term |-> t, value |-> Nil]
    IN /\ currentTerm = [i \in Server |-> IF i = s[3] THEN 3 ELSE 4]
       /\ state = [i \in Server |-> IF i = s[1] THEN Leader ELSE Follower]
       /\ votedFor = [i \in Server |-> IF i = s[3] THEN s[3] ELSE s[1]]
       /\ log = [i \in Server |-> IF i = s[1] THEN <<entry(2), entry(4)>> ELSE IF i = s[3] THEN <<entry(3)>> ELSE <<>>]
       /\ commitIndex = [i \in Server |-> 0]
       /\ votesGranted = [i \in Server |-> IF i = s[1] THEN {s[1], s[2]} ELSE {}]
       /\ nextIndex = [i \in Server |-> [j \in Server |-> 2]]
       /\ matchIndex = [i \in Server |-> [j \in Server |-> 0]]
       /\ messages = {}
       /\ leaders = {<<2, s[1]>>, <<3, s[3]>>, <<4, s[1]>>}
       /\ committed = {}

\* ---- the specification ---------------------------------------------------------------------------------------

Next ==
    \/ \E i \in Server : Restart(i) \/ Timeout(i) \/ BecomeLeader(i) \/ AdvanceCommitIndex(i)
    \/ \E i, j \in Server : RequestVote(i, j) \/ AppendEntries(i, j)
    \/ \E i \in Server, v \in Value : ClientRequest(i, v)
    \/ \E m \in messages : \/ HandleRequestVote(m) \/ HandleVoteReply(m) \/ UpdateTerm(m)
                           \/ ReturnToFollower(m) \/ HandleAppendEntries(m) \/ HandleAppendReply(m)

Spec == Init /\ [][Next]_vars

\* ---- what is checked -----------------------------------------------------------------------------------------

TypeOK ==
    /\ currentTerm \in [Server -> 1..MaxTerm]
    /\ state \in [Server -> {Follower, Candidate, Leader}]
    /\ votedFor \in [Server -> Server \cup {Nil}]
    /\ \A i \in Server : Len(log[i]) <= MaxLogLen + 1
    /\ \A i \in Server : commitIndex[i] <= Len(log[i])

\* At most one leader is ever elected in a term.
ElectionSafety ==
    \A a, b \in leaders : a[1] = b[1] => a[2] = b[2]

\* If two logs have an entry with the same index and term, they are the same up to that index.
LogMatching ==
    \A i, j \in Server :
        \A k \in 1..Min(Len(log[i]), Len(log[j])) :
            log[i][k].term = log[j][k].term => SubSeq(log[i], 1, k) = SubSeq(log[j], 1, k)

\* The same entry is committed at an index by every node that has committed it.
StateMachineSafety ==
    \A i, j \in Server :
        \A k \in 1..Min(commitIndex[i], commitIndex[j]) : log[i][k] = log[j][k]

\* An index is never committed with two different entries, over the whole history (the same entry may be committed
\* again by a later leader, whose commit index starts from nothing).
CommittedAreUnique ==
    \A a, b \in committed : a[1] = b[1] => /\ a[2] = b[2] /\ a[3] = b[3]

\* Every leader holds every entry that was committed in an earlier term (the term of the leader that committed
\* it, not the term of the entry: a leader may commit an old entry, and a later leader must still have it).
LeaderCompleteness ==
    \A i \in Server :
        state[i] = Leader =>
            \A e \in committed :
                e[4] < currentTerm[i] =>
                    /\ Len(log[i]) >= e[1]
                    /\ log[i][e[1]].term = e[2]
                    /\ log[i][e[1]].value = e[3]

\* An entry that was committed is on the disks of a majority: what was acknowledged is not lost with one node.
CommittedOnAMajority ==
    \A e \in committed :
        \E Q \in Quorums : \A s \in Q : /\ Len(log[s]) >= e[1]
                                       /\ log[s][e[1]].term = e[2]
                                       /\ log[s][e[1]].value = e[3]

\* ---- for the model checker ------------------------------------------------------------------------------------

Bounded == /\ \A i \in Server : currentTerm[i] <= MaxTerm /\ Len(log[i]) <= MaxLogLen
           /\ Cardinality(messages) <= MaxMessages

SymmetricServers == Permutations(Server)

=============================================================================
