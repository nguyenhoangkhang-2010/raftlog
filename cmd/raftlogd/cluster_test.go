package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

// These tests run real raftlogd nodes, each with its own data directory, in this
// process and talk to them over gRPC like a client would. Ticks are short so
// that elections take a fraction of a second.
const clusterWait = 20 * time.Second

type testNode struct {
	id     int
	addr   string
	dir    string
	cancel context.CancelFunc
	done   chan error
	// metrics is the address the node serves /metrics on; empty if it does not.
	metrics string
}

// enableMetrics gives every node an address for its metrics. Call it before the
// nodes start.
func (c *testCluster) enableMetrics() {
	c.t.Helper()
	for _, n := range c.nodes {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			c.t.Fatal(err)
		}
		n.metrics = l.Addr().String()
		_ = l.Close()
	}
}

type testCluster struct {
	t     *testing.T
	nodes []*testNode
	peers string
	extra []string
}

// newTestCluster reserves addresses and directories for size nodes; it does not
// start them.
func newTestCluster(t *testing.T, size int, extra ...string) *testCluster {
	t.Helper()
	c := &testCluster{t: t, extra: extra}
	var peers []string
	for i := 1; i <= size; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close() // free it again; the node binds it when it starts
		c.nodes = append(c.nodes, &testNode{id: i, addr: addr, dir: filepath.Join(t.TempDir(), fmt.Sprintf("node%d", i))})
		peers = append(peers, fmt.Sprintf("%d=%s", i, addr))
	}
	c.peers = strings.Join(peers, ",")
	t.Cleanup(func() {
		for _, n := range c.nodes {
			c.stop(n)
		}
	})
	return c
}

func (c *testCluster) node(id int) *testNode { return c.nodes[id-1] }

// start launches a node, retrying while the operating system still holds its port.
func (c *testCluster) start(n *testNode) {
	c.t.Helper()
	args := append([]string{
		"-node-id", strconv.Itoa(n.id), "-peers", c.peers,
		"-listen", n.addr, "-data-dir", n.dir,
		"-tick-interval", "10ms", "-election-ticks", "10", "-heartbeat-ticks", "2",
	}, c.extra...)
	if n.metrics != "" {
		args = append(args, "-metrics-listen", n.metrics)
	}

	deadline := time.Now().Add(clusterWait)
	for {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, args, func(string) string { return "" }, io.Discard)
		}()
		// A node that cannot bind its port fails at once; one that started stays up.
		select {
		case err := <-done:
			cancel()
			if time.Now().After(deadline) {
				c.t.Fatalf("node %d would not start: %v", n.id, err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		case <-time.After(300 * time.Millisecond):
		}
		n.cancel, n.done = cancel, done
		return
	}
}

// stop shuts a node down and waits for it. It is safe on a stopped node.
func (c *testCluster) stop(n *testNode) {
	if n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(clusterWait):
		c.t.Errorf("node %d did not stop", n.id)
	}
	n.cancel = nil
}

func (c *testCluster) startAll() {
	for _, n := range c.nodes {
		c.start(n)
	}
}

func dial(t *testing.T, addr string) pb.BrokerClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewBrokerClient(conn)
}

func rpcCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// statusOf asks a node for its Raft status; it returns nil if the node cannot answer.
func statusOf(c pb.BrokerClient) *pb.NodeStatusResponse {
	ctx, cancel := rpcCtx()
	defer cancel()
	resp, err := c.NodeStatus(ctx, &pb.NodeStatusRequest{})
	if err != nil {
		return nil
	}
	return resp
}

