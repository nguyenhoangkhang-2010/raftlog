// Command orderservice is the kitchen system's service: customers and staff order, pay,
// prepare and hand over through a JSON API, and every order is a sequence of events kept in
// one raftlog topic. The service stores nothing else; a restarted service rebuilds its orders
// and its stock by reading that topic.
//
// Usage:
//
//	orderservice -addr h1:9092,h2:9092,h3:9092 -menu menu.json -users users.json
//
// The files that describe the venue are in examples/kitchen; docs/kitchen/service.md describes
// them and the API. Run one service per topic: it decides each command on the state it has
// built from the log, so two writers could decide on state the other has already changed.
//
// Every setting can also come from an environment variable; a flag wins. The service listens
// on 127.0.0.1 unless told otherwise.
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
	"strings"
	"time"
	_ "time/tzdata" // the venue's time zone must be found on a machine that has no zone database

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/orderservice"
)

// version is set at build time via -ldflags.
var version = "dev"

// Environment variables.
const (
	envAddr     = "RAFTLOG_ADDR"
	envListen   = "ORDERSERVICE_LISTEN"
	envTopic    = "ORDERSERVICE_TOPIC"
	envMenu     = "ORDERSERVICE_MENU"
	envPromos   = "ORDERSERVICE_PROMOTIONS"
	envUsers    = "ORDERSERVICE_USERS"
	envTimezone = "ORDERSERVICE_TIMEZONE"
)

// Built-in defaults, used only when neither a flag nor a variable is given.
const (
	defaultListen       = "127.0.0.1:8100"
	defaultTopic        = "kitchen"
	defaultTimezone     = "Local"
	defaultCommandTime  = 10 * time.Second
	defaultStartTime    = 60 * time.Second
	defaultShutdownWait = 10 * time.Second
	defaultHeaderTime   = 10 * time.Second
	defaultReadBatch    = 500
)

type config struct {
	addrs      []string
	listen     string
	topic      string
	menuFile   string
	promoFile  string
	usersFile  string
	location   *time.Location
	commandTO  time.Duration
	startTO    time.Duration
	readBatch  int
	jsonLogs   bool
	timezoneIn string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "orderservice:", err)
		os.Exit(1)
	}
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func parseConfig(args []string, getenv func(string) string, out io.Writer) (config, error) {
	cfg := config{}
	addr := getenv(envAddr)
	listen := orDefault(getenv(envListen), defaultListen)
	topic := orDefault(getenv(envTopic), defaultTopic)
	menuFile, promoFile, usersFile := getenv(envMenu), getenv(envPromos), getenv(envUsers)
	tz := orDefault(getenv(envTimezone), defaultTimezone)

	fs := flag.NewFlagSet("orderservice", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&addr, "addr", addr, "raftlog node addresses, comma separated; give them all ["+envAddr+"]")
	fs.StringVar(&listen, "listen", listen, "address of the HTTP API ["+envListen+"]")
	fs.StringVar(&topic, "topic", topic, "raftlog topic that holds the orders; created with one partition if missing ["+envTopic+"]")
	fs.StringVar(&menuFile, "menu", menuFile, "the menu file (see examples/kitchen/menu.json) ["+envMenu+"]")
	fs.StringVar(&promoFile, "promotions", promoFile, "the promotions file; none if empty ["+envPromos+"]")
	fs.StringVar(&usersFile, "users", usersFile, "the users file: who may call the service, with which role ["+envUsers+"]")
	fs.StringVar(&tz, "timezone", tz, "the venue's time zone, such as Asia/Ho_Chi_Minh: prices by time of day follow its clock ["+envTimezone+"]")
	fs.DurationVar(&cfg.commandTO, "command-timeout", defaultCommandTime, "time allowed to one command, including the wait for the log")
	fs.DurationVar(&cfg.startTO, "start-timeout", defaultStartTime, "time allowed to connect and read the whole log at start")
	fs.IntVar(&cfg.readBatch, "read-batch", defaultReadBatch, "log entries read at a time when rebuilding the state")
	fs.BoolVar(&cfg.jsonLogs, "log-json", false, "write the service's own log as JSON")
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
	switch {
	case len(cfg.addrs) == 0:
		return config{}, errors.New("no raftlog nodes: give -addr")
	case menuFile == "":
		return config{}, errors.New("no menu: give -menu")
	case usersFile == "":
		return config{}, errors.New("nobody could use the service: give -users")
	case cfg.commandTO <= 0 || cfg.startTO <= 0 || cfg.readBatch < 1:
		return config{}, errors.New("-command-timeout, -start-timeout and -read-batch must be positive")
	case topic == "":
		return config{}, errors.New("the topic needs a name")
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return config{}, fmt.Errorf("listen address %q: %w", listen, err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return config{}, fmt.Errorf("time zone %q: %w", tz, err)
	}
	cfg.listen, cfg.topic, cfg.menuFile, cfg.promoFile, cfg.usersFile, cfg.location, cfg.timezoneIn = listen, topic, menuFile, promoFile, usersFile, loc, tz
	return cfg, nil
}

// run serves until ctx ends. It prints "orderservice VERSION listening on URL" once the API is up.
func run(ctx context.Context, args []string, getenv func(string) string, out, errOut io.Writer) error {
	cfg, err := parseConfig(args, getenv, errOut)
	if err != nil {
		return err
	}
	var h slog.Handler = slog.NewTextHandler(errOut, nil)
	if cfg.jsonLogs {
		h = slog.NewJSONHandler(errOut, nil)
	}
	logger := slog.New(h)

	m, lowAt, err := orderservice.LoadMenu(cfg.menuFile)
	if err != nil {
		return err
	}
	promos, err := orderservice.LoadPromotions(cfg.promoFile)
	if err != nil {
		return err
	}
	users, err := orderservice.LoadUsers(cfg.usersFile)
	if err != nil {
		return err
	}

	cl, err := client.New(cfg.addrs, client.WithRetryPause(50*time.Millisecond))
	if err != nil {
		return err
	}
	defer cl.Close()

	startCtx, cancel := context.WithTimeout(ctx, cfg.startTO)
	defer cancel()
	l := orderservice.NewRaftLog(cl, cfg.topic)
	if err := l.Ensure(startCtx); err != nil {
		return err
	}
	svc, err := orderservice.Open(startCtx, l, orderservice.Options{
		Menu: m, Promos: promos, LowAt: lowAt, Location: cfg.location, Logger: logger,
		CommandTimeout: cfg.commandTO, ReadBatch: cfg.readBatch,
	})
	if err != nil {
		return err
	}
	health := svc.Health()
	logger.Info("state rebuilt from the log", "topic", cfg.topic, "orders", health.Orders, "records", health.Next, "skipped", health.Skipped)
	// A write whose answer is lost leaves the state possibly behind the log; this makes it
	// complete again without waiting for the next command.
	reconcileCtx, stopReconcile := context.WithCancel(ctx)
	defer stopReconcile()
	go svc.Reconcile(reconcileCtx, time.Second)

	lis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.listen, err)
	}
	srv := &http.Server{Handler: orderservice.NewAPI(svc, users, logger), ReadHeaderTimeout: defaultHeaderTime}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()
	fmt.Fprintf(out, "orderservice %s listening on http://%s, log %q at %s, time zone %s\n", version, lis.Addr(), cfg.topic, strings.Join(cfg.addrs, ","), cfg.timezoneIn)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	fmt.Fprintln(out, "orderservice: shutting down")
	sctx, stopShutdown := context.WithTimeout(context.Background(), defaultShutdownWait)
	defer stopShutdown()
	_ = srv.Shutdown(sctx)
	return nil
}
