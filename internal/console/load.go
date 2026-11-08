package console

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// LogClient is the part of the raftlog client the load generator uses.
type LogClient interface {
	CreateTopic(ctx context.Context, topic string, partitions int) error
	Produce(ctx context.Context, topic string, msgs []client.Message, opts ...client.ProduceOption) (client.ProduceResult, error)
	Fetch(ctx context.Context, topic string, partition int, offset uint64, limit int, opts ...client.FetchOption) ([]client.Record, error)
}

// LoadConfig configures the load generator.
type LoadConfig struct {
	Topic      string
	Partitions int
	// Workers is how many writes can be in flight at once.
	Workers int
	// RequestTimeout bounds one write, which includes waiting out an election.
	RequestTimeout time.Duration
	// MaxRate is the highest rate that may be asked for, in messages per second.
	MaxRate int
	// MaxTracked is how many acknowledged values are remembered for verification.
	MaxTracked int
	// Window is the period the throughput and latency figures are taken over.
	Window time.Duration
}

// Validate checks that every field is set.
func (c LoadConfig) Validate() error {
	switch {
	case c.Topic == "" || c.Partitions < 1 || c.Workers < 1 || c.RequestTimeout <= 0 || c.MaxRate < 1 || c.MaxTracked < 1 || c.Window <= 0:
		return errors.New("console: the load configuration is incomplete")
	}
	return nil
}

// LoadStats is what the generator has done, measured by the generator.
type LoadStats struct {
	Running bool `json:"running"`
	Rate    int  `json:"rate"`
	// Sent counts writes attempted, Acked those the cluster confirmed, Failed those
	// that returned an error. A failed write may still have been stored.
	Sent   uint64 `json:"sent"`
	Acked  uint64 `json:"acked"`
	Failed uint64 `json:"failed"`
	// Dropped counts writes that were due but not started because every worker was
	// busy: the cluster, or the election in progress, could not keep up.
	Dropped uint64 `json:"dropped"`
	// Throughput is acknowledged writes per second over the window, and the
	// latencies are those of the acknowledged writes in it, in milliseconds.
	Throughput float64 `json:"throughput"`
	P50Ms      float64 `json:"p50_ms"`
	P99Ms      float64 `json:"p99_ms"`
	LastError  string  `json:"last_error,omitempty"`
	// Tracked is how many acknowledged values are remembered for verification.
	Tracked int `json:"tracked"`
}

// Verification is the outcome of checking the acknowledged writes against the log.
type Verification struct {
	// Acked is how many acknowledged values were checked.
	Acked int `json:"acked"`
	// Missing is how many of them are not in the log: acknowledged and lost. It must be 0.
	Missing         int      `json:"missing"`
	MissingExamples []string `json:"missing_examples,omitempty"`
	// Duplicates counts values stored more than once, which a retried write after
	// an ambiguous failure can cause: at-least-once delivery, not a loss.
	Duplicates int `json:"duplicates"`
	// Stored is the number of messages in the topic.
	Stored int `json:"stored"`
	// Untracked counts acknowledged writes that were not remembered (the limit was
	// reached), so they were not checked.
	Untracked uint64 `json:"untracked"`
}

type sample struct {
	at  time.Time
	dur time.Duration
}

// Load writes numbered messages to a topic at a chosen rate and remembers which
// ones the cluster acknowledged.
type Load struct {
	c   LogClient
	cfg LoadConfig
	now func() time.Time

	mu        sync.Mutex
	running   bool
	rate      int
	cancel    context.CancelFunc
	topicDone bool
	runID     string
	seq       uint64
	samples   []sample
	lastErr   string
	acked     map[string]struct{}
	untracked uint64

	sent, ackedN, failed, dropped uint64
}

// NewLoad returns a generator; it does nothing until Start.
func NewLoad(c LogClient, cfg LoadConfig) (*Load, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Load{c: c, cfg: cfg, now: time.Now, acked: map[string]struct{}{}}, nil
}

// Start begins writing rate messages per second. It creates the topic if needed.
func (l *Load) Start(rate int) error {
	if rate < 1 || rate > l.cfg.MaxRate {
		return fmt.Errorf("console: the rate must be between 1 and %d messages per second", l.cfg.MaxRate)
	}
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return errors.New("console: the load is already running")
	}
	l.running, l.rate = true, rate
	l.runID = strconv.FormatInt(l.now().UnixMilli(), 36)
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	needTopic := !l.topicDone
	l.mu.Unlock()

	go l.run(ctx, rate, needTopic)
	return nil
}

// Stop stops starting new writes. Writes already in flight finish and are counted.
func (l *Load) Stop() {
	l.mu.Lock()
	if l.running {
		l.cancel()
		l.running = false
	}
	l.mu.Unlock()
}

