// Command raftlogbench measures a raftlog cluster.
//
// Usage:
//
//	raftlogbench produce  [flags]   writes, then reads back: messages per second and request latency
//	raftlogbench failover [flags]   kills the leader several times while a client writes
//
// By default it starts the nodes itself (-spawn 3, from the raftlogd next to this
// program) in a temporary directory and removes them afterwards. With -addr it measures a
// cluster that is already running (for instance the Docker Compose one); the failover
// measurement needs -spawn, because it has to kill processes.
//
// It prints what it measured together with the settings that produced it. Throughput is
// counted over a window that starts after a warm-up, from requests that began and ended
// inside it. Every setting can also come from an environment variable; a flag wins.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/bench"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/console"
)

// Environment variables.
const (
	envAddr    = "BENCH_ADDR"
	envBinary  = "BENCH_RAFTLOGD"
	envDataDir = "BENCH_DATA_DIR"
)

// Built-in defaults, used only when neither a flag nor a variable is given.
const (
	defaultSpawn       = 3
	defaultBasePort    = 19291
	defaultProducers   = 16
	defaultPartitions  = 3
	defaultMessageSize = 128
	defaultBatch       = 1
	defaultWarmup      = 3 * time.Second
	defaultDuration    = 10 * time.Second
	defaultRequestTime = 15 * time.Second
	defaultFetchLimit  = 500
	defaultRounds      = 5
	defaultSteady      = 2 * time.Second
	defaultSettle      = 30 * time.Second
	defaultPoll        = 10 * time.Millisecond
	readyTimeout       = 60 * time.Second
	quietFor           = 400 * time.Millisecond
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "raftlogbench:", err)
		os.Exit(1)
	}
}

// common holds what both measurements need to find or start a cluster.
type common struct {
	addrs    []string // a running cluster, in node ID order
	spawn    int      // nodes to start; 0 means use addrs
	binary   string
	dataDir  string // empty: a temporary directory, removed afterwards
	basePort int
	nodeArgs []string
	asJSON   bool
}

type produceConfig struct {
	common
	produce  bench.ProduceConfig
	fetch    bool
	fetchLim int
}

type failoverConfig struct {
	common
	failover bench.FailoverConfig
}

func (c *common) register(fs *flag.FlagSet, getenv func(string) string, nodeArgs *string, syncOnAppend *bool, addr *string) {
	c.binary = getenv(envBinary)
	c.dataDir = getenv(envDataDir)
	*addr = getenv(envAddr)
	fs.StringVar(addr, "addr", *addr, "measure a running cluster: node addresses in node ID order, comma separated ["+envAddr+"]")
	fs.IntVar(&c.spawn, "spawn", 0, "start this many nodes as child processes (default "+strconv.Itoa(defaultSpawn)+" when -addr is not given)")
	fs.StringVar(&c.binary, "raftlogd", c.binary, "the raftlogd executable to start; default: next to this program ["+envBinary+"]")
	fs.StringVar(&c.dataDir, "data-dir", c.dataDir, "directory for the started nodes' data and logs, which must be empty or absent; default: a temporary one that is removed ["+envDataDir+"]")
	fs.IntVar(&c.basePort, "base-port", defaultBasePort, "node N listens on this port plus N-1")
	fs.StringVar(nodeArgs, "node-args", "", "extra arguments for every started node, space separated, e.g. \"-tick-interval 50ms\"")
	fs.BoolVar(syncOnAppend, "sync-on-append", false, "start the nodes with fsync after every append (the durable setting)")
	fs.BoolVar(&c.asJSON, "json", false, "print the result as JSON")
}

func (c *common) finish(addr string, nodeArgs string, syncOnAppend bool) error {
	for _, a := range strings.Split(addr, ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.addrs = append(c.addrs, a)
		}
	}
	switch {
	case c.spawn < 0:
		return errors.New("-spawn must not be negative")
	case c.spawn > 0 && len(c.addrs) > 0:
		return errors.New("use either -spawn (start nodes) or -addr (measure running nodes), not both")
	case c.spawn == 0 && len(c.addrs) == 0:
		c.spawn = defaultSpawn
	}
	if c.spawn > 0 && (c.basePort < 1 || c.basePort+c.spawn > 65535) {
		return errors.New("-base-port is out of range for that many nodes")
	}
	c.nodeArgs = strings.Fields(nodeArgs)
	if syncOnAppend {
		c.nodeArgs = append(c.nodeArgs, "-sync-on-append")
	}
	if c.spawn > 0 {
		if c.binary == "" {
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("cannot find this program to look for raftlogd next to it: %w", err)
			}
			name := "raftlogd"
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			c.binary = filepath.Join(filepath.Dir(exe), name)
		}
		for i := 0; i < c.spawn; i++ {
			c.addrs = append(c.addrs, fmt.Sprintf("127.0.0.1:%d", c.basePort+i))
		}
	}
	return nil
}

