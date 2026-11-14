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

The same scenarios run again after pre-vote and check-quorum were added (ADR 10), once
each on the same laptop, again without any violation:

| Scenario | Acknowledged | Refused | Unknown | Faults injected |
|---|---|---|---|---|
| no faults | 465 | 34 | 3 | 0 |
| process crashes | 1408 | 246 | 289 | 19 |
| network faults (5 nodes) | 618 | 310 | 20 | 43 |
| stale nodes return | 745 | 890 | 48 | 62 |
| everything + snapshots | 817 | 1449 | 307 | 52 |

These are single runs with different random faults, so the two tables are not a controlled
comparison: the counts vary from run to run. The controlled comparison is the pair of
hardening scenarios below.

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

## Hardening scenarios
Three more tests (`hardening_test.go`) hold the effect of pre-vote and check-quorum (ADR 10)
against real processes. They use a 50 ms tick and an election timeout of 1 to 2 s
(`steadyTimings`), not the 100 to 200 ms they were first written with: they assert that
*nothing happens* (no election, no change of term), and with a timeout that short a
machine whose cores are all busy, or a slow CI runner, looks like a leader that has been
lost. The first version failed in CI for that reason and was reproduced locally with every
core busy.

| Test | What it does | Result (one local run, every core busy) |
|---|---|---|
| `TestAnIsolatedNodeDoesNotDisturbTheClusterWhenItComesBack` | cut one follower off for 10 s while writes go on, then reconnect it | its term stays at the cluster's (1); after it returns the leader and the term are unchanged |
| `TestWithoutHardeningTheReturnOfAnIsolatedNodeCausesAnElection` | the same with `-pre-vote=false -check-quorum=false` | the cut-off node's term rises to 7 while alone and the cluster ends in term 9: the control that shows what the hardening prevents |
| `TestACutOffLeaderStopsLeadingWithinAFewElectionTimeouts` | cut the leader off | it stops leading within the 15 s the test allows (the whole test took about 4 s), the others elect a new leader in a higher term, and healing changes nothing |

The terms in the second row depend on the timing and on the load; they are one run, not a
property.

## Observation: availability when a node returns (fixed)
In `TestChaosStaleNodesReturn` only 447 writes were acknowledged in 15 s,
against 1548 with crashes alone. A node that was cut off keeps raising its term;
when it returns, its high term makes the current leader step down and forces a new
election, although the cluster was fine. Safety holds (nothing was lost), but
availability drops. Raft's *pre-vote* and *check-quorum* extensions address this,
and have been implemented and are on by default (ADR 10); the numbers in the table above
are from before that. The hardening scenarios above show the effect directly.

## Kitchen scenarios
The kitchen service (`cmd/orderservice`, ADR 13) is a real application of the cluster: its state
is the events in one topic. `orders_test.go` runs it as a **real process** next to the cluster, with
many clients that take orders through their whole lives, while the service is killed every second or
two and the nodes are killed or cut off from each other. The clients are the till, the kitchen
stations, the runner, the manager and customers in the app, from the sample users of
`examples/kitchen`. Each order is one of: paid and handed over; cancelled before it was paid; paid and
cancelled before any station started (the ingredients go back); paid, a station started, and a
cancellation approved with part of the money back; or a cancellation that the manager rejects and
the order goes on. Every command has an ID, and a command that gets no answer is sent again with the
same ID until the client gives up.

After the faults the checker (`ordercheck.go`) compares the history with the state read three ways:
from the service that ran through the faults, from a new service started afterwards, and built by the
test itself from the raw log. It checks that:

