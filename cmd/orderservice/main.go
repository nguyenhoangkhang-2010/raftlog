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
	"github.com/nguyenhoangkhang-2010/raftlog/internal/catalog"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/orderservice"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/reports"
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
	envExpiry   = "ORDERSERVICE_EXPIRY_WARNING"
	envBoard    = "ORDERSERVICE_PUBLIC_BOARD"
	envShift    = "ORDERSERVICE_REQUIRE_SHIFT"

	envDatabase      = "ORDERSERVICE_DATABASE_URL"
	envSessionIdle   = "ORDERSERVICE_SESSION_IDLE"
	envSessionMax    = "ORDERSERVICE_SESSION_MAX"
	envSecureCookies = "ORDERSERVICE_SECURE_COOKIES"
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
	defaultSessionIdle  = 12 * time.Hour
	defaultSessionMax   = 7 * 24 * time.Hour
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
	// expiryWarn is how long before it expires a lot of goods is flagged.
	expiryWarn time.Duration
	// publicBoard lets anyone read the call board without signing in.
	publicBoard bool
	// requireShift refuses a payment in cash from a cashier who has no shift open.
	requireShift bool
	// rebuildReports throws away the reports at start and builds them again from the log.
	rebuildReports bool
	startTO    time.Duration
	readBatch  int
	jsonLogs   bool
	timezoneIn string

	// Accounts in PostgreSQL, instead of the tokens of the users file.
	databaseURL   string
	noMigrate     bool
	sessionIdle   time.Duration
	sessionMax    time.Duration
	secureCookies bool
}

