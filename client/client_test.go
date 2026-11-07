package client_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

func TestProduceAndFetchRoundTrip(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 2); err != nil {
		t.Fatal(err)
	}

	stamp := time.UnixMilli(1_700_000_000_123)
	res, err := c.Produce(ctx(t, 10*time.Second), "events", []client.Message{
		{Key: []byte("u1"), Value: []byte("first"), Timestamp: stamp},
		{Key: []byte("u2"), Value: []byte("second")},
	}, client.ToPartition(1))
	if err != nil {
		t.Fatal(err)
	}
	if res.Partition != 1 || res.Offset != 0 {
		t.Fatalf("Produce = %+v, want partition 1 offset 0", res)
	}
	next, err := c.Produce(ctx(t, 10*time.Second), "events", []client.Message{{Value: []byte("third")}}, client.ToPartition(1))
	if err != nil || next.Offset != 2 {
		t.Fatalf("second Produce = %+v, %v; want offset 2", next, err)
	}

	recs, err := c.Fetch(ctx(t, 10*time.Second), "events", 1, 0, 10)
	if err != nil || len(recs) != 3 {
		t.Fatalf("Fetch = %v, %v", recs, err)
	}
	if recs[0].Offset != 0 || string(recs[0].Key) != "u1" || string(recs[0].Value) != "first" || !recs[0].Timestamp.Equal(stamp) {
		t.Fatalf("first record = %+v, want the key, value and timestamp that were sent", recs[0])
	}
	if recs[1].Timestamp.IsZero() || recs[1].Timestamp.Unix() == 0 {
		t.Fatalf("a message sent without a timestamp was stored with %v", recs[1].Timestamp)
	}
	if recs[2].Offset != 2 || string(recs[2].Value) != "third" {
		t.Fatalf("third record = %+v", recs[2])
	}

	// Reading from the middle, and at the end.
	if mid, _ := c.Fetch(ctx(t, 10*time.Second), "events", 1, 2, 10); len(mid) != 1 || mid[0].Offset != 2 {
		t.Fatalf("Fetch from offset 2 = %v", mid)
	}
	if end, err := c.Fetch(ctx(t, 10*time.Second), "events", 1, 3, 10); err != nil || len(end) != 0 {
		t.Fatalf("Fetch at the end = %v, %v; want nothing and no error", end, err)
	}
	// The other partition is empty.
	if other, err := c.Fetch(ctx(t, 10*time.Second), "events", 0, 0, 10); err != nil || len(other) != 0 {
		t.Fatalf("Fetch of partition 0 = %v, %v", other, err)
	}
}

