# raftlog

> A distributed, fault-tolerant message broker built from scratch in Go, with Raft consensus.

![CI](https://github.com/nguyenhoangkhang-2010/raftlog/actions/workflows/ci.yml/badge.svg)

<!-- TODO: add GIF demo of killing the leader -->

## Why this project
<!-- TODO: your story. Why did you build this? What problem did you hit with existing brokers? -->

## Architecture
<!-- TODO: architecture diagram (docs/architecture.png) -->

```
Producer ──► [Node 1 (Leader)] ◄──Raft──► [Node 2 (Follower)]
                    ▲                         ▲
                    └────────Raft─────────────┘
                          [Node 3 (Follower)]
Consumer ◄── read by offset
```

## Features (roadmap)
- [x] Append-only log storage with segments and sparse index
- [x] gRPC API: `CreateTopic`, `Produce`, `Fetch` (`CommitOffset` comes with consumer groups)
- [x] Topics, partitions, committed offsets per consumer group (no dynamic membership or rebalance yet)
- [x] Raft leader election: terms, randomized timeouts, durable vote, heartbeats, step-down
- [x] Raft log replication: durable log (WAL), AppendEntries with consistency check, commit index, batching, catch-up of lagging followers
- [x] Raft snapshots: log compaction, InstallSnapshot for followers that fell behind, recovery from a snapshot at startup
- [x] Broker data replicated through the Raft log: topics, produce and consumer offsets survive the loss of a node; clients follow the leader
- [x] Chaos tests: real processes killed and partitioned while clients write; the history is checked for lost, duplicated or misordered writes and dirty reads (`test/chaos`)
- [x] Long-poll `Fetch`: a reader waits for the next message instead of polling (`-wait`, `client.WaitUpTo`, `-max-fetch-wait`)
- [x] Client library (`client`) that follows the leader and rides through failover
- [x] Demo application: a virtual waiting room for course registration, whose whole state is two raftlog topics ([docs/waitingroom.md](docs/waitingroom.md))
- [ ] Kitchen system (orders, stations, till) on raftlog: the domain core is done (`internal/kitchen`), the service, database and screens are next ([docs/kitchen](docs/kitchen/README.md))
- [ ] Failover and follower catch-up
- [ ] Chaos tests and linearizability checking
- [x] Prometheus metrics (`-metrics-listen`), `raftlogctl topics` and `groups`, and a cluster console that kills nodes, writes under load and verifies that no acknowledged write was lost ([docs/observability.md](docs/observability.md))
- [ ] Grafana dashboard

## Quick start
```bash
# starts one broker node (the 3-node Raft cluster comes in later phases)
docker compose -f deploy/docker-compose.yml up
```

## Try a cluster
Start three nodes (each in its own terminal; every node needs the same `-peers`):
```bash
raftlogd -node-id 1 -listen 127.0.0.1:9091 -data-dir data1 -peers 1=127.0.0.1:9091,2=127.0.0.1:9092,3=127.0.0.1:9093
raftlogd -node-id 2 -listen 127.0.0.1:9092 -data-dir data2 -peers 1=127.0.0.1:9091,2=127.0.0.1:9092,3=127.0.0.1:9093
raftlogd -node-id 3 -listen 127.0.0.1:9093 -data-dir data3 -peers 1=127.0.0.1:9091,2=127.0.0.1:9092,3=127.0.0.1:9093
```
Then use all three addresses, so the client follows the leader and survives its loss:
```bash
raftlogctl -addr 127.0.0.1:9091,127.0.0.1:9092,127.0.0.1:9093 create-topic -partitions 2 orders
raftlogctl -addr 127.0.0.1:9091,127.0.0.1:9092,127.0.0.1:9093 produce -key user-1 -value hello orders
raftlogctl -addr 127.0.0.1:9091 status     # role, term, leader, commit and snapshot index of that node
```
Stop the leader and keep producing: the writes continue on the new leader.

To watch it happen, or to try it without opening four terminals, run the cluster
console. It starts three nodes, shows who leads, lets you kill and start them while a
writer sends messages, and checks at the end that no write the cluster acknowledged has
gone missing:
```bash
make build && ./bin/raftlogconsole -spawn 3     # then open http://127.0.0.1:8090
```
See [docs/observability.md](docs/observability.md) for the page, the metrics each node can
serve for Prometheus (`-metrics-listen`), and `raftlogctl topics` / `groups`.

To follow a topic as it grows, wait for messages instead of polling (the `-timeout`
of the command must be longer than the `-wait`):
```bash
raftlogctl -addr 127.0.0.1:9091,127.0.0.1:9092,127.0.0.1:9093 -timeout 40s consume -group billing -wait 30s orders
```
The longest wait a node allows is `-max-fetch-wait` (default 30s, `0` turns waiting off).

## Use it from Go
The `client` package is the same leader-following client that `raftlogctl` uses.
Give it every node's address; it finds the leader, follows leader changes and waits
out an election (bounded by the context you pass):
```go
c, err := client.New([]string{"127.0.0.1:9091", "127.0.0.1:9092", "127.0.0.1:9093"})
if err != nil { /* ... */ }
defer c.Close()

ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
_, err = c.Produce(ctx, "orders", []client.Message{{Key: []byte("user-1"), Value: []byte("hello")}})
if errors.Is(err, client.ErrNotFound) { /* the topic does not exist */ }
```
`c.Fetch(ctx, topic, partition, offset, limit, client.WaitUpTo(20*time.Second))` waits
for the next message instead of returning nothing at once.
Delivery is at-least-once: a request that failed after the leader stored it may be
stored again when you retry, so consumers should tolerate repeats (see `client/doc.go`).

## Demo: a waiting room for course registration
`cmd/waitingroom` is a web application built only on the public `client` package:
people join a line, are let in a few at a time, and see their place update live.
The line lives in two topics, so killing the leader of the cluster does not cost
anyone their place; `cmd/wrsim` simulates the crowd. See
[docs/waitingroom.md](docs/waitingroom.md) for how to run it, how it works, what the
tests prove and what a measured run looked like.

## Known limitations
- One Raft group for the whole cluster: every write goes through a single leader.
- A snapshot is a copy of all data held in memory and sent in one message
  (`-max-message-bytes`), so the data set is bounded.
- Delivery is at-least-once (a retried request can store a message twice); reads can
  lag on a follower and are not linearizable; node-to-node traffic is not encrypted.
- See `docs/adr` for the reasoning behind each design decision.

## Results
<!-- TODO: benchmark table (throughput, p50/p99 latency, failover time) -->

## Design decisions and trade-offs
See [docs/design.md](docs/design.md) and [docs/adr](docs/adr).

## Known limitations
<!-- TODO: be honest, e.g. no security, no dynamic membership yet, no exactly-once -->

## Project layout
| Path | Purpose |
|---|---|
| `cmd/raftlogd` | Broker server |
| `cmd/raftlogctl` | CLI client |
| `cmd/raftlogconsole`, `internal/console` | Cluster console: node cards, Kill/Start, write load, Verify, timeline |
| `internal/metrics`, `internal/observe` | Prometheus metrics, and what is measured in a node |
| `client` | Go client library: finds the leader, follows failover |
| `cmd/waitingroom`, `internal/waitingroom` | Demo application: virtual waiting room (web page, API, admission controller) |
| `cmd/wrsim` | Simulates a crowd arriving at the waiting room |
| `internal/kitchen` | Domain core of the kitchen system: money, roles, menu, promotions, stock, event-sourced orders ([docs/kitchen](docs/kitchen/README.md)) |
| `internal/storage` | Segments, index, crash recovery |
| `internal/raft` | Raft consensus implementation |
| `internal/broker` | Topics, partitions, consumer groups |
| `internal/server` | gRPC handlers |
| `api` | Protobuf definitions |
| `deploy` | Dockerfile, Compose with Prometheus |
| `test/chaos` | Fault-injection scenarios |
| `bench` | Benchmarks and results |
| `docs` | Design doc and architecture decision records |

## Development
```bash
make build   # build raftlogd and raftlogctl into ./bin
make test    # run tests with the race detector
make lint    # run golangci-lint
make proto   # generate gRPC code (needs protoc)
```

## License
MIT
