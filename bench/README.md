# Benchmarks

Every number here was measured by a command in this repository, on the machine named below,
and the raw output is kept in [results/](results/). A number comes with what produced it, and
none is a claim about production: these are single runs on one laptop, with the nodes and the
client on the same machine.

## The cluster benchmark: `raftlogbench`

`raftlogbench` (source: `cmd/raftlogbench`, `internal/bench`) starts a cluster of real `raftlogd`
processes in a temporary directory, or measures one that is already running (`-addr`), and has
two commands.

```bash
make build
./bin/raftlogbench produce  -spawn 3 -producers 16 -batch 100        # write, then read everything back
./bin/raftlogbench failover -spawn 3 -rounds 10                      # kill the leader ten times under a writer
bench/run-matrix.sh bench/results/my-machine                         # everything in the tables below
```

How it measures:

- **Throughput and latency (`produce`).** A number of producers (`-producers`) each keep one
  request in flight, sending `-batch` messages of `-message-size` bytes to one partition. After a
  warm-up (3 s) it counts, over a window (10 s), the requests that started and ended inside the
  window. Latency is the time of one request from sending to acknowledgement; throughput is
  acknowledged messages divided by the window. A request cut off by the end of the window is not
  counted, so the throughput is slightly conservative. The messages are pseudo-random bytes from
  a fixed seed, built once and reused, so the tool measures the cluster and not data generation.
- **Reading back.** The same run then reads the whole topic from the start, one reader per
  partition. This takes a fraction of a second, so it is reported in the raw results and not in the
  tables: it is too short to be a read benchmark.
- **Failover (`failover`).** One writer sends numbered values one at a time. The tool kills the
  leader's process (no clean-up, as in a crash), then records when a surviving node first reported
  itself leader, polling every 10 ms, and when the first write after the kill was acknowledged to
  the client, including the client's retries. Then it starts the node again, waits for the cluster
  to settle, and repeats. At the end it reads the log **of each node** and checks that every
  acknowledged value is there. A node that was restarted applies the entries it missed one at a
  time, so a node that is behind is given up to 30 s to catch up before anything is called lost.
  The time starts after `Kill` has returned, so it leaves out the milliseconds the kill itself
  took.

The tool prints the settings next to every result (and `-json` prints everything, including
the Go version and the number of CPUs). It cannot read the CPU model or the disk, so those are
written here by hand.

### Machine

Intel Core i7-12700H (20 hardware threads), 40 GB RAM, NVMe SSD (KIOXIA KBG50ZNS1T02, 1 TB),
Windows 11, Go 1.27.3, `raftlogd` built from commit `fa8ab93` (the code under test is
unchanged by the branch that added the benchmark). Three nodes and the client share the
machine, over the loopback interface. No other test or benchmark was running during the matrix; I only read files while it ran.

### Writing (3 nodes unless stated; value 128 bytes unless stated)

One run each, 10 s window after 3 s of warm-up, default Raft timings. Results:
`results/2026-11-09-i7-12700h-windows/produce-*.json`.

| Nodes | Producers | Messages per request | Messages/s | MB/s of values | Request latency p50 | p95 | p99 |
|---|---|---|---|---|---|---|---|
| 1 | 1 | 1 | 158 | 0.02 | 6.0 ms | 9.8 ms | 12.0 ms |
| 3 | 1 | 1 | 90 | 0.01 | 10.9 ms | 15.3 ms | 18.3 ms |
| 3 | 16 | 1 | 178 | 0.02 | 88.9 ms | 96.9 ms | 104.9 ms |
| 3 | 16 | 10 | 1,759 | 0.23 | 89.3 ms | 100.2 ms | 105.6 ms |
| 3 | 16 | 100 | 13,850 | 1.77 | 112.6 ms | 127.5 ms | 155.0 ms |
| 3 | 16 | 100, 1 KiB values | 7,430 | 7.61 | 208.7 ms | 238.0 ms | 247.0 ms |
| 1 | 16 | 100 | 21,210 | 2.71 | 73.8 ms | 91.5 ms | 96.6 ms |

No request failed in any of these runs.

What the table says:

- **A write costs a fixed amount, about 6 ms with one node and 11 ms with three.** One producer
  gets 90 to 160 messages per second whatever else is true.
- **Sixteen producers do not help**: the latency of a request grows from 11 ms to 89 ms and the
  throughput only doubles. Requests are being handled one at a time, so the producers queue.
- **Bigger requests do help, almost linearly in the number of messages**: 1, 10 and 100 messages
  per request give 178, 1,759 and 13,850 messages per second at nearly the same latency for the
  first two. The cost is per request, not per message.
- **Replication costs about a third of the throughput** at the largest batch (21,210 with one
  node against 13,850 with three), and raises the latency of a lone producer's request from
  6.0 to 10.9 ms.

### Where the time goes (a diagnosis, not a profile)

The code explains the shape of the table, and I have not profiled it to split the 6 ms into parts.
For each `Produce`, the leader appends one Raft entry and fsyncs the Raft log; the state
machine, which applies entries one at a time, then rewrites its journal file (create, write,
fsync, rename) before and after the work, appends to the partition log and fsyncs it
(ADR 4 lists these costs). For comparison, a bare `write` plus `fsync` of 160 bytes took 0.64 ms
(median of 2,000 on this disk), so the fsyncs alone would allow about 1,500 writes per second one
after another. The measured 158 per second with one producer is about 6.3 ms per request, far more than
three fsyncs of 0.64 ms. That suggests the journal file replacement (creating and renaming a
file is slow on this file system) and the work around it cost more than the fsyncs do, but it is
an inference: it needs a profile to be a finding.

