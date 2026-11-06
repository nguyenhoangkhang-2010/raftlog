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
- [ ] Topics, partitions, consumer groups
- [ ] Raft: leader election, log replication, persistence, snapshots
- [ ] Failover and follower catch-up
- [ ] Chaos tests and linearizability checking
- [ ] Prometheus metrics and Grafana dashboard

## Quick start
```bash
# starts one broker node (the 3-node Raft cluster comes in later phases)
docker compose -f deploy/docker-compose.yml up
```

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
| `internal/storage` | Segments, index, crash recovery |
| `internal/raft` | Raft consensus implementation |
| `internal/broker` | Topics, partitions, consumer groups |
| `internal/server` | gRPC handlers |
| `api` | Protobuf definitions |
| `deploy` | Dockerfile, Compose, Prometheus, Grafana |
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