func TestFetchCanWaitForAMessage(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatal(err)
	}

	type result struct {
		recs    []client.Record
		err     error
		elapsed time.Duration
	}
	got := make(chan result, 1)
	go func() {
		start := time.Now()
		recs, err := c.Fetch(ctx(t, 20*time.Second), "events", 0, 0, 10, client.WaitUpTo(15*time.Second))
		got <- result{recs, err, time.Since(start)}
	}()

	select {
	case r := <-got:
		t.Fatalf("returned before anything was produced: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := c.Produce(ctx(t, 10*time.Second), "events", []client.Message{{Value: []byte("late")}}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.err != nil || len(r.recs) != 1 || string(r.recs[0].Value) != "late" {
			t.Fatalf("Fetch = %+v", r)
		}
		if r.elapsed > 10*time.Second {
			t.Fatalf("took %v: it waited for the timeout, not for the message", r.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting Fetch never returned")
	}
}

func TestFetchGivesNothingWhenTheWaitIsOver(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	recs, err := c.Fetch(ctx(t, 10*time.Second), "events", 0, 0, 10, client.WaitUpTo(200*time.Millisecond))
	if err != nil || len(recs) != 0 {
		t.Fatalf("Fetch = %v, %v; want nothing and no error", recs, err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatalf("returned after %v although it was to wait 200 ms", time.Since(start))
	}
}

func TestFetchRefusesANegativeWait(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if _, err := c.Fetch(ctx(t, time.Second), "events", 0, 0, 10, client.WaitUpTo(-time.Second)); !errors.Is(err, client.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestBinaryKeysAndValuesSurvive(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "bin", 1); err != nil {
		t.Fatal(err)
	}
	key := []byte{0, 1, 0xff, 0xfe, 0}
	value := bytes.Repeat([]byte{0x80, 0x00, 0xc3, 0x28}, 1000) // not valid UTF-8
	if _, err := c.Produce(ctx(t, 10*time.Second), "bin", []client.Message{{Key: key, Value: value}}); err != nil {
		t.Fatal(err)
	}
	recs, err := c.Fetch(ctx(t, 10*time.Second), "bin", 0, 0, 1)
	if err != nil || len(recs) != 1 || !bytes.Equal(recs[0].Key, key) || !bytes.Equal(recs[0].Value, value) {
		t.Fatalf("binary data changed on the way: %v", err)
	}
}

func TestThePartitionFollowsTheKeyUnlessOneIsGiven(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 4); err != nil {
		t.Fatal(err)
	}
	send := func(key string) int {
		res, err := c.Produce(ctx(t, 10*time.Second), "events", []client.Message{{Key: []byte(key), Value: []byte("v")}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Partition
	}

	first := send("user-9")
	for i := 0; i < 10; i++ {
		if got := send("user-9"); got != first {
			t.Fatalf("the same key went to partition %d, then %d", first, got)
		}
	}
	seen := map[int]bool{}
	for i := 0; i < 12; i++ {
		seen[send("")] = true // no key: spread over the partitions
	}
	if len(seen) != 4 {
		t.Fatalf("messages without a key reached %d of 4 partitions", len(seen))
	}
}

func TestErrorsAreNamed(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatal(err)
	}
	one := []client.Message{{Value: []byte("x")}}

	checks := []struct {
		name string
		err  error
		want error
	}{
		{"duplicate topic", c.CreateTopic(ctx(t, 10*time.Second), "events", 1), client.ErrAlreadyExists},
		{"bad topic name", c.CreateTopic(ctx(t, 10*time.Second), "a/b", 1), client.ErrInvalidArgument},
		{"zero partitions", c.CreateTopic(ctx(t, 10*time.Second), "zero", 0), client.ErrInvalidArgument},
		{"too many partitions", c.CreateTopic(ctx(t, 10*time.Second), "wide", 99), client.ErrInvalidArgument},
		{"produce to a missing topic", func() error {
			_, err := c.Produce(ctx(t, 10*time.Second), "nope", one)
			return err
		}(), client.ErrNotFound},
		{"produce to a missing partition", func() error {
			_, err := c.Produce(ctx(t, 10*time.Second), "events", one, client.ToPartition(5))
			return err
		}(), client.ErrInvalidArgument},
		{"produce nothing", func() error {
			_, err := c.Produce(ctx(t, 10*time.Second), "events", nil)
			return err
		}(), client.ErrInvalidArgument},
		{"produce to a negative partition", func() error {
			_, err := c.Produce(ctx(t, 10*time.Second), "events", one, client.ToPartition(-1))
			return err
		}(), client.ErrInvalidArgument},
		{"fetch from a missing topic", func() error {
			_, err := c.Fetch(ctx(t, 10*time.Second), "nope", 0, 0, 10)
			return err
		}(), client.ErrNotFound},
		{"fetch beyond the end", func() error {
			_, err := c.Fetch(ctx(t, 10*time.Second), "events", 0, 99, 10)
			return err
		}(), client.ErrOffsetOutOfRange},
		{"fetch nothing", func() error {
			_, err := c.Fetch(ctx(t, 10*time.Second), "events", 0, 0, 0)
			return err
		}(), client.ErrInvalidArgument},
		{"commit beyond the end", c.CommitOffset(ctx(t, 10*time.Second), "g", "events", 0, 99), client.ErrOffsetOutOfRange},
		{"commit for a bad group", c.CommitOffset(ctx(t, 10*time.Second), "bad group", "events", 0, 0), client.ErrInvalidArgument},
		{"commit to a negative partition", c.CommitOffset(ctx(t, 10*time.Second), "g", "events", -1, 0), client.ErrInvalidArgument},
		{"position in a missing topic", func() error {
			_, err := c.GroupPosition(ctx(t, 10*time.Second), "g", "nope", 0)
			return err
		}(), client.ErrNotFound},
	}
	for _, ch := range checks {
		if !errors.Is(ch.err, ch.want) {
			t.Errorf("%s: err = %v, want %v", ch.name, ch.err, ch.want)
		}
	}
}

func TestConsumerGroupPosition(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatal(err)
	}
	msgs := make([]client.Message, 10)
	for i := range msgs {
		msgs[i] = client.Message{Value: []byte(fmt.Sprint(i))}
	}
	if _, err := c.Produce(ctx(t, 10*time.Second), "events", msgs); err != nil {
		t.Fatal(err)
	}

	pos, err := c.GroupPosition(ctx(t, 10*time.Second), "billing", "events", 0)
	if err != nil || pos.Committed || pos.Offset != 0 || pos.End != 10 || pos.Lag != 10 {
		t.Fatalf("before any commit: %+v, %v", pos, err)
	}
	if err := c.CommitOffset(ctx(t, 10*time.Second), "billing", "events", 0, 4); err != nil {
		t.Fatal(err)
	}
	pos, err = c.GroupPosition(ctx(t, 10*time.Second), "billing", "events", 0)
	if err != nil || !pos.Committed || pos.Offset != 4 || pos.Lag != 6 {
		t.Fatalf("after committing 4: %+v, %v", pos, err)
	}
	// Another group is unaffected, and going back is allowed.
	other, _ := c.GroupPosition(ctx(t, 10*time.Second), "analytics", "events", 0)
	if other.Committed {
		t.Fatal("a commit leaked into another group")
	}
	if err := c.CommitOffset(ctx(t, 10*time.Second), "billing", "events", 0, 1); err != nil {
		t.Fatalf("rewinding: %v", err)
	}
}

func TestOneClientServesManyGoroutines(t *testing.T) {
	c := newClient(t, []string{startBroker(t)})
	if err := c.CreateTopic(ctx(t, 10*time.Second), "events", 1); err != nil {
		t.Fatal(err)
	}

	const workers, each = 8, 25
	var mu sync.Mutex
	offsets := map[uint64]bool{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				res, err := c.Produce(ctx(t, 10*time.Second), "events", []client.Message{{Value: []byte(fmt.Sprintf("w%d-%d", w, i))}})
				if err != nil {
					t.Errorf("Produce: %v", err)
					return
				}
				mu.Lock()
				if offsets[res.Offset] {
					t.Errorf("offset %d was given twice", res.Offset)
				}
				offsets[res.Offset] = true
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	// Every message is there, read in pages of at most 100 (the server's cap).
	total := 0
	for offset := uint64(0); ; {
		recs, err := c.Fetch(ctx(t, 10*time.Second), "events", 0, offset, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			break
		}
		total += len(recs)
		offset = recs[len(recs)-1].Offset + 1
	}
	if total != workers*each || len(offsets) != workers*each {
		t.Fatalf("read %d messages and saw %d offsets, want %d of each", total, len(offsets), workers*each)
	}
}
