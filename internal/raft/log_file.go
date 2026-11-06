package raft

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// FileLog is a LogStore backed by a write-ahead log file.
//
// Record layout (big-endian):
//
//	[bodyLen u32][crc32 u32][body]
//	body = index u64 | term u64 | type u8 | data
//
// After a compaction the file starts with one special record (type
// entryTypeBase) whose index and term are the base of the log, followed by the
// entries that were kept.
//
// All entries are also kept in memory, so reads never touch the disk.
//
// Every Append and TruncateSuffix is fsynced before it returns, because a
// follower must not acknowledge an entry it could lose in a crash. CompactTo
// writes a complete new file and swaps it in with an atomic rename, so a crash
// leaves either the old log or the new one.
type FileLog struct {
	path      string
	f         *os.File
	baseIndex uint64
	baseTerm  uint64
	entries   []Entry // entries[i] has index baseIndex+1+i
	offsets   []int64 // offsets[i] is where the record of entries[i] starts
	size      int64   // bytes of valid data in the file
}

const (
	fileLogHeaderSize = 8
	fileLogBodyFixed  = 8 + 8 + 1
	// maxEntryBytes is a sanity limit on one record body.
	maxEntryBytes = 256 << 20

	// entryTypeBase marks the record that holds the base of a compacted log.
	entryTypeBase EntryType = 255
)

var fileLogCRC = crc32.MakeTable(crc32.Castagnoli)

var (
	errLogTorn = errors.New("raft log: torn record")
	errLogCRC  = errors.New("raft log: checksum mismatch")
)

// OpenFileLog opens (or creates) the log file at path and loads its entries.
//
// A torn or garbled record at the very end of the file is the signature of a
// crash during a write; it was never acknowledged, so it is dropped. Damage
// anywhere else would mean acknowledged data was lost, so it is an error.
func OpenFileLog(path string) (*FileLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create raft log dir: %w", err)
	}
	// A leftover temporary file is a compaction that did not finish. The real
	// file is still complete, so the leftover is simply discarded.
	_ = os.Remove(path + ".tmp")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open raft log: %w", err)
	}
	l := &FileLog{path: path, f: f}
	if err := l.load(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

func (l *FileLog) load() error {
	info, err := l.f.Stat()
	if err != nil {
		return fmt.Errorf("stat raft log: %w", err)
	}
	fileSize := info.Size()
	r := bufio.NewReader(io.NewSectionReader(l.f, 0, fileSize))

	var pos int64
	for {
		e, n, err := readLogRecord(r)
		if errors.Is(err, io.EOF) {
			break
		}
		// A bad checksum on the final record is a partial write; anywhere else
		// it is corruption.
		tornTail := errors.Is(err, errLogTorn) || (errors.Is(err, errLogCRC) && pos+n >= fileSize)
		if tornTail {
			if terr := l.f.Truncate(pos); terr != nil {
				return fmt.Errorf("truncate torn raft log tail: %w", terr)
			}
			break
		}
		if err != nil {
			return fmt.Errorf("raft log: corrupt record at byte %d: %w", pos, err)
		}

		if e.Type == entryTypeBase {
			if pos != 0 {
				return fmt.Errorf("raft log: base record at byte %d, but it must come first", pos)
			}
			l.baseIndex, l.baseTerm = e.Index, e.Term
			pos += n
			continue
		}
		if want := l.baseIndex + uint64(len(l.entries)) + 1; e.Index != want {
			return fmt.Errorf("raft log: record at byte %d has index %d, expected %d", pos, e.Index, want)
		}
		l.entries = append(l.entries, e)
		l.offsets = append(l.offsets, pos)
		pos += n
	}
	l.size = pos
	return nil
}

func encodeLogRecord(e Entry) []byte {
	bodyLen := fileLogBodyFixed + len(e.Data)
	buf := make([]byte, fileLogHeaderSize+bodyLen)
	body := buf[fileLogHeaderSize:]
	binary.BigEndian.PutUint64(body[0:8], e.Index)
	binary.BigEndian.PutUint64(body[8:16], e.Term)
	body[16] = byte(e.Type)
	copy(body[17:], e.Data)
	binary.BigEndian.PutUint32(buf[0:4], uint32(bodyLen))
	binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(body, fileLogCRC))
	return buf
}

