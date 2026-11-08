package server

import (
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

func TestListTopicsAndGroupsOverGRPC(t *testing.T) {
	c := startServer(t)
	for _, name := range []string{"zeta", "alpha"} {
		if _, err := c.CreateTopic(ctx(t), &pb.CreateTopicRequest{Topic: name, Partitions: 2}); err != nil {
			t.Fatal(err)
		}
	}
	zero, one := uint32(0), uint32(1)
	for _, p := range []*uint32{&zero, &zero, &zero, &one} {
		if _, err := c.Produce(ctx(t), &pb.ProduceRequest{Topic: "alpha", Partition: p, Messages: []*pb.Message{{Value: []byte("v")}}}); err != nil {
			t.Fatal(err)
		}
	}

	topics, err := c.ListTopics(ctx(t), &pb.ListTopicsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(topics.Topics) != 2 || topics.Topics[0].Name != "alpha" || topics.Topics[1].Name != "zeta" {
		t.Fatalf("topics = %v, want alpha then zeta", topics.Topics)
	}
	if e := topics.Topics[0].EndOffsets; len(e) != 2 || e[0] != 3 || e[1] != 1 {
		t.Fatalf("alpha ends = %v, want [3 1]", e)
	}
	if e := topics.Topics[1].EndOffsets; len(e) != 2 || e[0] != 0 || e[1] != 0 {
		t.Fatalf("zeta ends = %v, want [0 0]", e)
	}

	// No group has committed yet.
	groups, err := c.ListGroups(ctx(t), &pb.ListGroupsRequest{})
	if err != nil || len(groups.Groups) != 0 {
		t.Fatalf("groups before any commit = %v, %v", groups, err)
	}
	if _, err := c.CommitOffset(ctx(t), &pb.CommitOffsetRequest{Group: "billing", Topic: "alpha", Partition: 0, Offset: 1}); err != nil {
		t.Fatal(err)
	}
	groups, err = c.ListGroups(ctx(t), &pb.ListGroupsRequest{})
	if err != nil || len(groups.Groups) != 1 {
		t.Fatalf("groups = %v, %v", groups, err)
	}
	g := groups.Groups[0]
	if g.Group != "billing" || len(g.Partitions) != 1 {
		t.Fatalf("group = %v", g)
	}
	if p := g.Partitions[0]; p.Topic != "alpha" || p.Partition != 0 || p.Offset != 1 || p.EndOffset != 3 || p.Lag != 2 {
		t.Fatalf("partition = %v, want alpha/0 at 1 of 3 with lag 2", p)
	}
}

func TestListingIsUnimplementedForABackendThatCannotDescribe(t *testing.T) {
	c := clientFor(t, failingBackend{err: errors.New("x")}, Options{})
	_, err := c.ListTopics(ctx(t), &pb.ListTopicsRequest{})
	wantCode(t, err, codes.Unimplemented)
	_, err = c.ListGroups(ctx(t), &pb.ListGroupsRequest{})
	wantCode(t, err, codes.Unimplemented)
}

// countingObserver records what the handlers report.
type countingObserver struct {
	mu                 sync.Mutex
	produced, bytes    int
	fetched            int
	waiting, maxWaited int
}

func (o *countingObserver) Produced(messages, bytes int) {
	o.mu.Lock()
	o.produced += messages
	o.bytes += bytes
	o.mu.Unlock()
}

func (o *countingObserver) Fetched(messages int) {
	o.mu.Lock()
	o.fetched += messages
	o.mu.Unlock()
}

func (o *countingObserver) FetchWaiting(delta int) {
	o.mu.Lock()
	o.waiting += delta
	if o.waiting > o.maxWaited {
		o.maxWaited = o.waiting
	}
	o.mu.Unlock()
}

func (o *countingObserver) snapshot() (produced, bytes, fetched, waiting, maxWaited int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.produced, o.bytes, o.fetched, o.waiting, o.maxWaited
}

func TestTheObserverHearsWhatTheHandlersDo(t *testing.T) {
	obs := &countingObserver{}
	c, _ := startWaitServer(t, Options{MaxFetchWait: time.Minute, Observer: obs})
	mustCreate(t, c, "events", 1)

	if _, err := c.Produce(ctx(t), &pb.ProduceRequest{Topic: "events", Messages: []*pb.Message{
		{Key: []byte("k1"), Value: []byte("hello")}, {Value: []byte("world!")},
	}}); err != nil {
		t.Fatal(err)
	}
	// A refused produce is not counted.
	if _, err := c.Produce(ctx(t), &pb.ProduceRequest{Topic: "nope", Messages: []*pb.Message{{Value: []byte("x")}}}); err == nil {
		t.Fatal("produce to a missing topic succeeded")
	}
	if produced, bytes, _, _, _ := obs.snapshot(); produced != 2 || bytes != len("k1")+len("hello")+len("world!") {
		t.Fatalf("produced %d messages, %d bytes", produced, bytes)
	}

	if _, err := c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", Offset: 0, MaxMessages: 5}); err != nil {
		t.Fatal(err)
	}
	// A fetch that finds nothing is not counted as fetched messages.
	if _, err := c.Fetch(ctx(t), &pb.FetchRequest{Topic: "events", Offset: 2, MaxMessages: 5}); err != nil {
		t.Fatal(err)
	}
	if _, _, fetched, _, _ := obs.snapshot(); fetched != 2 {
		t.Fatalf("fetched %d messages, want 2", fetched)
	}

	// A fetch that waits is counted while it waits, and not after.
	got := fetchAsync(c, &pb.FetchRequest{Topic: "events", Offset: 2, MaxMessages: 5, MaxWaitMs: 30_000})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, _, waiting, _ := obs.snapshot(); waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the waiting fetch was never reported")
		}
		time.Sleep(5 * time.Millisecond)
	}
	produceOne(t, c, "events", 0, "late")
	if o := await(t, got, 5*time.Second); o.err != nil || len(o.resp.Messages) != 1 {
		t.Fatalf("the waiting fetch: %+v", o)
	}
	for {
		if _, _, _, waiting, _ := obs.snapshot(); waiting == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the waiting fetch was never reported as finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
