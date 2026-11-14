package chaos

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scenario is one chaos run.
type scenario struct {
	nodes   int
	clients int
	faults  []fault
	extra   []string // more flags for the daemons
}

// chaosSeed returns the seed for the random choices, from CHAOS_SEED if set.
// The faults themselves depend on timing, so a seed does not replay a run
// exactly; it only makes the choices (which node, which fault) repeatable.
func chaosSeed(t *testing.T) int64 {
	if s := os.Getenv("CHAOS_SEED"); s != "" {
		seed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("CHAOS_SEED=%q: %v", s, err)
		}
		return seed
	}
	return time.Now().UnixNano() % 1_000_000
}

// chaosDuration is how long faults and writes go on, from CHAOS_SECONDS if set.
func chaosDuration(def time.Duration) time.Duration {
	if s := os.Getenv("CHAOS_SECONDS"); s != "" {
		secs, err := strconv.Atoi(s)
		if err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return def
}

func runScenario(t *testing.T, s scenario, duration time.Duration, minAcked int) {
	t.Helper()
	if testing.Short() {
		t.Skip("chaos tests start real processes and take a while")
	}
	seed := chaosSeed(t)
	t.Logf("seed %d (rerun the choices with CHAOS_SEED=%d), faults for %v", seed, seed, duration)

	c := newCluster(t, s.nodes, s.extra...)
	for id := 1; id <= s.nodes; id++ {
		c.mustStart(id)
	}
	w := newWorkload(t, c, "chaos", 2)
	t.Cleanup(w.close)
	w.createTopic(30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	waitWriters := w.runWriters(ctx, s.clients, seed)
	waitReader := w.runReader(ctx, seed+1)
	var nem *nemesis
	if len(s.faults) > 0 {
		nem = newNemesis(t, c, w, seed+2, s.faults)
		nem.run(ctx)
	} else {
		<-ctx.Done()
	}
	waitWriters()
	waitReader()
	if nem != nil {
		nem.stop()
	}

	// The faults are over. Give the cluster time to recover completely, then
	// compare what the clients were told with what it holds.
	if err := w.waitSettled(90 * time.Second); err != nil {
		if nem != nil {
			t.Log(nem.report())
		}
		t.Fatal(err)
	}
	final, violations := w.finalState()
	h := w.history()
	violations = append(violations, Check(h, final)...)

	t.Log(w.summary())
	for p := 0; p < w.parts; p++ {
		t.Logf("partition %d holds %d messages on every node", p, len(final[p]))
	}
	if nem != nil {
		t.Log(nem.report())
	}

	if len(violations) > 0 {
		shown := violations
		if len(shown) > 20 {
			shown = shown[:20]
		}
		lines := make([]string, len(shown))
		for i, v := range shown {
			lines[i] = "  " + v.String()
		}
		t.Fatalf("%d promises were broken (first %d):\n%s", len(violations), len(shown), strings.Join(lines, "\n"))
	}

	acked := 0
	for _, wr := range h.Writes {
		if wr.Outcome == Acked {
			acked++
		}
	}
	if acked < minAcked {
		t.Fatalf("only %d writes were acknowledged (want at least %d): the run proves too little", acked, minAcked)
	}
}

// With no faults at all, the harness itself is under test: every write must go
// through, and the checker must have nothing to say.
func TestChaosHarnessWithoutFaults(t *testing.T) {
	runScenario(t, scenario{nodes: 3, clients: 3}, chaosDuration(5*time.Second), 50)
}

func TestChaosProcessCrashes(t *testing.T) {
	runScenario(t, scenario{
		nodes: 3, clients: 4,
		faults: []fault{faultCrash, faultKillLeader},
	}, chaosDuration(12*time.Second), 20)
}

func TestChaosNetworkFaults(t *testing.T) {
	runScenario(t, scenario{
		nodes: 5, clients: 4,
		faults: []fault{faultIsolate, faultSplit, faultAsymmetric, faultDelay},
	}, chaosDuration(12*time.Second), 20)
}

func TestChaosStaleNodesReturn(t *testing.T) {
	// A node that was cut off for a while comes back with a high term and an old
	// log. The cluster must not let it take over and undo committed writes.
	runScenario(t, scenario{
		nodes: 5, clients: 4,
		faults: []fault{faultStaleNode},
	}, chaosDuration(15*time.Second), 20)
}

func TestChaosEverythingWithSnapshots(t *testing.T) {
	// Snapshots are taken every few entries and the log is cut, so nodes that
	// come back after a crash or a partition must install snapshots.
	runScenario(t, scenario{
		nodes: 5, clients: 6,
		faults: []fault{faultCrash, faultKillLeader, faultIsolate, faultSplit, faultAsymmetric, faultDelay},
		extra:  []string{"-snapshot-every", "15", "-snapshot-trailing", "3"},
	}, chaosDuration(15*time.Second), 20)
}
