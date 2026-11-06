// Command raftlogd is the raftlog broker server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/config"
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

	b, err := broker.Open(cfg.DataDir, broker.Options{
		Storage:       cfg.StorageOptions(),
		MaxPartitions: cfg.MaxPartitions,
	})
	if err != nil {
		return fmt.Errorf("open broker: %w", err)
	}
	defer b.Close()

	srv, err := server.New(b, server.Options{MaxFetchMessages: cfg.MaxFetchMessages})
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	gs := grpc.NewServer()
	server.Register(gs, srv)

	fmt.Fprintf(out, "raftlogd %s listening on %s, data in %s\n", version, lis.Addr(), cfg.DataDir)

	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()

	select {
	case <-ctx.Done():
		fmt.Fprintln(out, "raftlogd: shutting down")
		gs.GracefulStop()
		return <-serveErr
	case err := <-serveErr:
		return err
	}
}
