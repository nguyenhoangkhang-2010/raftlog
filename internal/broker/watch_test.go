package broker

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// woken reports whether ch was closed within d.
func woken(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestWatchFiresWhenAMessageIsProduced(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 2); err != nil {
		t.Fatal(err)
	}

	ch, err := b.Watch("events", 0)
	if err != nil {
		t.Fatal(err)
	}
	if woken(ch, 30*time.Millisecond) {
		t.Fatal("Watch fired before anything was produced")
	}
	if _, _, err := b.Produce("events", 0, []storage.Message{msg("k", "v")}); err != nil {
		t.Fatal(err)
	}
	if !woken(ch, time.Second) {
		t.Fatal("Watch did not fire after a produce")
	}

	// A channel is used up once it fired; the next Watch starts a new wait.
	next, err := b.Watch("events", 0)
	if err != nil {
		t.Fatal(err)
	}
	if woken(next, 30*time.Millisecond) {
		t.Fatal("a new Watch fired for a message that was already there")
	}
}

func TestWatchIgnoresOtherPartitions(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 2); err != nil {
		t.Fatal(err)
	}
	ch, err := b.Watch("events", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Produce("events", 1, []storage.Message{msg("k", "v")}); err != nil {
		t.Fatal(err)
	}
	if woken(ch, 50*time.Millisecond) {
		t.Fatal("a write to partition 1 woke a reader of partition 0")
	}
}

func TestWatchRefusesWhatDoesNotExist(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Watch("nope", 0); !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("missing topic: err = %v", err)
	}
	if _, err := b.Watch("events", 1); !errors.Is(err, ErrInvalidPartition) {
		t.Fatalf("missing partition: err = %v", err)
	}
	if _, err := b.Watch("events", -1); !errors.Is(err, ErrInvalidPartition) {
		t.Fatalf("negative partition: err = %v", err)
	}
}

func TestWatchFiresWhenTheBrokerCloses(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 1); err != nil {
		t.Fatal(err)
	}
	ch, err := b.Watch("events", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if !woken(ch, time.Second) {
		t.Fatal("a reader would wait on a closed broker until its own timeout")
	}
	if _, err := b.Watch("events", 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Watch after Close: err = %v, want ErrClosed", err)
	}
}

// The way a reader uses Watch: subscribe, read, wait, repeat. Writers run at
// the same time; no message may be missed and the reader never has to rely on
// a timeout to see one.
func TestAReaderUsingWatchSeesEveryMessageWithoutPolling(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 1); err != nil {
		t.Fatal(err)
	}

	const total = 300
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < total/3; i++ {
				if _, _, err := b.Produce("events", 0, []storage.Message{msg("k", "v")}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}

	seen := uint64(0)
	deadline := time.After(10 * time.Second)
	for seen < total {
		ch, err := b.Watch("events", 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Fetch("events", 0, seen, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 0 {
			seen += uint64(len(got))
			continue
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("stuck at %d of %d messages: a wake-up was lost", seen, total)
		}
	}
	wg.Wait()
}
