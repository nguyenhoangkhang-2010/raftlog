package storage

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrOffsetOutOfRange is returned by Read when the requested offset is beyond
// the end of the log.
var ErrOffsetOutOfRange = errors.New("storage: offset out of range")

// ErrClosed is returned when using a Log after Close.
var ErrClosed = errors.New("storage: log is closed")

// Options configures a Log.
type Options struct {
	// SegmentMaxBytes is the size at which a new segment is started.
	// Defaults to 64 MiB.
	SegmentMaxBytes int64
	// SyncOnAppend calls fsync after every Append. It is the durable setting
	// and the slow one; batching and group commit come later.
	SyncOnAppend bool
}

const defaultSegmentMaxBytes = 64 << 20

// Log is an append-only, segmented message log stored in a directory.
// It is safe for concurrent use.
type Log struct {
	mu       sync.RWMutex
	dir      string
	opts     Options
	segments []*segment // sorted by baseOffset; the last one is active
	closed   bool
}

// Open opens the log stored in dir, creating it if needed. A torn or corrupt
// tail in the active segment (e.g. after a crash) is truncated.
func Open(dir string, opts Options) (*Log, error) {
	if opts.SegmentMaxBytes <= 0 {
		opts.SegmentMaxBytes = defaultSegmentMaxBytes
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}

	bases, err := listSegmentBases(dir)
	if err != nil {
		return nil, err
	}

	l := &Log{dir: dir, opts: opts}
	if len(bases) == 0 {
		bases = []uint64{0}
	}
	for i, base := range bases {
		isActive := i == len(bases)-1
		seg, err := openSegment(dir, base, isActive)
		if err != nil {
			_ = l.closeSegments()
			return nil, err
		}
		l.segments = append(l.segments, seg)
	}
	return l, nil
}

// listSegmentBases returns the base offsets of all segment files, ascending.
func listSegmentBases(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read log dir: %w", err)
	}
	var bases []uint64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, segmentSuffix) {
			continue
		}
		base, err := strconv.ParseUint(strings.TrimSuffix(name, segmentSuffix), 10, 64)
		if err != nil {
			continue // not one of ours
		}
		bases = append(bases, base)
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i] < bases[j] })
	return bases, nil
}

func (l *Log) active() *segment { return l.segments[len(l.segments)-1] }

// Append adds a message to the end of the log and returns the offset assigned
// to it. If m.Timestamp is zero, the current time is used.
func (l *Log) Append(m Message) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}

	if m.Timestamp == 0 {
		m.Timestamp = time.Now().UnixMilli()
	}
	seg := l.active()
	m.Offset = seg.nextOffset

	rec, err := encode(&m)
	if err != nil {
		return 0, err
	}

	// Start a new segment if this record would push the active one over the
	// limit. An empty segment always accepts at least one record.
	if seg.size > 0 && seg.size+int64(len(rec)) > l.opts.SegmentMaxBytes {
		if seg, err = l.roll(); err != nil {
			return 0, err
		}
	}

	if err := seg.append(rec); err != nil {
		return 0, err
	}
	if l.opts.SyncOnAppend {
		if err := seg.sync(); err != nil {
			return 0, fmt.Errorf("sync: %w", err)
		}
	}
	return m.Offset, nil
}

// roll seals the active segment and starts a new one.
func (l *Log) roll() (*segment, error) {
	old := l.active()
	if err := old.sync(); err != nil {
		return nil, fmt.Errorf("sync before roll: %w", err)
	}
	next, err := openSegment(l.dir, old.nextOffset, true)
	if err != nil {
		return nil, err
	}
	l.segments = append(l.segments, next)
	return next, nil
}

// Read returns up to limit messages starting at offset, in order, crossing
// segment boundaries if needed. Reading at NextOffset returns no messages;
// reading beyond it returns ErrOffsetOutOfRange.
func (l *Log) Read(offset uint64, limit int) ([]Message, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return nil, ErrClosed
	}

	end := l.active().nextOffset
	if offset > end || offset < l.segments[0].baseOffset {
		return nil, ErrOffsetOutOfRange
	}
	if limit <= 0 || offset == end {
		return nil, nil
	}

	// Find the last segment whose baseOffset <= offset.
	i := sort.Search(len(l.segments), func(i int) bool {
		return l.segments[i].baseOffset > offset
	}) - 1

	var out []Message
	for ; i < len(l.segments) && len(out) < limit; i++ {
		msgs, err := l.segments[i].read(offset, limit-len(out))
		if err != nil {
			return out, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// NextOffset returns the offset that the next appended message will get.
func (l *Log) NextOffset() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.active().nextOffset
}

// Sync flushes the active segment to stable storage.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	return l.active().sync()
}

// Close syncs and closes all segment files.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	syncErr := l.active().sync()
	closeErr := l.closeSegments()
	return errors.Join(syncErr, closeErr)
}

func (l *Log) closeSegments() error {
	var errs []error
	for _, s := range l.segments {
		errs = append(errs, s.close())
	}
	return errors.Join(errs...)
}
