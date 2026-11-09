package bench

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

type fakeProducer struct {
	delay time.Duration
	err   error
	calls atomic.Int64
	size  atomic.Int64 // bytes of the last request's values
	batch atomic.Int64
}

func (f *fakeProducer) Produce(ctx context.Context, _ string, msgs []client.Message, _ ...client.ProduceOption) (client.ProduceResult, error) {
	f.calls.Add(1)
	f.batch.Store(int64(len(msgs)))
	f.size.Store(int64(len(msgs[0].Value)))
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return client.ProduceResult{}, ctx.Err()
	}
	return client.ProduceResult{}, f.err
}

func baseProduce() ProduceConfig {
	return ProduceConfig{Topic: "t", Partitions: 2, Producers: 3, MessageSize: 100, Batch: 4,
		Warmup: 50 * time.Millisecond, Duration: 300 * time.Millisecond, RequestTimeout: time.Second}
}

func TestRunProduceCountsOnlyTheMeasuredWindow(t *testing.T) {
	f := &fakeProducer{delay: 10 * time.Millisecond}
	cfg := baseProduce()
	res, err := RunProduce(context.Background(), f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 0 || res.Requests == 0 {
		t.Fatalf("requests %d errors %d (%s)", res.Requests, res.Errors, res.FirstError)
	}
	if res.Messages != res.Requests*4 || res.Bytes != res.Messages*100 {
		t.Fatalf("messages %d bytes %d for %d requests of 4 messages of 100 bytes", res.Messages, res.Bytes, res.Requests)
	}
	if int64(res.Requests) >= f.calls.Load() {
		t.Fatalf("%d requests counted but %d were made: the warm-up must not be counted", res.Requests, f.calls.Load())
	}
	// 3 producers, 10 ms per request, 300 ms: at most 90 requests can fit.
	if res.Requests > 90 {
		t.Fatalf("%d requests in 300 ms from 3 producers at 10 ms each: more than is possible", res.Requests)
	}
	if res.Latency.P50 < 10*time.Millisecond || res.Latency.Min < 10*time.Millisecond {
		t.Fatalf("latency %+v is below the fake's 10 ms", res.Latency)
	}
	wantRate := float64(res.Messages) / 0.3
	if res.MessagesPerSec != wantRate {
		t.Fatalf("messages per second %v, want %v", res.MessagesPerSec, wantRate)
	}
	if f.size.Load() != 100 || f.batch.Load() != 4 {
		t.Fatalf("requests carried %d messages of %d bytes, want 4 of 100", f.batch.Load(), f.size.Load())
	}
}

func TestRunProduceReportsFailuresInsteadOfCountingThem(t *testing.T) {
	f := &fakeProducer{delay: time.Millisecond, err: errors.New("no leader")}
	res, err := RunProduce(context.Background(), f, baseProduce())
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 0 || res.Messages != 0 || res.Errors == 0 || res.FirstError != "no leader" {
		t.Fatalf("result %+v: failed requests must not count as throughput", res)
	}
}

func TestRunProduceStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	go func() { time.Sleep(60 * time.Millisecond); once.Do(cancel) }()
	cfg := baseProduce()
	cfg.Duration = 10 * time.Second
	start := time.Now()
	_, err := RunProduce(ctx, &fakeProducer{delay: time.Millisecond}, cfg)
	if err == nil {
		t.Fatal("an interrupted run reported success")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v to stop", time.Since(start))
	}
}

func TestProduceConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*ProduceConfig){
		"no topic":    func(c *ProduceConfig) { c.Topic = "" },
		"no producer": func(c *ProduceConfig) { c.Producers = 0 },
		"no size":     func(c *ProduceConfig) { c.MessageSize = 0 },
		"no batch":    func(c *ProduceConfig) { c.Batch = 0 },
		"no duration": func(c *ProduceConfig) { c.Duration = 0 },
		"no timeout":  func(c *ProduceConfig) { c.RequestTimeout = 0 },
	} {
		cfg := baseProduce()
		mutate(&cfg)
		if _, err := RunProduce(context.Background(), &fakeProducer{}, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