// waitForLeader waits until exactly one running node is leader and all the other
// running nodes agree on who it is. It returns the leader's node.
func (c *testCluster) waitForLeader() *testNode {
	c.t.Helper()
	deadline := time.Now().Add(clusterWait)
	for time.Now().Before(deadline) {
		var leader *testNode
		leaders := 0
		for _, n := range c.nodes {
			if n.cancel == nil {
				continue
			}
			if st := statusOf(dial(c.t, n.addr)); st != nil && st.Role == "leader" {
				leader = n
				leaders++
			}
		}
		if leaders == 1 {
			agreed := true
			for _, n := range c.nodes {
				if n.cancel == nil || n == leader {
					continue
				}
				if st := statusOf(dial(c.t, n.addr)); st == nil || st.LeaderId != uint64(leader.id) {
					agreed = false
				}
			}
			if agreed {
				return leader
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatal("no stable leader before the deadline")
	return nil
}

// write runs a write request against whoever is the leader, retrying while the
// leadership is changing, as a real client would.
func (c *testCluster) write(f func(ctx context.Context, cl pb.BrokerClient) error) {
	c.t.Helper()
	deadline := time.Now().Add(clusterWait)
	var last error
	for time.Now().Before(deadline) {
		leader := c.waitForLeader()
		ctx, cancel := rpcCtx()
		err := f(ctx, dial(c.t, leader.addr))
		cancel()
		if err == nil {
			return
		}
		if code := status.Code(err); code != codes.FailedPrecondition && code != codes.Unavailable {
			c.t.Fatalf("write failed: %v", err)
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatalf("write did not succeed in time: %v", last)
}

func (c *testCluster) createTopic(name string, partitions uint32) {
	c.t.Helper()
	c.write(func(ctx context.Context, cl pb.BrokerClient) error {
		_, err := cl.CreateTopic(ctx, &pb.CreateTopicRequest{Topic: name, Partitions: partitions})
		if status.Code(err) == codes.AlreadyExists {
			return nil // a retry of a request that had already succeeded
		}
		return err
	})
}

// produce sends the values to one partition and returns the offset of the first.
func (c *testCluster) produce(topic string, partition uint32, values ...string) uint64 {
	c.t.Helper()
	msgs := make([]*pb.Message, len(values))
	for i, v := range values {
		msgs[i] = &pb.Message{Key: []byte("k"), Value: []byte(v)}
	}
	var base uint64
	c.write(func(ctx context.Context, cl pb.BrokerClient) error {
		resp, err := cl.Produce(ctx, &pb.ProduceRequest{Topic: topic, Partition: &partition, Messages: msgs})
		if err == nil {
			base = resp.BaseOffset
		}
		return err
	})
	return base
}

// fetchAll reads every message of a partition from one node.
func fetchAll(t *testing.T, addr, topic string, partition uint32) ([]string, error) {
	t.Helper()
	cl := dial(t, addr)
	var out []string
	for offset := uint64(0); ; {
		ctx, cancel := rpcCtx()
		resp, err := cl.Fetch(ctx, &pb.FetchRequest{Topic: topic, Partition: partition, Offset: offset, MaxMessages: 500})
		cancel()
		if err != nil {
			return nil, err
		}
		if len(resp.Messages) == 0 {
			return out, nil
		}
		for _, m := range resp.Messages {
			if m.Offset != offset {
				return nil, fmt.Errorf("offset %d where %d was expected", m.Offset, offset)
			}
			out = append(out, string(m.Value))
			offset++
		}
	}
}

// waitForMessages waits until the node holds exactly want in the partition.
func (c *testCluster) waitForMessages(n *testNode, topic string, partition uint32, want []string) {
	c.t.Helper()
	deadline := time.Now().Add(clusterWait)
	var got []string
	var err error
	for time.Now().Before(deadline) {
		got, err = fetchAll(c.t, n.addr, topic, partition)
		if err == nil && strings.Join(got, ",") == strings.Join(want, ",") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("node %d holds %d messages (err %v), want %d:\n got  %v\n want %v", n.id, len(got), err, len(want), got, want)
}

func values(prefix string, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("%s-%03d", prefix, i))
	}
	return out
}

// --- tests -------------------------------------------------------------------

func TestWritesGoThroughTheLeaderAndReachEveryNode(t *testing.T) {
	c := newTestCluster(t, 3)
	c.startAll()
	c.waitForLeader()

	c.createTopic("events", 2)
	want := values("m", 0, 25)
	if base := c.produce("events", 1, want[:10]...); base != 0 {
		t.Fatalf("first batch got base offset %d, want 0", base)
	}
	if base := c.produce("events", 1, want[10:]...); base != 10 {
		t.Fatalf("second batch got base offset %d, want 10", base)
	}

	for _, n := range c.nodes { // every node, followers included, serves the data
		c.waitForMessages(n, "events", 1, want)
		c.waitForMessages(n, "events", 0, nil)
	}
}

func TestAFollowerRefusesWritesAndNamesTheLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	c.startAll()
	leader := c.waitForLeader()
	c.createTopic("events", 1)

	for _, n := range c.nodes {
		if n == leader {
			continue
		}
		var trailer metadata.MD
		ctx, cancel := rpcCtx()
		_, err := dial(t, n.addr).Produce(ctx, &pb.ProduceRequest{
			Topic: "events", Messages: []*pb.Message{{Value: []byte("x")}},
		}, grpc.Trailer(&trailer))
		cancel()

		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("follower %d answered %v, want FailedPrecondition", n.id, err)
		}
		if got := trailer.Get(server.LeaderIDKey); len(got) != 1 || got[0] != strconv.Itoa(leader.id) {
			t.Fatalf("follower %d names leader %v, want %d", n.id, got, leader.id)
		}
		if got := trailer.Get(server.LeaderAddrKey); len(got) != 1 || got[0] != leader.addr {
			t.Fatalf("follower %d gives leader address %v, want %s", n.id, got, leader.addr)
		}
	}
	// Nothing was written by the refused requests.
	c.waitForMessages(leader, "events", 0, nil)
}

func TestAcknowledgedMessagesSurviveTheLossOfTheLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	c.startAll()
	c.waitForLeader()
	c.createTopic("events", 1)

	before := values("a", 0, 15)
	c.produce("events", 0, before...)

	old := c.waitForLeader()
	c.stop(old)
	c.waitForLeader() // a new leader among the survivors

	after := values("b", 0, 15)
	c.produce("events", 0, after...)
	want := append(append([]string(nil), before...), after...)
	for _, n := range c.nodes {
		if n != old {
			c.waitForMessages(n, "events", 0, want)
		}
	}

	// The old leader comes back with its own data and catches up.
	c.start(old)
	c.waitForMessages(old, "events", 0, want)
}

func TestRestartingEveryNodeKeepsEachMessageExactlyOnce(t *testing.T) {
	// After a restart Raft replays its log. The state machine must recognise the
	// entries it applied before the restart, or every message would be stored twice.
	c := newTestCluster(t, 3)
	c.startAll()
	c.waitForLeader()
	c.createTopic("events", 1)
	want := values("m", 0, 30)
	c.produce("events", 0, want[:10]...)
	c.produce("events", 0, want[10:30]...)
	for _, n := range c.nodes {
		c.waitForMessages(n, "events", 0, want)
	}

	for _, n := range c.nodes {
		c.stop(n)
	}
	c.startAll()
	c.waitForLeader()

	for _, n := range c.nodes {
		c.waitForMessages(n, "events", 0, want) // 30 messages, not 60
	}
	// Writing continues from where it was.
	more := values("m", 30, 35)
	if base := c.produce("events", 0, more...); base != 30 {
		t.Fatalf("base offset after the restart = %d, want 30", base)
	}
	want = append(want, more...)
	for _, n := range c.nodes {
		c.waitForMessages(n, "events", 0, want)
	}
}

func TestConsumerOffsetsAreReplicated(t *testing.T) {
	c := newTestCluster(t, 3)
	c.startAll()
	c.waitForLeader()
	c.createTopic("events", 1)
	c.produce("events", 0, values("m", 0, 8)...)

	c.write(func(ctx context.Context, cl pb.BrokerClient) error {
		_, err := cl.CommitOffset(ctx, &pb.CommitOffsetRequest{Group: "g1", Topic: "events", Offset: 5})
		return err
	})

	for _, n := range c.nodes {
		deadline := time.Now().Add(clusterWait)
		for {
			ctx, cancel := rpcCtx()
			resp, err := dial(t, n.addr).FetchOffset(ctx, &pb.FetchOffsetRequest{Group: "g1", Topic: "events"})
			cancel()
			if err == nil && resp.Committed && resp.Offset == 5 && resp.EndOffset == 8 && resp.Lag == 3 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("node %d: %+v, err %v; want committed offset 5, end 8, lag 3", n.id, resp, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestANodeThatFellBehindIsRebuiltFromASnapshot(t *testing.T) {
	// Every 10 applied entries a node snapshots all broker data and cuts its log,
	// keeping nothing below the snapshot. Node 3 is down meanwhile, so when it
	// returns the entries it missed no longer exist and it must install a snapshot.
	c := newTestCluster(t, 3, "-snapshot-every", "10", "-snapshot-trailing", "0")
	c.start(c.node(1))
	c.start(c.node(2))
	c.waitForLeader()
	c.createTopic("events", 2)

	var want0, want1 []string
	for batch := 0; batch < 12; batch++ {
		v0 := values(fmt.Sprintf("p0-%02d", batch), 0, 3)
		v1 := values(fmt.Sprintf("p1-%02d", batch), 0, 3)
		c.produce("events", 0, v0...)
		c.produce("events", 1, v1...)
		want0, want1 = append(want0, v0...), append(want1, v1...)
	}
	c.write(func(ctx context.Context, cl pb.BrokerClient) error {
		_, err := cl.CommitOffset(ctx, &pb.CommitOffsetRequest{Group: "g1", Topic: "events", Partition: 1, Offset: 7})
		return err
	})

	leader := c.waitForLeader()
	deadline := time.Now().Add(clusterWait)
	for {
		if st := statusOf(dial(t, leader.addr)); st != nil && st.SnapshotIndex > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the leader never took a snapshot")
		}
		time.Sleep(20 * time.Millisecond)
	}

	c.start(c.node(3))
	late := c.node(3)
	c.waitForMessages(late, "events", 0, want0)
	c.waitForMessages(late, "events", 1, want1)

	deadline = time.Now().Add(clusterWait)
	for {
		st := statusOf(dial(t, late.addr))
		ctx, cancel := rpcCtx()
		off, err := dial(t, late.addr).FetchOffset(ctx, &pb.FetchOffsetRequest{Group: "g1", Topic: "events", Partition: 1})
		cancel()
		if st != nil && st.SnapshotIndex > 0 && err == nil && off.Committed && off.Offset == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late node: status %+v, offset %+v, err %v; want a snapshot and the committed offset 7", st, off, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheClientLibraryRidesThroughALeaderFailover(t *testing.T) {
	// A program that uses the client library does nothing special about failures:
	// it keeps calling Produce. The library follows the leader and waits out the
	// election.
	c := newTestCluster(t, 3)
	c.startAll()
	leader := c.waitForLeader()

	addrs := make([]string, len(c.nodes))
	for i, n := range c.nodes {
		addrs[i] = n.addr
	}
	cl, err := client.New(addrs, client.WithRetryPause(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	call := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 30*time.Second)
	}
	ctx, cancel := call()
	if err := cl.CreateTopic(ctx, "events", 1); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	cancel()

	var sent []string
	produce := func(from, to int) {
		for i := from; i < to; i++ {
			v := fmt.Sprintf("m-%03d", i)
			ctx, cancel := call()
			_, err := cl.Produce(ctx, "events", []client.Message{{Key: []byte("k"), Value: []byte(v)}}, client.ToPartition(0))
			cancel()
			if err != nil {
				t.Fatalf("Produce(%s): %v", v, err)
			}
			sent = append(sent, v)
		}
	}

	produce(0, 20)
	c.stop(leader) // the leader dies; the next Produce calls must still succeed
	produce(20, 40)

	// Read everything back through the client, from whichever nodes answer.
	var got []string
	for offset := uint64(0); ; {
		ctx, cancel := call()
		recs, err := cl.Fetch(ctx, "events", 0, offset, 1000)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			break
		}
		for _, r := range recs {
			got = append(got, string(r.Value))
		}
		offset = recs[len(recs)-1].Offset + 1
	}

	// Delivery is at-least-once: a write that was applied just as the leader died
	// may have been repeated by the library. So every message must be there and
	// in order, but a repeat is allowed.
	next := 0
	for _, v := range got {
		if next < len(sent) && v == sent[next] {
			next++
		}
	}
	if next != len(sent) {
		t.Fatalf("the log does not contain the %d acknowledged messages in order: it holds %d messages, matched %d", len(sent), len(got), next)
	}
	if len(got) != len(sent) {
		t.Logf("%d message(s) were stored twice because a retry followed an ambiguous failure (allowed: at-least-once)", len(got)-len(sent))
	}

	// The survivors agree on a leader, which is not the node that was stopped.
	roles := map[string]int{}
	for _, n := range c.nodes {
		if n == leader {
			continue
		}
		ctx, cancel := call()
		st, err := cl.NodeStatus(ctx, n.addr)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		roles[st.Role]++
	}
	if roles["leader"] != 1 || roles["follower"] != 1 {
		t.Fatalf("roles among the survivors = %v, want one leader and one follower", roles)
	}
}

func TestAWaitingFetchOnAFollowerIsWokenByAReplicatedWrite(t *testing.T) {
	// The reader is on a follower and the writer on the leader: the message
	// travels through the Raft log before the follower applies it, and only then
	// may the waiting Fetch return.
	c := newTestCluster(t, 3)
	c.startAll()
	leader := c.waitForLeader()
	var follower *testNode
	for _, n := range c.nodes {
		if n != leader {
			follower = n
			break
		}
	}
	c.createTopic("events", 1)

	reader := dial(t, follower.addr)
	// Wait until the follower has applied the topic.
	deadline := time.Now().Add(clusterWait)
	for {
		ctx, cancel := rpcCtx()
		_, err := reader.Fetch(ctx, &pb.FetchRequest{Topic: "events", MaxMessages: 1})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the follower never learned the topic: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	type result struct {
		resp *pb.FetchResponse
		err  error
	}
	got := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := reader.Fetch(ctx, &pb.FetchRequest{Topic: "events", MaxMessages: 5, MaxWaitMs: 20_000})
		got <- result{resp, err}
	}()

	select {
	case r := <-got:
		t.Fatalf("returned before anything was written: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	c.produce("events", 0, "through-the-log")
	select {
	case r := <-got:
		if r.err != nil || len(r.resp.Messages) != 1 || string(r.resp.Messages[0].Value) != "through-the-log" {
			t.Fatalf("Fetch = %+v, %v", r.resp, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting Fetch was not woken by the write")
	}
}

func TestSnapshotsDoNotBreakARestart(t *testing.T) {
	// Restarting nodes that hold both a snapshot and a log suffix must neither
	// lose nor duplicate data.
	c := newTestCluster(t, 3, "-snapshot-every", "7", "-snapshot-trailing", "2")
	c.startAll()
	c.waitForLeader()
	c.createTopic("events", 1)
	var want []string
	for batch := 0; batch < 10; batch++ {
		v := values(fmt.Sprintf("b%02d", batch), 0, 4)
		c.produce("events", 0, v...)
		want = append(want, v...)
	}
	for _, n := range c.nodes {
		c.waitForMessages(n, "events", 0, want)
	}

	for _, n := range c.nodes {
		c.stop(n)
	}
	c.startAll()
	c.waitForLeader()
	for _, n := range c.nodes {
		c.waitForMessages(n, "events", 0, want)
	}
	more := values("after", 0, 3)
	c.produce("events", 0, more...)
	want = append(want, more...)
	for _, n := range c.nodes {
		c.waitForMessages(n, "events", 0, want)
	}
}
