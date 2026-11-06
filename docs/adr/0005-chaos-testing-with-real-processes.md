# 5. Chaos testing with real processes and a history checker

Date: 2026-11-06

## Status
Accepted

## Context
Unit tests and the simulated cluster check the protocol rules, but not the
whole system the way an operator meets it: separate processes, real sockets,
files, crashes in the middle of a write, and clients that cannot tell whether a
request was applied. The risk to retire is a claim like "no acknowledged message
is ever lost" that only holds in the tests that were written to show it.

## Decision
- **Real processes, killed for real.** The daemon is built once and each node runs
  as its own process. A crash is `Process.Kill`, so the journal, the Raft log and
  the snapshots really have to recover from a stop at an arbitrary instant.
- **The network is a set of proxies owned by the test**, one per ordered pair of
  nodes. Each node gets its own `-peers` list that points at its proxies, so a
  link can be cut or delayed in either direction independently. No external tool
  (Toxiproxy, `tc`, iptables) is needed, so the tests run the same on Windows and
  on the CI runner.
- **Clients never repeat an attempt and every attempt has a unique value.** This
  turns the final log into a record of which attempts were applied, so the check
  is exact instead of statistical. The cost is that retry-induced duplicates
  (at-least-once delivery) are deliberately not exercised.
- **A purpose-built checker instead of Porcupine.** Reads are not linearizable by
  design (followers can lag; a deposed leader can answer), so a general
  linearizability checker would report failures the system never promised to
  avoid. The checker verifies what is promised: acknowledged writes are in the log
  at their offset, no duplicates or phantoms, refused writes are absent, appends
  are linearizable in real time, reads never show data outside the final log, and
  the nodes agree. It is itself tested with injected violations.
- **The suite is validated by breaking the system** (a quorum that is one replica
  too small, replay protection removed, the vote check removed) and requiring that
  the run fails.
- Chaos runs apart from the unit tests in CI (`make chaos`), and `-short` skips it.

## Consequences
- Each of the three injected bugs was caught on the first run. One of them was
  caught as a liveness failure ("did not settle") rather than data loss, because
  followers refuse to overwrite committed entries: the code defends in depth, and
  the suite is able to see both kinds of failure.
- The suite showed a real availability cost: when a long-isolated node returns with
  a high term, the leader steps down and an election follows although nothing was
  wrong. Safety held. Pre-vote and check-quorum would fix it and are not built.
- Limits: a killed process does not lose data the operating system has buffered, so
  a missing `fsync` cannot be seen (a power-loss test would need a virtual disk);
  runs last seconds, not hours; clusters have three or five nodes.
- Runs are not deterministic (timing differs). The seed repeats the random
  choices only. A failure prints the history violations, the fault timeline and
  the tail of each node's log.
