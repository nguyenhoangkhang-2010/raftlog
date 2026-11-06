package raft

import "errors"

// ErrIndexOutOfRange is returned when a log index does not exist.
var ErrIndexOutOfRange = errors.New("raft: log index out of range")

// LogStore is the persistent, ordered log of entries. Indexes start at 1;
// index 0 stands for "before the first entry" and has term 0.
//
// A LogStore is used by one goroutine at a time (Runner serializes access).
type LogStore interface {
	// LastIndex returns the index of the last entry, or 0 for an empty log.
	LastIndex() uint64
	// Term returns the term of the entry at index. Term(0) is 0.
	Term(index uint64) (uint64, error)
	// Entries returns the entries in [lo, hi). It needs 1 <= lo <= hi <= LastIndex()+1.
	Entries(lo, hi uint64) ([]Entry, error)
	// Append adds entries at the end. The first one must have index
	// LastIndex()+1 and the rest must follow without gaps. When Append returns
	// nil the entries are durable.
	Append(entries []Entry) error
	// TruncateSuffix removes every entry with index >= from. When it returns
	// nil the removal is durable. Raft only does this to uncommitted entries.
	TruncateSuffix(from uint64) error
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
	entries []Entry
}

// NewMemoryLog returns an empty in-memory log.
func NewMemoryLog() *MemoryLog { return &MemoryLog{} }

// LastIndex implements LogStore.
func (l *MemoryLog) LastIndex() uint64 { return uint64(len(l.entries)) }

// Term implements LogStore.
func (l *MemoryLog) Term(index uint64) (uint64, error) {
	if index == 0 {
		return 0, nil
	}
	if index > l.LastIndex() {
		return 0, ErrIndexOutOfRange
	}
	return l.entries[index-1].Term, nil
}

// Entries implements LogStore.
func (l *MemoryLog) Entries(lo, hi uint64) ([]Entry, error) {
	if lo < 1 || lo > hi || hi > l.LastIndex()+1 {
		return nil, ErrIndexOutOfRange
	}
	return append([]Entry(nil), l.entries[lo-1:hi-1]...), nil
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
	if from <= l.LastIndex() {
		l.entries = l.entries[:from-1]
	}
	return nil
}

// Close implements LogStore.
func (l *MemoryLog) Close() error { return nil }
