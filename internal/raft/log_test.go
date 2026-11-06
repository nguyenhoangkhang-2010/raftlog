package raft

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func mkEntries(from uint64, n int, term uint64) []Entry {
	out := make([]Entry, n)
	for i := range out {
		idx := from + uint64(i)
		out[i] = Entry{Index: idx, Term: term, Data: []byte(fmt.Sprintf("data-%d", idx))}
	}
	return out
}

// testLogStore checks the behaviour every LogStore must have.
func testLogStore(t *testing.T, open func(t *testing.T) LogStore) {
	t.Run("empty", func(t *testing.T) {
		l := open(t)
		if l.LastIndex() != 0 {
			t.Fatalf("LastIndex = %d", l.LastIndex())
		}
		if term, err := l.Term(0); err != nil || term != 0 {
			t.Fatalf("Term(0) = %d, %v", term, err)
		}
		if _, err := l.Term(1); !errors.Is(err, ErrIndexOutOfRange) {
			t.Fatalf("Term(1) err = %v", err)
		}
		if es, err := l.Entries(1, 1); err != nil || len(es) != 0 {
			t.Fatalf("Entries(1,1) = %v, %v", es, err)
		}
	})

	t.Run("append and read", func(t *testing.T) {
		l := open(t)
		if err := l.Append(mkEntries(1, 3, 1)); err != nil {
			t.Fatal(err)
		}
		if err := l.Append(mkEntries(4, 2, 2)); err != nil {
			t.Fatal(err)
		}
		if l.LastIndex() != 5 {
			t.Fatalf("LastIndex = %d", l.LastIndex())
		}
		for i, want := range []uint64{1, 1, 1, 2, 2} {
			if term, err := l.Term(uint64(i + 1)); err != nil || term != want {
				t.Fatalf("Term(%d) = %d, %v; want %d", i+1, term, err, want)
			}
		}
		got, err := l.Entries(2, 5)
		if err != nil || !reflect.DeepEqual(indexes(got), []uint64{2, 3, 4}) || string(got[0].Data) != "data-2" {
			t.Fatalf("Entries(2,5) = %v, %v", got, err)
		}
		if err := l.Append(nil); err != nil {
			t.Fatalf("appending nothing: %v", err)
		}
	})

	t.Run("entry types and binary data survive", func(t *testing.T) {
		l := open(t)
		in := []Entry{
			{Index: 1, Term: 1, Type: EntryNoop},
			{Index: 2, Term: 1, Type: EntryNormal, Data: []byte{0, 1, 2, 0xff, 0}},
			{Index: 3, Term: 1, Type: EntryNormal, Data: nil},
		}
		if err := l.Append(in); err != nil {
			t.Fatal(err)
		}
		got, _ := l.Entries(1, 4)
		for i := range in {
			if got[i].Type != in[i].Type || !bytes.Equal(got[i].Data, in[i].Data) {
				t.Fatalf("entry %d = %+v, want %+v", i+1, got[i], in[i])
			}
		}
	})

	t.Run("rejects entries that do not continue the log", func(t *testing.T) {
		l := open(t)
		if err := l.Append(mkEntries(1, 2, 1)); err != nil {
			t.Fatal(err)
		}
		for name, bad := range map[string][]Entry{
			"gap":       mkEntries(4, 1, 1),
			"overlap":   mkEntries(2, 1, 1),
			"from zero": mkEntries(0, 1, 1),
			"inner gap": {{Index: 3, Term: 1}, {Index: 5, Term: 1}},
		} {
			if err := l.Append(bad); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		if l.LastIndex() != 2 {
			t.Fatalf("a rejected append changed the log: LastIndex = %d", l.LastIndex())
		}
	})

	t.Run("range checks", func(t *testing.T) {
		l := open(t)
		_ = l.Append(mkEntries(1, 3, 1))
		for _, r := range [][2]uint64{{0, 2}, {3, 2}, {1, 5}, {4, 5}} {
			if _, err := l.Entries(r[0], r[1]); !errors.Is(err, ErrIndexOutOfRange) {
				t.Errorf("Entries(%d,%d) err = %v", r[0], r[1], err)
			}
		}
		if _, err := l.Term(4); !errors.Is(err, ErrIndexOutOfRange) {
			t.Errorf("Term(4) err = %v", err)
		}
	})

	t.Run("truncate suffix", func(t *testing.T) {
		l := open(t)
		_ = l.Append(mkEntries(1, 5, 1))
		if err := l.TruncateSuffix(4); err != nil {
			t.Fatal(err)
		}
		if l.LastIndex() != 3 {
			t.Fatalf("LastIndex = %d, want 3", l.LastIndex())
		}
		// The log can grow again from the cut, with different content.
		if err := l.Append(mkEntries(4, 2, 7)); err != nil {
			t.Fatal(err)
		}
		if term, _ := l.Term(5); term != 7 || l.LastIndex() != 5 {
			t.Fatalf("after re-append: Term(5) = %d, LastIndex = %d", term, l.LastIndex())
		}
		if err := l.TruncateSuffix(99); err != nil {
			t.Fatalf("truncating past the end should be a no-op: %v", err)
		}
		if err := l.TruncateSuffix(1); err != nil || l.LastIndex() != 0 {
			t.Fatalf("truncate everything: %v, LastIndex %d", err, l.LastIndex())
		}
		if err := l.TruncateSuffix(0); err == nil {
			t.Fatal("TruncateSuffix(0) accepted")
		}
	})
}

func TestMemoryLog(t *testing.T) {
	testLogStore(t, func(*testing.T) LogStore { return NewMemoryLog() })
}

func TestFileLogBehavesLikeAnyLogStore(t *testing.T) {
	testLogStore(t, func(t *testing.T) LogStore {
		l, err := OpenFileLog(filepath.Join(t.TempDir(), "raft", "log.wal"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	})
}

// --- durability of the file log ----------------------------------------------

func openFileLog(t *testing.T, path string) *FileLog {
	t.Helper()
	l, err := OpenFileLog(path)
	if err != nil {
		t.Fatalf("OpenFileLog: %v", err)
	}
	return l
}

func TestFileLogSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.wal")
	l := openFileLog(t, path)
	_ = l.Append(mkEntries(1, 4, 1))
	_ = l.TruncateSuffix(3)
	_ = l.Append(mkEntries(3, 2, 2))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2 := openFileLog(t, path)
	defer l2.Close()
	if !reflect.DeepEqual(logTerms(t, l2), []uint64{1, 1, 2, 2}) {
		t.Fatalf("terms after reopen = %v, want [1 1 2 2]", logTerms(t, l2))
	}
	got, _ := l2.Entries(3, 5)
	if string(got[0].Data) != "data-3" || string(got[1].Data) != "data-4" {
		t.Fatalf("entries after reopen = %+v", got)
	}
	// It keeps working after the reload.
	if err := l2.Append(mkEntries(5, 1, 2)); err != nil || l2.LastIndex() != 5 {
		t.Fatalf("append after reopen: %v, LastIndex %d", err, l2.LastIndex())
	}
}

func writeThreeEntries(t *testing.T) (path string, sizes []int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "log.wal")
	l := openFileLog(t, path)
	for _, e := range mkEntries(1, 3, 1) {
		if err := l.Append([]Entry{e}); err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, l.size)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return path, sizes
}

func TestFileLogDropsATornFinalRecord(t *testing.T) {
	// A crash in the middle of a write leaves a partial record at the end. It
	// was never acknowledged, so dropping it is correct.
	for _, cut := range []int64{1, 3, 9} {
		t.Run(fmt.Sprintf("last record cut by %d bytes", cut), func(t *testing.T) {
			path, sizes := writeThreeEntries(t)
			if err := os.Truncate(path, sizes[2]-cut); err != nil {
				t.Fatal(err)
			}
			l := openFileLog(t, path)
			defer l.Close()
			if l.LastIndex() != 2 {
				t.Fatalf("LastIndex = %d, want 2", l.LastIndex())
			}
			if info, _ := os.Stat(path); info.Size() != sizes[1] {
				t.Fatalf("file size = %d, want it cut back to %d", info.Size(), sizes[1])
			}
			// The log is writable again from the cut point.
			if err := l.Append(mkEntries(3, 1, 5)); err != nil {
				t.Fatalf("append after recovery: %v", err)
			}
		})
	}

	t.Run("partial header", func(t *testing.T) {
		path, sizes := writeThreeEntries(t)
		f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		_, _ = f.Write([]byte{0, 0, 0})
		_ = f.Close()
		l := openFileLog(t, path)
		defer l.Close()
		if l.LastIndex() != 3 || l.size != sizes[2] {
			t.Fatalf("LastIndex = %d, size = %d; want 3 entries intact", l.LastIndex(), l.size)
		}
	})
}

func TestFileLogDropsAGarbledFinalRecord(t *testing.T) {
	path, _ := writeThreeEntries(t)
	data, _ := os.ReadFile(path)
	data[len(data)-1] ^= 0xFF // damage the checksum-covered body of the last record
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	l := openFileLog(t, path)
	defer l.Close()
	if l.LastIndex() != 2 {
		t.Fatalf("LastIndex = %d, want 2", l.LastIndex())
	}
}

func TestFileLogRefusesToOpenWhenAnEarlierRecordIsDamaged(t *testing.T) {
	// Damage before the last record cannot be a torn write: acknowledged
	// entries are gone. Silently starting with a shorter log could let a node
	// vote or commit against the cluster's history, so it must fail loudly.
	path, sizes := writeThreeEntries(t)
	data, _ := os.ReadFile(path)
	data[sizes[0]/2] ^= 0xFF // inside the first record
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileLog(path); err == nil {
		t.Fatal("opened a log with a damaged record in the middle")
	}
}

func TestFileLogRefusesAGapInTheIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.wal")
	var buf []byte
	buf = append(buf, encodeLogRecord(Entry{Index: 1, Term: 1})...)
	buf = append(buf, encodeLogRecord(Entry{Index: 3, Term: 1})...) // index 2 is missing
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileLog(path); err == nil {
		t.Fatal("opened a log with a gap")
	}
}

func TestFileLogKeepsTruncationAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.wal")
	l := openFileLog(t, path)
	_ = l.Append(mkEntries(1, 5, 1))
	if err := l.TruncateSuffix(3); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	l2 := openFileLog(t, path)
	defer l2.Close()
	if l2.LastIndex() != 2 {
		t.Fatalf("LastIndex = %d after reopening a truncated log, want 2", l2.LastIndex())
	}
}

func TestNodeOnAFileLogKeepsItsEntriesAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.wal")

	storage, _ := NewFileStorage(filepath.Join(dir, "state.json"))
	l := openFileLog(t, logPath)
	cfg := testConfig(2, clusterIDs(3), storage, 1)
	cfg.Log = l
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Step(appendMsg(4, 0, 0, 2, entry(1, 4), entry(2, 4), entry(3, 4))); err != nil {
		t.Fatal(err)
	}
	if r := only(t, n.Messages()); !r.Success || r.MatchIndex != 3 {
		t.Fatalf("reply = %+v", r)
	}
	_ = l.Close()

	// "Restart the process": reopen everything from disk.
	storage2, _ := NewFileStorage(filepath.Join(dir, "state.json"))
	l2 := openFileLog(t, logPath)
	defer l2.Close()
	cfg2 := testConfig(2, clusterIDs(3), storage2, 1)
	cfg2.Log = l2
	n2, err := New(cfg2)
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	if st := n2.Status(); st.Term != 4 || st.LastIndex != 3 {
		t.Fatalf("status after restart = %+v, want term 4 and 3 entries", st)
	}
}
