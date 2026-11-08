package chaos

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/console"
)

// These tests run the cluster console's own machinery on real raftlogd processes:
// the supervisor that kills and starts nodes, the observer that sees the failover,
// and the writer whose acknowledged writes are checked afterwards.

func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l.Addr().String())
		_ = l.Close()
	}
	return out
}

type consoleRig struct {
	sup   *console.Supervisor
	obs   *console.Observer
	load  *console.Load
	cl    *client.Client
	nodes []console.NodeSpec
}

// newConsoleRig starts three real nodes under a supervisor, with quick Raft timings.
func newConsoleRig(t *testing.T) *consoleRig {
	t.Helper()
	addrs := freeAddrs(t, 3)
	var nodes []console.NodeSpec
	for i, a := range addrs {
		nodes = append(nodes, console.NodeSpec{ID: i + 1, Addr: a})
	}
	sup, err := console.NewSupervisor(console.SupervisorConfig{
		Binary: daemon(t), DataDir: filepath.Join(t.TempDir(), "console"), Nodes: nodes,
		ExtraArgs: []string{"-tick-interval", "20ms", "-election-ticks", "10", "-heartbeat-ticks", "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)
	if err := sup.StartAll(); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(addrs, client.WithRetryPause(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	obs := console.NewObserver(cl, nodes, func(id int) (bool, bool) { return true, sup.Running(id) }, 500*time.Millisecond)
	load, err := console.NewLoad(cl, console.LoadConfig{
		Topic: "console-test", Partitions: 2, Workers: 8, RequestTimeout: 20 * time.Second, MaxRate: 1000, MaxTracked: 100_000, Window: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(load.Stop)
	return &consoleRig{sup: sup, obs: obs, load: load, cl: cl, nodes: nodes}
}

// settle polls until cond holds on the summary.
func (r *consoleRig) settle(t *testing.T, what string, cond func(console.Summary, []console.NodeView) bool) console.Summary {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r.obs.Poll(context.Background())
		nodes, sum, _ := r.obs.Snapshot()
		if cond(sum, nodes) {
			return sum
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s (summary %+v)", what, sum)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestTheSupervisorKillsAndRestartsRealNodesAndTheObserverSeesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	r := newConsoleRig(t)
	first := r.settle(t, "a leader and three nodes up", func(s console.Summary, _ []console.NodeView) bool { return s.CanCommit && s.Up == 3 })
	for id := 1; id <= 3; id++ {
		if !r.sup.Running(id) {
			t.Fatalf("node %d is not running", id)
		}
	}

	// Kill the leader: a process that is gone, a new leader in a higher term.
	if err := r.sup.Kill(first.LeaderID); err != nil {
		t.Fatal(err)
	}
	if r.sup.Running(first.LeaderID) {
		t.Fatal("the node still runs after Kill")
	}
	if err := r.sup.Kill(first.LeaderID); err != nil {
		t.Fatalf("killing a node twice: %v", err)
	}
	second := r.settle(t, "a new leader among the two that are left", func(s console.Summary, _ []console.NodeView) bool {
		return s.CanCommit && s.Up == 2 && s.LeaderID != first.LeaderID && s.LeaderID != 0
	})
	if second.Term <= first.Term {
		t.Fatalf("the new leader's term %d is not above the old one %d", second.Term, first.Term)
	}

	// The killed node returns with the data it had and follows the leader.
	if err := r.sup.Start(first.LeaderID); err != nil {
		t.Fatal(err)
	}
	third := r.settle(t, "all three nodes up again", func(s console.Summary, _ []console.NodeView) bool { return s.Up == 3 && s.CanCommit })

	// A node that comes back must not disturb a healthy cluster: for a few seconds
	// the leader and the term stay as they are.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		r.obs.Poll(context.Background())
		_, sum, _ := r.obs.Snapshot()
		if sum.LeaderID != third.LeaderID || sum.Term != third.Term {
			t.Fatalf("the returned node caused an election: leader %d term %d, was leader %d term %d", sum.LeaderID, sum.Term, third.LeaderID, third.Term)
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Closing the supervisor ends every process.
	r.sup.Close()
	for id := 1; id <= 3; id++ {
		if r.sup.Running(id) {
			t.Fatalf("node %d runs after Close", id)
		}
	}
}

func TestNoAcknowledgedWriteIsLostWhenTheLeaderIsKilledUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	r := newConsoleRig(t)
	first := r.settle(t, "a leader", func(s console.Summary, _ []console.NodeView) bool { return s.CanCommit && s.Up == 3 })

	if err := r.load.Start(100); err != nil {
		t.Fatal(err)
	}
	waitUntilLoad(t, r.load, "writes to be acknowledged", func(s console.LoadStats) bool { return s.Acked >= 100 })

	// Kill the leader while the writes go on, then bring it back and kill the next leader.
	victims := []int{first.LeaderID}
	for round := 0; round < 2; round++ {
		victim := victims[len(victims)-1]
		if err := r.sup.Kill(victim); err != nil {
			t.Fatal(err)
		}
		sum := r.settle(t, "a new leader", func(s console.Summary, _ []console.NodeView) bool {
			return s.CanCommit && s.LeaderID != victim && s.LeaderID != 0
		})
		before := r.load.Stats().Acked
		waitUntilLoad(t, r.load, "writes to carry on through the new leader", func(s console.LoadStats) bool { return s.Acked >= before+100 })
		if err := r.sup.Start(victim); err != nil {
			t.Fatal(err)
		}
		r.settle(t, "all nodes up", func(s console.Summary, _ []console.NodeView) bool { return s.Up == 3 && s.CanCommit })
		victims = append(victims, sum.LeaderID)
	}

	r.load.Stop()
	waitUntilLoad(t, r.load, "the writes in flight to finish", func(s console.LoadStats) bool { return s.Sent == s.Acked+s.Failed })

	vctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	v, err := r.load.Verify(vctx, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := r.load.Stats()
	t.Logf("%d writes acknowledged, %d failed, %d not sent; verification: %+v", s.Acked, s.Failed, s.Dropped, v)
	if v.Missing != 0 {
		t.Fatalf("%d of %d acknowledged writes are missing, for example %v", v.Missing, v.Acked, v.MissingExamples)
	}
	if v.Acked < 300 || s.Acked < 300 {
		t.Fatalf("only %d writes were acknowledged: the run proves too little", s.Acked)
	}
}

func waitUntilLoad(t *testing.T, l *console.Load, what string, cond func(console.LoadStats) bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond(l.Stats()) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s (%+v)", what, l.Stats())
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = fmt.Sprint
}
