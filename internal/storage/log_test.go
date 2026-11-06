package storage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTestLog(t *testing.T, opts Options) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, dir
}

func mustAppend(t *testing.T, l *Log, key, value string) uint64 {
	t.Helper()
	off, err := l.Append(Message{Key: []byte(key), Value: []byte(value)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return off
}

func TestRecordRoundTrip(t *testing.T) {
	cases := []Message{
		{Offset: 0, Timestamp: 1, Key: nil, Value: nil},
		{Offset: 7, Timestamp: 1730851200000, Key: []byte("user_88"), Value: []byte(`{"event":"view"}`)},
		{Offset: 1 << 40, Timestamp: -5, Key: []byte{}, Value: bytes.Repeat([]byte{0xAB}, 10_000)},
	}
	for i, want := range cases {
		buf, err := encode(&want)
		if err != nil {
			t.Fatalf("case %d encode: %v", i, err)
		}
		if int64(len(buf)) != recordSize(&want) {
			t.Fatalf("case %d: recordSize=%d, encoded=%d", i, recordSize(&want), len(buf))
		}
		got, n, err := readRecord(bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("case %d readRecord: %v", i, err)
		}
		if n != int64(len(buf)) {
			t.Fatalf("case %d: consumed %d of %d bytes", i, n, len(buf))
		}
		if got.Offset != want.Offset || got.Timestamp != want.Timestamp ||
			!bytes.Equal(got.Key, want.Key) || !bytes.Equal(got.Value, want.Value) {
			t.Fatalf("case %d: got %+v, want %+v", i, got, want)
		}
	}
}

func TestReadRecordDetectsDamage(t *testing.T) {
	m := Message{Offset: 3, Timestamp: 9, Key: []byte("k"), Value: []byte("hello")}
	buf, _ := encode(&m)

	t.Run("empty input is clean EOF", func(t *testing.T) {
		if _, _, err := readRecord(bytes.NewReader(nil)); err == nil || err.Error() != "EOF" {
			t.Fatalf("want EOF, got %v", err)
		}
	})
	t.Run("truncated header is torn", func(t *testing.T) {
		if _, _, err := readRecord(bytes.NewReader(buf[:3])); !errors.Is(err, errTornRecord) {
			t.Fatalf("want errTornRecord, got %v", err)
		}
	})
	t.Run("truncated body is torn", func(t *testing.T) {
		if _, _, err := readRecord(bytes.NewReader(buf[:len(buf)-2])); !errors.Is(err, errTornRecord) {
			t.Fatalf("want errTornRecord, got %v", err)
		}
	})
	t.Run("flipped bit is corrupt", func(t *testing.T) {
		bad := append([]byte(nil), buf...)
		bad[len(bad)-1] ^= 0x01
		if _, _, err := readRecord(bytes.NewReader(bad)); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
}

func TestAppendAssignsSequentialOffsets(t *testing.T) {
	l, _ := openTestLog(t, Options{})
	for i := 0; i < 5; i++ {
		if got := mustAppend(t, l, "k", fmt.Sprintf("v%d", i)); got != uint64(i) {
			t.Fatalf("append %d: offset=%d", i, got)
		}
	}
	if l.NextOffset() != 5 {
		t.Fatalf("NextOffset=%d, want 5", l.NextOffset())
	}
}

func TestAppendSetsTimestamp(t *testing.T) {
	l, _ := openTestLog(t, Options{})
	mustAppend(t, l, "k", "v")
	msgs, err := l.Read(0, 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Read: %v, %d msgs", err, len(msgs))
	}
	if msgs[0].Timestamp == 0 {
		t.Fatal("timestamp was not filled in")
	}
}

func TestReadWindow(t *testing.T) {
	l, _ := openTestLog(t, Options{})
	for i := 0; i < 10; i++ {
		mustAppend(t, l, "k", fmt.Sprintf("v%d", i))
	}

	msgs, err := l.Read(3, 4)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d msgs, want 4", len(msgs))
	}
	for i, m := range msgs {
		if m.Offset != uint64(3+i) || string(m.Value) != fmt.Sprintf("v%d", 3+i) {
			t.Fatalf("msg %d = %+v", i, m)
		}
	}

	// A limit larger than what is left returns only what exists.
	msgs, err = l.Read(8, 100)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("tail read: %v, %d msgs", err, len(msgs))
	}
}

func TestReadBounds(t *testing.T) {
	l, _ := openTestLog(t, Options{})
	mustAppend(t, l, "k", "v")

	if msgs, err := l.Read(1, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("read at NextOffset: %v, %d msgs; want empty, no error", err, len(msgs))
	}
	if _, err := l.Read(2, 10); !errors.Is(err, ErrOffsetOutOfRange) {
		t.Fatalf("read past end: err=%v, want ErrOffsetOutOfRange", err)
	}
	if msgs, err := l.Read(0, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("limit=0: %v, %d msgs", err, len(msgs))
	}
}

func TestReopenKeepsDataAndContinuesOffsets(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Options{SyncOnAppend: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		mustAppend(t, l, "k", fmt.Sprintf("v%d", i))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()

	if l2.NextOffset() != 3 {
		t.Fatalf("NextOffset after reopen = %d, want 3", l2.NextOffset())
	}
	if off := mustAppend(t, l2, "k", "v3"); off != 3 {
		t.Fatalf("first offset after reopen = %d, want 3", off)
	}
	msgs, err := l2.Read(0, 10)
	if err != nil || len(msgs) != 4 {
		t.Fatalf("Read: %v, %d msgs", err, len(msgs))
	}
	if string(msgs[3].Value) != "v3" {
		t.Fatalf("last value = %q", msgs[3].Value)
	}
}

func TestSegmentRollAndCrossSegmentRead(t *testing.T) {
	// Tiny segments force a roll every couple of records.
	l, dir := openTestLog(t, Options{SegmentMaxBytes: 120})
	const n = 20
	for i := 0; i < n; i++ {
		mustAppend(t, l, "key", fmt.Sprintf("value-%02d", i))
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*"+segmentSuffix))
	if len(files) < 3 {
		t.Fatalf("expected several segment files, got %d", len(files))
	}

	msgs, err := l.Read(0, n)
	if err != nil || len(msgs) != n {
		t.Fatalf("Read all: %v, %d msgs", err, len(msgs))
	}
	for i, m := range msgs {
		if m.Offset != uint64(i) || string(m.Value) != fmt.Sprintf("value-%02d", i) {
			t.Fatalf("msg %d = %+v", i, m)
		}
	}

	// A read that starts in the middle of the log and spans a boundary.
	msgs, err = l.Read(7, 9)
	if err != nil || len(msgs) != 9 || msgs[0].Offset != 7 || msgs[8].Offset != 15 {
		t.Fatalf("mid read: err=%v n=%d", err, len(msgs))
	}

	// Segments must survive reopen.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(dir, Options{SegmentMaxBytes: 120})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	if l2.NextOffset() != n {
		t.Fatalf("NextOffset after reopen = %d, want %d", l2.NextOffset(), n)
	}
	msgs, err = l2.Read(0, n)
	if err != nil || len(msgs) != n {
		t.Fatalf("Read after reopen: %v, %d msgs", err, len(msgs))
	}
}

// activeSegmentPath returns the path of the highest-numbered segment file.
func activeSegmentPath(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"+segmentSuffix))
	if err != nil || len(files) == 0 {
		t.Fatalf("no segment files in %s", dir)
	}
	return files[len(files)-1] // zero-padded names sort lexicographically
}

func TestRecoverFromTornWrite(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		mustAppend(t, l, "k", fmt.Sprintf("v%d", i))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash in the middle of writing the last record.
	path := activeSegmentPath(t, dir)
	info, _ := os.Stat(path)
	if err := os.Truncate(path, info.Size()-3); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen after torn write: %v", err)
	}
	defer l2.Close()

	if l2.NextOffset() != 2 {
		t.Fatalf("NextOffset = %d, want 2 (torn record dropped)", l2.NextOffset())
	}
	msgs, err := l2.Read(0, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("Read: %v, %d msgs", err, len(msgs))
	}
	// The log must be writable again and reuse the lost offset.
	if off := mustAppend(t, l2, "k", "again"); off != 2 {
		t.Fatalf("append after recovery got offset %d, want 2", off)
	}
}

func TestRecoverFromCorruptTail(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		mustAppend(t, l, "k", fmt.Sprintf("v%d", i))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Flip a bit inside the last record's body.
	path := activeSegmentPath(t, dir)
	data, _ := os.ReadFile(path)
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen after corruption: %v", err)
	}
	defer l2.Close()
	if l2.NextOffset() != 2 {
		t.Fatalf("NextOffset = %d, want 2", l2.NextOffset())
	}
}

func TestSealedSegmentCorruptionIsAnError(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Options{SegmentMaxBytes: 120})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		mustAppend(t, l, "key", fmt.Sprintf("value-%02d", i))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the FIRST (sealed) segment. Silently dropping data from the
	// middle of the log would be wrong, so Open must refuse.
	files, _ := filepath.Glob(filepath.Join(dir, "*"+segmentSuffix))
	data, _ := os.ReadFile(files[0])
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(files[0], data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir, Options{SegmentMaxBytes: 120}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open err = %v, want ErrCorrupt", err)
	}
}

