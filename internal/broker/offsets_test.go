package broker

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// topicWith creates a one-partition topic holding n messages.
func topicWith(t *testing.T, b *Broker, name string, n int) {
	t.Helper()
	if err := b.CreateTopic(name, 1); err != nil {
		t.Fatal(err)
	}
	batch := make([]storage.Message, n)
	for i := range batch {
		batch[i] = msg("k", fmt.Sprint(i))
	}
	if _, _, err := b.Produce(name, 0, batch); err != nil {
		t.Fatal(err)
	}
}

func TestNothingCommittedYet(t *testing.T) {
	b := openBroker(t, t.TempDir())
	topicWith(t, b, "events", 5)

	info, err := b.GroupOffset("g1", "events", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.Committed || info.Offset != 0 || info.End != 5 || info.Lag() != 5 {
		t.Fatalf("info = %+v (lag %d)", info, info.Lag())
	}
}

func TestCommitAndReadBack(t *testing.T) {
	b := openBroker(t, t.TempDir())
	topicWith(t, b, "events", 10)

	if err := b.CommitOffset("g1", "events", 0, 4); err != nil {
		t.Fatalf("CommitOffset: %v", err)
	}
	info, err := b.GroupOffset("g1", "events", 0)
	if err != nil || !info.Committed || info.Offset != 4 || info.End != 10 || info.Lag() != 6 {
		t.Fatalf("info = %+v, err = %v", info, err)
	}

	// Later commits replace earlier ones, and going backwards is allowed.
	for _, off := range []uint64{9, 2} {
		if err := b.CommitOffset("g1", "events", 0, off); err != nil {
			t.Fatal(err)
		}
		if info, _ := b.GroupOffset("g1", "events", 0); info.Offset != off {
			t.Fatalf("after committing %d the offset is %d", off, info.Offset)
		}
	}
}

func TestCommitAtEndIsValidBeyondIsNot(t *testing.T) {
	b := openBroker(t, t.TempDir())
	topicWith(t, b, "events", 3)

	if err := b.CommitOffset("g1", "events", 0, 3); err != nil {
		t.Fatalf("committing the end offset: %v", err)
	}
	if info, _ := b.GroupOffset("g1", "events", 0); info.Lag() != 0 {
		t.Fatalf("lag at the end = %d, want 0", info.Lag())
	}
	if err := b.CommitOffset("g1", "events", 0, 4); !errors.Is(err, ErrOffsetBeyondEnd) {
		t.Fatalf("err = %v, want ErrOffsetBeyondEnd", err)
	}
	if info, _ := b.GroupOffset("g1", "events", 0); info.Offset != 3 {
		t.Fatalf("a rejected commit changed the offset to %d", info.Offset)
	}
}

func TestGroupsAreIndependent(t *testing.T) {
	b := openBroker(t, t.TempDir())
	topicWith(t, b, "events", 10)
	if err := b.CreateTopic("other", 2); err != nil {
		t.Fatal(err)
	}

	if err := b.CommitOffset("g1", "events", 0, 7); err != nil {
		t.Fatal(err)
	}
	if err := b.CommitOffset("g2", "events", 0, 2); err != nil {
		t.Fatal(err)
	}
	for group, want := range map[string]uint64{"g1": 7, "g2": 2} {
		if info, _ := b.GroupOffset(group, "events", 0); info.Offset != want {
			t.Fatalf("group %s offset = %d, want %d", group, info.Offset, want)
		}
	}
	// Same group, other topic/partition: untouched.
	if info, _ := b.GroupOffset("g1", "other", 1); info.Committed {
		t.Fatalf("commit leaked into another topic: %+v", info)
	}
}

func TestOffsetsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir, Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	topicWith(t, b, "events", 20)
	if err := b.CreateTopic("wide", 3); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Produce("wide", 2, []storage.Message{msg("k", "v")}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		group, topic string
		part         int
		off          uint64
	}{
		{"g1", "events", 0, 5},
		{"g1", "events", 0, 12}, // replaces the previous commit
		{"g2", "events", 0, 3},
		{"g1", "wide", 2, 1},
	} {
		if err := b.CommitOffset(c.group, c.topic, c.part, c.off); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b2 := openBroker(t, dir)
	for _, c := range []struct {
		group, topic string
		part         int
		want         uint64
	}{
		{"g1", "events", 0, 12},
		{"g2", "events", 0, 3},
		{"g1", "wide", 2, 1},
	} {
		info, err := b2.GroupOffset(c.group, c.topic, c.part)
		if err != nil || !info.Committed || info.Offset != c.want {
			t.Fatalf("%s/%s/%d: %+v, err %v; want offset %d", c.group, c.topic, c.part, info, err, c.want)
		}
	}
}

func TestOffsetsReplayAcrossManyRecordsAndSegments(t *testing.T) {
	dir := t.TempDir()
	// Small segments make the offsets log roll several times.
	opts := Options{MaxPartitions: 8, Storage: storage.Options{SegmentMaxBytes: 20_000}}
	b, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	topicWith(t, b, "events", 50)
	// More commits than offsetReplaySize forces replay to read in several chunks.
	const commits = offsetReplaySize*2 + 7
	for i := 0; i < commits; i++ {
		if err := b.CommitOffset(fmt.Sprintf("g%d", i%3), "events", 0, uint64(i%50)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b2, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b2.Close()
	for g := 0; g < 3; g++ {
		last := commits - 1 - ((commits - 1 - g) % 3) // last i with i%3 == g
		want := uint64(last % 50)
		info, err := b2.GroupOffset(fmt.Sprintf("g%d", g), "events", 0)
		if err != nil || info.Offset != want {
			t.Fatalf("group g%d: offset %d (err %v), want %d", g, info.Offset, err, want)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, offsetsDirName, "*.log"))
	if len(files) < 2 {
		t.Fatalf("expected the offsets log to span several segments, got %d", len(files))
	}
}

func TestOffsetValidation(t *testing.T) {
	b := openBroker(t, t.TempDir())
	topicWith(t, b, "events", 3)

	for _, g := range []string{"", "a b", "a/b", "a\x00b", strings.Repeat("g", 250)} {
		if err := b.CommitOffset(g, "events", 0, 1); !errors.Is(err, ErrInvalidGroup) {
			t.Errorf("group %q: err = %v, want ErrInvalidGroup", g, err)
		}
		if _, err := b.GroupOffset(g, "events", 0); !errors.Is(err, ErrInvalidGroup) {
			t.Errorf("GroupOffset group %q: err = %v, want ErrInvalidGroup", g, err)
		}
	}
	if err := b.CommitOffset("g1", "nope", 0, 0); !errors.Is(err, ErrTopicNotFound) {
		t.Errorf("unknown topic: %v", err)
	}
	for _, p := range []int{-1, 1} {
		if err := b.CommitOffset("g1", "events", p, 0); !errors.Is(err, ErrInvalidPartition) {
			t.Errorf("partition %d: %v", p, err)
		}
		if _, err := b.GroupOffset("g1", "events", p); !errors.Is(err, ErrInvalidPartition) {
			t.Errorf("GroupOffset partition %d: %v", p, err)
		}
	}
}

func TestConcurrentCommits(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir, Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	topicWith(t, b, "events", 100)

	const groups, rounds = 8, 50
	var wg sync.WaitGroup
	for g := 0; g < groups; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 1; r <= rounds; r++ {
				if err := b.CommitOffset(fmt.Sprintf("g%d", g), "events", 0, uint64(r)); err != nil {
					t.Errorf("CommitOffset: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Each group committed 1..rounds in order, so the final value is rounds,
	// both in memory and after replaying the log.
	b2 := openBroker(t, dir)
	for g := 0; g < groups; g++ {
		info, err := b2.GroupOffset(fmt.Sprintf("g%d", g), "events", 0)
		if err != nil || info.Offset != rounds {
			t.Fatalf("group g%d: offset %d (err %v), want %d", g, info.Offset, err, rounds)
		}
	}
}

func TestOffsetRecordRoundTrip(t *testing.T) {
	k := offsetKey{group: "analytics", topic: "video-events", partition: 7}
	got, off, err := decodeOffsetRecord(encodeOffsetRecord(k, 1<<40))
	if err != nil || got != k || off != 1<<40 {
		t.Fatalf("got %+v %d (err %v)", got, off, err)
	}
	for _, bad := range []storage.Message{
		{Key: []byte("only-one-part"), Value: make([]byte, 8)},
		{Key: []byte("g\x00t\x00x"), Value: make([]byte, 8)}, // partition is not a number
		{Key: []byte("g\x00t\x001"), Value: []byte("short")},
	} {
		if _, _, err := decodeOffsetRecord(bad); !errors.Is(err, errCorruptOffsetRec) {
			t.Errorf("record %q: err = %v, want errCorruptOffsetRec", bad.Key, err)
		}
	}
}
