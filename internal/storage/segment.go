package storage

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const segmentSuffix = ".log"

// segmentFileName returns the file name of the segment whose first offset is base.
// Zero-padding keeps names sortable lexicographically.
func segmentFileName(base uint64) string {
	return fmt.Sprintf("%020d%s", base, segmentSuffix)
}

// segment is one append-only file holding a contiguous range of offsets
// [baseOffset, nextOffset), plus a sparse index file next to it.
type segment struct {
	baseOffset uint64
	nextOffset uint64
	size       int64 // bytes of valid data in the .log file
	path       string
	f          *os.File

	idxF          *os.File
	idxSize       int64        // bytes of valid data in the .index file
	entries       []indexEntry // in-memory copy of the index, ascending
	lastIndexPos  int64        // log position of the most recent index entry (0 if none)
	indexInterval int64        // add an entry after this many log bytes
}

// openSegment opens (or creates) the segment for the given base offset and
// recovers its state.
//
// If repair is true (used for the last, active segment), a torn or corrupt tail
// is truncated away, which is how the log recovers from a crash in the middle of
// a write. If repair is false (sealed segments), any damage is returned as an error.
func openSegment(dir string, base uint64, repair bool, indexInterval int64) (*segment, error) {
	path := filepath.Join(dir, segmentFileName(base))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open segment %s: %w", path, err)
	}
	idxPath := filepath.Join(dir, indexFileName(base))
	idxF, err := os.OpenFile(idxPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("open index %s: %w", idxPath, err)
	}

	s := &segment{
		baseOffset:    base,
		nextOffset:    base,
		path:          path,
		f:             f,
		idxF:          idxF,
		indexInterval: indexInterval,
	}
	if err := s.recover(repair); err != nil {
		_ = f.Close()
		_ = idxF.Close()
		return nil, err
	}
	return s, nil
}

