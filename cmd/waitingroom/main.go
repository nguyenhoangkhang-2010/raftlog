// Command waitingroom serves a virtual waiting room for course registration on
// top of a raftlog cluster.
//
// Usage:
//
//	waitingroom [-addr host:port[,host:port...]] [-listen :8080] [flags]
//
// The process keeps no data of its own. The line is stored in two raftlog topics
// (-queue-topic, -gate-topic) and rebuilt from them at start, so it can be
// restarted, or run more than once for the web part, without losing anybody's
// place. Run exactly one with -controller (the default): it is the one that lets
// people in, -admit-batch of them every -admit-interval.
//
// Every setting can also come from an environment variable; a flag wins.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/waitingroom"
)

// version is set at build time via -ldflags.
var version = "dev"

// Environment variables.
const (
	envAddr          = "RAFTLOG_ADDR"
	envListen        = "WAITINGROOM_LISTEN"
	envQueueTopic    = "WAITINGROOM_QUEUE_TOPIC"
	envGateTopic     = "WAITINGROOM_GATE_TOPIC"
	envAdmitInterval = "WAITINGROOM_ADMIT_INTERVAL"
	envAdmitBatch    = "WAITINGROOM_ADMIT_BATCH"
	envController    = "WAITINGROOM_CONTROLLER"
)

// Built-in defaults, used only when neither a flag nor a variable is given.
const (
	defaultAddr          = "localhost:9092"
	defaultListen        = ":8080"
	defaultAdmitInterval = 5 * time.Second
	defaultAdmitBatch    = 10
	defaultPollWait      = 20 * time.Second
	defaultRequestTime   = 10 * time.Second
	defaultRetryPause    = 200 * time.Millisecond
	defaultFetchLimit    = 500
	defaultKeepalive     = 15 * time.Second
)

type config struct {
	addrs         []string
	listen        string
	room          waitingroom.Config
	admitInterval time.Duration
	admitBatch    int
	controller    bool
	setup         bool
	keepalive     time.Duration
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0) // the usage was printed already
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "waitingroom:", err)
		os.Exit(1)
	}
}

// parseConfig reads the settings: flag, then environment variable, then default.
func parseConfig(args []string, getenv func(string) string, out io.Writer) (config, error) {
	cfg := config{controller: true, setup: true}
	var errs []error

	addr := orDefault(getenv(envAddr), defaultAddr)
	listen := orDefault(getenv(envListen), defaultListen)
	queue := orDefault(getenv(envQueueTopic), waitingroom.DefaultQueueTopic)
	gate := orDefault(getenv(envGateTopic), waitingroom.DefaultGateTopic)
	cfg.admitInterval = defaultAdmitInterval
	cfg.admitBatch = defaultAdmitBatch
	if v := getenv(envAdmitInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", envAdmitInterval, err))
		}
		cfg.admitInterval = d
	}
	if v := getenv(envAdmitBatch); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", envAdmitBatch, err))
		}
		cfg.admitBatch = n
	}
	if v := getenv(envController); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			cfg.controller = true
		case "0", "false", "no":
			cfg.controller = false
		default:
			errs = append(errs, fmt.Errorf("%s: %q is not true or false", envController, v))
		}
	}
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}

	fs := flag.NewFlagSet("waitingroom", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&addr, "addr", addr, "raftlog node addresses, comma separated; give all nodes of the cluster ["+envAddr+"]")
	fs.StringVar(&listen, "listen", listen, "HTTP listen address ["+envListen+"]")
	fs.StringVar(&queue, "queue-topic", queue, "topic that holds the line ["+envQueueTopic+"]")
	fs.StringVar(&gate, "gate-topic", gate, "topic that holds the admissions ["+envGateTopic+"]")
	fs.DurationVar(&cfg.admitInterval, "admit-interval", cfg.admitInterval, "time between two admissions ["+envAdmitInterval+"]")
	fs.IntVar(&cfg.admitBatch, "admit-batch", cfg.admitBatch, "people let in at each admission ["+envAdmitBatch+"]")
	fs.BoolVar(&cfg.controller, "controller", cfg.controller, "run the admission controller; exactly one process per room should ["+envController+"]")
	fs.BoolVar(&cfg.setup, "create-topics", cfg.setup, "create the two topics if they do not exist")
	fs.DurationVar(&cfg.room.PollWait, "poll-wait", defaultPollWait, "how long a read of the log waits for news; keep it below the nodes' -max-fetch-wait")
	fs.DurationVar(&cfg.room.RequestTimeout, "request-timeout", defaultRequestTime, "time allowed to a join or an admission")
	fs.DurationVar(&cfg.room.RetryPause, "retry-pause", defaultRetryPause, "pause before retrying a failed read")
	fs.IntVar(&cfg.room.FetchLimit, "fetch-limit", defaultFetchLimit, "most records read in one request")
	fs.DurationVar(&cfg.keepalive, "keepalive", defaultKeepalive, "interval of keepalive lines on idle event streams")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	for _, a := range strings.Split(addr, ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.addrs = append(cfg.addrs, a)
		}
	}
	if len(cfg.addrs) == 0 {
		return config{}, errors.New("no raftlog address: set -addr or " + envAddr)
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return config{}, fmt.Errorf("listen address %q: %w", listen, err)
	}
	cfg.listen = listen
	cfg.room.QueueTopic, cfg.room.GateTopic = queue, gate
	if cfg.controller && (cfg.admitInterval <= 0 || cfg.admitBatch < 1) {
		return config{}, errors.New("the admission interval must be positive and the batch at least 1")
	}
	if cfg.keepalive <= 0 {
		return config{}, errors.New("keepalive must be positive")
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// run serves until ctx ends. It prints "waitingroom listening on ADDR" once the
// HTTP server accepts connections.
func run(ctx context.Context, args []string, getenv func(string) string, out, errOut io.Writer) error {
	cfg, err := parseConfig(args, getenv, errOut)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(errOut, nil))
	cfg.room.Logger = logger

	log, err := client.New(cfg.addrs)
	if err != nil {
		return err
	}
	defer log.Close()
	room, err := waitingroom.New(log, cfg.room)
	if err != nil {
		return err
	}
	handler, err := waitingroom.NewHandler(room, waitingroom.HandlerOptions{Keepalive: cfg.keepalive})
	if err != nil {
		return err
	}

	if cfg.setup {
		// The cluster may be starting too: keep trying until it answers or we are stopped.
		for {
			setupCtx, cancel := context.WithTimeout(ctx, cfg.room.RequestTimeout)
			err := room.Setup(setupCtx)
			cancel()
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Warn("cannot create the topics yet; will retry", "err", err)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.listen, err)
	}
	// No WriteTimeout: event streams stay open for as long as people wait.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 3)
	go func() { errc <- room.Run(runCtx) }()
	if cfg.controller {
		go func() { errc <- room.RunController(runCtx, cfg.admitInterval, cfg.admitBatch) }()
	}
	go func() { errc <- srv.Serve(lis) }()

	fmt.Fprintf(out, "waitingroom %s listening on %s, raftlog at %s, controller %t\n", version, lis.Addr(), strings.Join(cfg.addrs, ","), cfg.controller)

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
			cancel()
			_ = srv.Close()
			return err
		}
	}
	fmt.Fprintln(out, "waitingroom: shutting down")
	cancel()
	// Open event streams would keep a graceful shutdown waiting; close them.
	return srv.Close()
}
