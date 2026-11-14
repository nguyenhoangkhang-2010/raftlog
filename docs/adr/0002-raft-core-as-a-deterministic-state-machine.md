# 2. Raft core as a deterministic state machine

Date: 2026-11-06

## Status
Accepted

## Context
Raft is easy to get subtly wrong and hard to debug when it depends on real
clocks, goroutines and sockets. We want to test election and replication
exhaustively, including crashes, partitions and reordering, and to be able to
reproduce any failure from a seed.

## Decision
`raft.Node` is a pure state machine with no I/O except its `Storage` and
`LogStore`, and no clock:

- time advances only through `Tick()`;
- messages come in through `Step(m)` and go out through `Messages()`;
- committed entries are handed over through `Committed()`;
- RPCs are modelled as one-way messages (a reply is another message).

`Runner` adds the real world around it: a ticker, per-peer sender goroutines
that never block the protocol, and a separate apply goroutine. `GRPCTransport`
carries the messages.

The test harness (`helpers_test.go`) simulates a network of nodes on logical
ticks, with cut links, crashes and restarts, and checks election safety, log
matching and state machine safety after every delivered message.

## Replication decisions
- Followers persist entries (fsync) before acknowledging; a node never sends a
  message that depends on state it has not made durable.
- A new leader appends a no-op of its own term. A leader only counts replicas
  for entries of its own term (section 5.4.2), so earlier entries are committed
  together with the no-op.
- On a conflict the follower answers with a hint (the start of its conflicting
  term) so the leader backs up by term, not by one entry.
- A reject that would not change what the leader sends is not retried at once;
  the next heartbeat retries. Retrying immediately turned a follower with a
  shorter log than expected into an endless message loop (found by the random
  fault test).
- `FileLog` drops a damaged record only at the very end of the file (a torn
  write, never acknowledged). Damage anywhere else is an error, because
  acknowledged entries would be lost.

## Consequences
- Tests are fast and reproducible (300 random fault scenarios in about a second)
  and found real bugs; the cost is a message-passing style instead of direct RPC.
- Mutation checks (breaking one rule at a time) are caught by the tests, with one
  caveat: the "only commit entries of the current term" rule is caught by a
  targeted unit test, not by the random test, which does not reproduce Figure 8.
- Not done yet: snapshots and log compaction (the `FileLog` keeps all entries in
  memory), membership changes, encrypted transport. (Snapshots, pre-vote and
  check-quorum have been added since: see ADRs 3 and 10.)
- After a restart `Committed()` replays the log from index 1, so a durable state
  machine must remember which index it has applied.
