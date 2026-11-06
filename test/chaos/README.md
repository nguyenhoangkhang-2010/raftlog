# Chaos tests

These tests start real `raftlogd` processes, hurt them while clients write, and
then check that the cluster kept every promise it makes.

## How it works
- **Real processes.** The daemon is built once and run as separate operating
  system processes. A crash is a process killed at once (`Process.Kill`), in the
  middle of whatever it was doing, not a polite shutdown.
- **A network we control.** Every ordered pair of nodes talks through a small TCP
  proxy written for the tests (`proxy_test.go`). A link can be cut in one
  direction, or slowed. No tool outside the repository is needed.
- **A nemesis** (`nemesis_test.go`) picks faults at random: kill a node, kill the
  current leader, isolate a node, split the cluster in two, cut one direction
  between two nodes, slow a link, keep a node away for a long while. It never
  takes down more nodes than a majority can lose; the network it may cut freely.
  Every fault is undone after a short random time.
- **Clients** (`workload_test.go`) write unique values and never repeat an
  attempt, so a message found in the log identifies the attempt that put it
  there. They record when each attempt started and ended and what they were
  told: *acknowledged* (with an offset), *refused* (e.g. "not the leader"), or
  *unknown* (a timeout, a broken connection). A reader fetches from random nodes
  throughout.
- After the faults stop, every node is restarted and the cluster is given time to
  settle. Then the **checker** (`checker.go`) compares the history with the final
  log of every node.

## What is checked
| Promise | Violation if broken |
|---|---|
| Every acknowledged write is in the final log, at the offset it was told | `lost-acknowledged-write`, `acknowledged-write-at-another-offset` |
| No message is stored twice and none appears that was never sent | `duplicate-message`, `message-nobody-sent` |
| A write the cluster refused is not in the log | `refused-write-was-stored` |
| Appends are linearizable: if one finished before another began, it has the lower offset | `append-order-violates-real-time` |
| A read never shows data that is not in the final log (no dirty reads) | `read-of-data-not-in-the-final-log` |
| After recovery all nodes hold the same log | `nodes-diverged` |
| The cluster recovers once the faults stop | the run fails with "did not settle" |

A write of unknown outcome may be in the log (once) or not; both are fine.

### Why not a general linearizability checker (Porcupine)?
The cluster does not promise linearizable *reads*: a follower can lag, and a
leader that was just deposed may still answer. A general checker would flag that
by design. The properties above are the ones the system does promise, checked
exactly; the checker itself is tested against histories with each kind of
violation injected (`checker_test.go`).

## Run
```bash
go test -count=1 -v ./test/chaos/            # about two minutes
```
Optional environment variables: `CHAOS_SECONDS` (how long faults and writes go
on, per scenario) and `CHAOS_SEED` (the random choices; timing still varies, so a
seed repeats the choices, not the exact run). `go test -short` skips these tests.
On a failure the tail of every node's log is printed.

## Scenarios
| Test | Nodes | Faults |
|---|---|---|
| `TestChaosHarnessWithoutFaults` | 3 | none: tests the harness itself |
| `TestChaosProcessCrashes` | 3 | kill random nodes, kill the leader |
| `TestChaosNetworkFaults` | 5 | isolation, partitions, one-way cuts, delays |
| `TestChaosStaleNodesReturn` | 5 | a node away for a long while, then back with an old log |
| `TestChaosEverythingWithSnapshots` | 5 | all of the above, with a snapshot every 15 entries |

## Results of one local run per scenario
(Windows 11, Intel i7-12700H; each run 5–15 s of faults. Numbers vary from run to run.)

| Scenario | Acknowledged | Refused | Unknown | Messages in the final log | Faults injected |
|---|---|---|---|---|---|
| no faults | 867 | 67 | 3 | 870 | 0 |
| process crashes | 1548 | 963 | 434 | 1569 | 9 kills (4 of the leader) |
| network faults | 756 | 997 | 28 | 771 | 47 |
| stale nodes return | 447 | 2159 | 33 | 473 | 63 |
| everything + snapshots | 890 | 1910 | 347 | 923 | 57 |

No violation in any of them. The messages in the final log are the acknowledged
ones plus the unknown ones that did get stored.

### Does the suite catch bugs?
A test that always passes proves nothing, so bugs were put into the system on
purpose (the daemon is rebuilt from the source for every run) and the suite was run:

| Bug introduced | Caught? | By |
|---|---|---|
| A leader commits with one replica fewer than a majority | yes, first run | `lost-acknowledged-write` (20 times) in the network scenarios |
| The state machine applies entries again after a restart | yes, first run | `duplicate-message` (16), `nodes-diverged` (12), `acknowledged-write-at-another-offset` (12) |
| A node votes for any candidate, even one with an older log | yes, first run | "did not settle": the stale leader cannot overwrite committed entries (a follower refuses), but the cluster is stuck |

(A first attempt at the third bug did not compile, and the script reported "not
caught". The conclusion was wrong, not the suite; the script now detects an
invalid change.)

## What these tests do not cover
- **Power loss.** Killing a process does not lose what the operating system has
  buffered, so a missing `fsync` would not show up here.
- Disk errors or full disks, clock jumps, very slow disks.
- Long soak runs: each scenario lasts seconds, not hours.
- Clusters larger than five nodes, and membership changes (not implemented).
- Retried requests: an attempt is never repeated, so the at-least-once duplicates
  that a retrying client can cause are not exercised.

## Observation: availability when a node returns
In `TestChaosStaleNodesReturn` only 447 writes were acknowledged in 15 s,
against 1548 with crashes alone. A node that was cut off keeps raising its term;
when it returns, its high term makes the current leader step down and forces a new
election, although the cluster was fine. Safety holds (nothing was lost), but
availability drops. Raft's *pre-vote* and *check-quorum* extensions address this;
they are not implemented (see the optional `feat/raft-hardening` branch idea).
