package raft

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileStorageRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft", "state.json") // parent does not exist yet
	s, err := NewFileStorage(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Load()
	if err != nil || got != (HardState{}) {
		t.Fatalf("Load on a fresh storage = %+v, %v; want zero state", got, err)
	}

	want := HardState{Term: 12, VotedFor: 3}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	// A different instance on the same path sees it: this is what survives a restart.
	s2, _ := NewFileStorage(path)
	if got, err := s2.Load(); err != nil || got != want {
		t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
	}

	// Overwriting replaces the old state and leaves no temp file behind.
	want = HardState{Term: 13}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.Load(); got != want {
		t.Fatalf("after overwrite Load = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestFileStorageRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := NewFileStorage(path)
	if _, err := s.Load(); err == nil {
		t.Fatal("expected an error for a corrupt state file")
	}
}

func TestNodeRestoresStateFromFileStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := NewFileStorage(path)

	n, err := New(testConfig(1, clusterIDs(3), s, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Step(requestVote(2, 1, 6, 0, 0)); err != nil {
		t.Fatal(err)
	}

	s2, _ := NewFileStorage(path)
	n2, err := New(testConfig(1, clusterIDs(3), s2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if st := n2.Status(); st.Term != 6 || st.VotedFor != 2 {
		t.Fatalf("restored status = %+v", st)
	}
}
