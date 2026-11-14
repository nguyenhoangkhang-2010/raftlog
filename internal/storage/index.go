package storage

import (
	"encoding/binary"
	"fmt"
	"os"
)

const (
	indexSuffix    = ".index"
	indexEntrySize = 16 // offset u64 + position u64

	// defaultIndexIntervalBytes is used when Options.IndexIntervalBytes is not set.
	defaultIndexIntervalBytes = 4096
)

// indexFileName returns the index file name for the segment starting at base.
func indexFileName(base uint64) string {
	return fmt.Sprintf("%020d%s", base, indexSuffix)
}

// indexEntry says: the record with this offset starts at this byte position
// in the segment's .log file.
//
// The index is sparse: an entry is added only after at least
// IndexIntervalBytes of log data since the previous entry. A lookup finds the
// closest entry at or before the wanted offset and scans forward from there,
// so at most about IndexIntervalBytes of data is read instead of the segment.
//
// The index is only a hint derived from the log. It is never fsynced and is
// rebuilt from the .log file whenever it is missing, malformed or untrusted.
//
// File layout (big-endian): repeated [offset u64][position u64], with offsets
// and positions strictly increasing.
type indexEntry struct {
	offset   uint64
	position uint64
}

func (e indexEntry) appendTo(b []byte) []byte {
	var buf [indexEntrySize]byte
	binary.BigEndian.PutUint64(buf[0:8], e.offset)
	binary.BigEndian.PutUint64(buf[8:16], e.position)
	return append(b, buf[:]...)
}

// readIndex loads and validates an index file for a segment with the given
// base offset and log size. ok is false if the file cannot be trusted, in
// which case the caller must rebuild the index from the log.
func readIndex(f *os.File, base uint64, logSize int64) (entries []indexEntry, size int64, ok bool) {
	info, err := f.Stat()
	if err != nil {
		return nil, 0, false
	}
	size = info.Size()
	if size%indexEntrySize != 0 {
		return nil, 0, false
	}
	// Every record takes at least headerSize+bodyFixedSize bytes, so a valid
	// index can never be larger than this. It guards against huge garbage files.
	if size > logSize/(headerSize+bodyFixedSize)*indexEntrySize+indexEntrySize {
		return nil, 0, false
	}
	if size == 0 {
		return nil, 0, true
	}

	data := make([]byte, size)
	if n, err := f.ReadAt(data, 0); err != nil || int64(n) != size {
		return nil, 0, false
	}

	entries = make([]indexEntry, 0, size/indexEntrySize)
	for i := int64(0); i < size; i += indexEntrySize {
		e := indexEntry{
			offset:   binary.BigEndian.Uint64(data[i : i+8]),
			position: binary.BigEndian.Uint64(data[i+8 : i+16]),
		}
		if e.offset < base || e.position > uint64(logSize) {
			return nil, 0, false
		}
		if n := len(entries); n > 0 {
			prev := entries[n-1]
			if e.offset <= prev.offset || e.position <= prev.position {
				return nil, 0, false
			}
		}
		entries = append(entries, e)
	}
	return entries, size, true
}
