package bench

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// Fetcher is the part of the client that the read benchmark uses.
type Fetcher interface {
	Fetch(ctx context.Context, topic string, partition int, offset uint64, limit int, opts ...client.FetchOption) ([]client.Record, error)
}

// FetchConfig says what to read back.
type FetchConfig struct {
	Topic      string `json:"topic"`
	Partitions int    `json:"partitions"`
	// Limit is the number of messages asked for in one request; the cluster may return
	// fewer (it has its own cap, -max-fetch-messages).
	Limit          int           `json:"limit"`
	RequestTimeout time.Duration `json:"request_timeout_ns"`
}

// FetchResult is what the read benchmark measured.
type FetchResult struct {
	Config   FetchConfig   `json:"config"`
	Requests uint64        `json:"requests"`
	Messages uint64        `json:"messages"`
	Bytes    uint64        `json:"bytes"` // keys and values
	Elapsed  time.Duration `json:"elapsed_ns"`
	Latency  Summary       `json:"latency"` // of one request

	MessagesPerSec  float64 `json:"messages_per_sec"`
	MegabytesPerSec float64 `json:"megabytes_per_sec"`
}

// RunFetch reads every partition of the topic from the first offset to its end, one
// reader per partition at the same time, and reports how fast. It measures reading what
// is already stored, not following a live topic.
func RunFetch(ctx context.Context, f Fetcher, cfg FetchConfig) (FetchResult, error) {
	switch {
	case cfg.Topic == "" || cfg.Partitions < 1 || cfg.Limit < 1 || cfg.RequestTimeout <= 0:
		return FetchResult{}, errors.New("bench: a topic, partitions, a limit and a timeout are needed")
	}
	type part struct {
		lat      []time.Duration
		requests uint64
		messages uint64
		bytes    uint64
		err      error
	}
	parts := make([]part, cfg.Partitions)
	start := time.Now()
	var wg sync.WaitGroup
	for p := 0; p < cfg.Partitions; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			pt := &parts[p]
			var offset uint64
			for {
				reqCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
				t0 := time.Now()
				recs, err := f.Fetch(reqCtx, cfg.Topic, p, offset, cfg.Limit)
				d := time.Since(t0)
				cancel()
				if err != nil {
					pt.err = fmt.Errorf("partition %d offset %d: %w", p, offset, err)
					return
				}
				if len(recs) == 0 {
					return
				}
				pt.requests++
				pt.lat = append(pt.lat, d)
				for _, r := range recs {
					pt.messages++
					pt.bytes += uint64(len(r.Key) + len(r.Value))
				}
				offset = recs[len(recs)-1].Offset + 1
			}
		}(p)
	}
	wg.Wait()
	elapsed := time.Since(start)

	res := FetchResult{Config: cfg, Elapsed: elapsed}
	var all []time.Duration
	for _, pt := range parts {
		if pt.err != nil {
			return res, pt.err
		}
		res.Requests += pt.requests
		res.Messages += pt.messages
		res.Bytes += pt.bytes
		all = append(all, pt.lat...)
	}
	res.Latency = Summarize(all)
	if elapsed > 0 {
		res.MessagesPerSec = float64(res.Messages) / elapsed.Seconds()
		res.MegabytesPerSec = float64(res.Bytes) / elapsed.Seconds() / 1e6
	}
	return res, nil
}