Nothing batches: two concurrent requests become two entries, two sets of fsyncs and two journal
rewrites, applied in order. That is why more producers only add queueing, and why one request
with many messages (one entry, one set of fsyncs) is so much cheaper per message. Group commit
(many proposals, one entry or one fsync) and a journal that is not replaced for every entry are
the obvious improvements, and this table is the baseline to compare them against.

### Failover

The leader of a three-node cluster killed ten times while one writer sends one message at a time
(no batching). Results: `results/2026-11-09-i7-12700h-windows/failover-*.json`.

| Raft timings | Leader visible after the kill: min / median / max | First write acknowledged after the kill: min / median / max |
|---|---|---|
| default: tick 100 ms, election timeout 1.0 to 2.0 s | 910 / 1,236 / 1,772 ms | 916 / 1,276 / 1,784 ms |
| tick 20 ms, election timeout 0.2 to 0.4 s | 150 / 205 / 273 ms | 158 / 225 / 369 ms |

| Timings | Writes acknowledged | Failed | Acknowledged writes missing from the log of any node | Values stored twice |
|---|---|---|---|---|
| default | 5,828 | 0 | 0 | 8 |
| tick 20 ms | 5,785 | 0 | 0 | 8 |

- A failover cannot be faster than the election timeout: the followers wait that long without a
  heartbeat before one of them stands. Making the timeout five times shorter made the median
  failover about six times shorter (1,236 ms against 205 ms). The price is that on a slow or loaded network a healthy leader looks
  dead; pre-vote and check-quorum (ADR 10) soften the harm, they do not remove the trade-off.
- The first acknowledged write follows the moment a new leader is visible by a few tens of
  milliseconds in most rounds, and by 0.2 s in the slowest one; the cause of that one was not
  investigated.
- No write the cluster acknowledged was lost, on any of the three nodes. The values stored twice
  are writes whose answer was lost in the failover and which the client sent again: delivery is
  at-least-once (README, known limitations).
- In each run one restarted node needed about one second after the writes stopped to hold every
  acknowledged write: it was still applying what it had missed.

### What was wrong with the first failover run

The first version of the failover check read the topic from whichever node answered and reported
345 acknowledged writes missing, in one run. They were the newest 345 writes, and the node that
answered was a restarted one that had committed them but not yet applied them. It was not data
loss. The check now reads every node separately and waits for a node that is behind; a unit test
fails if a node that is only behind is called lossy, and another fails if one that never catches
up is not reported. The numbers above are from the run after the fix.

### Limits of these measurements

- One run of each scenario, on one laptop, with client and nodes sharing the CPU and a loopback
  network. The differences between two runs of the same scenario were not measured, so small
  differences in the tables mean nothing.
- Values are 128 bytes, or 1 KiB where stated; there is one topic, three partitions, and the
  producers do not use keys.
- The disks are an NVMe SSD with fsync enabled. A different disk changes the fixed cost per write,
  and so the whole table.
- Nodes in containers, or on separate machines, have not been measured. `docker compose` was not
  run for this document (the Docker daemon was not running on the machine), so there is no
  Docker figure. To measure a running cluster, including the Compose one:

  ```bash
  ./bin/raftlogbench produce -addr localhost:9091,localhost:9092,localhost:9093 -producers 16 -batch 100
  ```

- There is no comparison with Kafka or NATS, because one would need the same machine, the same
  replication and durability settings and a comparable client, and has not been made.

## Earlier measurements

### Storage: reading with and without the sparse offset index
`BenchmarkRead` in `internal/storage/index_test.go`: one segment of 200,000 records
(100-byte values, about 27 MB on disk), reading a 10-message window at a random
offset (fixed RNG seed). Command: `go test -run '^$' -bench BenchmarkRead -benchtime 2s ./internal/storage/`

| Index interval | Time per read | Speedup vs no index |
|---|---|---|
| none (scan from segment start) | 59,793,770 ns (about 59.8 ms) | 1x |
| 64 KiB | 180,688 ns (about 181 us) | about 331x |
| 4 KiB | 30,381 ns (about 30 us) | about 1,968x |

Machine: Intel Core i7-12700H, 40 GB RAM, Windows 11, Go 1.27.3. The file is small
enough that reads are mostly served from the OS page cache. Single run, so treat the ratios as an order of
magnitude, not a precise figure.

### Raft: time to elect a leader

**Simulation** (`TestElectionTimeAcrossSeeds`): 5 nodes, election timeout 10-19 ticks,
instant message delivery, 300 random seeds. Time is counted in ticks, so the result
does not depend on the machine. Run with
`go test -run TestElectionTimeAcrossSeeds -v ./internal/raft/`.

| Ticks until exactly one leader is followed by all nodes | Value |
|---|---|
| min | 10 |
| median | 11 |
| p99 | 16 |
| max | 24 |

**Real processes** (3 `raftlogd` processes on one machine, defaults: 100 ms tick, election
timeout 1.0-2.0 s, heartbeat every tick). One run each, so only an order of magnitude:

| Event | Time |
|---|---|
| Cluster started to leader visible through `raftlogctl status` | 1.52 s |
| Leader process killed to new leader visible | 1.13 s (a later run on the same machine: 2.1 s) |

The failover table above repeats the second measurement ten times with a writer running.

### Writes across a leader failure
Three real `raftlogd` processes on one machine, `raftlogctl` with all three addresses,
12 acknowledged writes, then the leader killed and 10 more writes issued at once
(one run): 0 of the 10 failed, they took 1.75 s in total (almost all of it waiting for the
election), and each node ended with exactly 22 messages. One run only: not a throughput
or latency figure.

The election timeout is the main lever: a failover cannot be faster than the shortest
timeout, and lowering it makes spurious elections more likely on a slow network.
