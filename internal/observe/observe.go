// Package observe connects the parts of a raftlogd process to the metrics registry:
// what the gRPC server does, the state of the Raft node, and the contents of the
// broker. Nothing here changes how any of them works; it only reads, and counts.
package observe

import (
	"context"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/metrics"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
)

// MaxSeries bounds how many series one per-partition or per-group metric may
// produce, so that a broker with very many partitions cannot make a scrape huge.
// When the limit is hit the extra series are left out, and the metric
// raftlog_series_truncated says so.
const MaxSeries = 1000

// RaftSource is what the Raft metrics need; *raft.Runner provides it.
type RaftSource interface {
	Observe() raft.Observation
}

// BrokerSource is what the broker metrics need; the broker and the replicated
// broker provide it.
type BrokerSource interface {
	Describe() []broker.TopicDescription
	Groups() []broker.GroupDescription
}

// Node holds the metrics of one process. It implements server.Observer.
type Node struct {
	reg *metrics.Registry

	requests *metrics.CounterVec
	duration *metrics.HistogramVec

	producedMessages *metrics.Counter
	producedBytes    *metrics.Counter
	fetchedMessages  *metrics.Counter
	fetchesWaiting   *metrics.Gauge
}

// New registers the metrics that every process has: its version, the Go runtime,
// and what the gRPC server does.
func New(reg *metrics.Registry, version string) *Node {
	n := &Node{reg: reg}
	n.requests = reg.NewCounterVec("raftlog_requests_total", "gRPC requests handled, by method and result code.", "method", "code")
	n.duration = reg.NewHistogramVec("raftlog_request_duration_seconds",
		"Time to handle a gRPC request. A Fetch that waits for messages includes the time it waited.",
		metrics.DefaultLatencyBuckets, "method")
	n.producedMessages = reg.NewCounter("raftlog_produced_messages_total", "Messages stored through Produce.")
	n.producedBytes = reg.NewCounter("raftlog_produced_bytes_total", "Bytes of keys and values stored through Produce.")
	n.fetchedMessages = reg.NewCounter("raftlog_fetched_messages_total", "Messages returned by Fetch.")
	n.fetchesWaiting = reg.NewGauge("raftlog_fetches_waiting", "Fetch requests waiting for new messages right now.")

	reg.AddCollector([]string{"raftlog_build_info"}, func() []metrics.Family {
		return []metrics.Family{{
			Name: "raftlog_build_info", Help: "Always 1; the version is in the label.", Kind: metrics.KindGauge,
			Samples: []metrics.Sample{{Labels: []metrics.Label{{Name: "version", Value: version}}, Value: 1}},
		}}
	})
	started := float64(time.Now().UnixNano()) / 1e9
	reg.AddCollector([]string{"go_goroutines", "go_memstats_alloc_bytes", "process_start_time_seconds"}, func() []metrics.Family {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		one := func(name, help string, kind metrics.Kind, v float64) metrics.Family {
			return metrics.Family{Name: name, Help: help, Kind: kind, Samples: []metrics.Sample{{Value: v}}}
		}
		return []metrics.Family{
			one("go_goroutines", "Goroutines that currently exist.", metrics.KindGauge, float64(runtime.NumGoroutine())),
			one("go_memstats_alloc_bytes", "Bytes of allocated heap objects.", metrics.KindGauge, float64(ms.Alloc)),
			one("process_start_time_seconds", "Start time of the process, in seconds since the Unix epoch.", metrics.KindGauge, started),
		}
	})
	return n
}

// Produced implements server.Observer.
func (n *Node) Produced(messages, bytes int) {
	n.producedMessages.Add(uint64(messages))
	n.producedBytes.Add(uint64(bytes))
}

// Fetched implements server.Observer.
func (n *Node) Fetched(messages int) { n.fetchedMessages.Add(uint64(messages)) }

// FetchWaiting implements server.Observer.
func (n *Node) FetchWaiting(delta int) { n.fetchesWaiting.Add(float64(delta)) }

