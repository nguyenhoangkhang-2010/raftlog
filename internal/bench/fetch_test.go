package bench

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// fakeLog holds the same number of records in every partition.
type fakeLog struct {
	perPartition uint64
	maxPerFetch  int // most records one fetch returns, like the server's own cap
	failAt       uint64
	delay        time.Duration // the time one fetch takes
}

func (f *fakeLog) Fetch(_ context.Context, _ string, _ int, offset uint64, limit int, _ ...client.FetchOption) ([]client.Record, error) {
	time.Sleep(f.delay)
	if f.failAt != 0 && offset >= f.failAt {
		return nil, errors.New("disk on fire")
	}
	if f.maxPerFetch > 0 && limit > f.maxPerFetch {
		limit = f.maxPerFetch
	}
	var out []client.Record
	for o := offset; o < f.perPartition && len(out) < limit; o++ {
		out = append(out, client.Record{Offset: o, Key: []byte("kk"), Value: make([]byte, 98)})
	}
	return out, nil
}

func TestRunFetchReadsEveryPartitionToTheEnd(t *testing.T) {
	f := &fakeLog{perPartition: 1050, maxPerFetch: 100, delay: time.Millisecond}
	res, err := RunFetch(context.Background(), f, FetchConfig{Topic: "t", Partitions: 3, Limit: 500, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 3*1050 || res.Bytes != 3*1050*100 {
		t.Fatalf("read %d messages and %d bytes, want %d and %d", res.Messages, res.Bytes, 3*1050, 3*1050*100)
	}
	// The cap of 100 per fetch means 11 requests with data per partition.
	if res.Requests != 3*11 {
		t.Fatalf("%d requests, want %d", res.Requests, 3*11)
	}
	if res.MessagesPerSec <= 0 || res.Latency.Count != int(res.Requests) {
		t.Fatalf("rate %v, latency samples %d", res.MessagesPerSec, res.Latency.Count)
	}
}

func TestRunFetchReportsAFailureInsteadOfAShortResult(t *testing.T) {
	_, err := RunFetch(context.Background(), &fakeLog{perPartition: 1000, failAt: 300}, FetchConfig{Topic: "t", Partitions: 1, Limit: 100, RequestTimeout: time.Second})
	if err == nil {
		t.Fatal("a failed fetch was reported as the end of the topic")
	}
}

func TestRunFetchNeedsItsSettings(t *testing.T) {
	if _, err := RunFetch(context.Background(), &fakeLog{}, FetchConfig{}); err == nil {
		t.Fatal("empty configuration accepted")
	}
}
