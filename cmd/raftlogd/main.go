// Command raftlogd is the raftlog broker server.
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
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/config"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/metrics"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/observe"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/raft"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()

	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return
	default:
		fmt.Fprintln(os.Stderr, "raftlogd:", err)
		os.Exit(1)
	}
}

// run starts the broker and serves until ctx is cancelled or the server fails.
func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	cfg, err := config.Load(args, getenv, out)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(out, nil))

	b, err := broker.Open(cfg.DataDir, broker.Options{
		Storage:       cfg.StorageOptions(),
		MaxPartitions: cfg.MaxPartitions,
	})
	if err != nil {
		return fmt.Errorf("open broker: %w", err)
	}
	defer b.Close()

	// A standalone node applies requests to the broker directly. A node of a
	// cluster turns every change into a command in the replicated log; the broker
	// is then the state machine that applies the committed commands.
	backend := server.Backend(server.Local(b))
	srvOpts := server.Options{MaxFetchMessages: cfg.MaxFetchMessages, MaxFetchWait: cfg.MaxFetchWait}
	var runner *raft.Runner
	if cfg.NodeID != 0 {
		machine, err := broker.NewMachine(b, filepath.Join(cfg.DataDir, "machine"))
		if err != nil {
			return fmt.Errorf("open state machine: %w", err)
		}
		var closeTransport func() error
		runner, closeTransport, err = newRaftRunner(cfg, logger, machine)
		if err != nil {
			return err
		}
		defer func() { _ = closeTransport() }() // shutting down: nothing useful to do with a close error
		backend = broker.NewReplicated(machine, runner)
		srvOpts.Raft = runner
		// Tell clients where the leader is, in the address they use for this node.
		srvOpts.LeaderAddr = func(id raft.NodeID) string { return cfg.Peers[uint64(id)] }
	}

	// Metrics are off unless an address is given for them.
	var obs *observe.Node
	var reg *metrics.Registry
	if cfg.MetricsListen != "" {
		reg = metrics.NewRegistry()
		obs = observe.New(reg, version)
		srvOpts.Observer = obs
		var src observe.BrokerSource = b
		if rep, ok := backend.(*broker.Replicated); ok {
			src = rep // read through the state machine's lock
		}
		obs.WatchBroker(src)
		if runner != nil {
			obs.WatchRaft(runner)
		}
	}

	srv, err := server.New(backend, srvOpts)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	var serverOpts []grpc.ServerOption
	if runner != nil {
		// Raft messages, snapshots included, can be larger than gRPC's default.
		serverOpts = raft.ServerOptions(cfg.MaxMessageBytes)
	}
	if obs != nil {
		serverOpts = append(serverOpts, grpc.UnaryInterceptor(obs.UnaryInterceptor()))
	}
	gs := grpc.NewServer(serverOpts...)
	server.Register(gs, srv)
	if runner != nil {
		raft.RegisterGRPC(gs, runner)
	}

	mode := "standalone"
	if runner != nil {
		mode = fmt.Sprintf("raft node %d of %s", cfg.NodeID, config.FormatPeers(cfg.Peers))
	}
	fmt.Fprintf(out, "raftlogd %s listening on %s, data in %s, %s\n", version, lis.Addr(), cfg.DataDir, mode)

	if reg != nil {
		metricsLis, err := net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			return fmt.Errorf("listen for metrics on %s: %w", cfg.MetricsListen, err)
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", reg.Handler())
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
		ms := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = ms.Serve(metricsLis) }()
		defer ms.Close()
		fmt.Fprintf(out, "raftlogd: metrics on http://%s/metrics\n", metricsLis.Addr())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()
	if runner != nil {
		// Start only once we can receive messages from the other nodes.
		runner.Start()
		defer runner.Stop()
	}

	select {
	case <-ctx.Done():
		fmt.Fprintln(out, "raftlogd: shutting down")
		if runner != nil {
			runner.Stop()
		}
		srv.Shutdown() // fetches waiting for messages would hold up the stop
		gs.GracefulStop()
		return <-serveErr
	case err := <-serveErr:
		return err
	}
}

// newRaftRunner builds the Raft runner for this node from the configuration.
// The returned function closes the connections to the other nodes and the Raft
// log; call it only after the runner has stopped.
func newRaftRunner(cfg config.Config, logger *slog.Logger, machine raft.StateMachine) (*raft.Runner, func() error, error) {
	store, err := raft.NewFileStorage(filepath.Join(cfg.DataDir, "raft", "state.json"))
	if err != nil {
		return nil, nil, err
	}
	raftLog, err := raft.OpenFileLog(filepath.Join(cfg.DataDir, "raft", "log.wal"))
	if err != nil {
		return nil, nil, fmt.Errorf("open raft log: %w", err)
	}
	snapshots, err := raft.NewFileSnapshotStore(filepath.Join(cfg.DataDir, "raft", "snapshot.bin"))
	if err != nil {
		_ = raftLog.Close()
		return nil, nil, err
	}

	addrs := make(map[raft.NodeID]string, len(cfg.Peers))
	cluster := make([]raft.NodeID, 0, len(cfg.Peers))
	for id, addr := range cfg.Peers {
		addrs[raft.NodeID(id)] = addr
		cluster = append(cluster, raft.NodeID(id))
	}
	sort.Slice(cluster, func(i, j int) bool { return cluster[i] < cluster[j] })

	tr := raft.NewGRPCTransport(addrs, raft.WithMaxMessageBytes(cfg.MaxMessageBytes))
	runner, err := raft.NewRunner(raft.Config{
		ID:             raft.NodeID(cfg.NodeID),
		Cluster:        cluster,
		ElectionTicks:  cfg.ElectionTicks,
		HeartbeatTicks: cfg.HeartbeatTicks,
		Storage:        store,
		Log:            raftLog,
		Snapshots:      snapshots,
		Logger:         logger,

		SnapshotTrailing: cfg.SnapshotTrailing,
	}, tr, cfg.TickInterval, raft.WithStateMachine(machine), raft.WithSnapshotEvery(cfg.SnapshotEvery))
	if err != nil {
		_ = tr.Close()
		_ = raftLog.Close()
		return nil, nil, fmt.Errorf("start raft: %w", err)
	}
	return runner, func() error { return errors.Join(tr.Close(), raftLog.Close()) }, nil
}
