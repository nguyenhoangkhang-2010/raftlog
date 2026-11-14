package raft

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileStorage keeps the HardState in a single small JSON file.
//
// Save writes a temporary file, fsyncs it and renames it over the real one, so
// the file always holds either the old or the new state, never a mix.
type FileStorage struct {
	path string
}

// NewFileStorage returns a storage that keeps its state in the file at path.
// The parent directory is created if it does not exist.
func NewFileStorage(path string) (*FileStorage, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create raft state dir: %w", err)
	}
	return &FileStorage{path: path}, nil
}

type hardStateFile struct {
	Term     uint64 `json:"term"`
	VotedFor uint64 `json:"voted_for"`
}

// Load implements Storage.
func (s *FileStorage) Load() (HardState, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return HardState{}, nil
	}
	if err != nil {
		return HardState{}, fmt.Errorf("read raft state: %w", err)
	}
	var f hardStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return HardState{}, fmt.Errorf("parse raft state %s: %w", s.path, err)
	}
	return HardState{Term: f.Term, VotedFor: NodeID(f.VotedFor)}, nil
}

// Save implements Storage.
func (s *FileStorage) Save(hs HardState) error {
	data, err := json.Marshal(hardStateFile{Term: hs.Term, VotedFor: uint64(hs.VotedFor)})
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create raft state temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write raft state: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync raft state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close raft state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit raft state: %w", err)
	}
	return nil
}
