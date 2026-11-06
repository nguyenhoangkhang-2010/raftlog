package raft

import "errors"

var (
	// ErrIndexOutOfRange is returned when a log index does not exist.
	ErrIndexOutOfRange = errors.New("raft: log index out of range")
	// ErrCompacted is returned when a log entry was removed because a snapshot
	// covers it.
	ErrCompacted = errors.New("raft: log entry was compacted into a snapshot")
)

// LogStore is the persistent, ordered log of entries. Indexes start at 1;
// index 0 stands for "before the first entry" and has term 0.
//
// A snapshot replaces a prefix of the log. After CompactTo(index, term) the
// entries up to index are gone: only their last index and term (the "base")
// are remembered, because the next entry's consistency check needs the term of
// the entry just before it.
//
// A LogStore is used by one goroutine at a time (Runner serializes access).
type LogStore interface {
	// Base returns the index and term of the last entry removed by compaction,
	// or (0, 0) if the log was never compacted.
	Base() (index, term uint64)
	// FirstIndex is the index of the first entry still stored: Base index + 1.
	FirstIndex() uint64
	// LastIndex returns the index of the last entry, or the base index if the
	// log holds no entries.
	LastIndex() uint64
	// Term returns the term of the entry at index. It works for the base index
	// and for indexes up to LastIndex; below the base it returns ErrCompacted.
	Term(index uint64) (uint64, error)
	// Entries returns the entries in [lo, hi). It needs FirstIndex() <= lo <= hi
	// <= LastIndex()+1; entries below FirstIndex give ErrCompacted.
	Entries(lo, hi uint64) ([]Entry, error)
	// Append adds entries at the end. The first one must have index
	// LastIndex()+1 and the rest must follow without gaps. When Append returns
	// nil the entries are durable.
	Append(entries []Entry) error
	// TruncateSuffix removes every entry with index >= from. When it returns
	// nil the removal is durable. Raft only does this to uncommitted entries,
	// so it never reaches into compacted history (ErrCompacted).
	TruncateSuffix(from uint64) error
	// CompactTo removes the entries up to and including index, which has the
	// given term. If the log has an entry at index with that term, the entries
	// after it are kept; otherwise (the log is shorter, or disagrees with the
	// snapshot) the whole log is discarded and index becomes the new base. It
	// does nothing if index is not above the current base. When it returns nil
	// the change is durable.
	CompactTo(index, term uint64) error
	Close() error
}

// checkAppend validates that entries continue a log whose last index is last.
func checkAppend(last uint64, entries []Entry) error {
	for i, e := range entries {
		if e.Index != last+1+uint64(i) {
			return errors.New("raft: appended entries are not contiguous with the log")
		}
	}
	return nil
}

// MemoryLog is a LogStore that keeps everything in memory. It is not durable;
// it is meant for tests and for nodes that do not need to survive a restart.
type MemoryLog struct {
	baseIndex uint64
	baseTerm  uint64
	entries   []Entry // entries[i] has index baseIndex+1+i
}

// NewMemoryLog returns an empty in-memory log.
func NewMemoryLog() *MemoryLog { return &MemoryLog{} }

// Base implements LogStore.
func (l *MemoryLog) Base() (uint64, uint64) { return l.baseIndex, l.baseTerm }

// FirstIndex implements LogStore.
func (l *MemoryLog) FirstIndex() uint64 { return l.baseIndex + 1 }

// LastIndex implements LogStore.
func (l *MemoryLog) LastIndex() uint64 { return l.baseIndex + uint64(len(l.entries)) }

// Term implements LogStore.
func (l *MemoryLog) Term(index uint64) (uint64, error) {
	switch {
	case index == l.baseIndex:
		return l.baseTerm, nil
	case index < l.baseIndex:
		return 0, ErrCompacted
	case index > l.LastIndex():
		return 0, ErrIndexOutOfRange
	}
	return l.entries[index-l.baseIndex-1].Term, nil
}

// Entries implements LogStore.
func (l *MemoryLog) Entries(lo, hi uint64) ([]Entry, error) {
	if lo < 1 || lo > hi || hi > l.LastIndex()+1 {
		return nil, ErrIndexOutOfRange
	}
	if lo < hi && lo <= l.baseIndex {
		return nil, ErrCompacted
	}
	if lo == hi {
		return nil, nil
	}
	return append([]Entry(nil), l.entries[lo-l.baseIndex-1:hi-l.baseIndex-1]...), nil
}

// Append implements LogStore.
func (l *MemoryLog) Append(entries []Entry) error {
	if err := checkAppend(l.LastIndex(), entries); err != nil {
		return err
	}
	l.entries = append(l.entries, entries...)
	return nil
}

// TruncateSuffix implements LogStore.
func (l *MemoryLog) TruncateSuffix(from uint64) error {
	if from < 1 {
		return ErrIndexOutOfRange
	}
	if from <= l.baseIndex {
		return ErrCompacted
	}
	if from <= l.LastIndex() {
		l.entries = l.entries[:from-l.baseIndex-1]
	}
	return nil
}

// CompactTo implements LogStore.
func (l *MemoryLog) CompactTo(index, term uint64) error {
	if index <= l.baseIndex {
		return nil
	}
	if index <= l.LastIndex() {
		if have, _ := l.Term(index); have == term {
			l.entries = append([]Entry(nil), l.entries[index-l.baseIndex:]...)
			l.baseIndex, l.baseTerm = index, term
			return nil
		}
	}
	l.entries = nil
	l.baseIndex, l.baseTerm = index, term
	return nil
}

// Close implements LogStore.
func (l *MemoryLog) Close() error { return nil }
