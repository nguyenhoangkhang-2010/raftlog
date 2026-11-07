// Command wrsim simulates people arriving at the waiting room, to try it without
// a crowd. Every simulated person joins through the HTTP API, then watches their
// place through the same event stream the web page uses, until they are let in.
//
// Usage:
//
//	wrsim [-url http://localhost:8080] [-users 100] [-arrival-rate 20] [-seed N]
//
// Arrivals follow a Poisson process: the gaps between two arrivals are random
// with a mean of 1/arrival-rate seconds. The seed is printed, so a run can be
// repeated. What the report states is measured, from the point of view of the
// simulated people: how long joining took, how long they waited, and whether
// anything looked wrong.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	envURL = "WAITINGROOM_URL"

	defaultURL         = "http://localhost:8080"
	defaultUsers       = 100
	defaultArrivalRate = 20.0
	defaultTimeout     = 10 * time.Minute
	defaultGrace       = time.Second
)

type config struct {
	url         string
	users       int
	arrivalRate float64
	seed        int64
	timeout     time.Duration
	grace       time.Duration
	prefix      string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run returns 0 when every person was admitted and nothing looked wrong, 1 when
// something did, 2 for a usage error.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg := config{url: defaultURL, users: defaultUsers, arrivalRate: defaultArrivalRate, timeout: defaultTimeout, grace: defaultGrace}
	if v := getenv(envURL); v != "" {
		cfg.url = v
	}
	fs := flag.NewFlagSet("wrsim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.url, "url", cfg.url, "base URL of the waiting room ["+envURL+"]")
	fs.IntVar(&cfg.users, "users", cfg.users, "number of people to simulate")
	fs.Float64Var(&cfg.arrivalRate, "arrival-rate", cfg.arrivalRate, "mean arrivals per second")
	fs.Int64Var(&cfg.seed, "seed", 0, "seed of the random arrivals; 0 picks one from the clock (it is printed)")
	fs.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "give up waiting for admission after this long")
	fs.DurationVar(&cfg.grace, "grace", cfg.grace, "how much later than a person behind them someone may be seen admitted before it counts as an overtake (event streams are not synchronized)")
	fs.StringVar(&cfg.prefix, "prefix", "", "prefix of the ticket IDs; empty picks one from the clock, so a second run does not meet the tickets of the first")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || cfg.users < 1 || cfg.arrivalRate <= 0 || cfg.timeout <= 0 || cfg.grace < 0 {
		fmt.Fprintln(stderr, "wrsim: -users and -arrival-rate must be positive, and -timeout too; -grace must not be negative; no arguments are expected")
		return 2
	}
	if cfg.seed == 0 {
		cfg.seed = time.Now().UnixNano()
	}
	if cfg.prefix == "" {
		cfg.prefix = fmt.Sprintf("sim%x", time.Now().UnixNano()&0xffffff)
	}
	cfg.url = strings.TrimRight(cfg.url, "/")

	fmt.Fprintf(stdout, "simulating %d people against %s at %.1f arrivals/s, seed %d, tickets %s-N\n", cfg.users, cfg.url, cfg.arrivalRate, cfg.seed, cfg.prefix)
	rep := simulate(ctx, cfg)
	rep.print(stdout, cfg)
	if rep.problems() > 0 || rep.overtakes(cfg.grace) > 0 {
		return 1
	}
	return 0
}

// person is what one simulated person saw.
type person struct {
	ticket    string
	joinTook  time.Duration // how long the join request took
	joinedAt  time.Time
	offset    uint64
	first     int // the first position seen
	last      int // the last position seen
	updates   int
	increased int // times the position got worse
	admitted  bool
	admitAt   time.Time
	err       error
}

type report struct {
	people   []*person
	started  time.Time
	finished time.Time
}

// arrivals draws the gaps between arrivals: exponential, mean 1/rate seconds.
type arrivals struct {
	rng  *rand.Rand
	rate float64
}

func newArrivals(seed int64, rate float64) *arrivals {
	return &arrivals{rng: rand.New(rand.NewSource(seed)), rate: rate}
}

func (a *arrivals) next() time.Duration {
	return time.Duration(-math.Log(1-a.rng.Float64()) / a.rate * float64(time.Second))
}

func simulate(ctx context.Context, cfg config) *report {
	gaps := newArrivals(cfg.seed, cfg.arrivalRate)
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	rep := &report{started: time.Now()}
	var wg sync.WaitGroup
	for i := 0; i < cfg.users; i++ {
		p := &person{ticket: fmt.Sprintf("%s-%d", cfg.prefix, i)}
		rep.people = append(rep.people, p)
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.live(ctx, cfg.url)
		}()
		select {
		case <-time.After(gaps.next()):
		case <-ctx.Done():
		}
	}
	wg.Wait()
	rep.finished = time.Now()
	return rep
}

type status struct {
	State    string `json:"state"`
	Offset   uint64 `json:"offset"`
	Position int    `json:"position"`
}

