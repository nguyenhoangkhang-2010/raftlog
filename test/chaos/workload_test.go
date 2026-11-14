package chaos

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

// workload drives the cluster like clients would, and records what they saw.
//
// Every write attempt carries a value of its own and is never repeated. That is
// what lets the checker tell, from the final log alone, which attempt put a
// message there. The price is that an attempt whose outcome is unknown is just
// that: a message that may or may not be stored.
type workload struct {
	t         *testing.T
	c         *cluster
	topic     string
	parts     int
	start     time.Time
	opTimeout time.Duration

	clients map[int]pb.BrokerClient // by node id, over the node's real address
	closers []func()

	mu     sync.Mutex
	writes []Write
	reads  []Read
	leader int // last leader named by a node; 0 if unknown
}

func newWorkload(t *testing.T, c *cluster, topic string, parts int) *workload {
	t.Helper()
	w := &workload{
		t: t, c: c, topic: topic, parts: parts,
		start: time.Now(), opTimeout: 2 * time.Second,
		clients: make(map[int]pb.BrokerClient),
	}
	for _, nd := range c.nodes {
		cl, closeConn := dial(t, nd.addr)
		w.clients[nd.id] = cl
		w.closers = append(w.closers, closeConn)
	}
	return w
}

func (w *workload) close() {
	for _, f := range w.closers {
		f()
	}
}

func (w *workload) now() time.Duration { return time.Since(w.start) }

func (w *workload) history() History {
	w.mu.Lock()
	defer w.mu.Unlock()
	return History{
		Writes: append([]Write(nil), w.writes...),
		Reads:  append([]Read(nil), w.reads...),
	}
}

func (w *workload) setLeader(id int) {
	w.mu.Lock()
	w.leader = id
	w.mu.Unlock()
}

func (w *workload) pickTarget(rng *rand.Rand) int {
	w.mu.Lock()
	leader := w.leader
	w.mu.Unlock()
	if leader != 0 && rng.Intn(10) != 0 {
		return leader
	}
	return 1 + rng.Intn(w.c.n)
}

// createTopic creates the topic, waiting for a leader if there is none yet.
func (w *workload) createTopic(timeout time.Duration) {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for id := 1; id <= w.c.n; id++ {
			ctx, cancel := shortCtx(2 * time.Second)
			_, err := w.clients[id].CreateTopic(ctx, &pb.CreateTopicRequest{Topic: w.topic, Partitions: uint32(w.parts)})
			cancel()
			if err == nil || status.Code(err) == codes.AlreadyExists {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatal("could not create the topic: the cluster never elected a leader")
}

// attempt performs one write and records it.
func (w *workload) attempt(ctx context.Context, rng *rand.Rand, client, seq int) {
	target := w.pickTarget(rng)
	partition := rng.Intn(w.parts)
	value := fmt.Sprintf("c%d-%d", client, seq)
	part := uint32(partition)

	opCtx, cancel := context.WithTimeout(ctx, w.opTimeout)
	var trailer metadata.MD
	invoke := w.now()
	resp, err := w.clients[target].Produce(opCtx, &pb.ProduceRequest{
		Topic: w.topic, Partition: &part,
		Messages: []*pb.Message{{Key: []byte("k"), Value: []byte(value)}},
	}, grpc.Trailer(&trailer))
	ret := w.now()
	cancel()

	wr := Write{Client: client, Partition: partition, Value: value, Invoke: invoke, Return: ret}
	switch {
	case err == nil:
		wr.Outcome, wr.Offset = Acked, resp.BaseOffset
	case status.Code(err) == codes.FailedPrecondition:
		// Not the leader: refused before anything was stored.
		wr.Outcome = Rejected
		if ids := trailer.Get(server.LeaderIDKey); len(ids) == 1 {
			id, _ := strconv.Atoi(ids[0])
			w.setLeader(id)
		} else {
			w.setLeader(0)
		}
	case status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.NotFound || status.Code(err) == codes.OutOfRange:
		wr.Outcome = Rejected
	default:
		// A timeout, a broken connection, a leader that stepped down while the
		// request was in flight: the client cannot know.
		wr.Outcome = Unknown
		w.setLeader(0)
	}

	w.mu.Lock()
	w.writes = append(w.writes, wr)
	w.mu.Unlock()

	switch wr.Outcome {
	case Rejected:
		sleepCtx(ctx, 10*time.Millisecond)
	case Unknown:
		sleepCtx(ctx, 20*time.Millisecond)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// runWriters starts the writing clients; the returned function waits for them
// to stop, which they do when ctx ends.
func (w *workload) runWriters(ctx context.Context, clients int, seed int64) (wait func()) {
	var wg sync.WaitGroup
	for client := 0; client < clients; client++ {
		wg.Add(1)
		go func(client int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(client)))
			for seq := 0; ctx.Err() == nil; seq++ {
				w.attempt(ctx, rng, client, seq)
			}
		}(client)
	}
	return wg.Wait
}

// runReader reads from random nodes at random offsets, and records what comes
// back, so that the checker can tell whether any node ever showed data that was
// not really committed.
func (w *workload) runReader(ctx context.Context, seed int64) (wait func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(seed))
		end := make([]uint64, w.parts) // the longest log seen so far, per partition
		for ctx.Err() == nil {
			node := 1 + rng.Intn(w.c.n)
			partition := rng.Intn(w.parts)
			offset := uint64(0)
			if end[partition] > 0 {
				offset = uint64(rng.Int63n(int64(end[partition]) + 1))
			}

			opCtx, cancel := context.WithTimeout(ctx, w.opTimeout)
			resp, err := w.clients[node].Fetch(opCtx, &pb.FetchRequest{
				Topic: w.topic, Partition: uint32(partition), Offset: offset, MaxMessages: 20,
			})
			cancel()
			if err == nil && len(resp.Messages) > 0 {
				values := make([]string, len(resp.Messages))
				for i, m := range resp.Messages {
					values[i] = string(m.Value)
				}
				w.mu.Lock()
				w.reads = append(w.reads, Read{Node: node, Partition: partition, Offset: resp.Messages[0].Offset, Values: values})
				w.mu.Unlock()
				if e := resp.Messages[len(resp.Messages)-1].Offset + 1; e > end[partition] {
					end[partition] = e
				}
			}
			sleepCtx(ctx, 5*time.Millisecond)
		}
	}()
	return wg.Wait
}

