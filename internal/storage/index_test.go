package storage

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func value(i int) string { return fmt.Sprintf("value-%05d", i) }

// fillLog appends n messages whose values are derived from their offset.
func fillLog(t testing.TB, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(Message{Key: []byte("k"), Value: []byte(value(i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

// checkAllReads verifies that reading from every offset returns exactly the
// messages that were written, regardless of how the index is laid out.
func checkAllReads(t *testing.T, l *Log, n int) {
	t.Helper()
	const window = 5
	for from := 0; from < n; from++ {
		msgs, err := l.Read(uint64(from), window)
		if err != nil {
			t.Fatalf("Read(%d): %v", from, err)
		}
		want := window
		if n-from < want {
			want = n - from
		}
		if len(msgs) != want {
			t.Fatalf("Read(%d): got %d msgs, want %d", from, len(msgs), want)
		}
		for j, m := range msgs {
			if m.Offset != uint64(from+j) || string(m.Value) != value(from+j) {
				t.Fatalf("Read(%d)[%d] = offset %d value %q", from, j, m.Offset, m.Value)
			}
		}
	}
}

func indexFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"+indexSuffix))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func indexEntryCount(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Size() / indexEntrySize)
}

func TestIndexEntryEncoding(t *testing.T) {
	e := indexEntry{offset: 1 << 40, position: 987654321}
	buf := e.appendTo(nil)
	if len(buf) != indexEntrySize {
		t.Fatalf("encoded size = %d, want %d", len(buf), indexEntrySize)
	}
	path := filepath.Join(t.TempDir(), "x.index")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	got, size, ok := readIndex(f, 0, 1<<30)
	if !ok || size != indexEntrySize || len(got) != 1 || got[0] != e {
		t.Fatalf("readIndex = %+v size=%d ok=%v", got, size, ok)
	}
}

func TestSegmentLookup(t *testing.T) {
	s := &segment{
		baseOffset: 100,
		entries: []indexEntry{
			{offset: 110, position: 1000},
			{offset: 130, position: 3000},
			{offset: 160, position: 6000},
		},
	}
	cases := []struct {
		offset  uint64
		wantPos int64
		wantOff uint64
	}{
		{100, 0, 100},     // before the first entry: start of segment
		{109, 0, 100},     // still before the first entry
		{110, 1000, 110},  // exactly on an entry
		{129, 1000, 110},  // between entries: closest one before
		{130, 3000, 130},  // exactly on an entry
		{159, 3000, 130},  // between entries
		{160, 6000, 160},  // last entry
		{5000, 6000, 160}, // beyond the last entry
	}
	for _, c := range cases {
		pos, off := s.lookup(c.offset)
		if pos != c.wantPos || off != c.wantOff {
			t.Errorf("lookup(%d) = (%d, %d), want (%d, %d)", c.offset, pos, off, c.wantPos, c.wantOff)
		}
	}
}

func TestIndexIsSparse(t *testing.T) {
	const n = 400
	l, dir := openTestLog(t, Options{IndexIntervalBytes: 500})
	fillLog(t, l, n)

	files := indexFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("got %d index files, want 1", len(files))
	}
	entries := indexEntryCount(t, files[0])
	if entries < 2 {
		t.Fatalf("index has %d entries, expected several", entries)
	}
	if entries >= n/2 {
		t.Fatalf("index has %d entries for %d records: not sparse", entries, n)
	}

	// The lookup must actually skip data for an offset near the end.
	pos, off := l.active().lookup(n - 1)
	if pos == 0 || off == 0 {
		t.Fatalf("lookup near the end returned the segment start (%d, %d): index not used", pos, off)
	}
}

func TestReadIsCorrectForAnyIndexInterval(t *testing.T) {
	const n = 300
	// 1 = every record, tiny = a few, huge = effectively no index.
	for _, interval := range []int64{1, 40, 500, 1 << 40} {
		for _, segMax := range []int64{0, 400} { // 0 = default (one segment), 400 = many segments
			t.Run(fmt.Sprintf("interval=%d/seg=%d", interval, segMax), func(t *testing.T) {
				l, _ := openTestLog(t, Options{IndexIntervalBytes: interval, SegmentMaxBytes: segMax})
				fillLog(t, l, n)
				checkAllReads(t, l, n)
			})
		}
	}
}

func TestReadCorrectAfterReopenWithIndex(t *testing.T) {
	const n = 300
	dir := t.TempDir()
	opts := Options{IndexIntervalBytes: 200, SegmentMaxBytes: 1500}
	l, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	fillLog(t, l, n)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	if l2.NextOffset() != n {
		t.Fatalf("NextOffset = %d, want %d", l2.NextOffset(), n)
	}
	checkAllReads(t, l2, n)
	if off, err := l2.Append(Message{Value: []byte("more")}); err != nil || off != n {
		t.Fatalf("append after reopen: off=%d err=%v", off, err)
	}
}

func TestIndexRebuiltWhenMissing(t *testing.T) {
	const n = 200
	dir := t.TempDir()
	opts := Options{IndexIntervalBytes: 300}
	l, _ := Open(dir, opts)
	fillLog(t, l, n)
	l.Close()

	for _, p := range indexFiles(t, dir) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}

	l2, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("reopen without index: %v", err)
	}
	defer l2.Close()
	checkAllReads(t, l2, n)

	files := indexFiles(t, dir)
	if len(files) != 1 || indexEntryCount(t, files[0]) < 2 {
		t.Fatalf("index was not rebuilt: %v", files)
	}
}

func TestIndexRebuiltWhenDamaged(t *testing.T) {
	const n = 200
	cases := map[string]func(t *testing.T, path string){
		"size not multiple of entry size": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("garbage-bytes-1"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"entries out of order": func(t *testing.T, path string) {
			var b []byte
			b = indexEntry{offset: 50, position: 900}.appendTo(b)
			b = indexEntry{offset: 10, position: 100}.appendTo(b)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"position points into the middle of a record": func(t *testing.T, path string) {
			b := indexEntry{offset: 5, position: 3}.appendTo(nil)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"offset does not match the record at position": func(t *testing.T, path string) {
			// Position 0 holds offset 0, so claiming offset 9 there is wrong.
			b := indexEntry{offset: 9, position: 0}.appendTo(nil)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"position beyond end of log": func(t *testing.T, path string) {
			b := indexEntry{offset: 5, position: 1 << 40}.appendTo(nil)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"huge file of zeros": func(t *testing.T, path string) {
			if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			opts := Options{IndexIntervalBytes: 300}
			l, _ := Open(dir, opts)
			fillLog(t, l, n)
			l.Close()

			files := indexFiles(t, dir)
			damage(t, files[0])

			l2, err := Open(dir, opts)
			if err != nil {
				t.Fatalf("reopen with damaged index: %v", err)
			}
			defer l2.Close()
			if l2.NextOffset() != n {
				t.Fatalf("NextOffset = %d, want %d", l2.NextOffset(), n)
			}
			checkAllReads(t, l2, n)
		})
	}
}

func TestIndexCatchesUpWhenItLagsBehindTheLog(t *testing.T) {
	const n = 300
	dir := t.TempDir()
	opts := Options{IndexIntervalBytes: 100}
	l, _ := Open(dir, opts)
	fillLog(t, l, n)
	l.Close()

	// Simulate a crash where the log reached disk but the index did not:
	// keep only the first two index entries.
	path := indexFiles(t, dir)[0]
	full := indexEntryCount(t, path)
	if full <= 3 {
		t.Fatalf("test needs a bigger index, got %d entries", full)
	}
	if err := os.Truncate(path, 2*indexEntrySize); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	checkAllReads(t, l2, n)
	if got := indexEntryCount(t, path); got <= 2 {
		t.Fatalf("index did not catch up: %d entries", got)
	}
	if off, err := l2.Append(Message{Value: []byte("more")}); err != nil || off != n {
		t.Fatalf("append after catch-up: off=%d err=%v", off, err)
	}
}

func TestTornWriteWithIndex(t *testing.T) {
	const n = 50
	dir := t.TempDir()
	opts := Options{IndexIntervalBytes: 1} // an entry for (almost) every record
	l, _ := Open(dir, opts)
	fillLog(t, l, n)
	l.Close()

	// Cut the last record in half. The index still has an entry pointing at it.
	path := activeSegmentPath(t, dir)
	info, _ := os.Stat(path)
	if err := os.Truncate(path, info.Size()-4); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if l2.NextOffset() != n-1 {
		t.Fatalf("NextOffset = %d, want %d", l2.NextOffset(), n-1)
	}
	checkAllReads(t, l2, n-1)

	// The lost offset is reused, and everything survives another reopen.
	if off, err := l2.Append(Message{Key: []byte("k"), Value: []byte(value(n - 1))}); err != nil || off != n-1 {
		t.Fatalf("append: off=%d err=%v", off, err)
	}
	l2.Close()
	l3, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer l3.Close()
	checkAllReads(t, l3, n)
}

func TestSealedSegmentsKeepTheirOwnIndex(t *testing.T) {
	l, dir := openTestLog(t, Options{IndexIntervalBytes: 100, SegmentMaxBytes: 800})
	fillLog(t, l, 200)
	logs, _ := filepath.Glob(filepath.Join(dir, "*"+segmentSuffix))
	if len(logs) < 3 {
		t.Fatalf("expected several segments, got %d", len(logs))
	}
	if idx := indexFiles(t, dir); len(idx) != len(logs) {
		t.Fatalf("%d index files for %d segments", len(idx), len(logs))
	}
}

// BenchmarkRead measures reading a small window at random offsets in a single
// large segment, with and without a useful index. Results depend on the
// machine; record them together with the hardware, never quote them blindly.
func BenchmarkRead(b *testing.B) {
	const (
		records = 200_000
		window  = 10
		seed    = 1
	)
	payload := bytes.Repeat([]byte("x"), 100)

	for _, tc := range []struct {
		name     string
		interval int64
	}{
		{"no-index", 1 << 40},
		{"interval-4KiB", 4096},
		{"interval-64KiB", 64 << 10},
	} {
		b.Run(tc.name, func(b *testing.B) {
			l, err := Open(b.TempDir(), Options{IndexIntervalBytes: tc.interval})
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()
			for i := 0; i < records; i++ {
				if _, err := l.Append(Message{Key: []byte("k"), Value: payload}); err != nil {
					b.Fatal(err)
				}
			}
			rng := rand.New(rand.NewSource(seed))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				from := uint64(rng.Intn(records - window))
				msgs, err := l.Read(from, window)
				if err != nil || len(msgs) != window {
					b.Fatalf("Read(%d): %d msgs, err=%v", from, len(msgs), err)
				}
			}
		})
	}
}
