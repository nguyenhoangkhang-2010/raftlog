// Command raftlogconsole is a web console for a raftlog cluster: it shows the nodes,
// who the leader is and how far each node has got, writes to the cluster under a load
// you choose, and checks afterwards that no write it was told was stored has gone
// missing.
//
// Usage:
//
//	raftlogconsole -spawn 3                    run three nodes as child processes (demo)
//	raftlogconsole -addr h1:9092,h2:9092,...   only watch a cluster that is already running
//
// With -spawn the console owns the node processes, so the page has Kill and Start
// buttons: Kill ends a process at once, as a crash would. With -addr it can only
// look. The addresses are taken to be in node ID order (1, 2, 3, ...).
//
// Every setting can also come from an environment variable; a flag wins. The page
// is served on 127.0.0.1 by default because it can stop processes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/console"
)

// version is set at build time via -ldflags.
var version = "dev"

// Environment variables.
const (
	envListen  = "CONSOLE_LISTEN"
	envAddr    = "RAFTLOG_ADDR"
	envSpawn   = "CONSOLE_SPAWN"
	envBinary  = "CONSOLE_RAFTLOGD"
	envDataDir = "CONSOLE_DATA_DIR"
)

// Built-in defaults, used only when neither a flag nor a variable is given.
const (
	defaultListen       = "127.0.0.1:8090"
	defaultDataDir      = "console-data"
	defaultBasePort     = 19091
	defaultTopic        = "console-demo"
	defaultPartitions   = 2
	defaultMaxRate      = 2000
	defaultWorkers      = 16
	defaultRequestTime  = 15 * time.Second
	defaultPoll         = 500 * time.Millisecond
	defaultPollTimeout  = 400 * time.Millisecond
	defaultWindow       = 5 * time.Second
	defaultMaxTracked   = 200_000
	defaultVerifyWait   = 15 * time.Second
	defaultPeerTimeout  = 3 * time.Second
	defaultClientPause  = 50 * time.Millisecond
	defaultShutdownWait = 5 * time.Second
)

type config struct {
	listen      string
	addrs       []string // nodes to watch, in node ID order
	spawn       int      // nodes to run; 0 means only watch
	binary      string
	dataDir     string
	basePort    int
	metricsBase int
	nodeArgs    []string
	load        console.LoadConfig
	poll        time.Duration
	pollTimeout time.Duration
	verifyWait  time.Duration
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "raftlogconsole:", err)
		os.Exit(1)
	}
}

func parseConfig(args []string, getenv func(string) string, out io.Writer) (config, error) {
	cfg := config{}
	listen := orDefault(getenv(envListen), defaultListen)
	addr := getenv(envAddr)
	dataDir := orDefault(getenv(envDataDir), defaultDataDir)
	binary := getenv(envBinary)
	spawn := 0
	if v := getenv(envSpawn); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", envSpawn, err)
		}
		spawn = n
	}
	var nodeArgs string

	fs := flag.NewFlagSet("raftlogconsole", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&listen, "listen", listen, "address of the web page ["+envListen+"]")
	fs.StringVar(&addr, "addr", addr, "watch a running cluster: node addresses in node ID order, comma separated ["+envAddr+"]")
	fs.IntVar(&spawn, "spawn", spawn, "run this many nodes as child processes (the demo mode) ["+envSpawn+"]")
	fs.StringVar(&binary, "raftlogd", binary, "the raftlogd executable to run with -spawn; default: next to this program ["+envBinary+"]")
	fs.StringVar(&dataDir, "data-dir", dataDir, "directory for the spawned nodes' data and logs ["+envDataDir+"]")
	fs.IntVar(&cfg.basePort, "base-port", defaultBasePort, "with -spawn, node N listens on this port plus N-1")
	fs.IntVar(&cfg.metricsBase, "metrics-base-port", 0, "with -spawn, node N serves /metrics on this port plus N-1; 0 = no metrics")
	fs.StringVar(&nodeArgs, "node-args", "", "extra arguments for every spawned node, space separated, e.g. \"-tick-interval 50ms\"")
	fs.StringVar(&cfg.load.Topic, "topic", defaultTopic, "topic the writer uses")
	fs.IntVar(&cfg.load.Partitions, "partitions", defaultPartitions, "partitions of that topic")
	fs.IntVar(&cfg.load.MaxRate, "max-rate", defaultMaxRate, "highest write rate the page may ask for, messages per second")
	fs.IntVar(&cfg.load.Workers, "workers", defaultWorkers, "writes in flight at once")
	fs.DurationVar(&cfg.load.RequestTimeout, "request-timeout", defaultRequestTime, "time allowed to one write, which includes waiting out an election")
	fs.DurationVar(&cfg.poll, "poll", defaultPoll, "how often the nodes are asked for their state")
	fs.DurationVar(&cfg.pollTimeout, "poll-timeout", defaultPollTimeout, "how long a node has to answer")
	fs.DurationVar(&cfg.verifyWait, "verify-wait", defaultVerifyWait, "how long a verification waits for a lagging node to show a write")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return config{}, fmt.Errorf("listen address %q: %w", listen, err)
	}
	cfg.listen = listen
	cfg.dataDir = dataDir
	cfg.spawn = spawn
	cfg.nodeArgs = strings.Fields(nodeArgs)
	cfg.load.Window = defaultWindow
	cfg.load.MaxTracked = defaultMaxTracked

	for _, a := range strings.Split(addr, ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.addrs = append(cfg.addrs, a)
		}
	}
	switch {
	case spawn < 0:
		return config{}, errors.New("-spawn must not be negative")
	case spawn > 0 && len(cfg.addrs) > 0:
		return config{}, errors.New("use either -spawn (run nodes) or -addr (watch nodes), not both")
	case spawn == 0 && len(cfg.addrs) == 0:
		return config{}, errors.New("nothing to show: give -spawn N to run a cluster, or -addr to watch one")
	case spawn > 0 && (cfg.basePort < 1 || cfg.basePort+spawn > 65535):
		return config{}, errors.New("-base-port is out of range for that many nodes")
	case cfg.metricsBase < 0 || (cfg.metricsBase > 0 && cfg.metricsBase+spawn > 65535):
		return config{}, errors.New("-metrics-base-port is out of range for that many nodes")
	case cfg.poll <= 0 || cfg.pollTimeout <= 0 || cfg.verifyWait <= 0:
		return config{}, errors.New("-poll, -poll-timeout and -verify-wait must be positive")
	}

	if spawn > 0 {
		if binary == "" {
			exe, err := os.Executable()
			if err != nil {
				return config{}, fmt.Errorf("cannot find this program to look for raftlogd next to it: %w", err)
			}
			name := "raftlogd"
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			binary = filepath.Join(filepath.Dir(exe), name)
		}
		cfg.binary = binary
		for i := 0; i < spawn; i++ {
			cfg.addrs = append(cfg.addrs, fmt.Sprintf("127.0.0.1:%d", cfg.basePort+i))
		}
	}
	if err := cfg.load.Validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// run serves the console until ctx ends. It prints "raftlogconsole listening on URL"