// readLogRecord reads one record. n is the size of the record on disk, set
// whenever the header was read, including when the checksum fails.
func readLogRecord(r io.Reader) (e Entry, n int64, err error) {
	var hdr [fileLogHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Entry{}, 0, io.EOF
		}
		return Entry{}, 0, errLogTorn
	}
	bodyLen := binary.BigEndian.Uint32(hdr[0:4])
	wantCRC := binary.BigEndian.Uint32(hdr[4:8])
	if bodyLen < fileLogBodyFixed || bodyLen > maxEntryBytes {
		return Entry{}, 0, fmt.Errorf("record length %d is impossible: %w", bodyLen, errLogCRC)
	}
	n = fileLogHeaderSize + int64(bodyLen)

	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return Entry{}, n, errLogTorn
	}
	if crc32.Checksum(body, fileLogCRC) != wantCRC {
		return Entry{}, n, errLogCRC
	}
	e = Entry{
		Index: binary.BigEndian.Uint64(body[0:8]),
		Term:  binary.BigEndian.Uint64(body[8:16]),
		Type:  EntryType(body[16]),
		Data:  append([]byte(nil), body[17:]...),
	}
	return e, n, nil
}

// Base implements LogStore.
func (l *FileLog) Base() (uint64, uint64) { return l.baseIndex, l.baseTerm }

// FirstIndex implements LogStore.
func (l *FileLog) FirstIndex() uint64 { return l.baseIndex + 1 }

// LastIndex implements LogStore.
func (l *FileLog) LastIndex() uint64 { return l.baseIndex + uint64(len(l.entries)) }

// Term implements LogStore.
func (l *FileLog) Term(index uint64) (uint64, error) {
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
func (l *FileLog) Entries(lo, hi uint64) ([]Entry, error) {
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
func (l *FileLog) Append(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := checkAppend(l.LastIndex(), entries); err != nil {
		return err
	}

	var buf []byte
	offsets := make([]int64, len(entries))
	pos := l.size
	for i, e := range entries {
		offsets[i] = pos
		rec := encodeLogRecord(e)
		buf = append(buf, rec...)
		pos += int64(len(rec))
	}

	// One write and one fsync for the whole batch. If either fails, cut the
	// file back so that memory and disk still agree.
	if _, err := l.f.WriteAt(buf, l.size); err != nil {
		_ = l.f.Truncate(l.size)
		return fmt.Errorf("write raft log: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		_ = l.f.Truncate(l.size)
		return fmt.Errorf("sync raft log: %w", err)
	}
	for _, e := range entries {
		e.Data = append([]byte(nil), e.Data...)
		l.entries = append(l.entries, e)
	}
	l.offsets = append(l.offsets, offsets...)
	l.size = pos
	return nil
}

// TruncateSuffix implements LogStore.
func (l *FileLog) TruncateSuffix(from uint64) error {
	if from < 1 {
		return ErrIndexOutOfRange
	}
	if from <= l.baseIndex {
		return ErrCompacted
	}
	if from > l.LastIndex() {
		return nil
	}
	keep := from - l.baseIndex - 1 // entries that stay
	cut := l.offsets[keep]
	if err := l.f.Truncate(cut); err != nil {
		return fmt.Errorf("truncate raft log: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("sync raft log: %w", err)
	}
	l.entries = l.entries[:keep]
	l.offsets = l.offsets[:keep]
	l.size = cut
	return nil
}

// CompactTo implements LogStore. It writes a new file that holds the base
// record and the entries that are kept, then replaces the old file with it.
func (l *FileLog) CompactTo(index, term uint64) error {
	if index <= l.baseIndex {
		return nil
	}
	var kept []Entry
	if index <= l.LastIndex() {
		if have, _ := l.Term(index); have == term {
			kept = l.entries[index-l.baseIndex:]
		}
	}

	buf := encodeLogRecord(Entry{Index: index, Term: term, Type: entryTypeBase})
	offsets := make([]int64, len(kept))
	pos := int64(len(buf))
	for i, e := range kept {
		offsets[i] = pos
		rec := encodeLogRecord(e)
		buf = append(buf, rec...)
		pos += int64(len(rec))
	}

	tmp := l.path + ".tmp"
	if err := writeFileSynced(tmp, buf); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write compacted raft log: %w", err)
	}

	// The open handle must be closed before the file can be replaced on Windows.
	if err := l.f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close raft log: %w", err)
	}
	renameErr := os.Rename(tmp, l.path)
	f, openErr := os.OpenFile(l.path, os.O_RDWR, 0o644)
	if openErr != nil {
		return fmt.Errorf("reopen raft log after compaction: %w", openErr)
	}
	l.f = f
	if renameErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace raft log: %w", renameErr)
	}

	l.entries = append([]Entry(nil), kept...)
	l.offsets = offsets
	l.baseIndex, l.baseTerm = index, term
	l.size = pos
	return nil
}

// writeFileSynced writes data to a new file and makes it durable.
func writeFileSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Close implements LogStore.
func (l *FileLog) Close() error {
	syncErr := l.f.Sync()
	return errors.Join(syncErr, l.f.Close())
}