func (l *Load) ensureTopic(ctx context.Context) bool {
	for ctx.Err() == nil {
		cctx, cancel := context.WithTimeout(ctx, l.cfg.RequestTimeout)
		err := l.c.CreateTopic(cctx, l.cfg.Topic, l.cfg.Partitions)
		cancel()
		if err == nil || errors.Is(err, client.ErrAlreadyExists) {
			l.mu.Lock()
			l.topicDone = true
			l.mu.Unlock()
			return true
		}
		l.noteError(err)
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return false
}

func (l *Load) noteError(err error) {
	l.mu.Lock()
	l.lastErr = err.Error()
	l.mu.Unlock()
}

func (l *Load) run(ctx context.Context, rate int, needTopic bool) {
	if needTopic && !l.ensureTopic(ctx) {
		return
	}
	work := make(chan struct{}, l.cfg.Workers*4)
	var wg sync.WaitGroup
	for i := 0; i < l.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				if ctx.Err() != nil {
					continue // stopped: what was queued is not written
				}
				l.writeOne()
			}
		}()
	}
	defer func() {
		close(work)
		wg.Wait()
	}()

	const tick = 20 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	var due float64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			due += float64(rate) * tick.Seconds()
			for ; due >= 1; due-- {
				select {
				case work <- struct{}{}:
				default:
					l.mu.Lock()
					l.dropped++
					l.mu.Unlock()
				}
			}
		}
	}
}

func (l *Load) writeOne() {
	l.mu.Lock()
	l.seq++
	n := l.seq
	value := fmt.Sprintf("%s:%d", l.runID, n)
	l.sent++
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), l.cfg.RequestTimeout)
	start := l.now()
	_, err := l.c.Produce(ctx, l.cfg.Topic, []client.Message{{Key: []byte(value), Value: []byte(value)}}, client.ToPartition(int(n)%l.cfg.Partitions))
	cancel()
	dur := l.now().Sub(start)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.failed++
		l.lastErr = err.Error()
		return
	}
	l.ackedN++
	l.samples = append(l.samples, sample{at: start.Add(dur), dur: dur})
	if len(l.acked) < l.cfg.MaxTracked {
		l.acked[value] = struct{}{}
	} else {
		l.untracked++
	}
}

// Stats returns the counters and the figures over the window.
func (l *Load) Stats() LoadStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.cfg.Window)
	i := sort.Search(len(l.samples), func(i int) bool { return l.samples[i].at.After(cut) })
	l.samples = l.samples[i:] // older samples are no longer needed
	s := LoadStats{
		Running: l.running, Rate: l.rate, Sent: l.sent, Acked: l.ackedN, Failed: l.failed, Dropped: l.dropped,
		LastError: l.lastErr, Tracked: len(l.acked),
	}
	if len(l.samples) > 0 {
		s.Throughput = float64(len(l.samples)) / l.cfg.Window.Seconds()
		durs := make([]time.Duration, len(l.samples))
		for k, sm := range l.samples {
			durs[k] = sm.dur
		}
		sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
		s.P50Ms = ms(percentile(durs, 0.50))
		s.P99Ms = ms(percentile(durs, 0.99))
	}
	return s
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q*float64(len(sorted))+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Verify reads the whole topic back and checks that every acknowledged value is in
// it. A node that is a little behind may not show the newest messages yet, so a
// value that is missing is looked for again until patience runs out.
func (l *Load) Verify(ctx context.Context, patience time.Duration) (Verification, error) {
	l.mu.Lock()
	want := make(map[string]struct{}, len(l.acked))
	for v := range l.acked {
		want[v] = struct{}{}
	}
	untracked := l.untracked
	l.mu.Unlock()

	deadline := time.Now().Add(patience)
	for {
		v, err := l.checkOnce(ctx, want)
		if err != nil {
			if ctx.Err() != nil || time.Now().After(deadline) {
				return Verification{}, err
			}
		} else {
			v.Untracked = untracked
			if v.Missing == 0 || time.Now().After(deadline) {
				return v, nil
			}
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return Verification{}, ctx.Err()
		}
	}
}

func (l *Load) checkOnce(ctx context.Context, want map[string]struct{}) (Verification, error) {
	counts := make(map[string]int, len(want))
	stored := 0
	for p := 0; p < l.cfg.Partitions; p++ {
		for offset := uint64(0); ; {
			recs, err := l.c.Fetch(ctx, l.cfg.Topic, p, offset, 500)
			if err != nil {
				return Verification{}, fmt.Errorf("read partition %d from %d: %w", p, offset, err)
			}
			if len(recs) == 0 {
				break
			}
			for _, r := range recs {
				counts[string(r.Value)]++
				stored++
			}
			offset = recs[len(recs)-1].Offset + 1
		}
	}
	v := Verification{Acked: len(want), Stored: stored}
	var missing []string
	for val := range want {
		if counts[val] == 0 {
			missing = append(missing, val)
		}
	}
	sort.Strings(missing)
	v.Missing = len(missing)
	if len(missing) > 5 {
		missing = missing[:5]
	}
	v.MissingExamples = missing
	for val, n := range counts {
		if n > 1 && strings.Contains(val, ":") {
			v.Duplicates++
		}
	}
	return v, nil
}

// Reset forgets what was counted and acknowledged, for a fresh start. It does not
// touch the topic.
func (l *Load) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return // the counters of a run that is going on are not to be wiped
	}
	l.sent, l.ackedN, l.failed, l.dropped, l.untracked = 0, 0, 0, 0, 0
	l.samples, l.lastErr = nil, ""
	l.acked = map[string]struct{}{}
}
