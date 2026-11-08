package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

// scrapeNode fetches /metrics of a node and returns the text.
func scrapeNode(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("scrape %s: %v", addr, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("scrape %s: %d", addr, resp.StatusCode)
	}
	return string(b)
}

// metricValue returns the value of the first sample whose line starts with prefix.
func metricValue(out, prefix string) (float64, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix+" ")), 64)
			return v, err == nil
		}
	}
	return 0, false
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAStandaloneNodeServesItsMetrics(t *testing.T) {
	var out lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-listen", "127.0.0.1:0", "-data-dir", filepath.Join(t.TempDir(), "d"), "-metrics-listen", "127.0.0.1:0"},
			func(string) string { return "" }, &out)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the node did not stop")
		}
	}()

	grpcRE := regexp.MustCompile(`listening on (\S+),`)
	metricsRE := regexp.MustCompile(`metrics on http://(\S+)/metrics`)
	var grpcAddr, metricsAddr string
	for deadline := time.Now().Add(10 * time.Second); grpcAddr == "" || metricsAddr == ""; {
		if m := grpcRE.FindStringSubmatch(out.String()); m != nil {
			grpcAddr = m[1]
		}
		if m := metricsRE.FindStringSubmatch(out.String()); m != nil {
			metricsAddr = m[1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node did not announce its addresses: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	c := dial(t, grpcAddr)
	rctx, rcancel := rpcCtx()
	defer rcancel()
	if _, err := c.CreateTopic(rctx, &pb.CreateTopicRequest{Topic: "events", Partitions: 2}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := c.Produce(rctx, &pb.ProduceRequest{Topic: "events", Messages: []*pb.Message{{Value: []byte("12345")}}}); err != nil {
			t.Fatal(err)
		}
	}

	m := scrapeNode(t, metricsAddr)
	for _, want := range []string{
		`raftlog_requests_total{method="Broker/Produce",code="OK"} 4`,
		"raftlog_produced_messages_total 4\n",
		"raftlog_produced_bytes_total 20\n",
		"raftlog_topics 1\n", "raftlog_partitions 2\n",
		`raftlog_build_info{version="dev"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("the metrics lack %q:\n%s", want, m)
		}
	}
	// A standalone node has no Raft state to report.
	if strings.Contains(m, "raftlog_raft_term") {
		t.Errorf("a standalone node reports Raft metrics:\n%s", m)
	}
	// And the health endpoint answers.
	resp, err := http.Get("http://" + metricsAddr + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()
}

func TestANodeWithoutAnAddressServesNoMetrics(t *testing.T) {
	var out lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-listen", "127.0.0.1:0", "-data-dir", filepath.Join(t.TempDir(), "d")}, func(string) string { return "" }, &out)
	}()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "listening on"); {
		if time.Now().After(deadline) {
			t.Fatal("the node did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if strings.Contains(out.String(), "metrics on") {
		t.Fatalf("metrics were started without an address: %q", out.String())
	}
}

func TestTheMetricsOfARealClusterFollowTheLeaderAndTheFailover(t *testing.T) {
	c := newTestCluster(t, 3)
	c.enableMetrics()
	c.startAll()
	leader := c.waitForLeader()
	c.createTopic("events", 1)
	c.produce("events", 0, "a", "b", "c")

	// The leader says so, the followers say they are followers, and the leader knows
	// how far each follower has got, which ends at its own log.
	waitUntil(t, "the leader's metrics to show both followers caught up", func() bool {
		m := scrapeNode(t, leader.metrics)
		if v, ok := metricValue(m, "raftlog_raft_is_leader"); !ok || v != 1 {
			return false
		}
		last, ok := metricValue(m, "raftlog_raft_last_index")
		if !ok || last == 0 {
			return false
		}
		for _, n := range c.nodes {
			if n == leader {
				continue
			}
			lag, ok := metricValue(m, fmt.Sprintf(`raftlog_raft_follower_lag_entries{peer="%d"}`, n.id))
			if !ok || lag != 0 {
				return false
			}
		}
		return true
	})
	var wins float64
	for _, n := range c.nodes {
		m := scrapeNode(t, n.metrics)
		isLeader, _ := metricValue(m, "raftlog_raft_is_leader")
		if (n == leader) != (isLeader == 1) {
			t.Fatalf("node %d: is_leader = %v, but the leader is node %d", n.id, isLeader, leader.id)
		}
		w, _ := metricValue(m, "raftlog_raft_elections_won_total")
		wins += w
		if topics, _ := metricValue(m, "raftlog_topics"); topics != 1 {
			t.Fatalf("node %d reports %v topics", n.id, topics)
		}
	}
	if wins != 1 {
		t.Fatalf("the cluster has won %v elections, want exactly 1", wins)
	}
	lm := scrapeNode(t, leader.metrics)
	if v, _ := metricValue(lm, "raftlog_produced_messages_total"); v != 3 {
		t.Fatalf("the leader counted %v produced messages, want 3", v)
	}
	oldTerm, _ := metricValue(lm, "raftlog_raft_term")

	// Kill the leader: the survivor that wins reports a higher term and a won election.
	c.stop(leader)
	second := c.waitForLeader()
	waitUntil(t, "the new leader to report itself", func() bool {
		m := scrapeNode(t, second.metrics)
		term, _ := metricValue(m, "raftlog_raft_term")
		isLeader, _ := metricValue(m, "raftlog_raft_is_leader")
		won, _ := metricValue(m, "raftlog_raft_elections_won_total")
		return isLeader == 1 && term > oldTerm && won >= 1
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(clusterWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
