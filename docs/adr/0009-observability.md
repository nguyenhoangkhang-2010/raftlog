# 9. Observability: our own metrics, and a console that checks the promise

Date: 2026-11-08

## Status
Accepted

## Context
A replicated log claims things: one leader, committed entries are never lost, a returning
node catches up. A person or a monitoring system must be able to see whether the claims
hold while the system runs, and a demonstration must be able to show a failover without
reading logs.

## Decision
- **Metrics in the Prometheus text format, from a small package of our own.** The project
  has no dependency on a metrics library; counters, gauges, histograms and labels take a few
  hundred lines, and the output format is simple and fully tested (exact text, escaping,
  histogram bucket semantics, consistency under concurrent scrapes). A mistake in a metric
  name or label panics when the program starts, not later.
- **State that is cheaper to read than to maintain is read at scrape time.** The Raft node's
  role, term and indexes, the topics and the consumer-group lag come from collector
  functions that ask the node when Prometheus asks. The protocol never touches a metric.
  Only the few counters that need history (elections started and won, snapshots, dropped and
  failed sends) are kept, as plain integers beside the code that causes them.
- **The server reports through an interface.** `server.Observer` has three methods; the
  handlers call it if one is set. The server does not know about metrics, and the tests use a
  counting fake.
- **Off unless asked for.** `-metrics-listen` is empty by default, so a plain node opens no
  extra port and does no extra work.
- **Series are bounded.** Per-partition and per-group metrics stop at 1000 series each and
  say so in `raftlog_series_truncated`; the totals still count everything.
- **Listing is an RPC.** `ListTopics` and `ListGroups` answer from the node's applied state,
  through an optional interface, so the many fake backends in the tests needed no change.
- **The console is a separate process that owns the nodes only when asked to.** It reads
  everything through the public gRPC API. With `-spawn` it also runs the node processes and
  can kill them (a real kill, no clean-up); with `-addr` it only watches.
- **The console checks the promise.** Its writer remembers the values the cluster
  acknowledged, and Verify reads the topic back and reports any that are missing, being
  patient with a node that is a few moments behind. Retries can store a value twice; that is
  reported as a duplicate, not a loss.
- **The topics are read from every node and the furthest-ahead answer is shown**, because a
  node that has just restarted would otherwise understate what the cluster holds.
- **Changing requests need a marker header.** A page from another site cannot add it without
  the console's consent, so it cannot stop a node through the browser of someone who has the
  console open. The page binds to `127.0.0.1` by default.

## Consequences
- The console found a real defect the unit tests and the chaos suite had not: a restarted
  node disturbed a healthy cluster because the Raft transport's reconnect backoff grew to
  minutes. The transport now caps it at one second, and a test requires that a returning node
  causes no election. The underlying weakness, no pre-vote, remains and is the next piece of
  work.
- Latency percentiles shown by the console come from its own writer over a window of a few
  seconds, and those in the metrics from fixed histogram buckets; neither is a benchmark.
  Real numbers need a controlled run, which belongs to the benchmark phase.
- The metrics are per node. Summing them over nodes, or asking which node is the leader, is
  Prometheus's job; for example the produced-message counters are counted by whichever node
  accepted the request.
- The console is a demonstration tool: no login, no history beyond a short timeline, and it
  keeps its writer's record of acknowledged values in memory (the first 200 000).
