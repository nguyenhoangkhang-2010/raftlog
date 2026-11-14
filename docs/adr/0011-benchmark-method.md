# 11. How the benchmarks are made, and what they may claim

Date: 2026-11-09

## Status
Accepted

## Context
The project needs numbers for throughput, latency and failover time. Numbers are easy to
produce and easy to get wrong: a short run, a lucky run, a measurement that includes warm-up,
or a check that reports a defect that is not there. The first version of the failover check
reported 345 acknowledged writes as lost, in a cluster that had lost none: it read the log from
a node that had committed those writes but had not applied them yet.

## Decision
- **One tool measures, and it records its settings.** `raftlogbench` starts real `raftlogd`
  processes (or is pointed at running ones), and prints the workload, the number of CPUs, the
  Go version and the extra node arguments next to every result. `-json` keeps everything.
- **A window after a warm-up.** Only requests that started and ended inside the window count, so
  a request cut off by the end of the run does not flatter the latency, and the figures are
  slightly conservative.
- **Failover is measured as a client sees it.** The clock starts when the leader's process is
  gone. The tool records when a survivor first reports itself leader (polled every 10 ms) and
  when the first write after the kill is acknowledged, retries included.
- **A benchmark that kills nodes also checks for loss, node by node.** At the end the log of
  every node is read separately. A node that is behind is given time to catch up, because a
  restarted node applies the entries it missed one at a time; only a write still missing when the
  time is up is called lost. Both outcomes are covered by tests, so the check can neither cry
  wolf for a slow node nor miss a real loss.
- **Raw results are committed**, with the command line of each run, under `bench/results/`.
  The README tables are copied from them.
- **No claim without a comparison made under the same conditions.** There is no table against
  Kafka or NATS. Statements about where the time goes are labelled as diagnosis unless a
  profile backs them.

## Consequences
- The numbers are single runs on one laptop and say so. They are good for ratios and for finding
  a bottleneck (a write costs a fixed amount per request, so batching helps and concurrency does
  not), not for comparing with other systems.
- The benchmark found the cluster's main weakness: no group commit. Improving it can be
  measured against the committed baseline with the same command.
- The tool's own end-to-end tests start real processes and run in the chaos job in CI.