| Promise | Violation if broken |
|---|---|
| An order whose placing was acknowledged exists, for the right customer, with the lines asked for and the price of those lines; no order exists that nobody placed; no order was made from a request that was refused | `acknowledged-order-missing`, `order-nobody-placed`, `refused-order-exists`, `order-of-the-wrong-customer`, `order-with-other-lines-than-asked`, `order-total-is-not-the-price-of-the-lines` |
| Order numbers are not given twice | `order-number-given-twice` |
| Every acknowledged step is in the order: the payment (for the whole total, with the right change for cash), started and finished tickets, the hand-over, the cancellation, the refund the manager set; nothing a client was refused outright happened | `acknowledged-payment-missing`, `payment-is-not-the-total`, `cash-change-is-not-what-was-handed-over-minus-the-total`, `acknowledged-step-missing`, `refund-is-not-what-the-manager-set`, `command-refused-that-should-have-worked` |
| The stock is what was received minus what the paid orders took (not what a cancellation before work gave back), computed from the menu, and never below zero | `stock-is-not-what-the-orders-imply`, `stock-below-zero` |
| The three readings of the state are the same, and no record of the log was skipped | `live-state-differs-from-the-log`, `fresh-service-differs-from-the-live-one`, `records-of-the-log-were-skipped` |

The checker has its own tests (`ordercheck_test.go`): a healthy run, and one test for each kind of
violation.

| Test | Nodes | Faults | One local run |
|---|---|---|---|
| `TestOrdersChaosHarnessWithoutFaults` | 3 | none: tests the harness | 138 orders, 620 later commands, all well |
| `TestOrdersChaosServiceAndLeaderKilledWhileOrdersAreTaken` | 3 | the service killed every 1 to 2.5 s; nodes killed, the leader killed | 353 orders, 1,564 later commands, the service killed 10 times |
| `TestOrdersChaosNetworkFaultsAndAScarceStock` | 5 | the service killed; isolation, partitions, one-way cuts, a node away for long, the leader killed; snapshots every 25 entries; few baguettes, eggs and coffee beans, so payments compete for the last ones; clients give up after 1 s | 144 orders, 549 later commands, the service killed 8 times, 1 command with no answer; payments refused for lack of stock are allowed, nothing else is |

Longer runs (`CHAOS_SECONDS=60`), one run each: 1,052 orders and 4,891 later commands with the
service killed 26 times; and, in the scarce-stock scenario, 270 orders acknowledged, 46 placings with
no answer (one of them was stored, which the checker allows), the service killed 22 times. No promise
was broken.

### What it found
The first run of the network scenario reported `order-number-given-twice`: two orders had number 9.
The service had been restarted and had rebuilt its state by reading the log from a cluster node that
was behind, so it did not see the newest orders and numbered a new one from what it did see. Reads of
raftlog are not linearizable, so a service cannot tell from a read that it has seen everything.
Two things changed (ADR 13): the service now writes a **barrier** record when it starts, and after a
write whose answer was lost, and reads until it sees its own barrier, so its state is complete before
it decides anything; and a command that does not fit when it is applied in the order of the log is
answered with a conflict instead of a success. Unit tests reproduce the case without a cluster.

The same mistake was then found in the test itself: it read the raw log from any node, and once
reported an order that the log did have as missing. It now reads until it has as many records as a
service that made its state complete says the log holds.

### Does the suite catch bugs?
As in the first table, bugs were put into the service and the core on purpose, one at a time:

| Bug introduced | Caught? | By |
|---|---|---|
| A payment does not take the stock; a cancellation before work does not give it back | yes, first run | `stock-is-not-what-the-orders-imply` |
| Order numbers are not unique | yes, first run | `order-number-given-twice` |
| An acknowledged command is not applied to the service's own state | yes, first run | `command-refused-that-should-have-worked` (the next step on that order is refused), `order-number-given-twice` |
| The cash change is one đồng too much; a refund is recorded as the whole total | yes, first run | the change and refund checks |
| The service does not make its state complete when it starts | **3 runs of 6** | `order-number-given-twice`; the case needs the service to restart just when a node that is behind answers its reads, so a chaos run finds it by chance, and the unit tests in `internal/orderservice/barrier_test.go` are what guard it every time |

## What the kitchen scenarios do not cover
The service is one writer and is not tested with two. Power loss, a disk that lies, and clock jumps
are not tested (as above). The clients do not read orders while they take them, so what a customer
sees on the stream during a failover is not checked here. Each scenario lasts 6 to 60 seconds, not hours.
