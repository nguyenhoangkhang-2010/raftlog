# Benchmarks

Record here, for every published number:
- Hardware (CPU, RAM, disk), OS, Go version
- Message size, number of producers/consumers, batch size, replication factor
- Throughput (msg/s), latency p50/p99, failover time
- Comparison baseline (Kafka / NATS) with the same configuration

Run: `make bench`

## Results

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

### Writes across a leader failure
Three real `raftlogd` processes on one machine, `raftlogctl` with all three addresses,
12 acknowledged writes, then the leader killed and 10 more writes issued at once
(one run): 0 of the 10 failed, they took 1.75 s in total (almost all of it waiting for the
election), and each node ended with exactly 22 messages. One run only: not a throughput
or latency figure.

Machine: Intel Core i7-12700H, Windows 11, Go 1.27.3, loopback network.
The election timeout is the main lever: a failover cannot be faster than the shortest
timeout, and lowering it makes spurious elections more likely on a slow network.