// live is one person's visit: join, then watch until admitted.
func (p *person) live(ctx context.Context, base string) {
	client := &http.Client{} // no overall timeout: the watch lasts as long as the wait
	body, _ := json.Marshal(map[string]string{"ticket": p.ticket, "user": p.ticket})

	var st status
	for attempt := 0; ; attempt++ {
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/join", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			err = decodeJSON(resp, &st)
		}
		if err == nil {
			p.joinTook = time.Since(start)
			break
		}
		// The room may be failing over; a person would press the button again.
		if ctx.Err() != nil || attempt >= 20 {
			p.err = fmt.Errorf("join: %w", err)
			return
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			p.err = fmt.Errorf("join: %w", ctx.Err())
			return
		}
	}
	p.joinedAt = time.Now()
	p.offset = st.Offset
	p.observe(st)

	for !p.admitted {
		if err := p.watch(ctx, base); err != nil && ctx.Err() != nil {
			p.err = fmt.Errorf("watch: %w", ctx.Err())
			return
		}
		// The stream ended (a restart of the room, a dropped connection): open it
		// again, as the browser does.
		if !p.admitted {
			select {
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}
}

func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// observe records one status the person saw.
func (p *person) observe(st status) {
	p.updates++
	switch st.State {
	case "admitted":
		if !p.admitted {
			p.admitted = true
			p.admitAt = time.Now()
		}
	case "waiting":
		if p.first == 0 {
			p.first = st.Position
		}
		if p.last != 0 && st.Position > p.last {
			p.increased++
		}
		p.last = st.Position
	}
}

// watch reads the event stream until the person is admitted or it breaks.
func (p *person) watch(ctx context.Context, base string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/watch?ticket="+p.ticket, nil)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var st status
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &st); err != nil {
			continue
		}
		if st.State == "unknown" {
			continue // the room has not read our join yet
		}
		p.observe(st)
		if p.admitted {
			return nil
		}
	}
	return sc.Err()
}

// overtakes counts the people who were seen admitted more than grace later than
// somebody who joined behind them. People are compared in pairs of neighbours in
// join order, which is enough to see an order that is wrong.
func (r *report) overtakes(grace time.Duration) int {
	var admitted []*person
	for _, p := range r.people {
		if p.admitted {
			admitted = append(admitted, p)
		}
	}
	sort.Slice(admitted, func(i, j int) bool { return admitted[i].offset < admitted[j].offset })
	n := 0
	var latest time.Time // latest admission seen among the people who joined earlier
	for _, p := range admitted {
		if !latest.IsZero() && latest.Sub(p.admitAt) > grace {
			n++ // somebody who joined before p was seen admitted well after p
		}
		if p.admitAt.After(latest) {
			latest = p.admitAt
		}
	}
	return n
}

func (r *report) count(f func(*person) bool) int {
	n := 0
	for _, p := range r.people {
		if f(p) {
			n++
		}
	}
	return n
}

func (r *report) problems() int {
	return r.count(func(p *person) bool { return !p.admitted || p.increased > 0 || p.err != nil })
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

func (r *report) print(w io.Writer, cfg config) {
	var joins, waits []time.Duration
	for _, p := range r.people {
		if p.joinTook > 0 {
			joins = append(joins, p.joinTook)
		}
		if p.admitted {
			waits = append(waits, p.admitAt.Sub(p.joinedAt))
		}
	}
	sort.Slice(joins, func(i, j int) bool { return joins[i] < joins[j] })
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })

	admitted := r.count(func(p *person) bool { return p.admitted })
	failed := r.count(func(p *person) bool { return p.err != nil })
	worse := r.count(func(p *person) bool { return p.increased > 0 })
	over := r.overtakes(cfg.grace)

	fmt.Fprintf(w, "\nran for %v\n", r.finished.Sub(r.started).Round(time.Millisecond))
	fmt.Fprintf(w, "people            %d\n", len(r.people))
	fmt.Fprintf(w, "admitted          %d\n", admitted)
	fmt.Fprintf(w, "failed            %d (could not join or lost the stream for good)\n", failed)
	fmt.Fprintf(w, "join request      p50 %v  p95 %v  max %v\n", percentile(joins, .5).Round(time.Microsecond), percentile(joins, .95).Round(time.Microsecond), percentile(joins, 1).Round(time.Microsecond))
	fmt.Fprintf(w, "wait to admission p50 %v  p95 %v  max %v\n", percentile(waits, .5).Round(time.Millisecond), percentile(waits, .95).Round(time.Millisecond), percentile(waits, 1).Round(time.Millisecond))
	fmt.Fprintf(w, "position got worse for %d people (must be 0)\n", worse)
	fmt.Fprintf(w, "admitted out of order by more than %v: %d people (must be 0)\n", cfg.grace, over)
	if r.problems() == 0 && over == 0 {
		fmt.Fprintln(w, "OK")
	} else {
		fmt.Fprintln(w, "PROBLEMS")
	}
}
