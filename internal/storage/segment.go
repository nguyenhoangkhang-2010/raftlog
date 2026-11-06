package storage

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const segmentSuffix = ".log"

// segmentFileName returns the file name of the segment whose first offset is base.
// Zero-padding keeps names sortable lexicographically.
func segmentFileName(base uint64) string {
	return fmt.Sprintf("%020d%s", base, segmentSuffix)
}

// segment is one append-only file holding a contiguous range of offsets
// [baseOffset, nextOffset).
type segment struct {
	baseOffset uint64
	nextOffset uint64
	size       int64 // bytes of valid data in the file
	path       string
	f          *os.File
}

// openSegment opens (or creates) the segment file for the given base offset and
// scans it to rebuild nextOffset and size.
//
// If repair is true (used for the last, active segment), a torn or corrupt tail
// is truncated away, which is how the log recovers from a crash in the middle of
// a write. If repair is false (sealed segments), any damage is returned as an error.
func openSegment(dir string, base uint64, repair bool) (*segment, error) {
	path := filepath.Join(dir, segmentFileName(base))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open segment %s: %w", path, err)
	}

	s := &segment{baseOffset: base, nextOffset: base, path: path, f: f}
	if err := s.recover(repair); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// recover scans the whole file, validating every record.
func (s *segment) recover(repair bool) error {
	info, err := s.f.Stat()
	if err != nil {
		return fmt.Errorf("stat segment %s: %w", s.path, err)
	}
	fileSize := info.Size()

	r := bufio.NewReader(io.NewSectionReader(s.f, 0, fileSize))
	var pos int64
	next := s.baseOffset
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
		pos += n
		next++
	}

	if _, err := s.f.Seek(pos, io.SeekStart); err != nil {
		return fmt.Errorf("seek segment %s: %w", s.path, err)
	}
	s.size = pos
	s.nextOffset = next
	return nil
}

// append writes an already-encoded record and advances the segment state.
func (s *segment) append(rec []byte) error {
	n, err := s.f.Write(rec)
	if err != nil {
		// Roll the file back so a partial write is not left in the middle of the log.
		_ = s.f.Truncate(s.size)
		_, _ = s.f.Seek(s.size, io.SeekStart)
		return fmt.Errorf("write segment %s: %w", s.path, err)
	}
	s.size += int64(n)
	s.nextOffset++
	return nil
}

// read returns up to max messages with offset >= from.
//
// It scans from the start of the file. That is O(segment size); the sparse
// index added in a later branch removes the scan.
func (s *segment) read(from uint64, max int) ([]Message, error) {
	if max <= 0 || from >= s.nextOffset {
		return nil, nil
	}
	r := bufio.NewReader(io.NewSectionReader(s.f, 0, s.size))
	var out []Message
	for len(out) < max {
		m, _, err := readRecord(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("read segment %s: %w", s.path, err)
		}
		if m.Offset >= from {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *segment) sync() error  { return s.f.Sync() }
func (s *segment) close() error { return s.f.Close() }
