# 10. Pre-vote and check-quorum

Date: 2026-11-08

## Status
Accepted

## Context
The chaos suite had already shown, and the cluster console later made visible, that a node
which cannot hear the leader can hurt a healthy cluster. Such a node (restarted, or cut off by
a broken link) times out, raises its term and asks for votes. Its term is higher than the
leader's, so everyone who hears it adopts the term, the leader steps down, and an election
follows, though nothing was wrong with the cluster. Safety holds, because the cut-off node's
log is old and it cannot win; availability drops. On real processes, a node isolated for six
seconds had raised its own term from 1 to 43, and when it came back the cluster, which had
been in term 1, ended in term 49 after a stream of elections.

The other half of the problem is the opposite one: a leader that has been cut off from the
majority keeps believing it leads, and keeps accepting proposals that can never commit,
until it happens to hear a higher term.

## Decision
- **Pre-vote.** A follower whose election timer runs out no longer starts an election at
  once. It stays in its own term and asks the others whether they would vote for it in the term
  it would start. Answering changes nothing in the answerer: no term, no vote, no timer. A
  node says yes only if the asker's term is newer than its own, the asker's log is at least as
  up to date as its own, and it has not heard from a leader for a whole election timeout. Only a
  majority of yeses turns the node into a candidate, and the election then proceeds as before.
  A refusal carries the refuser's own term, so an asker that was really out of date learns it.
- **A lease on the leader.** For the same reason, a real vote request from a higher term is
  ignored, and its term not adopted, by a node that has heard from a leader within the base
  election timeout (section 4.2.3 of the Raft thesis). With pre-vote this should not arise,
  since a pre-vote would have been refused; it protects against an asker that skipped one.
- **Check-quorum.** A leader counts the followers it has heard from. If, in a whole election
  timeout, they and the leader itself are not a majority, it steps down, keeping its term.
  Any answer in the current term counts, an acceptance or a refusal alike, since both show
  the follower is in touch. A leader in contact with a majority is in the lease and refuses
  pre-votes.
- **Two settings, off in the library and on in the daemon.** `Config.PreVote` and
  `Config.CheckQuorum` default to false, so the whole existing test suite still describes the
  unchanged protocol. `raftlogd` turns both on (`-pre-vote`, `-check-quorum`, or
  `RAFTLOG_PRE_VOTE` and `RAFTLOG_CHECK_QUORUM`).
- **The pre-vote state is not a new role.** A node that is asking is still a follower, so the
  role shown by `NodeStatus`, the metrics and the console is unchanged. Two counters are new:
  pre-vote rounds, and step-downs for lack of a quorum.
- **Two new wire messages**, `PRE_VOTE` and `PRE_VOTE_RESP`.

## Consequences
- **All nodes of a cluster must use the same `-pre-vote` setting.** A node without it does not
  understand the messages, so in a cluster with both kinds a node asking for pre-votes would be
  refused silently by the others and could not win. There is no rolling upgrade path; a cluster
  is restarted with the new setting on every node.
- **Elections take as long as before in the simulation.** Over 100 seeds with five nodes, the
  time from a leader crashing to a settled new one was a median of 11 ticks and a p99 of 23
  without the hardening, and 11 and 24 with it. On real processes the same holds for the
  chaos scenarios.
- **The cluster does not move when a node comes back.** A node cut off for fifteen election
  timeouts keeps the cluster's term; when it returns the leader and the term are unchanged,
  and the node catches up. A cut-off leader stops leading within a few election timeouts, on
  the same real processes in about half a second.
- **A leader can step down although it was fine**, if answers are delayed beyond an election
  timeout (a long pause of the process, a saturated network). It keeps its term and the cluster
  elects again; nothing is lost. The cost is a short unavailability, and it is the cost of
  not holding on to a leadership nobody can see.
- **Safety is unchanged.** The new rules only ever make a node less willing to start or
  support an election, never more. They were run under the random-fault tests (300 seeds of
  replication with partitions and crashes, 60 of election safety) with both options on, which
  check at every message that there is at most one leader per term, that logs match and that
  no two nodes apply different entries.
- **Not done:** leadership transfer, membership changes, and a real-time lease for reads (reads
  are still not linearizable).
