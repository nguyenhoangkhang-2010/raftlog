package chaos

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// fault is a kind of trouble the nemesis may cause.
type fault int

const (
	// faultCrash kills a random node's process, and restarts it a little later.
	faultCrash fault = iota
	// faultKillLeader kills whichever node is leader at that moment.
	faultKillLeader
	// faultIsolate cuts one node off from all the others while it keeps running.
	faultIsolate
	// faultSplit cuts a group of nodes off from the rest (a partition).
	faultSplit
	// faultAsymmetric cuts the traffic between two nodes in one direction only.
	faultAsymmetric
	// faultDelay slows the traffic between two nodes.
	faultDelay
	// faultStaleNode cuts a node off for a long while, so that it falls behind
	// and keeps campaigning with a higher and higher term, then lets it return.
	faultStaleNode
)

// nemesis causes trouble, at random, while the clients work. Every fault is
// undone after a short random time. It never takes down more nodes than the
// cluster can lose and still have a majority, so the cluster stays able to
// recover; what it does with the network is not limited.
type nemesis struct {
	t      *testing.T
	c      *cluster
	w      *workload
	rng    *rand.Rand
	faults []fault
	start  time.Time

	mu     sync.Mutex
	events []string
	timers []*time.Timer
}

func newNemesis(t *testing.T, c *cluster, w *workload, seed int64, faults []fault) *nemesis {
	return &nemesis{t: t, c: c, w: w, rng: rand.New(rand.NewSource(seed)), faults: faults, start: time.Now()}
}

func (n *nemesis) log(format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, fmt.Sprintf("%6.2fs  %s", time.Since(n.start).Seconds(), fmt.Sprintf(format, args...)))
}

// later runs f after a random delay between lo and hi, unless the nemesis is stopped first.
func (n *nemesis) later(lo, hi time.Duration, f func()) {
	d := lo + time.Duration(n.rng.Int63n(int64(hi-lo)+1))
	n.mu.Lock()
	n.timers = append(n.timers, time.AfterFunc(d, f))
	n.mu.Unlock()
}

func (n *nemesis) downCount() int {
	down := 0
	for id := 1; id <= n.c.n; id++ {
		if !n.c.running(id) {
			down++
		}
	}
	return down
}

// maxDown is how many nodes may be dead at once: the cluster must keep a majority.
func (n *nemesis) maxDown() int { return (n.c.n - 1) / 2 }

func (n *nemesis) leaderID() int {
	for id := 1; id <= n.c.n; id++ {
		if !n.c.running(id) {
			continue
		}
		if st := nodeStatus(n.w.clients[id]); st != nil && st.Role == "leader" {
			return id
		}
	}
	return 0
}

func (n *nemesis) crash(id int, why string) {
	if n.downCount() >= n.maxDown() || !n.c.running(id) {
		return
	}
	n.log("%s: kill node %d", why, id)
	n.c.kill(id)
	n.later(300*time.Millisecond, 1500*time.Millisecond, func() {
		n.log("restart node %d", id)
		if err := n.c.start(id); err != nil {
			n.t.Errorf("restart node %d: %v", id, err)
		}
	})
}

func (n *nemesis) healLater() {
	n.later(400*time.Millisecond, 1500*time.Millisecond, func() {
		n.log("heal the network")
		n.c.heal()
	})
}

// step causes one piece of trouble.
func (n *nemesis) step() {
	pick := func() int { return 1 + n.rng.Intn(n.c.n) }
	switch n.faults[n.rng.Intn(len(n.faults))] {
	case faultCrash:
		n.crash(pick(), "crash")
	case faultKillLeader:
		if id := n.leaderID(); id != 0 {
			n.crash(id, "kill the leader")
		}
	case faultIsolate:
		id := pick()
		n.log("isolate node %d", id)
		n.c.isolate(id)
		n.healLater()
	case faultSplit:
		// A random group of one up to half of the nodes against all the rest.
		size := 1 + n.rng.Intn(n.c.n/2)
		perm := n.rng.Perm(n.c.n)
		group := make(map[int]bool)
		for _, i := range perm[:size] {
			group[i+1] = true
		}
		n.log("partition: %v against the rest", keys(group))
		for a := range group {
			for b := 1; b <= n.c.n; b++ {
				if !group[b] {
					n.c.cut(a, b)
				}
			}
		}
		n.healLater()
	case faultStaleNode:
		id := pick()
		n.log("isolate node %d for a long while", id)
		n.c.isolate(id)
		n.later(1500*time.Millisecond, 3*time.Second, func() {
			n.log("node %d returns", id)
			n.c.heal()
		})
	case faultAsymmetric:
		a, b := pick(), pick()
		if a == b {
			return
		}
		n.log("cut node %d -> node %d only", a, b)
		n.c.cutDirected(a, b)
		n.healLater()
	case faultDelay:
		a, b := pick(), pick()
		if a == b {
			return
		}
		d := time.Duration(5+n.rng.Intn(60)) * time.Millisecond
		n.log("delay %d <-> %d by %v", a, b, d)
		n.c.setDelay(a, b, d)
		n.healLater()
	}
}

// run causes trouble until ctx ends.
func (n *nemesis) run(ctx context.Context) {
	for {
		sleepCtx(ctx, time.Duration(200+n.rng.Intn(500))*time.Millisecond)
		if ctx.Err() != nil {
			return
		}
		n.step()
	}
}

// stop undoes everything: pending timers are cancelled, the network is healed
// and every dead node is started again.
func (n *nemesis) stop() {
	n.mu.Lock()
	for _, tm := range n.timers {
		tm.Stop()
	}
	n.timers = nil
	n.mu.Unlock()

	n.log("stop: heal the network and restart every node")
	n.c.heal()
	for id := 1; id <= n.c.n; id++ {
		n.c.mustStart(id)
	}
}

func (n *nemesis) report() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := fmt.Sprintf("%d faults injected:\n", len(n.events)-1)
	for _, e := range n.events {
		out += "  " + e + "\n"
	}
	return out
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // tiny insertion sort: stable output for the report
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