func TestRecordTooLarge(t *testing.T) {
	l, _ := openTestLog(t, Options{})
	big := make([]byte, maxRecordBytes+1)
	if _, err := l.Append(Message{Value: big}); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("err = %v, want ErrRecordTooLarge", err)
	}
	if l.NextOffset() != 0 {
		t.Fatalf("failed append advanced the log to %d", l.NextOffset())
	}
}

func TestUseAfterClose(t *testing.T) {
	l, _ := openTestLog(t, Options{})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(Message{Value: []byte("x")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append err = %v, want ErrClosed", err)
	}
	if _, err := l.Read(0, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read err = %v, want ErrClosed", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}
}

func TestConcurrentAppendAndRead(t *testing.T) {
	l, _ := openTestLog(t, Options{SegmentMaxBytes: 4096})
	const writers, perWriter = 8, 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := l.Append(Message{Key: []byte{byte(w)}, Value: []byte(fmt.Sprintf("w%d-%d", w, i))}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(w)
	}
	// A reader running alongside the writers.
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() {
		defer rwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := l.Read(0, 50); err != nil {
					t.Errorf("Read: %v", err)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(stop)
	rwg.Wait()

	total := uint64(writers * perWriter)
	if l.NextOffset() != total {
		t.Fatalf("NextOffset = %d, want %d", l.NextOffset(), total)
	}
	msgs, err := l.Read(0, int(total))
	if err != nil || len(msgs) != int(total) {
		t.Fatalf("Read all: %v, %d msgs", err, len(msgs))
	}
	for i, m := range msgs {
		if m.Offset != uint64(i) {
			t.Fatalf("msg %d has offset %d: offsets are not dense and ordered", i, m.Offset)
		}
	}
}

func BenchmarkAppend(b *testing.B) {
	for _, size := range []int{100, 1024} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			l, err := Open(b.TempDir(), Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()
			val := bytes.Repeat([]byte("x"), size)
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := l.Append(Message{Key: []byte("k"), Value: val}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