// shortMethod turns "/raftlog.v1.Broker/Produce" into "Broker/Produce".
func shortMethod(full string) string {
	return strings.TrimPrefix(strings.TrimPrefix(full, "/"), "raftlog.v1.")
}

// UnaryInterceptor counts and times every unary gRPC call, the node-to-node Raft
// messages included.
func (n *Node) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		method := shortMethod(info.FullMethod)
		n.requests.With(method, status.Code(err).String()).Inc()
		n.duration.With(method).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

func gaugeFamily(name, help string, v float64) metrics.Family {
	return metrics.Family{Name: name, Help: help, Kind: metrics.KindGauge, Samples: []metrics.Sample{{Value: v}}}
}

func counterFamily(name, help string, v uint64) metrics.Family {
	return metrics.Family{Name: name, Help: help, Kind: metrics.KindCounter, Samples: []metrics.Sample{{Value: float64(v)}}}
}

// WatchRaft registers the metrics of the Raft node. They are read when the metrics
// are scraped, not kept up to date by the protocol.
func (n *Node) WatchRaft(src RaftSource) {
	names := []string{
		"raftlog_raft_term", "raftlog_raft_is_leader", "raftlog_raft_role", "raftlog_raft_leader_id",
		"raftlog_raft_commit_index", "raftlog_raft_last_index", "raftlog_raft_applied_index",
		"raftlog_raft_snapshot_index", "raftlog_raft_apply_backlog",
		"raftlog_raft_elections_started_total", "raftlog_raft_elections_won_total",
		"raftlog_raft_snapshots_taken_total", "raftlog_raft_snapshots_installed_total",
		"raftlog_raft_messages_dropped_total", "raftlog_raft_send_failures_total",
		"raftlog_raft_follower_match_index", "raftlog_raft_follower_lag_entries",
	}
	n.reg.AddCollector(names, func() []metrics.Family {
		o := src.Observe()
		st := o.Status

		role := metrics.Family{Name: "raftlog_raft_role", Help: "1 for the role this node has, 0 for the others.", Kind: metrics.KindGauge}
		for _, r := range []raft.Role{raft.Follower, raft.Candidate, raft.Leader} {
			v := 0.0
			if st.Role == r {
				v = 1
			}
			role.Samples = append(role.Samples, metrics.Sample{Labels: []metrics.Label{{Name: "role", Value: r.String()}}, Value: v})
		}
		isLeader := 0.0
		if st.Role == raft.Leader {
			isLeader = 1
		}

		fails := metrics.Family{Name: "raftlog_raft_send_failures_total", Help: "Messages to a peer that could not be delivered.", Kind: metrics.KindCounter}
		for _, p := range sortedPeers(o.SendFailures) {
			fails.Samples = append(fails.Samples, metrics.Sample{Labels: peerLabel(p), Value: float64(o.SendFailures[p])})
		}
		match := metrics.Family{Name: "raftlog_raft_follower_match_index", Help: "On the leader: the highest log index known to be stored on a follower.", Kind: metrics.KindGauge}
		lag := metrics.Family{Name: "raftlog_raft_follower_lag_entries", Help: "On the leader: log entries a follower is behind the leader's log.", Kind: metrics.KindGauge}
		for _, f := range o.Stats.Followers {
			match.Samples = append(match.Samples, metrics.Sample{Labels: peerLabel(f.ID), Value: float64(f.Match)})
			behind := uint64(0)
			if st.LastIndex > f.Match {
				behind = st.LastIndex - f.Match
			}
			lag.Samples = append(lag.Samples, metrics.Sample{Labels: peerLabel(f.ID), Value: float64(behind)})
		}

		return []metrics.Family{
			gaugeFamily("raftlog_raft_term", "The current Raft term.", float64(st.Term)),
			gaugeFamily("raftlog_raft_is_leader", "1 if this node is the leader.", isLeader),
			role,
			gaugeFamily("raftlog_raft_leader_id", "The node this node believes is the leader; 0 if it does not know.", float64(st.Leader)),
			gaugeFamily("raftlog_raft_commit_index", "The highest log index known to be committed.", float64(st.CommitIndex)),
			gaugeFamily("raftlog_raft_last_index", "The index of the last entry in the log.", float64(st.LastIndex)),
			gaugeFamily("raftlog_raft_applied_index", "The highest log index handed to the state machine.", float64(o.Stats.Applied)),
			gaugeFamily("raftlog_raft_snapshot_index", "The last log index covered by the latest snapshot; 0 if none.", float64(st.SnapshotIndex)),
			gaugeFamily("raftlog_raft_apply_backlog", "Committed items waiting for the state machine.", float64(o.ApplyBacklog)),
			counterFamily("raftlog_raft_elections_started_total", "Elections this node started.", o.Stats.Campaigns),
			counterFamily("raftlog_raft_elections_won_total", "Elections this node won.", o.Stats.Wins),
			counterFamily("raftlog_raft_snapshots_taken_total", "Snapshots this node made.", o.SnapshotsTaken),
			counterFamily("raftlog_raft_snapshots_installed_total", "Snapshots this node received from a leader.", o.SnapshotsInstalled),
			counterFamily("raftlog_raft_messages_dropped_total", "Messages dropped because the queue to a peer was full.", o.MessagesDropped),
			fails, match, lag,
		}
	})
}

