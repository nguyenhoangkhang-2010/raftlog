package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// testLogCompaction checks how every LogStore must behave around compaction.
func testLogCompaction(t *testing.T, open func(t *testing.T) LogStore) {
	// fill gives the log entries 1..3 in term 1 and 4..6 in term 2.
	fill := func(t *testing.T) LogStore {
		l := open(t)
		if err := l.Append(append(mkEntries(1, 3, 1), mkEntries(4, 3, 2)...)); err != nil {
			t.Fatal(err)
		}
		return l
	}

	t.Run("a fresh log has base zero", func(t *testing.T) {
		l := open(t)
		if i, term := l.Base(); i != 0 || term != 0 || l.FirstIndex() != 1 {
			t.Fatalf("base (%d,%d), first %d", i, term, l.FirstIndex())
		}
	})

	t.Run("entries after the snapshot are kept when the log agrees", func(t *testing.T) {
		l := fill(t)
		if err := l.CompactTo(4, 2); err != nil {
			t.Fatal(err)
		}
		if i, term := l.Base(); i != 4 || term != 2 || l.FirstIndex() != 5 || l.LastIndex() != 6 {
			t.Fatalf("base (%d,%d), first %d, last %d", i, term, l.FirstIndex(), l.LastIndex())
		}
		if term, err := l.Term(4); err != nil || term != 2 {
			t.Fatalf("Term(base) = %d, %v", term, err)
		}
		if term, err := l.Term(6); err != nil || term != 2 {
			t.Fatalf("Term(6) = %d, %v", term, err)
		}
		if _, err := l.Term(3); !errors.Is(err, ErrCompacted) {
			t.Fatalf("Term(3) err = %v, want ErrCompacted", err)
		}
		got, err := l.Entries(5, 7)
		if err != nil || !reflect.DeepEqual(indexes(got), []uint64{5, 6}) || string(got[0].Data) != "data-5" {
			t.Fatalf("Entries(5,7) = %v, %v", got, err)
		}
		if _, err := l.Entries(4, 6); !errors.Is(err, ErrCompacted) {
			t.Fatalf("Entries(4,6) err = %v, want ErrCompacted", err)
		}
		if empty, err := l.Entries(5, 5); err != nil || len(empty) != 0 {
			t.Fatalf("Entries(5,5) = %v, %v", empty, err)
		}
	})

	t.Run("the whole log is dropped when it disagrees with the snapshot", func(t *testing.T) {
		l := fill(t)
		if err := l.CompactTo(4, 9); err != nil { // entry 4 has term 2, not 9
			t.Fatal(err)
		}
		if i, term := l.Base(); i != 4 || term != 9 || l.LastIndex() != 4 {
			t.Fatalf("base (%d,%d), last %d", i, term, l.LastIndex())
		}
		if err := l.Append(mkEntries(5, 2, 9)); err != nil || l.LastIndex() != 6 {
			t.Fatalf("append after the reset: %v, last %d", err, l.LastIndex())
		}
	})

	t.Run("a snapshot beyond the end of the log replaces it", func(t *testing.T) {
		l := fill(t)
		if err := l.CompactTo(20, 3); err != nil {
			t.Fatal(err)
		}
		if i, term := l.Base(); i != 20 || term != 3 || l.FirstIndex() != 21 || l.LastIndex() != 20 {
			t.Fatalf("base (%d,%d), first %d, last %d", i, term, l.FirstIndex(), l.LastIndex())
		}
		if err := l.Append(mkEntries(21, 1, 3)); err != nil {
			t.Fatalf("append after the snapshot: %v", err)
		}
		if err := l.Append(mkEntries(7, 1, 3)); err == nil {
			t.Fatal("accepted an entry that does not follow the snapshot")
		}
	})

	t.Run("compacting at or below the base does nothing", func(t *testing.T) {
		l := fill(t)
		_ = l.CompactTo(4, 2)
		for _, idx := range []uint64{4, 3, 0} {
			if err := l.CompactTo(idx, 1); err != nil {
				t.Fatalf("CompactTo(%d): %v", idx, err)
			}
		}
		if i, term := l.Base(); i != 4 || term != 2 || l.LastIndex() != 6 {
			t.Fatalf("base (%d,%d), last %d: an old compaction changed the log", i, term, l.LastIndex())
		}
	})

	t.Run("compacting can be repeated", func(t *testing.T) {
		l := fill(t)
		_ = l.CompactTo(2, 1)
		_ = l.CompactTo(5, 2)
		if i, term := l.Base(); i != 5 || term != 2 || l.LastIndex() != 6 {
			t.Fatalf("base (%d,%d), last %d", i, term, l.LastIndex())
		}
		if got, err := l.Entries(6, 7); err != nil || len(got) != 1 {
			t.Fatalf("Entries(6,7) = %v, %v", got, err)
		}
	})

	t.Run("truncation cannot reach into the snapshot", func(t *testing.T) {
		l := fill(t)
		_ = l.CompactTo(4, 2)
		for _, from := range []uint64{1, 4} {
			if err := l.TruncateSuffix(from); !errors.Is(err, ErrCompacted) {
				t.Fatalf("TruncateSuffix(%d) err = %v, want ErrCompacted", from, err)
			}
		}
		if err := l.TruncateSuffix(6); err != nil || l.LastIndex() != 5 {
			t.Fatalf("TruncateSuffix(6): %v, last %d", err, l.LastIndex())
		}
		// Cutting right at the first stored entry leaves just the base.
		if err := l.TruncateSuffix(5); err != nil || l.LastIndex() != 4 {
			t.Fatalf("TruncateSuffix(5): %v, last %d", err, l.LastIndex())
		}
	})
}

