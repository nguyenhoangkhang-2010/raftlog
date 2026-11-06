package broker

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

func openBroker(t *testing.T, dir string) *Broker {
	t.Helper()
	b, err := Open(dir, Options{MaxPartitions: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func msg(key, val string) storage.Message {
	return storage.Message{Key: []byte(key), Value: []byte(val)}
}

func TestCreateProduceFetch(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 3); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	part, base, err := b.Produce("events", AnyPartition, []storage.Message{msg("u1", "a"), msg("u1", "b"), msg("u1", "c")})
	if err != nil {
		t.Fatalf("Produce: %v", err)
	}
	if base != 0 {
		t.Fatalf("base offset = %d, want 0", base)
	}

	got, err := b.Fetch("events", part, 0, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("Fetch: %v, %d msgs", err, len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if string(got[i].Value) != want || got[i].Offset != uint64(i) {
			t.Fatalf("msg %d = %+v", i, got[i])
		}
	}
}

func TestSameKeyGoesToSamePartition(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 8); err != nil {
		t.Fatal(err)
	}
	first, _, err := b.Produce("events", AnyPartition, []storage.Message{msg("user-42", "x")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		p, _, err := b.Produce("events", AnyPartition, []storage.Message{msg("user-42", "x")})
		if err != nil || p != first {
			t.Fatalf("iteration %d: partition %d (err %v), want %d", i, p, err, first)
		}
	}
}

func TestKeysSpreadAcrossPartitions(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 4); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for i := 0; i < 200; i++ {
		p, _, err := b.Produce("events", AnyPartition, []storage.Message{msg(fmt.Sprintf("key-%d", i), "v")})
		if err != nil {
			t.Fatal(err)
		}
		seen[p] = true
	}
	if len(seen) != 4 {
		t.Fatalf("200 distinct keys reached %d of 4 partitions", len(seen))
	}
}

func TestEmptyKeyIsRoundRobin(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 3); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		p, _, err := b.Produce("events", AnyPartition, []storage.Message{msg("", "v")})
		if err != nil || p != i%3 {
			t.Fatalf("message %d went to partition %d (err %v), want %d", i, p, err, i%3)
		}
	}
}

func TestExplicitPartition(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 3); err != nil {
		t.Fatal(err)
	}
	p, _, err := b.Produce("events", 2, []storage.Message{msg("k", "v")})
	if err != nil || p != 2 {
		t.Fatalf("partition = %d, err = %v; want 2", p, err)
	}
	if _, _, err := b.Produce("events", 3, []storage.Message{msg("k", "v")}); !errors.Is(err, ErrInvalidPartition) {
		t.Fatalf("err = %v, want ErrInvalidPartition", err)
	}
	if _, err := b.Fetch("events", -1, 0, 1); !errors.Is(err, ErrInvalidPartition) {
		t.Fatalf("err = %v, want ErrInvalidPartition", err)
	}
}

func TestTopicErrors(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 2); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("events", 2); !errors.Is(err, ErrTopicExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, _, err := b.Produce("nope", AnyPartition, []storage.Message{msg("k", "v")}); !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("produce to unknown topic: %v", err)
	}
	if _, err := b.Fetch("nope", 0, 0, 1); !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("fetch from unknown topic: %v", err)
	}
	if _, _, err := b.Produce("events", AnyPartition, nil); !errors.Is(err, ErrEmptyBatch) {
		t.Fatalf("empty batch: %v", err)
	}
	if _, err := b.Fetch("events", 0, 5, 1); !errors.Is(err, storage.ErrOffsetOutOfRange) {
		t.Fatalf("fetch past end: %v", err)
	}
}

func TestInvalidInput(t *testing.T) {
	b := openBroker(t, t.TempDir())
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "a b", "tên", strings.Repeat("x", 250)} {
		if err := b.CreateTopic(name, 1); !errors.Is(err, ErrInvalidTopicName) {
			t.Errorf("name %q: err = %v, want ErrInvalidTopicName", name, err)
		}
	}
	for _, n := range []int{0, -1, 9} { // limit is 8 in openBroker
		if err := b.CreateTopic("ok", n); !errors.Is(err, ErrInvalidPartitions) {
			t.Errorf("partitions %d: err = %v, want ErrInvalidPartitions", n, err)
		}
	}
	if len(b.Topics()) != 0 {
		t.Fatalf("failed creations left topics behind: %v", b.Topics())
	}
	if _, err := Open(t.TempDir(), Options{}); err == nil {
		t.Fatal("Open with MaxPartitions=0 should fail")
	}
}

func TestReopenKeepsTopicsAndData(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir, Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("alpha", 2); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("beta", 1); err != nil {
		t.Fatal(err)
	}
	part, _, err := b.Produce("alpha", AnyPartition, []storage.Message{msg("k", "one"), msg("k", "two")})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b2 := openBroker(t, dir)
	topics := b2.Topics()
	if len(topics) != 2 || topics[0] != (TopicInfo{"alpha", 2}) || topics[1] != (TopicInfo{"beta", 1}) {
		t.Fatalf("Topics = %+v", topics)
	}
	got, err := b2.Fetch("alpha", part, 0, 10)
	if err != nil || len(got) != 2 || string(got[1].Value) != "two" {
		t.Fatalf("Fetch after reopen: %v, %+v", err, got)
	}
	// New data continues after the old.
	_, base, err := b2.Produce("alpha", part, []storage.Message{msg("k", "three")})
	if err != nil || base != 2 {
		t.Fatalf("base = %d, err = %v; want 2", base, err)
	}
}

func TestBatchesStayContiguousUnderConcurrency(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 1); err != nil {
		t.Fatal(err)
	}
	const producers, batches, batchSize = 6, 30, 5

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < batches; i++ {
				batch := make([]storage.Message, batchSize)
				for j := range batch {
					batch[j] = msg("k", fmt.Sprintf("p%d-b%d-m%d", p, i, j))
				}
				if _, _, err := b.Produce("events", 0, batch); err != nil {
					t.Errorf("Produce: %v", err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	total := producers * batches * batchSize
	all, err := b.Fetch("events", 0, 0, total)
	if err != nil || len(all) != total {
		t.Fatalf("Fetch: %v, %d msgs, want %d", err, len(all), total)
	}
	// Within every batch the members must be adjacent and in order.
	for i := 0; i < total; i += batchSize {
		var prefix string
		for j := 0; j < batchSize; j++ {
			v := string(all[i+j].Value)
			want := fmt.Sprintf("m%d", j)
			if !strings.HasSuffix(v, want) {
				t.Fatalf("offset %d = %q, want suffix %q", i+j, v, want)
			}
			p := v[:strings.LastIndex(v, "-m")]
			if j == 0 {
				prefix = p
			} else if p != prefix {
				t.Fatalf("batch at offset %d interleaved: %q then %q", i, prefix, p)
			}
		}
	}
}

func TestUseAfterClose(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 1); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("more", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("CreateTopic: %v", err)
	}
	if _, _, err := b.Produce("events", AnyPartition, []storage.Message{msg("k", "v")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Produce: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
