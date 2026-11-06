# 4. The broker as a state machine on one Raft group

Date: 2026-11-06

## Status
Accepted (first version; see the limits below)

## Context
Election, replication and snapshots exist, but the broker still wrote only to
the node that received the request. To survive the loss of a node, every change
has to go through the replicated log.

## Decision
**One Raft group for the whole cluster; the broker is its state machine.**
Topic creation, produce and consumer offset commits are commands in the log.
Only the leader accepts them. A request returns after a majority stored the
command and this node applied it.

Per-partition Raft groups would scale better (each partition gets its own
leader) but need many logs, a way to move snapshots in chunks and a placement
scheme. One group is much simpler and keeps the whole protocol explainable; its
cost is that every write passes through a single leader.

### Applying must be deterministic
All nodes apply the same commands in the same order, so applying a command may
depend only on the command and on the state built by earlier ones.
- The **leader** chooses the partition (key hash or round robin) and stamps
  messages with the time before replicating. The state machine refuses a produce
  without timestamps: otherwise each node would take its own clock when applying
  and store a different timestamp for the same message (found by a mutation test).
- Offsets follow from the order. A command the broker refuses (unknown topic, bad
  partition, oversized message, offset beyond the end) is refused alike on every
  node and changes nothing; its outcome is returned to the proposer.
- A batch is applied whole or not at all.

### The state machine is durable, and Raft replays the log
After a restart Raft hands the log to the state machine again from the start (or
from the snapshot). Without care every message would be stored twice.
- A small **journal** (`applied.json`) records the last entry applied. Entries at
  or below it are skipped after a restart.
- A produce writes its **intent** (entry, partition, start offset, count) to the
  journal *before* appending, and syncs the partition afterwards. A crash in the
  middle is finished when Raft redelivers the entry: the partition's next offset
  says how many messages are already there. Topic creation and offset commits
  are idempotent, so they record the entry *after* doing the work.
- An I/O failure while applying stops the process (panic). The state machine
  cannot skip an entry, and the journal makes a restart finish what was begun.
- `Restore` replaces everything with a snapshot, *unless* the node is already at
  or past it and has no unfinished produce. The second condition matters: a
  snapshot covers entries Raft will never redeliver, so an unfinished produce
  could not be completed any other way (covered by a test).

### Snapshots are a dump of all broker data
`Snapshot` serializes every topic, message and offset to protobuf; `Restore`
wipes and reloads. That is what lets a node that fell behind the compacted log
catch up. It is simple, and it limits the data set: the snapshot is held in
memory and travels in one message, bounded by `-max-message-bytes`.

### Reads and clients
- Writes go to the leader. A follower answers `FailedPrecondition` and names the
  leader (id and address) in the response trailers.
- Reads are served by any node from its applied state, so they see only
  committed data, but a follower may lag, and a leader that was just deposed may
  not know yet (reads do not go through the log).
- `raftlogctl -addr a,b,c` follows the hint, skips nodes that are down, and keeps
  trying until `-timeout`, because after a leader dies the others keep naming it
  until the election ends (a real run showed 4 of 10 writes failing with a fixed
  number of attempts).
- Delivery is at-least-once: a request that fails after it was sent may be
  retried, and the message can be stored twice. There is no producer id yet.

## Consequences
- A three-node cluster keeps working and keeps every acknowledged message when one
  node is lost, and a restarted node rejoins with its data. This is covered
  by tests that run real nodes over gRPC (write, kill the leader, restart, restart
  everything, rebuild from a snapshot).
- Costs and limits, for now:
  - one leader handles all writes; a produce costs an fsync in the Raft log, one
    for the journal and one for the partition;
  - `-max-partitions` (and the other broker settings that change what a command
    does) must be the same on every node, or nodes would disagree;
  - directories are not fsynced, so a power failure right after a topic is created
    could lose that topic on one node until a snapshot restores it;
  - no linearizable reads, no idempotent producer, no membership changes, and the
    node-to-node traffic is unencrypted.
