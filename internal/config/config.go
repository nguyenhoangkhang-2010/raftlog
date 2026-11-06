package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

// Environment variables. A flag on the command line overrides its variable,
// and a variable overrides the built-in default.
const (
	EnvListenAddr         = "RAFTLOG_LISTEN_ADDR"
	EnvDataDir            = "RAFTLOG_DATA_DIR"
	EnvSegmentMaxBytes    = "RAFTLOG_SEGMENT_MAX_BYTES"
	EnvIndexIntervalBytes = "RAFTLOG_INDEX_INTERVAL_BYTES"
	EnvSyncOnAppend       = "RAFTLOG_SYNC_ON_APPEND"
	EnvMaxPartitions      = "RAFTLOG_MAX_PARTITIONS"
	EnvMaxFetchMessages   = "RAFTLOG_MAX_FETCH_MESSAGES"
)

// Built-in defaults, used only when neither a flag nor a variable is given.
const (
	defaultListenAddr    = ":9092"
	defaultDataDir       = "data"
	defaultMaxPartitions = 1024
	defaultMaxFetch      = 1000
)

// Config is the configuration of one raftlogd node.
type Config struct {
	// ListenAddr is the host:port the gRPC server listens on.
	ListenAddr string
	// DataDir is the directory where topics and logs are stored.
	DataDir string
	// SegmentMaxBytes is the log segment size; 0 means the storage default.
	SegmentMaxBytes int64
	// IndexIntervalBytes is the sparse index spacing; 0 means the storage default.
	IndexIntervalBytes int64
	// SyncOnAppend makes every append fsync before it is acknowledged.
	SyncOnAppend bool
	// MaxPartitions limits the partitions of a single topic.
	MaxPartitions int
	// MaxFetchMessages caps how many messages one Fetch request may return.
	MaxFetchMessages int
}

// Load builds a Config from environment variables (read through getenv) and
// command-line arguments (without the program name). Usage and flag errors are
// written to out.
//
// Precedence, highest first: flag, environment variable, built-in default.
func Load(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	cfg := Config{
		ListenAddr:       defaultListenAddr,
		DataDir:          defaultDataDir,
		MaxPartitions:    defaultMaxPartitions,
		MaxFetchMessages: defaultMaxFetch,
	}

	var errs []error
	if v := getenv(EnvListenAddr); v != "" {
		cfg.ListenAddr = v
	}
	if v := getenv(EnvDataDir); v != "" {
		cfg.DataDir = v
	}
	if v := getenv(EnvSegmentMaxBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvSegmentMaxBytes, err))
		}
		cfg.SegmentMaxBytes = n
	}
	if v := getenv(EnvIndexIntervalBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvIndexIntervalBytes, err))
		}
		cfg.IndexIntervalBytes = n
	}
	if v := getenv(EnvSyncOnAppend); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvSyncOnAppend, err))
		}
		cfg.SyncOnAppend = b
	}
	if v := getenv(EnvMaxPartitions); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvMaxPartitions, err))
		}
		cfg.MaxPartitions = n
	}
	if v := getenv(EnvMaxFetchMessages); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvMaxFetchMessages, err))
		}
		cfg.MaxFetchMessages = n
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	fs := flag.NewFlagSet("raftlogd", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "gRPC listen address (host:port) ["+EnvListenAddr+"]")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory for topics and logs ["+EnvDataDir+"]")
	fs.Int64Var(&cfg.SegmentMaxBytes, "segment-max-bytes", cfg.SegmentMaxBytes, "log segment size in bytes, 0 = storage default ["+EnvSegmentMaxBytes+"]")
	fs.Int64Var(&cfg.IndexIntervalBytes, "index-interval-bytes", cfg.IndexIntervalBytes, "bytes of log between index entries, 0 = storage default ["+EnvIndexIntervalBytes+"]")
	fs.BoolVar(&cfg.SyncOnAppend, "sync-on-append", cfg.SyncOnAppend, "fsync after every append ["+EnvSyncOnAppend+"]")
	fs.IntVar(&cfg.MaxPartitions, "max-partitions", cfg.MaxPartitions, "maximum partitions per topic ["+EnvMaxPartitions+"]")
	fs.IntVar(&cfg.MaxFetchMessages, "max-fetch-messages", cfg.MaxFetchMessages, "maximum messages returned by one fetch ["+EnvMaxFetchMessages+"]")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("listen address %q: %w", c.ListenAddr, err))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("data dir must not be empty"))
	}
	if c.SegmentMaxBytes < 0 {
		errs = append(errs, errors.New("segment max bytes must not be negative"))
	}
	if c.IndexIntervalBytes < 0 {
		errs = append(errs, errors.New("index interval bytes must not be negative"))
	}
	if c.MaxPartitions < 1 {
		errs = append(errs, errors.New("max partitions must be at least 1"))
	}
	if c.MaxFetchMessages < 1 {
		errs = append(errs, errors.New("max fetch messages must be at least 1"))
	}
	return errors.Join(errs...)
}

// StorageOptions returns the storage settings derived from the configuration.
func (c Config) StorageOptions() storage.Options {
	return storage.Options{
		SegmentMaxBytes:    c.SegmentMaxBytes,
		IndexIntervalBytes: c.IndexIntervalBytes,
		SyncOnAppend:       c.SyncOnAppend,
	}
}
