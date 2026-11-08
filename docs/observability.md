# Observability and the cluster console

Three ways to see what a cluster is doing, from the smallest to the most visual.

## 1. Look from the command line

```bash
raftlogctl -addr 127.0.0.1:9091 status     # role, term, leader, commit and snapshot index of that node
raftlogctl -addr 127.0.0.1:9091 topics     # every topic, its partitions, how many messages each holds
raftlogctl -addr 127.0.0.1:9091 groups     # every consumer group, its offset and lag per partition
```

`topics` and `groups` are answered by one node (the first of `-addr` that is up) from the
data it has applied, so a node that is a little behind shows a little less. They are the
`ListTopics` and `ListGroups` RPCs, also available as `Topics` and `Groups` in the Go client.

## 2. Metrics for Prometheus

Every node can serve its numbers in the Prometheus text format. It is off unless asked for:

```bash
raftlogd -node-id 1 ... -metrics-listen 127.0.0.1:2112     # or RAFTLOG_METRICS_LISTEN
curl http://127.0.0.1:2112/metrics
```

`docker compose -f deploy/docker-compose.yml up --build` turns it on for the three nodes and
starts a Prometheus that scrapes them, at <http://localhost:9090>. The metrics are written by
a small package of our own (`internal/metrics`, no dependency): counters, gauges, histograms
and labels, checked when the program starts, rendered by a test-covered writer.

| Metric | Kind | What it says |
|---|---|---|
| `raftlog_requests_total{method,code}` | counter | gRPC requests handled, by method (`Broker/Produce`, `Raft/Send`, ...) and result code |
| `raftlog_request_duration_seconds{method}` | histogram | time to handle a request; a long-poll `Fetch` includes the time it waited |
| `raftlog_produced_messages_total`, `raftlog_produced_bytes_total` | counter | messages and bytes of key and value stored through `Produce`, counted by the node that accepted them (the leader) |
| `raftlog_fetched_messages_total` | counter | messages returned by `Fetch` |
| `raftlog_fetches_waiting` | gauge | long-poll fetches waiting for a message right now |
| `raftlog_raft_term`, `raftlog_raft_is_leader`, `raftlog_raft_role{role}`, `raftlog_raft_leader_id` | gauge | who this node thinks it is, and who the leader is |
| `raftlog_raft_commit_index`, `_last_index`, `_applied_index`, `_snapshot_index`, `_apply_backlog` | gauge | how far the log has got, and how many committed items wait for the state machine |
| `raftlog_raft_elections_started_total`, `_elections_won_total` | counter | elections this node began and won |
| `raftlog_raft_snapshots_taken_total`, `_snapshots_installed_total` | counter | snapshots it made, and snapshots it received from a leader |
| `raftlog_raft_messages_dropped_total` | counter | messages thrown away because the queue to a peer was full |
| `raftlog_raft_send_failures_total{peer}` | counter | messages to a peer that could not be delivered |
| `raftlog_raft_follower_match_index{peer}`, `raftlog_raft_follower_lag_entries{peer}` | gauge | on the leader only: how far each follower has got, and how far behind the leader's log it is |
| `raftlog_topics`, `raftlog_partitions` | gauge | what the node holds |
| `raftlog_partition_end_offset{topic,partition}` | gauge | messages in a partition |
| `raftlog_consumer_group_lag{group,topic,partition}` | gauge | messages a group has not read |
| `raftlog_series_truncated` | gauge | 1 if the two per-partition metrics above left out series: they stop at 1000 each so that a scrape stays small |
| `raftlog_build_info{version}`, `go_goroutines`, `go_memstats_alloc_bytes`, `process_start_time_seconds` | gauge | the process |

The Raft metrics are read when Prometheus asks, from the node's own state; nothing in the
protocol depends on them. Counters start again from zero when a process restarts, as
Prometheus expects.

Queries worth trying:

```promql
sum(raftlog_raft_is_leader)                                   # exactly one, except for a moment during an election
max(raftlog_raft_term)                                        # rises by one for every election
increase(raftlog_raft_elections_started_total[10m])           # elections that began in the last 10 minutes
raftlog_raft_follower_lag_entries                             # how far behind each follower is (from the leader)
sum(rate(raftlog_produced_messages_total[1m]))                # messages per second written to the cluster
histogram_quantile(0.99, sum by (le, method) (rate(raftlog_request_duration_seconds_bucket[1m])))
sum by (method, code) (rate(raftlog_requests_total{code!="OK"}[1m]))   # failures, by method and code
raftlog_consumer_group_lag                                    # lag of every consumer group
```

## 3. The cluster console

A web page that shows the cluster, writes to it, and lets you stop its nodes:

```bash
make build
./bin/raftlogconsole -spawn 3            # runs three nodes as child processes (demo)
# open http://127.0.0.1:8090
```


What it shows and does:

- **Node cards** with the role (leader, follower, candidate, down), the term, the commit and last
  log index, and how many entries each follower is behind the leader. A banner says whether the
  cluster is healthy, degraded (a majority is still up, writes work) or down.
- **Kill and Start** on every node. Kill ends the process at once, as a crash would; Start brings
  it back with the data it had.
- **Write load**: a writer that sends messages at a rate you choose, with throughput and p50/p99
  latency measured by that writer, and counts of writes acknowledged, failed and not sent.
- **Verify**: reads the whole topic back and checks that every write the cluster acknowledged is
  there. It is the point of the page: kill the leader while the writes go on, start it again, and
  press the button.
- **Timeline** of the leader changes, nodes that stopped or came back, and your own actions.
- **Topics** and their sizes, taken from the furthest-ahead node so that a node that was just
  restarted does not understate them.

To watch a cluster that is already running, without controls:

```bash
./bin/raftlogconsole -addr 10.0.0.1:9092,10.0.0.2:9092,10.0.0.3:9092     # in node ID order
```

The page is served on `127.0.0.1` by default because it can stop processes, and every request
that changes anything must carry a header that a page from another site cannot add. It is a
demonstration tool, not an administration interface: there is no login.

### What it found

Running the console against the cluster showed something the earlier tests had not: after a
killed node was started again, the cluster held another election, and a healthy leader
stepped down. The cause was in the Raft transport: a peer that had been down for a while was
retried with gRPC's default backoff, which grows towards two minutes, so the restarted node did
not hear the leader's heartbeats, started an election of its own with a higher term, and
disturbed the cluster. The transport now retries a peer at least once a second. A test starts
real nodes, kills the leader, starts it again and requires that the leader and the term do
not change in the seconds after.

The weakness underneath is still there: without pre-vote, a node that cannot hear the leader
for any reason can force an election. That is the next piece of work (see the limits in the
README).

### Two measured runs

Two runs on one Windows laptop, three `raftlogd` processes with the default Raft timings
(heartbeat every 100 ms, election timeout 1 to 2 s), 50 messages per second, written by the
console's own writer. They show what the page does; they are not benchmarks.

| | Run 1: killing the leader from the page | Run 2: the leader killed and restarted twice, driven by a script |
|---|---|---|
| writes acknowledged | 759 | 2029 |
| writes that returned an error | 0 | 0 |
| acknowledged writes missing from the log | 0 (one stored twice, by a retry) | 0 (one stored twice, by a retry) |
| writer's p99 latency over a 5 s window around an election | about 1.1 s, later 1.7 s, against a few milliseconds otherwise | not recorded |
| elections after a killed node came back | one extra (the transport defect described above) | none: the term stayed 2 and then 3 |

Run 2 was made after the transport fix.
