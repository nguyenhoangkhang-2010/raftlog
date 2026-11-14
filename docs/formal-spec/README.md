# Formal specification

The consensus protocol of raftlog, and the rules of the customers' points, written in
[TLA+](https://lamport.azurewebsites.net/tla/tla.html) and checked with its model checker, TLC.
The reasoning is in [ADR 24](../adr/0024-specify-the-protocol-and-check-it.md); this page says what is here, what
was checked, with what result, and what the checking does **not** show.

| File | What it is |
|---|---|
| [`Raftlog.tla`](Raftlog.tla) | Raft as `internal/raft` implements it: elections, replication, commit. |
| [`Points.tla`](Points.tla) | The points of one customer: hold, spend, give back, take back, a manager's change (ADR 22 and 23). |
| `*.cfg` | One model each: the constants and the properties to check (see the tables below). |
| [`check.sh`](check.sh) | Runs them all and says whether each ended as it must. `make spec` calls it. |

## Why a specification, when there are tests
The tests (simulation, chaos) run many interleavings of messages and crashes and look at the result. TLC tries
**all** of them, within bounds, so a bug that needs a particular order is found, and not just made unlikely. And
the properties are written down as formulas, apart from the code, so that they can be read and argued with.

A specification is not a proof, and not the code: see [What this does not show](#what-this-does-not-show).

## Run it
You need Java 11 or later and `tla2tools.jar`
([releases of TLA+](https://github.com/tlaplus/tlaplus/releases)).

```bash
TLA2TOOLS=/path/to/tla2tools.jar docs/formal-spec/check.sh quick     # about a minute
TLA2TOOLS=/path/to/tla2tools.jar docs/formal-spec/check.sh           # all of them, about 20 minutes
TLC_STATES=/big/disk/tlc docs/formal-spec/check.sh                   # where TLC keeps its states (a few GB)
```
or one model by hand: `java -cp tla2tools.jar tlc2.TLC -workers auto -config Raftlog.cfg Raftlog.tla`.

## `Raftlog.tla`: what is modelled
**Modelled:** elections with terms and a vote that is on disk; replication with the consistency check of
`AppendEntries`, and the removal of a different entry in the place of the new one; commit by a majority, only for an
entry of the leader's own term; the no-op entry that a new leader appends; a crash that keeps the term, the vote and
the log and loses the rest; a network that may lose, delay, reorder and repeat any message (the messages are a set
that only grows, so a message sent can be received at any later time, any number of times, or never).

**Not modelled:** snapshots and log compaction, pre-vote and check-quorum (they make elections rarer and take nothing
safe away, ADR 10), batches of entries (the model sends one at a time), the disk (a write is whole or not made),
membership changes (there are none), time (a timeout can fire at any moment), and the broker above the log. The
implementation also **refuses to replace an entry that a node has committed** (`storeEntries`); the specification
leaves that out on purpose, so that the checks test the protocol and not that safeguard.

| Action of the specification | In `internal/raft/node.go` |
|---|---|
| `Timeout`, `RequestVote` | `campaign` |
| `HandleRequestVote`, `HandleVoteReply` | `handleRequestVote`, `handleRequestVoteResp`, `logUpToDate` |
| `BecomeLeader` (with the no-op entry) | `becomeLeader` |
| `UpdateTerm`, `ReturnToFollower` | `becomeFollower` |
| `AppendEntries`, `HandleAppendEntries`, `HandleAppendReply` | `sendAppend`, `handleAppend`, `matchesPrev`, `storeEntries`, `handleAppendResp` |
| `AdvanceCommitIndex` | `maybeCommit` |
| `Restart` | the start of a node from its hard state and log |

### The properties
| Property | Says | Why raftlog needs it |
|---|---|---|
| `ElectionSafety` | At most one leader is elected in a term. | Two leaders in a term could acknowledge different writes. |
| `LogMatching` | Two logs that have an entry with the same index and term are the same up to it. | What makes "the same index and term" a name for one entry. |
| `StateMachineSafety` | Two nodes never have different entries committed at one index. | Every node applies the same sequence to the broker. |
| `CommittedAreUnique` | No index is ever committed with two different entries, over the whole history. | The same, over time and not only at one instant. |
| `LeaderCompleteness` | A leader holds every entry committed in an earlier term. | A new leader does not lose what was acknowledged. |
| `CommittedOnAMajority` | An entry that was committed is in the log of a majority. | The sentence of the design document: an acknowledged write is on a majority of disks. |

## What was checked
Figures are from one run on a Windows laptop (Intel i7-12700H, 20 threads), 14 November 2026, with TLC 2.19 and
Java 21. They are the states TLC explored, not a measure of the cluster.

**The real protocol (must pass):**

| Model | Bounds | Result | States (distinct) | Depth | Time |
|---|---|---|---|---|---|
| `Raftlog.cfg` | 3 nodes, 2 terms, logs of 2 entries, 1 client value, 10 different messages | no error | 195,083 | 28 | 23 s |
| `Figure8.cfg` | 3 nodes, from the state of `InitFigure8`, 5 terms, logs of 2 entries, no client values, 10 messages | no error | 13,763,867 | 29 | 19 min 37 s (other jobs were using the machine) |
| `Points.cfg` | 3 orders, up to 2 points used, 2 earned, 2 given or taken by a manager, refunds on | no error | 324,352 | 16 | 6 s |
| `PointsNoRefunds.cfg` | the same, without refunds | no error | 72,463 | 13 | 4 s |

**Models with a deliberate mistake (must fail):** a model that cannot fail proves nothing, so each mistake is put in
on purpose and TLC must find it.

| Model | The mistake | Found | Depth reached |
|---|---|---|---|
| `BugVoteTwice.cfg` | a node votes for two candidates in one term | `ElectionSafety`: two leaders in a term | 14 |
| `BugNoUpToDateCheck.cfg` | a node votes for a candidate whose log is behind | `LeaderCompleteness`: a leader lacks a committed entry | 21 |
| `BugCommitOldTerms.cfg` | the leader commits an entry of an earlier term by counting replicas (figure 8 of the Raft paper) | `LeaderCompleteness`: a committed entry is overwritten | 18 |
| `BugNoAvailabilityCheck.cfg` | an order may use points that another order has set aside | `NoOverspend`: a point is spent twice | 4 |

The chaos tests ([test/chaos](../../test/chaos/README.md)) were tried against the same kinds of mistake (a leader
committing one replica short, a vote for a candidate with an older log). They caught them too; the difference is that
here the trace is the shortest one TLC can find, and the case of figure 8 is found for certain, not by chance.

### What the points model says
`Points.cfg` checks that the balance is always what was earned, less what was spent, plus what managers changed
(`BalanceIsTheSumOfItsParts`); that what is set aside is exactly what the placed orders use
(`HeldIsWhatPlacedOrdersUse`); and that a balance goes below zero, or a hold above the balance, **only after a
clawback** (`NoOverspendUnlessClawedBack`): the case ADR 22 states, a customer who spent the points of an order that is
refunded afterwards. Without refunds (`PointsNoRefunds.cfg`) `NoOverspend` holds: no point is spent twice and the
balance never goes below zero.

### A mistake that the checker found in the specification
The first version of `LeaderCompleteness` said that a leader must hold every committed entry *whose term is earlier
than the leader's*. For the real protocol TLC reported a violation: a stale leader of term 4 lacked an entry of
term 3 that a leader of term 5 had committed. The trace showed that the property was wrong, not the protocol: an
entry of term 3 can be committed in term 5, and a leader of term 4 owes only what was committed *in* an earlier term.
The history now records the term of the leader that committed (`committed`), and the property compares that. A green
check says that a property holds; it takes a person to say that it is the right one.

## What this does not show
- **Not a proof.** TLC checked every behaviour of the models above, within their bounds: two terms for the full model,
  five from the state of figure 8, logs of two entries, ten different messages. A bug that needs more is not found,
  and the number of states grows too fast for the bounds to be raised without more machine. Bounded model checking
  finds bugs; it does not say there are none.
- **Not the code.** The specification says what the protocol is. That `internal/raft` follows it is shown only by the
  table above and by the tests; a bug in the code that the specification does not describe (a batch with a wrong
  index, a lost write in the disk layer, a snapshot) is not found here.
- **The starting state of `Figure8.cfg` is written by hand.** It is a state that the protocol can reach (the comment
  in `Raftlog.tla` says how), but it was not found by the checker: the five terms that lead to it are too many to
  search from the start.
- **Safety only.** Nothing says that a leader is eventually elected, or that a write is eventually acknowledged.
- **One customer, no log.** `Points.tla` models the rules of one customer's points as steps of a single writer; that the
  log gives the service one order of all records, and that every reader applies the same rules, is the
  business of ADR 13 and 22 and of their tests.