// recover rebuilds the in-memory state of the segment after opening it.
//
// With a trustworthy index it only scans the log from the last index entry to
// the end (at most about one index interval), so opening is fast even for
// large segments. If the index is missing, malformed or does not match the
// log, it scans the whole log and rebuilds the index.
//
// Trade-off: with a trustworthy index, damage in the middle of a segment is not
// found at open time; it is caught by the CRC when that record is read.
func (s *segment) recover(repair bool) error {
	info, err := s.f.Stat()
	if err != nil {
		return fmt.Errorf("stat segment %s: %w", s.path, err)
	}
	logSize := info.Size()

	entries, idxSize, ok := readIndex(s.idxF, s.baseOffset, logSize)
	if !ok {
		entries, idxSize = nil, 0
	}

	startPos, startOffset, lastIdx := int64(0), s.baseOffset, int64(0)
	if n := len(entries); n > 0 {
		last := entries[n-1]
		if s.probe(int64(last.position), last.offset, logSize) {
			startPos, startOffset, lastIdx = int64(last.position), last.offset, int64(last.position)
		} else {
			entries, idxSize = nil, 0 // index does not match the log
		}
	}
	if len(entries) == 0 {
		// No usable index: start from scratch and rebuild it while scanning.
		if err := s.idxF.Truncate(0); err != nil {
			return fmt.Errorf("reset index %s: %w", s.idxF.Name(), err)
		}
		idxSize = 0
	}

	r := bufio.NewReader(io.NewSectionReader(s.f, startPos, logSize-startPos))
	pos, next := startPos, startOffset
	var added []indexEntry
	for {
		m, n, err := readRecord(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err == nil && m.Offset != next {
			err = ErrCorrupt // offsets must be contiguous
		}
		if err != nil {
			if !repair {
				return fmt.Errorf("segment %s at byte %d: %w", s.path, pos, err)
			}
			// Active segment: drop the damaged tail and carry on.
			if terr := s.f.Truncate(pos); terr != nil {
				return fmt.Errorf("truncate segment %s: %w", s.path, terr)
			}
			break
		}
		if pos-lastIdx >= s.indexInterval {
			added = append(added, indexEntry{offset: m.Offset, position: uint64(pos)})
			lastIdx = pos
		}
		pos += n
		next++
	}

	if _, err := s.f.Seek(pos, io.SeekStart); err != nil {
		return fmt.Errorf("seek segment %s: %w", s.path, err)
	}
	s.size = pos
	s.nextOffset = next
	s.entries = append(entries, added...)
	s.idxSize = idxSize
	s.lastIndexPos = lastIdx

	if len(added) > 0 {
		var buf []byte
		for _, e := range added {
			buf = e.appendTo(buf)
		}
		if _, err := s.idxF.WriteAt(buf, s.idxSize); err != nil {
			return fmt.Errorf("write index %s: %w", s.idxF.Name(), err)
		}
		s.idxSize += int64(len(buf))
	}
	return nil
}

// probe reports whether the record at pos in the log is a valid record with
// the given offset, i.e. whether an index entry pointing there can be trusted.
// A position exactly at the end of the log is accepted (the entry points at
// the next record to be written).
func (s *segment) probe(pos int64, offset uint64, logSize int64) bool {
	if pos > logSize {
		return false
	}
	if pos == logSize {
		return true
	}
	r := bufio.NewReader(io.NewSectionReader(s.f, pos, logSize-pos))
	m, _, err := readRecord(r)
	return err == nil && m.Offset == offset
}

// append writes an already-encoded record and advances the segment state.
func (s *segment) append(rec []byte) error {
	start := s.size
	n, err := s.f.Write(rec)
	if err != nil {
		// Roll the file back so a partial write is not left in the middle of the log.
		_ = s.f.Truncate(s.size)
		_, _ = s.f.Seek(s.size, io.SeekStart)
		return fmt.Errorf("write segment %s: %w", s.path, err)
	}
	s.size += int64(n)
	offset := s.nextOffset
	s.nextOffset++

	if start-s.lastIndexPos >= s.indexInterval {
		s.addIndexEntry(indexEntry{offset: offset, position: uint64(start)})
	}
	return nil
}

// addIndexEntry appends an entry to the index file and the in-memory copy.
// The index is only a hint, so a failed write is not an error: the record is
// already safely in the log, and the next append simply tries again.
func (s *segment) addIndexEntry(e indexEntry) {
	if _, err := s.idxF.WriteAt(e.appendTo(nil), s.idxSize); err != nil {
		_ = s.idxF.Truncate(s.idxSize)
		return
	}
	s.idxSize += indexEntrySize
	s.entries = append(s.entries, e)
	s.lastIndexPos = int64(e.position)
}

// lookup returns the log position and offset of the closest index entry at or
// before offset. With no such entry it returns the start of the segment.
func (s *segment) lookup(offset uint64) (pos int64, entryOffset uint64) {
	i := sort.Search(len(s.entries), func(i int) bool {
		return s.entries[i].offset > offset
	}) - 1
	if i < 0 {
		return 0, s.baseOffset
	}
	return int64(s.entries[i].position), s.entries[i].offset
}

// read returns up to limit messages with offset >= from. It jumps to the
// closest index entry and scans forward from there.
func (s *segment) read(from uint64, limit int) ([]Message, error) {
	if limit <= 0 || from >= s.nextOffset {
		return nil, nil
	}
	startPos, expected := s.lookup(from)
	r := bufio.NewReader(io.NewSectionReader(s.f, startPos, s.size-startPos))
	var out []Message
	for len(out) < limit {
		m, _, err := readRecord(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err == nil && m.Offset != expected {
			err = ErrCorrupt
		}
		if err != nil {
			return out, fmt.Errorf("read segment %s: %w", s.path, err)
		}
		expected++
		if m.Offset >= from {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *segment) sync() error { return s.f.Sync() }

func (s *segment) close() error {
	return errors.Join(s.f.Close(), s.idxF.Close())
}
