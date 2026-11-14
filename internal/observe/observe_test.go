package observe

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/metrics"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("the metrics lack %q:\n%s", w, out)
		}
	}
}

func TestTheServerIsCountedAndTimedThroughTheInterceptor(t *testing.T) {
	reg := metrics.NewRegistry()
	node := New(reg, "v-test")

	b, err := broker.Open(filepath.Join(t.TempDir(), "data"), broker.Options{MaxPartitions: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	srv, err := server.New(server.Local(b), server.Options{MaxFetchMessages: 10, MaxFetchWait: time.Second, Observer: node})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.UnaryInterceptor(node.UnaryInterceptor()))
	server.Register(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pb.NewBrokerClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.CreateTopic(ctx, &pb.CreateTopicRequest{Topic: "events", Partitions: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Produce(ctx, &pb.ProduceRequest{Topic: "events", Messages: []*pb.Message{{Key: []byte("k"), Value: []byte("12345")}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Fetch(ctx, &pb.FetchRequest{Topic: "events", MaxMessages: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(ctx, &pb.FetchRequest{Topic: "missing", MaxMessages: 10}); err == nil {
		t.Fatal("fetch from a missing topic succeeded")
	}

	out := scrape(t, reg)
	mustContain(t, out,
		`raftlog_requests_total{method="Broker/CreateTopic",code="OK"} 1`,
		`raftlog_requests_total{method="Broker/Produce",code="OK"} 3`,
		`raftlog_requests_total{method="Broker/Fetch",code="OK"} 1`,
		`raftlog_requests_total{method="Broker/Fetch",code="NotFound"} 1`,
		`raftlog_request_duration_seconds_count{method="Broker/Produce"} 3`,
		`raftlog_produced_messages_total 3`,
		`raftlog_produced_bytes_total 18`, // 3 x ("k" + "12345")
		`raftlog_fetched_messages_total 3`,
		`raftlog_build_info{version="v-test"} 1`,
		"go_goroutines ", "process_start_time_seconds ",
	)
	// The latency histogram has a +Inf bucket that holds every observation.
	mustContain(t, out, `raftlog_request_duration_seconds_bucket{method="Broker/Produce",le="+Inf"} 3`)
}

func TestShortMethodNames(t *testing.T) {
	cases := map[string]string{
		"/raftlog.v1.Broker/Produce": "Broker/Produce",
		"/raftlog.v1.Raft/Send":      "Raft/Send",
		"/other.Service/Method":      "other.Service/Method",
	}
	for in, want := range cases {
		if got := shortMethod(in); got != want {
			t.Errorf("shortMethod(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeRaft struct{ o raft.Observation }

func (f *fakeRaft) Observe() raft.Observation { return f.o }

func TestTheRaftMetricsAreReadWhenScraped(t *testing.T) {
	reg := metrics.NewRegistry()
	node := New(reg, "v")
	src := &fakeRaft{o: raft.Observation{
		Status:       raft.Status{ID: 1, Role: raft.Leader, Term: 7, Leader: 1, CommitIndex: 90, LastIndex: 100, SnapshotIndex: 50},
		Stats:        raft.Stats{Applied: 88, Campaigns: 3, Wins: 2, PreVotes: 5, QuorumLost: 1, Followers: []raft.FollowerProgress{{ID: 2, Match: 100, Next: 101}, {ID: 3, Match: 60, Next: 61}}},
		ApplyBacklog: 2, SnapshotsTaken: 4, SnapshotsInstalled: 1, MessagesDropped: 9,
		SendFailures: map[raft.NodeID]uint64{3: 12, 2: 1},
	}}
	node.WatchRaft(src)

	out := scrape(t, reg)
	mustContain(t, out,
		"raftlog_raft_term 7\n", "raftlog_raft_is_leader 1\n", `raftlog_raft_role{role="leader"} 1`, `raftlog_raft_role{role="follower"} 0`,
		"raftlog_raft_leader_id 1\n", "raftlog_raft_commit_index 90\n", "raftlog_raft_last_index 100\n", "raftlog_raft_applied_index 88\n",
		"raftlog_raft_snapshot_index 50\n", "raftlog_raft_apply_backlog 2\n",
		"raftlog_raft_elections_started_total 3\n", "raftlog_raft_elections_won_total 2\n",
		"raftlog_raft_prevote_rounds_total 5\n", "raftlog_raft_quorum_stepdowns_total 1\n",
		"raftlog_raft_snapshots_taken_total 4\n", "raftlog_raft_snapshots_installed_total 1\n", "raftlog_raft_messages_dropped_total 9\n",
		`raftlog_raft_send_failures_total{peer="2"} 1`, `raftlog_raft_send_failures_total{peer="3"} 12`,
		`raftlog_raft_follower_match_index{peer="3"} 60`,
		`raftlog_raft_follower_lag_entries{peer="2"} 0`, `raftlog_raft_follower_lag_entries{peer="3"} 40`,
		"# TYPE raftlog_raft_elections_won_total counter", "# TYPE raftlog_raft_term gauge",
	)

	// A follower reports no follower progress, and the role flips with the node.
	src.o = raft.Observation{Status: raft.Status{ID: 2, Role: raft.Follower, Term: 8, Leader: 1, LastIndex: 100}}
	out = scrape(t, reg)
	mustContain(t, out, "raftlog_raft_term 8\n", "raftlog_raft_is_leader 0\n", `raftlog_raft_role{role="follower"} 1`, `raftlog_raft_role{role="leader"} 0`)
	if strings.Contains(out, "raftlog_raft_follower_lag_entries{") {
		t.Fatalf("a follower reports follower lag:\n%s", out)
	}
}

type fakeBroker struct {
	topics []broker.TopicDescription
	groups []broker.GroupDescription
}

func (f fakeBroker) Describe() []broker.TopicDescription { return f.topics }
func (f fakeBroker) Groups() []broker.GroupDescription   { return f.groups }

func TestTheBrokerMetricsDescribeTopicsAndGroupLag(t *testing.T) {
	reg := metrics.NewRegistry()
	node := New(reg, "v")
	node.WatchBroker(fakeBroker{
		topics: []broker.TopicDescription{{Name: "orders", Ends: []uint64{10, 4}}, {Name: "audit", Ends: []uint64{0}}},
		groups: []broker.GroupDescription{{Group: "billing", Partitions: []broker.GroupPartition{{Topic: "orders", Partition: 1, Offset: 1, End: 4}}}},
	})
	out := scrape(t, reg)
	mustContain(t, out,
		"raftlog_topics 2\n", "raftlog_partitions 3\n",
		`raftlog_partition_end_offset{topic="orders",partition="0"} 10`, `raftlog_partition_end_offset{topic="orders",partition="1"} 4`,
		`raftlog_partition_end_offset{topic="audit",partition="0"} 0`,
		`raftlog_consumer_group_lag{group="billing",topic="orders",partition="1"} 3`,
		"raftlog_series_truncated 0\n",
	)
}

func TestVeryManyPartitionsAreCutOffAndSaid(t *testing.T) {
	reg := metrics.NewRegistry()
	node := New(reg, "v")
	ends := make([]uint64, MaxSeries+50)
	node.WatchBroker(fakeBroker{topics: []broker.TopicDescription{{Name: "huge", Ends: ends}}})
	out := scrape(t, reg)
	series := strings.Count(out, "raftlog_partition_end_offset{")
	if series != MaxSeries {
		t.Fatalf("%d per-partition series, want the limit %d", series, MaxSeries)
	}
	// The totals still count everything.
	mustContain(t, out, fmt.Sprintf("raftlog_partitions %d\n", MaxSeries+50), "raftlog_series_truncated 1\n")
}

// A compile-time check that the real types satisfy the interfaces.
var (
	_ RaftSource      = (*raft.Runner)(nil)
	_ BrokerSource    = (*broker.Broker)(nil)
	_ BrokerSource    = (*broker.Replicated)(nil)
	_ server.Observer = (*Node)(nil)
)