func TestMemoryLogCompaction(t *testing.T) {
	testLogCompaction(t, func(*testing.T) LogStore { return NewMemoryLog() })
}

func fileLogOpener(t *testing.T) LogStore {
	l, err := OpenFileLog(filepath.Join(t.TempDir(), "raft", "log.wal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestFileLogCompaction(t *testing.T) { testLogCompaction(t, fileLogOpener) }

// --- the file log across restarts --------------------------------------------

func TestFileLogKeepsItsBaseAcrossAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.wal")
	l := openFileLog(t, path)
	_ = l.Append(append(mkEntries(1, 3, 1), mkEntries(4, 3, 2)...))
	if err := l.CompactTo(4, 2); err != nil {
		t.Fatal(err)
	}
	// It keeps working after the file was swapped: append, cut, append.
	if err := l.Append(mkEntries(7, 2, 3)); err != nil {
		t.Fatal(err)
	}
	if err := l.TruncateSuffix(8); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(mkEntries(8, 1, 4)); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	l2 := openFileLog(t, path)
	defer l2.Close()
	if i, term := l2.Base(); i != 4 || term != 2 || l2.LastIndex() != 8 {
		t.Fatalf("after reopen base (%d,%d), last %d", i, term, l2.LastIndex())
	}
	if !reflect.DeepEqual(termsFrom(t, l2, 5, 8), []uint64{2, 2, 3, 4}) {
		t.Fatalf("terms = %v, want [2 2 3 4]", termsFrom(t, l2, 5, 8))
	}
	got, _ := l2.Entries(5, 6)
	if !bytes.Equal(got[0].Data, []byte("data-5")) {
		t.Fatalf("entry 5 = %q", got[0].Data)
	}
}

func termsFrom(t *testing.T, l LogStore, from, to uint64) []uint64 {
	t.Helper()
	var out []uint64
	for i := from; i <= to; i++ {
		term, err := l.Term(i)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, term)
	}
	return out
}

func TestFileLogShrinksWhenCompacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.wal")
	l := openFileLog(t, path)
	defer l.Close()
	_ = l.Append(mkEntries(1, 200, 1))
	before, _ := os.Stat(path)
	if err := l.CompactTo(190, 1); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size()/5 {
		t.Fatalf("file went from %d to %d bytes: compaction did not reclaim the space", before.Size(), after.Size())
	}
}