func parseProduce(args []string, getenv func(string) string, out io.Writer) (produceConfig, error) {
	var cfg produceConfig
	var addr, nodeArgs string
	var syncOnAppend bool
	fs := flag.NewFlagSet("raftlogbench produce", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.register(fs, getenv, &nodeArgs, &syncOnAppend, &addr)
	p := &cfg.produce
	fs.StringVar(&p.Topic, "topic", "", "topic to write; default: a new one named bench-<time>")
	fs.IntVar(&p.Partitions, "partitions", defaultPartitions, "partitions of the topic")
	fs.IntVar(&p.Producers, "producers", defaultProducers, "requests in flight at once, one goroutine each")
	fs.IntVar(&p.MessageSize, "message-size", defaultMessageSize, "bytes in the value of a message")
	fs.IntVar(&p.Batch, "batch", defaultBatch, "messages in one request")
	fs.DurationVar(&p.Warmup, "warmup", defaultWarmup, "time before the measurement, not counted")
	fs.DurationVar(&p.Duration, "duration", defaultDuration, "length of the measured window")
	fs.DurationVar(&p.RequestTimeout, "request-timeout", defaultRequestTime, "time allowed to one request")
	fs.BoolVar(&cfg.fetch, "fetch", true, "after writing, read the whole topic back and measure that too")
	fs.IntVar(&cfg.fetchLim, "fetch-limit", defaultFetchLimit, "messages asked for in one read request (the node caps it at -max-fetch-messages)")
	if err := fs.Parse(args); err != nil {
		return produceConfig{}, err
	}
	if fs.NArg() > 0 {
		return produceConfig{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := cfg.finish(addr, nodeArgs, syncOnAppend); err != nil {
		return produceConfig{}, err
	}
	if p.Topic == "" {
		p.Topic = "bench-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if err := p.Validate(); err != nil {
		return produceConfig{}, err
	}
	if cfg.fetchLim < 1 {
		return produceConfig{}, errors.New("-fetch-limit must be at least 1")
	}
	return cfg, nil
}

func parseFailover(args []string, getenv func(string) string, out io.Writer) (failoverConfig, error) {
	var cfg failoverConfig
	var addr, nodeArgs string
	var syncOnAppend bool
	fs := flag.NewFlagSet("raftlogbench failover", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.register(fs, getenv, &nodeArgs, &syncOnAppend, &addr)
	f := &cfg.failover
	fs.StringVar(&f.Topic, "topic", "", "topic to write; default: a new one named bench-<time>")
	fs.IntVar(&f.Rounds, "rounds", defaultRounds, "how many times the leader is killed")
	fs.DurationVar(&f.Steady, "steady", defaultSteady, "how long the writer runs before a kill, and after the first write that follows it")
	fs.DurationVar(&f.RequestTimeout, "request-timeout", defaultRequestTime, "time allowed to one write; longer than a failover")
	fs.DurationVar(&f.SettleTimeout, "settle-timeout", defaultSettle, "how long to wait for a leader, and for a restarted node to rejoin")
	fs.DurationVar(&f.PollEvery, "poll", defaultPoll, "how often the nodes are asked who the leader is")
	if err := fs.Parse(args); err != nil {
		return failoverConfig{}, err
	}
	if fs.NArg() > 0 {
		return failoverConfig{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := cfg.finish(addr, nodeArgs, syncOnAppend); err != nil {
		return failoverConfig{}, err
	}
	if cfg.spawn == 0 {
		return failoverConfig{}, errors.New("failover kills processes, so it needs -spawn, not -addr")
	}
	if f.Topic == "" {
		f.Topic = "bench-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	f.Addrs = cfg.addrs
	if err := f.Validate(); err != nil {
		return failoverConfig{}, err
	}
	return cfg, nil
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "usage: raftlogbench produce|failover [flags]   (raftlogbench <command> -h lists the flags)")
}

func run(ctx context.Context, args []string, getenv func(string) string, out, errOut io.Writer) error {
	if len(args) == 0 {
		usage(errOut)
		return errors.New("a command is needed")
	}
	switch args[0] {
	case "produce":
		cfg, err := parseProduce(args[1:], getenv, errOut)
		if err != nil {
			return err
		}
		return runProduce(ctx, cfg, out, errOut)
	case "failover":
		cfg, err := parseFailover(args[1:], getenv, errOut)
		if err != nil {
			return err
		}
		return runFailover(ctx, cfg, out, errOut)
	case "-h", "-help", "--help", "help":
		usage(out)
		return nil
	default:
		usage(errOut)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// environment is what a number needs next to it to mean anything.
type environment struct {
	Tool      string   `json:"tool"`
	GoVersion string   `json:"go_version"`
	GOOS      string   `json:"goos"`
	GOARCH    string   `json:"goarch"`
	CPUs      int      `json:"cpus"`
	Nodes     int      `json:"nodes"`
	Started   bool     `json:"nodes_started_by_the_tool"`
	NodeArgs  []string `json:"node_args,omitempty"`
}

func environmentOf(c common) environment {
	return environment{
		Tool: "raftlogbench " + version, GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		CPUs: runtime.NumCPU(), Nodes: len(c.addrs), Started: c.spawn > 0, NodeArgs: c.nodeArgs,
	}
}

// harness is a cluster ready to be measured.
type harness struct {
	cl    *client.Client
	addrs []string
	// readers has one client for each node, in node ID order, that talks to that node only:
	// a read shows what the node has applied, so the logs can be compared node by node.
	readers []bench.Fetcher
	sup     *console.Supervisor
	cleanup func()
}

// startCluster starts the nodes if asked to, connects, and waits until the cluster has
// a leader.
func startCluster(ctx context.Context, c common, errOut io.Writer) (*harness, error) {
	h := &harness{addrs: c.addrs}
	var cleanups []func()
	h.cleanup = func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*harness, error) { h.cleanup(); return nil, err }

	if c.spawn > 0 {
		dir := c.dataDir
		if dir == "" {
			d, err := os.MkdirTemp("", "raftlogbench-")
			if err != nil {
				return fail(err)
			}
			dir = d
			cleanups = append(cleanups, func() { _ = os.RemoveAll(d) })
		} else if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return fail(fmt.Errorf("-data-dir %s is not empty: the data of an earlier run would change the result", dir))
		}
		nodes := make([]console.NodeSpec, len(c.addrs))
		for i, a := range c.addrs {
			nodes[i] = console.NodeSpec{ID: i + 1, Addr: a}
		}
		sup, err := console.NewSupervisor(console.SupervisorConfig{Binary: c.binary, DataDir: dir, Nodes: nodes, ExtraArgs: c.nodeArgs})
		if err != nil {
			return fail(err)
		}
		cleanups = append(cleanups, sup.Close)
		if err := sup.StartAll(); err != nil {
			return fail(err)
		}
		h.sup = sup
		fmt.Fprintf(errOut, "started %d nodes from %s, data in %s\n", c.spawn, c.binary, dir)
	}

	cl, err := client.New(c.addrs, client.WithRetryPause(50*time.Millisecond))
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, func() { _ = cl.Close() })
	h.cl = cl
	for _, a := range c.addrs {
		one, err := client.New([]string{a}, client.WithMaxAttempts(1), client.WithRetryPause(50*time.Millisecond))
		if err != nil {
			return fail(err)
		}
		cleanups = append(cleanups, func() { _ = one.Close() })
		h.readers = append(h.readers, one)
	}

	leader, err := bench.WaitForLeader(ctx, cl, c.addrs, readyTimeout)
	if err != nil {
		return fail(fmt.Errorf("the cluster did not become ready: %w", err))
	}
	fmt.Fprintf(errOut, "node %d leads\n", leader)
	return h, nil
}

func createTopic(ctx context.Context, cl *client.Client, topic string, partitions int) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := cl.CreateTopic(cctx, topic, partitions)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, client.ErrAlreadyExists) {
			return fmt.Errorf("topic %q already exists: a measurement needs a topic of its own", topic)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("creating topic %q: %w", topic, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitQuiet waits until the topic's size has stopped changing, so that every node has
// applied all the writes before the topic is read back.
func waitQuiet(ctx context.Context, cl *client.Client, topic string) (uint64, error) {
	total := func() (uint64, bool) {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ts, err := cl.Topics(tctx)
		if err != nil {
			return 0, false
		}
		for _, t := range ts {
			if t.Name == topic {
				var sum uint64
				for _, e := range t.Ends {
					sum += e
				}
				return sum, true
			}
		}
		return 0, false
	}
	deadline := time.Now().Add(30 * time.Second)
	last, ok := total()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(quietFor / 4)
		now, ok2 := total()
		if ok2 && ok && now == last {
			if time.Since(stableSince) >= quietFor {
				return now, nil
			}
			continue
		}
		last, ok, stableSince = now, ok2, time.Now()
	}
	return 0, errors.New("the topic kept changing for 30 s")
}

type produceReport struct {
	Environment environment         `json:"environment"`
	Produce     bench.ProduceResult `json:"produce"`
	Fetch       *bench.FetchResult  `json:"fetch,omitempty"`
	Stored      uint64              `json:"messages_in_topic_afterwards"`
	Notes       []string            `json:"notes,omitempty"`
}

func runProduce(ctx context.Context, cfg produceConfig, out, errOut io.Writer) error {
	h, err := startCluster(ctx, cfg.common, errOut)
	if err != nil {
		return err
	}
	defer h.cleanup()
	if err := createTopic(ctx, h.cl, cfg.produce.Topic, cfg.produce.Partitions); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "writing for %s after a warm-up of %s\n", cfg.produce.Duration, cfg.produce.Warmup)
	rep := produceReport{Environment: environmentOf(cfg.common)}
	rep.Produce, err = bench.RunProduce(ctx, h.cl, cfg.produce)
	if err != nil {
		return err
	}
	if cfg.fetch {
		stored, err := waitQuiet(ctx, h.cl, cfg.produce.Topic)
		if err != nil {
			return err
		}
		rep.Stored = stored
		fmt.Fprintf(errOut, "reading back %d messages\n", stored)
		fr, err := bench.RunFetch(ctx, h.cl, bench.FetchConfig{
			Topic: cfg.produce.Topic, Partitions: cfg.produce.Partitions, Limit: cfg.fetchLim, RequestTimeout: cfg.produce.RequestTimeout,
		})
		if err != nil {
			return err
		}
		rep.Fetch = &fr
		if fr.Messages != stored {
			rep.Notes = append(rep.Notes, fmt.Sprintf("the read-back returned %d messages but the topic holds %d", fr.Messages, stored))
		}
	}
	if rep.Produce.Errors > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d requests failed inside the window; first error: %s", rep.Produce.Errors, rep.Produce.FirstError))
	}
	if cfg.asJSON {
		return writeJSON(out, rep)
	}
	printProduce(out, rep)
	return nil
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func lat(s bench.Summary) string {
	return fmt.Sprintf("p50 %.2f ms, p95 %.2f ms, p99 %.2f ms, max %.2f ms", bench.Millis(s.P50), bench.Millis(s.P95), bench.Millis(s.P99), bench.Millis(s.Max))
}

func printEnv(out io.Writer, e environment) {
	nodes := fmt.Sprintf("%d nodes already running", e.Nodes)
	if e.Started {
		nodes = fmt.Sprintf("%d nodes started by this run on this machine", e.Nodes)
	}
	fmt.Fprintf(out, "%s, %s, %s/%s, %d CPUs\n", e.Tool, e.GoVersion, e.GOOS, e.GOARCH, e.CPUs)
	fmt.Fprintf(out, "cluster:   %s", nodes)
	if len(e.NodeArgs) > 0 {
		fmt.Fprintf(out, "; extra node arguments: %s", strings.Join(e.NodeArgs, " "))
	}
	fmt.Fprintln(out)
}

func printProduce(out io.Writer, r produceReport) {
	printEnv(out, r.Environment)
	c := r.Produce.Config
	fmt.Fprintf(out, "workload:  %d producers, %d partitions, %d-byte values, %d per request, warm-up %s, window %s\n",
		c.Producers, c.Partitions, c.MessageSize, c.Batch, c.Warmup, c.Duration)
	fmt.Fprintf(out, "write:     %.0f messages/s, %.2f MB/s of values (%d messages in %s, %d requests, %d failed)\n",
		r.Produce.MessagesPerSec, r.Produce.MegabytesPerSec, r.Produce.Messages, r.Produce.Elapsed, r.Produce.Requests, r.Produce.Errors)
	fmt.Fprintf(out, "           request latency: %s\n", lat(r.Produce.Latency))
	if r.Fetch != nil {
		fmt.Fprintf(out, "read back: %.0f messages/s, %.2f MB/s of keys and values (%d messages in %s, %d requests of up to %d)\n",
			r.Fetch.MessagesPerSec, r.Fetch.MegabytesPerSec, r.Fetch.Messages, r.Fetch.Elapsed.Round(time.Millisecond), r.Fetch.Requests, r.Fetch.Config.Limit)
		fmt.Fprintf(out, "           request latency: %s\n", lat(r.Fetch.Latency))
	}
	for _, n := range r.Notes {
		fmt.Fprintln(out, "NOTE:", n)
	}
}

type failoverReport struct {
	Environment environment          `json:"environment"`
	Result      bench.FailoverResult `json:"failover"`
}

func runFailover(ctx context.Context, cfg failoverConfig, out, errOut io.Writer) error {
	h, err := startCluster(ctx, cfg.common, errOut)
	if err != nil {
		return err
	}
	defer h.cleanup()
	if err := createTopic(ctx, h.cl, cfg.failover.Topic, 1); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "killing the leader %d times\n", cfg.failover.Rounds)
	res, err := bench.RunFailover(ctx, h.cl, h.sup, h.readers, cfg.failover)
	rep := failoverReport{Environment: environmentOf(cfg.common), Result: res}
	if err != nil {
		if len(res.Rounds) > 0 {
			fmt.Fprintf(errOut, "stopped after %d of %d rounds\n", len(res.Rounds), cfg.failover.Rounds)
		}
		return err
	}
	if cfg.asJSON {
		return writeJSON(out, rep)
	}
	printFailover(out, rep)
	return nil
}

func printFailover(out io.Writer, r failoverReport) {
	printEnv(out, r.Environment)
	c := r.Result.Config
	fmt.Fprintf(out, "workload:  one writer, one message at a time to one partition; the leader is killed %d times, %s of writing before each kill\n", c.Rounds, c.Steady)
	fmt.Fprintln(out, "\nround  killed  new leader  leader visible  first write acknowledged")
	for i, x := range r.Result.Rounds {
		fmt.Fprintf(out, "%5d  %6d  %10d  %11.0f ms  %21.0f ms\n", i+1, x.Killed, x.NewLeader, bench.Millis(x.LeaderVisible), bench.Millis(x.FirstWrite))
	}
	lv, fw := r.Result.LeaderVisible, r.Result.FirstWrite
	fmt.Fprintf(out, "\nleader visible after the kill:        min %.0f ms, median %.0f ms, max %.0f ms\n", bench.Millis(lv.Min), bench.Millis(lv.P50), bench.Millis(lv.Max))
	fmt.Fprintf(out, "first write acknowledged after kill:  min %.0f ms, median %.0f ms, max %.0f ms\n", bench.Millis(fw.Min), bench.Millis(fw.P50), bench.Millis(fw.Max))
	fmt.Fprintf(out, "writes acknowledged %d, failed %d; acknowledged writes missing from the log: %d; values stored twice: %d\n",
		r.Result.Acked, r.Result.Failed, r.Result.Missing, r.Result.Duplicates)
	for _, n := range r.Result.Nodes {
		line := fmt.Sprintf("node %d: %d acknowledged writes missing", n.Node, n.Missing)
		if n.CaughtUpAfter > 0 {
			line += fmt.Sprintf("; held all of them only %s after the last write (it was behind)", n.CaughtUpAfter.Round(100*time.Millisecond))
		}
		fmt.Fprintln(out, line)
	}
	if r.Result.Missing > 0 {
		fmt.Fprintln(out, "NOTE: acknowledged writes were lost. That is a defect in the cluster, not a benchmark result.")
	}
}