// once the page is up.
func run(ctx context.Context, args []string, getenv func(string) string, out, errOut io.Writer) error {
	cfg, err := parseConfig(args, getenv, errOut)
	if err != nil {
		return err
	}
	nodes := make([]console.NodeSpec, len(cfg.addrs))
	for i, a := range cfg.addrs {
		nodes[i] = console.NodeSpec{ID: i + 1, Addr: a}
		if cfg.spawn > 0 && cfg.metricsBase > 0 {
			nodes[i].MetricsAddr = fmt.Sprintf("127.0.0.1:%d", cfg.metricsBase+i)
		}
	}

	cl, err := client.New(cfg.addrs, client.WithRetryPause(defaultClientPause))
	if err != nil {
		return err
	}
	defer cl.Close()

	// The topics are read from every node, one client each, and the furthest-ahead
	// answer is shown; a node that was just restarted would otherwise understate them.
	perNode := make([]console.Lister, len(cfg.addrs))
	for i, a := range cfg.addrs {
		c, err := client.New([]string{a}, client.WithMaxAttempts(1), client.WithRetryPause(defaultClientPause))
		if err != nil {
			return err
		}
		defer c.Close()
		perNode[i] = c
	}
	lister := console.NewMultiLister(perNode...)

	var sup *console.Supervisor
	var control console.Controller
	running := func(int) (bool, bool) { return false, false }
	if cfg.spawn > 0 {
		sup, err = console.NewSupervisor(console.SupervisorConfig{Binary: cfg.binary, DataDir: cfg.dataDir, Nodes: nodes, ExtraArgs: cfg.nodeArgs})
		if err != nil {
			return err
		}
		defer sup.Close()
		if err := sup.StartAll(); err != nil {
			return err
		}
		control = sup
		running = func(id int) (bool, bool) { return true, sup.Running(id) }
	}

	obs := console.NewObserver(cl, nodes, running, cfg.pollTimeout)
	load, err := console.NewLoad(cl, cfg.load)
	if err != nil {
		return err
	}
	defer load.Stop()
	handler, err := console.NewServer(console.ServerOptions{Observer: obs, Control: control, Load: load, Lister: lister, VerifyPatience: cfg.verifyWait})
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.listen, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: defaultPeerTimeout}
	go obs.Run(ctx, cfg.poll)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	mode := fmt.Sprintf("watching %s", strings.Join(cfg.addrs, ","))
	if cfg.spawn > 0 {
		mode = fmt.Sprintf("running %d nodes from %s, data in %s", cfg.spawn, cfg.binary, cfg.dataDir)
	}
	fmt.Fprintf(out, "raftlogconsole %s listening on http://%s, %s\n", version, lis.Addr(), mode)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	fmt.Fprintln(out, "raftlogconsole: shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), defaultShutdownWait)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}
