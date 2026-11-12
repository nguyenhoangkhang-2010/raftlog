# 12. Group commit: proposals and produce commands are made durable together

Date: 2026-11-12

## Status
Accepted

## Context
The first cluster benchmark (bench/README.md, ADR 11) showed that a write costs a fixed amount,
about 6 ms with one node and 11 ms with three on the test laptop, and that sixteen concurrent
producers did not help: the latency of a request grew from 11 to 89 ms and the throughput only
doubled. Each `Produce` was one Raft entry, and each entry paid for:

- an fsync of the Raft log, with the proposal made while the runner's lock was held;
- on every node, an application of the entry by the state machine, one entry at a time: a
  rewrite of the journal file (create a temporary file, write, fsync, rename), the appends, and
  an fsync of the partition.

A probe on the same disk, run before changing anything, split the cost: a bare write plus fsync
took 0.64 ms (median), but rewriting the journal alone took 2.5 ms of the 3.2 ms that applying a
one-message produce took. The first guess in bench/README.md, that the journal replacement cost
more than the fsyncs, was right. Nothing was shared between concurrent requests, so more
producers only added queueing.

## Decision
Two groupings, with the same rule in both: **never wait to fill a group.** Whatever is already
queued when the worker is free goes together; with nothing queued, a group is one item and
nothing is delayed.

1. **Proposals.** When nothing is queued and no append is under way, `Runner.ProposeResult`
   appends the caller's own command at once, in the caller's goroutine, as before grouping
   existed. When an append is under way, new proposals wait in a queue (guarded by its own mutex,
   because the node's main lock is held during the fsync). The proposer goroutine then takes every
   queued proposal (up to 256 or 4 MiB), appends them as consecutive entries with one fsync
   (`Node.ProposeBatch`), and sends them to the followers in one round. A caller that appended for
   itself wakes the proposer if proposals queued meanwhile. Each caller is answered with its own log
   index and its own outcome. While one group is appended, the next one builds up.
   The first version always went through the proposer goroutine. That added two goroutine
   hand-offs to every request; measured on a noisy day a lone producer was about 20% slower, and
   on a quiet day the difference was within the noise (see bench/README.md). The direct path is
   kept because it removes those hand-offs when there is nothing to group, but the measurements do
   not prove that it was needed.
2. **Applying.** A state machine that implements `BatchStateMachine` is given consecutive
   committed entries together, in groups of up to 256 entries. A group never runs past the entry
   at which a snapshot falls due, so snapshots are taken at the same indexes as before. The
   broker's machine applies consecutive Produce commands as one group:
   - one journal record for the whole group: the first and last entry, and for every partition
     it appends to, where the group's messages start and end;
   - the commands are applied in order, so offsets are what they would be one by one, and a
     refused command changes nothing, as before;
   - each touched partition is synced once, at the end.
   Create-topic and commit-offset commands, and entries that do not decode, end a group and are
   applied as before.

### Crash safety
The rule from ADR 4 is kept: write down what is about to happen before doing it, and let the
partitions say how far it got. With a group, the journal says: entries `First..Index` were begun,
and the messages of partition P of the group occupy `Start..End`. After a restart Raft hands the
log over again. Entries before `First` are skipped. The entries `First..Index` are applied again
one by one with a running offset per partition, starting at `Start`, and a message that is already
in the partition (its offset is below the partition's next offset) is not appended again. After the
last entry of the group **every partition of the group is synced**, even if nothing was appended,
because a message written before the crash may not have reached the disk. A refused command in the
middle of a group is refused again for the same reason as before, so replaying it changes nothing.
A journal written for a single Produce by the earlier version is read as a group of one. A
snapshot that replaces the state while a group is unfinished still takes effect (the pending check
looks at every partition of the group).

## Alternatives not taken
- **A timer** (wait a few milliseconds to fill a batch). It would raise throughput a little more
  and add latency to every request on a quiet cluster. Grouping what is queued costs nothing when
  idle.
- **A cheaper journal write** (overwrite a fixed-size record in place instead of replacing the
  file). It would help a single producer, and it needs a record that is safe against a torn write.
  The journal is now written once per group, so the gain is smaller; left for later.
- **Per-partition Raft groups** (ADR 4) spread the load across leaders and are a far larger change.

## Consequences
- Concurrent producers now share fsyncs, journal writes and replication rounds. Measured on one
  day on the test laptop (bench/README.md, raw results in `bench/results/`), 3 nodes, one run each,
  before against after: 16 producers sending 1 message per request 181 against 938 messages/s,
  with 10 messages 1,702 against 8,327, with 100 messages 14,500 against 65,280; the median latency
  of a request fell from 87 to 17 ms in the first case. A repeated comparison, five rounds with
  the order of the builds alternated, gave 183 (173 - 188) against 921 (893 - 965) messages/s for
  the first case. The size of the gain depends on the state of the machine: on a noisier day the
  same comparison gave 125 against 228.
- A group of large requests (100 messages of 1 KiB) is the least stable case, between 7,790 and
  19,230 messages/s in the runs made, and its p99 reached 447 ms once; a group can hold up to
  4 MiB. The group limit is a possible thing to tune; it was not tuned.
- A caller is answered when the whole group it was applied in is durable, so the first request of
  a group waits for the last. Under load that is the price of the shared fsyncs, and it is far
  smaller than the queueing it replaces.
- A single producer is unchanged: with nothing to group, the cost per request is the same.
- The apply path has more states (replaying a group after a crash). The tests cover it: the new
  and the old way of applying give the same state and the same answers on random command
  sequences; a crash at every point of a group, with the entries redelivered one by one, in
  chunks, and with a second restart before the group is finished; the old journal format; and
  deliberately introduced bugs in the new code (16 of them) were all caught. The chaos suite
  passes on the new code.
- The journal is still replaced once per group, and the Raft log is still fsynced once per
  group on every node: those costs are shared, not removed.