// fetchAll reads a whole partition from one node.
func (w *workload) fetchAll(node, partition int) ([]string, error) {
	var out []string
	for offset := uint64(0); ; {
		ctx, cancel := shortCtx(5 * time.Second)
		resp, err := w.clients[node].Fetch(ctx, &pb.FetchRequest{
			Topic: w.topic, Partition: uint32(partition), Offset: offset, MaxMessages: 500,
		})
		cancel()
		if err != nil {
			return nil, err
		}
		if len(resp.Messages) == 0 {
			return out, nil
		}
		for _, m := range resp.Messages {
			if m.Offset != offset {
				return nil, fmt.Errorf("node %d partition %d: offset %d where %d was expected", node, partition, m.Offset, offset)
			}
			out = append(out, string(m.Value))
			offset++
		}
	}
}

// settledSignature describes the cluster if it is calm: one leader, every node
// has committed everything it has, and all nodes hold the same log. The
// signature changes whenever anything moves, so two equal ones in a row mean
// the cluster has stopped moving.
func (w *workload) settledSignature() (string, bool) {
	leaders := 0
	var sig string
	var commit, last uint64
	for id := 1; id <= w.c.n; id++ {
		st := nodeStatus(w.clients[id])
		if st == nil || st.CommitIndex != st.LastIndex {
			return "", false
		}
		if id == 1 {
			commit, last = st.CommitIndex, st.LastIndex
		} else if st.CommitIndex != commit || st.LastIndex != last {
			return "", false
		}
		if st.Role == "leader" {
			leaders++
			sig += fmt.Sprintf("leader=%d term=%d ", st.NodeId, st.Term)
		}
	}
	if leaders != 1 {
		return "", false
	}
	return fmt.Sprintf("%scommit=%d", sig, commit), true
}

// waitSettled waits until the cluster has been calm for a while.
func (w *workload) waitSettled(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last, stable := "", 0
	for time.Now().Before(deadline) {
		sig, ok := w.settledSignature()
		if ok && sig == last {
			stable++
		} else {
			stable = 0
		}
		last = sig
		if ok && stable >= 4 {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the cluster did not settle within %v (last calm observation: %q)", timeout, last)
}

// finalState reads every partition from every node. The nodes must agree; if
// they do not, that is reported as a violation.
func (w *workload) finalState() (Final, []Violation) {
	final := make(Final)
	var vs []Violation
	for p := 0; p < w.parts; p++ {
		for id := 1; id <= w.c.n; id++ {
			vals, err := w.fetchAll(id, p)
			if err != nil {
				vs = append(vs, Violation{Kind: "unreadable-final-state", Detail: err.Error()})
				continue
			}
			if id == 1 {
				final[p] = vals
				continue
			}
			if !equalStrings(final[p], vals) {
				vs = append(vs, Violation{Kind: "nodes-diverged", Detail: fmt.Sprintf(
					"partition %d: node 1 holds %d messages, node %d holds %d, and they differ", p, len(final[p]), id, len(vals))})
			}
		}
	}
	return final, vs
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// summary counts the outcomes of the writes.
func (w *workload) summary() string {
	h := w.history()
	counts := map[Outcome]int{}
	for _, wr := range h.Writes {
		counts[wr.Outcome]++
	}
	byNode := map[int]int{}
	for _, r := range h.Reads {
		byNode[r.Node]++
	}
	nodes := make([]int, 0, len(byNode))
	for n := range byNode {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	reads := ""
	for _, n := range nodes {
		reads += fmt.Sprintf(" node%d=%d", n, byNode[n])
	}
	return fmt.Sprintf("writes: %d acknowledged, %d refused, %d unknown; reads checked:%s",
		counts[Acked], counts[Rejected], counts[Unknown], reads)
}
