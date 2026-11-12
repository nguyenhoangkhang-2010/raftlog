package orderservice

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// Entry is a record of the log with the offset it has.
type Entry struct {
	Offset uint64
	Value  []byte
}

// Log is the part of raftlog that the service uses: one ordered list of byte strings.
type Log interface {
	// Append stores the value and returns its offset. If it returns an error the value may
	// or may not have been stored: the service reads the log again before it goes on.
	Append(ctx context.Context, value []byte) (offset uint64, err error)
	// Read returns up to limit entries from the offset on, or none if the offset is the end
	// of the log. A positive wait lets it wait that long for a new entry.
	Read(ctx context.Context, from uint64, limit int, wait time.Duration) ([]Entry, error)
}

// RaftLog keeps the records in one partition of a raftlog topic.
type RaftLog struct {
	c     *client.Client
	topic string
}

// NewRaftLog returns a log on the topic. Call Ensure to create the topic.
func NewRaftLog(c *client.Client, topic string) *RaftLog { return &RaftLog{c: c, topic: topic} }

// Ensure creates the topic, with one partition so that the log has one order of events,
// unless it is there already.
func (l *RaftLog) Ensure(ctx context.Context) error {
	err := l.c.CreateTopic(ctx, l.topic, 1)
	if err != nil && !errors.Is(err, client.ErrAlreadyExists) {
		return fmt.Errorf("creating topic %q: %w", l.topic, err)
	}
	if err == nil {
		return nil
	}
	// It exists: it must have exactly one partition, or the order of events is lost.
	topics, terr := l.c.Topics(ctx)
	if terr != nil {
		return fmt.Errorf("checking topic %q: %w", l.topic, terr)
	}
	for _, t := range topics {
		if t.Name == l.topic && t.Partitions() != 1 {
			return fmt.Errorf("topic %q has %d partitions; the kitchen log needs exactly one", l.topic, t.Partitions())
		}
	}
	return nil
}

// Append implements Log.
func (l *RaftLog) Append(ctx context.Context, value []byte) (uint64, error) {
	res, err := l.c.Produce(ctx, l.topic, []client.Message{{Value: value}}, client.ToPartition(0))
	if err != nil {
		return 0, err
	}
	return res.Offset, nil
}

// Read implements Log.
func (l *RaftLog) Read(ctx context.Context, from uint64, limit int, wait time.Duration) ([]Entry, error) {
	var opts []client.FetchOption
	if wait > 0 {
		opts = append(opts, client.WaitUpTo(wait))
	}
	recs, err := l.c.Fetch(ctx, l.topic, 0, from, limit, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(recs))
	for i, r := range recs {
		out[i] = Entry{Offset: r.Offset, Value: r.Value}
	}
	return out, nil
}

// MemoryLog is a log in memory, for tests. It can be made to fail, or to store a value and
// then report a failure, as a cluster does when the answer is lost.
type MemoryLog struct {
	mu      sync.Mutex
	entries [][]byte
	// FailNext makes the next Append fail without storing anything.
	FailNext error
	// LoseAnswerNext makes the next Append store the value and then fail.
	LoseAnswerNext error
	// HideReads makes the next that many Reads pretend that the log ends after HideAfter
	// entries, as a follower that is behind does; later reads see everything.
	HideAfter int
	HideReads int
	appends   int
}

// Append implements Log.
func (l *MemoryLog) Append(_ context.Context, value []byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.FailNext; err != nil {
		l.FailNext = nil
		return 0, err
	}
	l.entries = append(l.entries, append([]byte(nil), value...))
	l.appends++
	off := uint64(len(l.entries) - 1)
	if err := l.LoseAnswerNext; err != nil {
		l.LoseAnswerNext = nil
		return 0, err
	}
	return off, nil
}

// Read implements Log.
func (l *MemoryLog) Read(_ context.Context, from uint64, limit int, _ time.Duration) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	end := len(l.entries)
	if l.HideReads > 0 {
		l.HideReads--
		if l.HideAfter < end {
			end = l.HideAfter
		}
	}
	var out []Entry
	for o := int(from); o < end && len(out) < limit; o++ {
		out = append(out, Entry{Offset: uint64(o), Value: append([]byte(nil), l.entries[o]...)})
	}
	return out, nil
}

// Len is the number of entries stored.
func (l *MemoryLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
