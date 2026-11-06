package raft

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// MemorySnapshotStore keeps the snapshot in memory. It is not durable; it is
// meant for tests and for nodes that do not need to survive a restart.
type MemorySnapshotStore struct {
	snap Snapshot
	ok   bool
}

// NewMemorySnapshotStore returns an empty store.
func NewMemorySnapshotStore() *MemorySnapshotStore { return &MemorySnapshotStore{} }

// Load implements SnapshotStore.
func (s *MemorySnapshotStore) Load() (Snapshot, bool, error) {
	if !s.ok {
		return Snapshot{}, false, nil
	}
	return copySnapshot(s.snap), true, nil
}

// Save implements SnapshotStore.
func (s *MemorySnapshotStore) Save(snap Snapshot) error {
	s.snap, s.ok = copySnapshot(snap), true
	return nil
}

func copySnapshot(s Snapshot) Snapshot {
	s.Data = append([]byte(nil), s.Data...)
	return s
}

// FileSnapshotStore keeps the snapshot in one file.
//
// Layout (big-endian): magic "RLSN" | index u64 | term u64 | dataLen u64 |
// data | crc32 of everything before it. Save writes a temporary file, fsyncs
// it and renames it over the old one, so the previous snapshot stays intact
// until the new one is complete.
type FileSnapshotStore struct {
	path string
}

const (
	snapshotMagic      = "RLSN"
	snapshotHeaderSize = 4 + 8 + 8 + 8
	snapshotTrailer    = 4
)

var snapshotCRC = crc32.MakeTable(crc32.Castagnoli)

// NewFileSnapshotStore returns a store that keeps its snapshot in the file at
// path. The parent directory is created if needed.
func NewFileSnapshotStore(path string) (*FileSnapshotStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}
	_ = os.Remove(path + ".tmp") // an unfinished Save; the real file is intact
	return &FileSnapshotStore{path: path}, nil
}

// Save implements SnapshotStore.
func (s *FileSnapshotStore) Save(snap Snapshot) error {
	buf := make([]byte, snapshotHeaderSize+len(snap.Data), snapshotHeaderSize+len(snap.Data)+snapshotTrailer)
	copy(buf[0:4], snapshotMagic)
	binary.BigEndian.PutUint64(buf[4:12], snap.Index)
	binary.BigEndian.PutUint64(buf[12:20], snap.Term)
	binary.BigEndian.PutUint64(buf[20:28], uint64(len(snap.Data)))
	copy(buf[snapshotHeaderSize:], snap.Data)
	buf = binary.BigEndian.AppendUint32(buf, crc32.Checksum(buf, snapshotCRC))

	tmp := s.path + ".tmp"
	if err := writeFileSynced(tmp, buf); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit snapshot: %w", err)
	}
	return nil
}

// Load implements SnapshotStore. A damaged file is an error, not "no snapshot":
// the log may already have been cut down to it.
func (s *FileSnapshotStore) Load() (Snapshot, bool, error) {
	buf, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("read snapshot: %w", err)
	}
	if len(buf) < snapshotHeaderSize+snapshotTrailer || string(buf[0:4]) != snapshotMagic {
		return Snapshot{}, false, fmt.Errorf("snapshot %s: not a snapshot file", s.path)
	}
	dataLen := binary.BigEndian.Uint64(buf[20:28])
	if uint64(len(buf)) != snapshotHeaderSize+dataLen+snapshotTrailer {
		return Snapshot{}, false, fmt.Errorf("snapshot %s: size does not match its header", s.path)
	}
	body := buf[:len(buf)-snapshotTrailer]
	if crc32.Checksum(body, snapshotCRC) != binary.BigEndian.Uint32(buf[len(buf)-snapshotTrailer:]) {
		return Snapshot{}, false, fmt.Errorf("snapshot %s: checksum mismatch", s.path)
	}
	return Snapshot{
		Index: binary.BigEndian.Uint64(buf[4:12]),
		Term:  binary.BigEndian.Uint64(buf[12:20]),
		Data:  append([]byte(nil), buf[snapshotHeaderSize:len(body)]...),
	}, true, nil
}
