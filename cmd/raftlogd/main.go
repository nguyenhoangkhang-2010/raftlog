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
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/config"
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

	srvOpts := server.Options{MaxFetchMessages: cfg.MaxFetchMessages}
	var runner *raft.Runner
	if cfg.NodeID != 0 {
		var closeTransport func() error
		runner, closeTransport, err = newRaftRunner(cfg, logger)
		if err != nil {
			return err
		}
		defer func() { _ = closeTransport() }() // shutting down: nothing useful to do with a close error
		srvOpts.Raft = runner
	}

	srv, err := server.New(b, srvOpts)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	gs := grpc.NewServer()
	server.Register(gs, srv)
	if runner != nil {
		raft.RegisterGRPC(gs, runner)
	}

	mode := "standalone"
	if runner != nil {
		mode = fmt.Sprintf("raft node %d of %s", cfg.NodeID, config.FormatPeers(cfg.Peers))
	}
	fmt.Fprintf(out, "raftlogd %s listening on %s, data in %s, %s\n", version, lis.Addr(), cfg.DataDir, mode)

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
		gs.GracefulStop()
		return <-serveErr
	case err := <-serveErr:
		return err
	}
}

// newRaftRunner builds the Raft runner for this node from the configuration.
// The returned function closes the connections to the other nodes and the Raft
// log; call it only after the runner has stopped.
func newRaftRunner(cfg config.Config, logger *slog.Logger) (*raft.Runner, func() error, error) {
	store, err := raft.NewFileStorage(filepath.Join(cfg.DataDir, "raft", "state.json"))
	if err != nil {
		return nil, nil, err
	}
	raftLog, err := raft.OpenFileLog(filepath.Join(cfg.DataDir, "raft", "log.wal"))
	if err != nil {
		return nil, nil, fmt.Errorf("open raft log: %w", err)
	}

	addrs := make(map[raft.NodeID]string, len(cfg.Peers))
	cluster := make([]raft.NodeID, 0, len(cfg.Peers))
	for id, addr := range cfg.Peers {
		addrs[raft.NodeID(id)] = addr
		cluster = append(cluster, raft.NodeID(id))
	}
	sort.Slice(cluster, func(i, j int) bool { return cluster[i] < cluster[j] })

	tr := raft.NewGRPCTransport(addrs)
	runner, err := raft.NewRunner(raft.Config{
		ID:             raft.NodeID(cfg.NodeID),
		Cluster:        cluster,
		ElectionTicks:  cfg.ElectionTicks,
		HeartbeatTicks: cfg.HeartbeatTicks,
		Storage:        store,
		Log:            raftLog,
		Logger:         logger,
	}, tr, cfg.TickInterval)
	if err != nil {
		_ = tr.Close()
		_ = raftLog.Close()
		return nil, nil, fmt.Errorf("start raft: %w", err)
	}
	return runner, func() error { return errors.Join(tr.Close(), raftLog.Close()) }, nil
}