func TestFileLogIgnoresALeftoverFromAnUnfinishedCompaction(t *testing.T) {
	// A crash while the new file was being written leaves "<log>.tmp" next to a
	// complete old log. The old log wins and the leftover is deleted.
	path := filepath.Join(t.TempDir(), "log.wal")
	l := openFileLog(t, path)
	_ = l.Append(mkEntries(1, 5, 1))
	_ = l.Close()
	if err := os.WriteFile(path+".tmp", []byte("half written garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	l2 := openFileLog(t, path)
	defer l2.Close()
	if l2.LastIndex() != 5 {
		t.Fatalf("LastIndex = %d, want 5", l2.LastIndex())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("the leftover temporary file was not removed: %v", err)
	}
}

func TestFileLogRefusesABaseRecordInTheMiddle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.wal")
	var buf []byte
	buf = append(buf, encodeLogRecord(Entry{Index: 1, Term: 1})...)
	buf = append(buf, encodeLogRecord(Entry{Index: 5, Term: 1, Type: entryTypeBase})...)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileLog(path); err == nil {
		t.Fatal("opened a log whose base record is not first")
	}
}

func TestFileLogDropsATornTailAfterTheBaseRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.wal")
	l := openFileLog(t, path)
	_ = l.Append(mkEntries(1, 5, 1))
	_ = l.CompactTo(3, 1)
	size := l.size
	_ = l.Append(mkEntries(6, 1, 1))
	_ = l.Close()
	if err := os.Truncate(path, size+5); err != nil { // cut the last record in half
		t.Fatal(err)
	}

	l2 := openFileLog(t, path)
	defer l2.Close()
	if i, _ := l2.Base(); i != 3 || l2.LastIndex() != 5 {
		t.Fatalf("base %d, last %d; want base 3 and the torn entry 6 dropped", i, l2.LastIndex())
	}
}

// --- the snapshot files --------------------------------------------------------

func TestMemorySnapshotStore(t *testing.T) {
	s := NewMemorySnapshotStore()
	if _, ok, err := s.Load(); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	data := []byte("original")
	if err := s.Save(Snapshot{Index: 3, Term: 2, Data: data}); err != nil {
		t.Fatal(err)
	}
	data[0] = 'X' // the caller reuses its buffer
	got, ok, _ := s.Load()
	if !ok || got.Index != 3 || string(got.Data) != "original" {
		t.Fatalf("Load = %+v, %v: the store must keep its own copy", got, ok)
	}
	got.Data[0] = 'Y' // and so must a reader
	if again, _, _ := s.Load(); string(again.Data) != "original" {
		t.Fatal("a reader modified the stored snapshot")
	}
}

func TestFileSnapshotStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft", "snapshot.bin")
	s, err := NewFileSnapshotStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Load(); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}

	big := bytes.Repeat([]byte{0, 1, 2, 0xff}, 100_000)
	want := Snapshot{Index: 1 << 33, Term: 7, Data: big}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	s2, _ := NewFileSnapshotStore(path) // a different instance, as after a restart
	got, ok, err := s2.Load()
	if err != nil || !ok || got.Index != want.Index || got.Term != want.Term || !bytes.Equal(got.Data, big) {
		t.Fatalf("Load: ok=%v err=%v index=%d term=%d len=%d", ok, err, got.Index, got.Term, len(got.Data))
	}

	// A newer snapshot replaces the old one, leaving no temporary file.
	if err := s.Save(Snapshot{Index: 5, Term: 8}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s2.Load()
	if got.Index != 5 || len(got.Data) != 0 {
		t.Fatalf("after overwrite: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

func TestFileSnapshotStoreRejectsDamagedFiles(t *testing.T) {
	good := func(t *testing.T) (string, []byte) {
		path := filepath.Join(t.TempDir(), "snapshot.bin")
		s, _ := NewFileSnapshotStore(path)
		if err := s.Save(Snapshot{Index: 4, Term: 2, Data: []byte("some state")}); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		return path, data
	}
	cases := map[string]func(data []byte) []byte{
		"bit flipped in the data":    func(d []byte) []byte { d[snapshotHeaderSize+2] ^= 0x01; return d },
		"bit flipped in the index":   func(d []byte) []byte { d[5] ^= 0x01; return d },
		"truncated":                  func(d []byte) []byte { return d[:len(d)-3] },
		"extra bytes at the end":     func(d []byte) []byte { return append(d, 0) },
		"wrong magic":                func(d []byte) []byte { d[0] = 'X'; return d },
		"empty":                      func([]byte) []byte { return nil },
		"length field too large":     func(d []byte) []byte { binary.BigEndian.PutUint64(d[20:28], 1<<40); return d },
		"only a header, no checksum": func(d []byte) []byte { return d[:snapshotHeaderSize] },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			path, data := good(t)
			if err := os.WriteFile(path, damage(data), 0o644); err != nil {
				t.Fatal(err)
			}
			s, _ := NewFileSnapshotStore(path)
			// A damaged snapshot must be an error, never "no snapshot": the log
			// may already have been cut down to it.
			if _, ok, err := s.Load(); err == nil || ok {
				t.Fatalf("a damaged snapshot file was accepted (ok=%v err=%v)", ok, err)
			}
		})
	}
}

func TestFileSnapshotStoreIgnoresAnUnfinishedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.bin")
	s, _ := NewFileSnapshotStore(path)
	if err := s.Save(Snapshot{Index: 2, Term: 1, Data: []byte("old")}); err != nil {
		t.Fatal(err)
	}
	// A crash while writing the next snapshot leaves a partial temporary file.
	if err := os.WriteFile(path+".tmp", []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	s2, _ := NewFileSnapshotStore(path)
	got, ok, err := s2.Load()
	if err != nil || !ok || got.Index != 2 || string(got.Data) != "old" {
		t.Fatalf("the previous snapshot did not survive: %+v ok=%v err=%v", got, ok, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the partial temporary file was not cleaned up")
	}
}

func TestNodeOnFilesRestartsFromItsSnapshot(t *testing.T) {
	dir := t.TempDir()
	open := func() (*Node, *FileLog) {
		storage, _ := NewFileStorage(filepath.Join(dir, "state.json"))
		log := openFileLog(t, filepath.Join(dir, "log.wal"))
		snaps, err := NewFileSnapshotStore(filepath.Join(dir, "snapshot.bin"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(2, clusterIDs(3), storage, 2)
		cfg.Log, cfg.Snapshots = log, snaps
		n, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return n, log
	}

	n, log := open()
	if err := n.Step(appendMsg(3, 0, 0, 6, entry(1, 3), entry(2, 3), entry(3, 3), entry(4, 3), entry(5, 3), entry(6, 3))); err != nil {
		t.Fatal(err)
	}
	n.Messages()
	if got, _ := n.Committed(); len(got) != 6 {
		t.Fatalf("Committed = %d entries", len(got))
	}
	if err := n.Compact(5, snapData(5)); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()

	n2, log2 := open()
	defer log2.Close()
	if st := n2.Status(); st.Term != 3 || st.SnapshotIndex != 5 || st.CommitIndex != 5 || st.LastIndex != 6 {
		t.Fatalf("status after restart = %+v", st)
	}
	if restore := n2.TakeRestore(); restore == nil || !bytes.Equal(restore.Data, snapData(5)) {
		t.Fatalf("TakeRestore = %+v", restore)
	}
}