// withAccounts reports whether people sign in with a password (a database) instead of a token.
func (c config) withAccounts() bool { return c.databaseURL != "" }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	var err error
	if len(os.Args) > 1 && os.Args[1] == "user" {
		err = runUser(ctx, os.Args[2:], os.Getenv, terminalPassword(os.Stdin, os.Stderr), os.Stdout, os.Stderr)
	} else {
		err = run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	}
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
	cfg.databaseURL = getenv(envDatabase)
	cfg.expiryWarn = 72 * time.Hour
	cfg.publicBoard = getenv(envBoard) == "1" || getenv(envBoard) == "true"
	cfg.requireShift = getenv(envShift) == "1" || getenv(envShift) == "true"
	if v := getenv(envExpiry); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", envExpiry, err)
		}
		cfg.expiryWarn = d
	}
	cfg.sessionIdle, cfg.sessionMax = defaultSessionIdle, defaultSessionMax
	if v := getenv(envSessionIdle); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", envSessionIdle, err)
		}
		cfg.sessionIdle = d
	}
	if v := getenv(envSessionMax); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", envSessionMax, err)
		}
		cfg.sessionMax = d
	}
	cfg.secureCookies = getenv(envSecureCookies) == "true" || getenv(envSecureCookies) == "1"

	fs := flag.NewFlagSet("orderservice", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&addr, "addr", addr, "raftlog node addresses, comma separated; give them all ["+envAddr+"]")
	fs.StringVar(&listen, "listen", listen, "address of the HTTP API ["+envListen+"]")
	fs.StringVar(&topic, "topic", topic, "raftlog topic that holds the orders; created with one partition if missing ["+envTopic+"]")
	fs.StringVar(&menuFile, "menu", menuFile, "the menu file (see examples/kitchen/menu.json) ["+envMenu+"]")
	fs.StringVar(&promoFile, "promotions", promoFile, "the promotions file; none if empty ["+envPromos+"]")
	fs.StringVar(&usersFile, "users", usersFile, "the users file: tokens and roles, for trying the service without a database ["+envUsers+"]")
	fs.StringVar(&cfg.databaseURL, "database-url", cfg.databaseURL, "PostgreSQL that holds the accounts: people sign in with a password. Prefer the variable ["+envDatabase+"]: a flag shows in the process list")
	fs.BoolVar(&cfg.noMigrate, "no-migrate", false, "do not bring the database schema up to date at start")
	fs.DurationVar(&cfg.sessionIdle, "session-idle", cfg.sessionIdle, "a session that is not used this long ends ["+envSessionIdle+"]")
	fs.DurationVar(&cfg.sessionMax, "session-max", cfg.sessionMax, "a session ends this long after sign-in at the latest ["+envSessionMax+"]")
	fs.BoolVar(&cfg.secureCookies, "secure-cookies", cfg.secureCookies, "mark the session cookie Secure although the service listens on plain HTTP: set it behind a proxy that does TLS ["+envSecureCookies+"]")
	fs.StringVar(&tz, "timezone", tz, "the venue's time zone, such as Asia/Ho_Chi_Minh: prices by time of day follow its clock ["+envTimezone+"]")
	fs.BoolVar(&cfg.publicBoard, "public-board", cfg.publicBoard, "let anyone who can reach the service read the call board (/board) without signing in, so that a screen in the shop can show it ["+envBoard+"]")
	fs.BoolVar(&cfg.requireShift, "require-shift", cfg.requireShift, "refuse a payment in cash unless the cashier who takes it has a shift open (needs -database-url) ["+envShift+"]")
	fs.BoolVar(&cfg.rebuildReports, "rebuild-reports", false, "throw away the reports at start and build them again from the whole log (needs -database-url)")
	fs.DurationVar(&cfg.expiryWarn, "expiry-warning", cfg.expiryWarn, "how long before it expires a lot of goods is flagged on the warehouse screen ["+envExpiry+"]")
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
	case menuFile == "" && cfg.databaseURL == "":
		return config{}, errors.New("no menu: give -menu (with -database-url it is only needed the first time, to fill the database)")
	case usersFile == "" && cfg.databaseURL == "":
		return config{}, errors.New("nobody could use the service: give -database-url (people sign in with a password) or -users (tokens, for trying it out)")
	case usersFile != "" && cfg.databaseURL != "":
		return config{}, errors.New("-users and -database-url are two ways to sign in: give one")
	case cfg.requireShift && cfg.databaseURL == "":
		return config{}, errors.New("-require-shift needs -database-url: the shifts are kept in the database")
	case cfg.rebuildReports && cfg.databaseURL == "":
		return config{}, errors.New("-rebuild-reports needs -database-url: the reports are kept in the database")
	case cfg.expiryWarn <= 0:
		return config{}, errors.New("-expiry-warning must be positive")
	case cfg.commandTO <= 0 || cfg.startTO <= 0 || cfg.readBatch < 1:
		return config{}, errors.New("-command-timeout, -start-timeout and -read-batch must be positive")
	case cfg.sessionIdle <= 0 || cfg.sessionMax <= 0 || cfg.sessionIdle > cfg.sessionMax:
		return config{}, errors.New("-session-idle and -session-max must be positive, and a session cannot be idle for longer than it can last")
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

	var authn orderservice.Authenticator
	if !cfg.withAccounts() {
		users, err := orderservice.LoadUsers(cfg.usersFile)
		if err != nil {
			return err
		}
		authn = orderservice.TokenAuth(users)
		logger.Warn("people sign in with the tokens of a file, which never expire: this is for trying the service; give -database-url to have accounts")
	}

	// The database is opened first: a wrong address or a migration that cannot run is a mistake
	// of the setup, and is better reported at once than after the whole log has been read.
	startCtx, cancel := context.WithTimeout(ctx, cfg.startTO)
	defer cancel()
	var apiOptions []orderservice.APIOption
	var store *catalog.Store
	var reportStore *reports.Store
	if cfg.withAccounts() {
		pool, acc, err := openAccounts(startCtx, cfg, logger)
		if err != nil {
			return err
		}
		defer pool.Close()
		if store, err = openCatalog(startCtx, pool, cfg, logger); err != nil {
			return err
		}
		inv, err := openInventory(startCtx, pool, cfg, logger)
		if err != nil {
			return err
		}
		sh, err := openShifts(startCtx, pool, cfg, logger)
		if err != nil {
			return err
		}
		if reportStore, err = openReports(startCtx, pool, cfg, logger); err != nil {
			return err
		}
		if reportStore != nil {
			apiOptions = append(apiOptions, orderservice.WithReports(reportStore, cfg.topic, cfg.location.String()))
			exp, err := openExpenses(startCtx, pool, cfg, logger)
			if err != nil {
				return err
			}
			apiOptions = append(apiOptions, orderservice.WithExpenses(exp))
		}
		go purgeSessions(ctx, acc, logger)
		apiOptions = append(apiOptions,
			orderservice.WithAccounts(acc, orderservice.AccountsOptions{SecureCookies: cfg.secureCookies, MaxLifetime: cfg.sessionMax}),
			orderservice.WithCatalog(store), orderservice.WithInventory(inv), orderservice.WithShifts(sh, cfg.requireShift))
	}
	// The menu and the promotions: from the database when there is one, else from the files.
	m, lowAt, promos, err := startCatalog(startCtx, cfg, store, logger)
	if err != nil {
		return err
	}

	cl, err := client.New(cfg.addrs, client.WithRetryPause(50*time.Millisecond))
	if err != nil {
		return err
	}
	defer cl.Close()

	l := orderservice.NewRaftLog(cl, cfg.topic)
	if err := l.Ensure(startCtx); err != nil {
		return err
	}
	svc, err := orderservice.Open(startCtx, l, orderservice.Options{
		Menu: m, Promos: promos, LowAt: lowAt, Location: cfg.location, Logger: logger,
		CommandTimeout: cfg.commandTO, ReadBatch: cfg.readBatch, ExpiryWarning: cfg.expiryWarn,
	})
	if err != nil {
		return err
	}
	if store != nil {
		hashes, err := store.ImageHashes(startCtx)
		if err != nil {
			return err
		}
		svc.SetImages(hashes)
	}
	if reportStore != nil {
		if cfg.rebuildReports {
			if err := reportStore.Reset(startCtx, cfg.topic); err != nil {
				return err
			}
			logger.Info("the reports were thrown away and are built again from the log")
		}
		go orderservice.NewProjector(reportStore, l, cfg.topic, cfg.readBatch, logger).Run(ctx)
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
	if cfg.publicBoard {
		logger.Info("the call board at /board is public: it shows the numbers of the orders and nothing else")
	}
	apiOptions = append(apiOptions, orderservice.WithPublicBoard(cfg.publicBoard))
	srv := &http.Server{Handler: orderservice.NewAPI(svc, authn, logger, apiOptions...), ReadHeaderTimeout: defaultHeaderTime}
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