func peerLabel(id raft.NodeID) []metrics.Label {
	return []metrics.Label{{Name: "peer", Value: strconv.FormatUint(uint64(id), 10)}}
}

func sortedPeers(m map[raft.NodeID]uint64) []raft.NodeID {
	out := make([]raft.NodeID, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// WatchBroker registers the metrics of the broker's contents.
func (n *Node) WatchBroker(src BrokerSource) {
	n.reg.AddCollector([]string{
		"raftlog_topics", "raftlog_partitions", "raftlog_partition_end_offset", "raftlog_consumer_group_lag", "raftlog_series_truncated",
	}, func() []metrics.Family {
		topics := src.Describe()
		partitions := 0
		ends := metrics.Family{Name: "raftlog_partition_end_offset", Help: "The offset the next message of a partition will get: how many messages it holds.", Kind: metrics.KindGauge}
		truncated := false
		for _, t := range topics {
			partitions += len(t.Ends)
			for i, e := range t.Ends {
				if len(ends.Samples) >= MaxSeries {
					truncated = true
					continue
				}
				ends.Samples = append(ends.Samples, metrics.Sample{
					Labels: []metrics.Label{{Name: "topic", Value: t.Name}, {Name: "partition", Value: strconv.Itoa(i)}},
					Value:  float64(e),
				})
			}
		}
		lag := metrics.Family{Name: "raftlog_consumer_group_lag", Help: "Messages a consumer group has not read, per partition it has committed in.", Kind: metrics.KindGauge}
		for _, g := range src.Groups() {
			for _, p := range g.Partitions {
				if len(lag.Samples) >= MaxSeries {
					truncated = true
					continue
				}
				lag.Samples = append(lag.Samples, metrics.Sample{
					Labels: []metrics.Label{{Name: "group", Value: g.Group}, {Name: "topic", Value: p.Topic}, {Name: "partition", Value: strconv.Itoa(p.Partition)}},
					Value:  float64(p.Lag()),
				})
			}
		}
		trunc := 0.0
		if truncated {
			trunc = 1
		}
		return []metrics.Family{
			gaugeFamily("raftlog_topics", "Topics on this node.", float64(len(topics))),
			gaugeFamily("raftlog_partitions", "Partitions on this node, over all topics.", float64(partitions)),
			ends, lag,
			gaugeFamily("raftlog_series_truncated", "1 if a per-partition or per-group metric left out series because there were more than the limit.", trunc),
		}
	})
}
