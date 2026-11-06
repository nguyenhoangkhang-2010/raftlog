# 3. Snapshots and log compaction

Date: 2026-11-06

## Status
Accepted

## Context
Without snapshots the Raft log grows forever, a restarted node replays every
entry ever committed, and a follower that was down for a long time has to be fed
the whole history. The log can be cut only if the state it produced is saved
somewhere else.

## Decision
- **The state machine owns the snapshot.** Raft does not know what the state
  looks like. The application implements `StateMachine` (`Apply`, `Snapshot`,
  `Restore`) and `Runner` calls `Snapshot` every `WithSnapshotEvery(n)` applied
  entries, then `Node.Compact(index, data)`.
- **Snapshot first, log second.** `Compact` saves the snapshot durably and only
  then cuts the log. A crash in between leaves a log that is longer than needed,
  never one that lost data. On startup the node reconciles the two: a snapshot
  ahead of the log replaces it; a log compacted past its snapshot, a compacted
  log with no snapshot, or a snapshot that disagrees with the log is a hard error.
- **The log remembers its base.** After compaction only the index and term of
  the last removed entry are kept, because the consistency check of the next
  `AppendEntries` needs the term of the entry just before it.
- **Trailing entries.** `SnapshotTrailing` keeps some entries below the snapshot
  so that a follower that is only a little behind is served from the log and
  does not receive the whole state.
- **InstallSnapshot is one message.** A leader sends the snapshot to a follower
  whose next index was compacted away. It then waits an election timeout before
  sending it again, and meanwhile sends plain heartbeats so the follower does
  not start an election while it installs. A follower that already has
  everything the snapshot covers just acknowledges it. Otherwise it saves the
  snapshot, keeps the log suffix if the log agrees with it (else drops the whole
  log), moves commit and applied to the snapshot index, and tells the state
  machine to `Restore` before it applies anything else.
- **Files are replaced atomically.** `FileLog.CompactTo` writes a complete new
  file (a base record plus the entries kept), fsyncs it and renames it over the
  old one. `FileSnapshotStore.Save` does the same. A damaged snapshot file is an
  error, never "no snapshot", because the log may already be cut down to it.

## Consequences
- Memory and disk stay bounded by the state size plus the log since the last
  snapshot; a restart loads the snapshot and replays only what came after.
- Tests found and now guard the sharp edges: a reject answered while the
  follower is served from a snapshot must not make the leader back up; an
  append that starts below a follower's snapshot must be trimmed, not
  rejected; a snapshot must not be re-sent on every heartbeat.
- Limitations, on purpose, for now:
  - the snapshot travels in a single message, so its size is bounded by
    `-max-message-bytes` (default 64 MiB); chunked transfer is future work;
  - the latest snapshot is also kept in memory so it can be sent quickly;
  - `Compact` writes the snapshot and rewrites the log while holding the node's
    lock, which pauses the protocol for that time; writing the snapshot outside
    the lock would shorten it;
  - `raftlogd` has no state machine yet, so it never takes a snapshot itself
    (it can store and serve one); the broker becomes the state machine in the
    next branch.
