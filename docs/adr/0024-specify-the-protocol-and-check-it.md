# 24. Specify the protocol in TLA+ and check it with a model checker

Date: 2026-11-14

## Status
Accepted

## Context
The Raft implementation is tested three ways (ADR 2 and 5): unit tests, a simulation that injects random faults,
and chaos tests on real processes. All three **sample** behaviours: they run a lot of interleavings, and a bug that
needs a particular one of them (five terms and four crashes, as in figure 8 of the Raft paper) can stay hidden for
as long as the dice do not roll it. A second weakness is that a test that passes may pass because the property it
checks is the wrong one, and nothing says so.

A specification in TLA+, checked by TLC, tries **every** interleaving of a small cluster, and the properties are
written as formulas that say what the protocol promises, apart from the code.

## Decision
- **Write the protocol as a TLA+ specification** (`docs/formal-spec/Raftlog.tla`): elections with terms and a durable
  vote, replication with the consistency check, commit by a majority of the current term, the no-op entry of a new
  leader, crashes that keep the term, the vote and the log, and a network that may lose, delay, reorder and repeat
  any message (the messages are a set that only grows).
- **Check the properties that the broker's promise rests on:** one leader per term, log matching, no two nodes
  committing different entries at an index, a leader holding every entry committed in an earlier term, and every
  committed entry on the disks of a majority. The last is the sentence of the design document "an acknowledged
  write is on a majority of disks".
- **A model that cannot fail proves nothing, so each property is also tried against a deliberate mistake:** voting
  twice in a term, voting for a candidate with an older log, and committing entries of earlier terms by counting
  replicas (the case of figure 8). TLC must reject each of them, with a trace. The same is done for the rules of
  points (ADR 22 and 23), in a second, small specification (`Points.tla`).
- **Bound the models and say so.** The checks are exhaustive **within bounds** (nodes, terms, the length of a log, the
  number of different messages sent), chosen so that a run ends in minutes. The figure 8 case, which needs five
  terms, is checked from a state that the protocol can reach, written down by hand, with and without the mistake.
- **Leave out what the specification does not need**, and keep what it does not model apart from what it does:
  snapshots, pre-vote and check-quorum, batches of entries, the disk, the broker above the log. The implementation has
  a safeguard that the specification leaves out on purpose: a node that is asked to replace an entry that it has
  committed returns an error. Without it, the checks test the protocol and not that safeguard.
- **Run it in CI**, the quick part (`make spec`, `check.sh quick`), so that the specification and its checks do not
  rot when the protocol changes.

## Consequences
- **A bug found by reading, not by running.** The first version of `LeaderCompleteness` compared the term of the
  *entry* with the term of the leader. TLC reported a violation in the real protocol: a stale leader of term 4
  lacked an entry of term 3 that a leader of term 5 had committed. Reading the trace showed that the property was
  wrong, not the protocol: a leader of term 4 owes only what was committed *in* terms before 4. The property now
  records the term of the leader that committed. This is what a specification is for, and also a reminder that a
  green check says only that the property holds, not that it is the right property.
- **A specification is not a proof, and is not the code.** It shows that within small bounds the *rules* are right;
  it does not show that `internal/raft` follows them. The two are connected only by a table in
  `docs/formal-spec/README.md` that says which action stands for which function, and by the tests. A bug in the code
  that the specification does not describe (a lost write in the disk layer, a wrong index in a batch) is not found
  here.
- **The bounds are small**: two terms for the full model, five for figure 8 from its starting state, logs of two
  entries, ten different messages. A bug that needs more is not found, and the number of states grows quickly with
  each bound, so the bounds cannot simply be raised.
- **Learning TLA+ is a cost for the next reader**; the README next to the specification is written to be read first.
- **Not done:** a refinement from the code (checking that the real node implements the specification, by testing its
  traces against it), snapshots and membership changes in the model, liveness (that a leader is eventually elected).
