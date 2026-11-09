package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// Producer is the part of the client that the write benchmark uses.
type Producer interface {
	Produce(ctx context.Context, topic string, msgs []client.Message, opts ...client.ProduceOption) (client.ProduceResult, error)
}

// ProduceConfig says what to measure.
type ProduceConfig struct {
	Topic      string `json:"topic"`
	Partitions int    `json:"partitions"`
	// Producers is the number of requests in flight at once; each has its own
	// goroutine and sends to one partition (producer i to partition i mod Partitions).
	Producers int `json:"producers"`
	// MessageSize is the length of a value in bytes.
	MessageSize int `json:"message_size_bytes"`
	// Batch is the number of messages in one request.
	Batch int `json:"batch"`
	// Warmup is run before the measurement and not counted.
	Warmup time.Duration `json:"warmup_ns"`
	// Duration is the length of the measured window.
	Duration time.Duration `json:"duration_ns"`
	// RequestTimeout bounds one request.
	RequestTimeout time.Duration `json:"request_timeout_ns"`
}

// Validate checks that every field is usable.
func (c ProduceConfig) Validate() error {
	switch {
	case c.Topic == "":
		return errors.New("bench: a topic is needed")
	case c.Partitions < 1 || c.Producers < 1 || c.MessageSize < 1 || c.Batch < 1:
		return errors.New("bench: partitions, producers, message size and batch must be at least 1")
	case c.Duration <= 0 || c.Warmup < 0 || c.RequestTimeout <= 0:
		return errors.New("bench: duration and request timeout must be positive, warm-up not negative")
	}
	return nil
}

// ProduceResult is what the write benchmark measured.
type ProduceResult struct {
	Config ProduceConfig `json:"config"`
	// Requests, Messages and Bytes count what the cluster acknowledged inside the
	// measured window. Bytes counts values only.
	Requests uint64        `json:"requests"`
	Messages uint64        `json:"messages"`
	Bytes    uint64        `json:"bytes"`
	Elapsed  time.Duration `json:"elapsed_ns"`
	// Errors counts requests that failed inside the window; they are not in the figures.
	Errors     uint64 `json:"errors"`
	FirstError string `json:"first_error,omitempty"`
	// Latency is the time of one request, from sending it to its acknowledgement.
	Latency Summary `json:"latency"`

	MessagesPerSec  float64 `json:"messages_per_sec"`
	MegabytesPerSec float64 `json:"megabytes_per_sec"`
}

// RunProduce sends batches from several producers for the configured time and reports
// what was acknowledged in the window after the warm-up. A request counts only if it
// started and finished inside the window, so a request cut off by the end of the run
// does not bias the latency downwards.
//
// The messages are built once per producer and reused for every request: the benchmark
// should measure the cluster, not the allocation of test data. Their content is
// pseudo-random bytes from a fixed seed.
func RunProduce(ctx context.Context, p Producer, cfg ProduceConfig) (ProduceResult, error) {
	return runProduce(ctx, p, cfg, time.Now)
}

func runProduce(ctx context.Context, p Producer, cfg ProduceConfig, now func() time.Time) (ProduceResult, error) {
	if err := cfg.Validate(); err != nil {
		return ProduceResult{}, err
	}
	begin := now()
	measureStart := begin.Add(cfg.Warmup)
	end := measureStart.Add(cfg.Duration)
	runCtx, cancel := context.WithDeadline(ctx, end)
	defer cancel()

	type part struct {
		lat      []time.Duration
		requests uint64
		errs     uint64
		firstErr string
	}
	parts := make([]part, cfg.Producers)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Producers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(i) + 1)) //nolint:gosec // load-generator payload, not a secret
			msgs := make([]client.Message, cfg.Batch)
			for k := range msgs {
				v := make([]byte, cfg.MessageSize)
				_, _ = rng.Read(v)
				msgs[k] = client.Message{Value: v}
			}
			part0 := i % cfg.Partitions
			pt := &parts[i]
			for runCtx.Err() == nil {
				t0 := now()
				reqCtx, cancelReq := context.WithTimeout(runCtx, cfg.RequestTimeout)
				_, err := p.Produce(reqCtx, cfg.Topic, msgs, client.ToPartition(part0))
				cancelReq()
				t1 := now()
				if t0.Before(measureStart) || t1.After(end) || (runCtx.Err() != nil && err != nil) {
					continue // warm-up, or cut off by the end of the run
				}
				if err != nil {
					pt.errs++
					if pt.firstErr == "" {
						pt.firstErr = err.Error()
					}
					select {
					case <-runCtx.Done():
					case <-time.After(10 * time.Millisecond): // do not spin on a failing cluster
					}
					continue
				}
				pt.requests++
				pt.lat = append(pt.lat, t1.Sub(t0))
			}
		}(i)
	}
	wg.Wait()

	res := ProduceResult{Config: cfg, Elapsed: cfg.Duration}
	var all []time.Duration
	for _, pt := range parts {
		res.Requests += pt.requests
		res.Errors += pt.errs
		if res.FirstError == "" {
			res.FirstError = pt.firstErr
		}
		all = append(all, pt.lat...)
	}
	res.Messages = res.Requests * uint64(cfg.Batch)
	res.Bytes = res.Messages * uint64(cfg.MessageSize)
	res.Latency = Summarize(all)
	res.MessagesPerSec = float64(res.Messages) / cfg.Duration.Seconds()
	res.MegabytesPerSec = float64(res.Bytes) / cfg.Duration.Seconds() / 1e6
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("bench: interrupted: %w", err)
	}
	return res, nil
}
