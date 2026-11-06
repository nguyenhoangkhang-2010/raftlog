package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

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
	EnvNodeID             = "RAFTLOG_NODE_ID"
	EnvPeers              = "RAFTLOG_PEERS"
	EnvElectionTicks      = "RAFTLOG_ELECTION_TICKS"
	EnvHeartbeatTicks     = "RAFTLOG_HEARTBEAT_TICKS"
	EnvTickInterval       = "RAFTLOG_TICK_INTERVAL"
	EnvMaxMessageBytes    = "RAFTLOG_MAX_MESSAGE_BYTES"
)

// Built-in defaults, used only when neither a flag nor a variable is given.
const (
	defaultListenAddr    = ":9092"
	defaultDataDir       = "data"
	defaultMaxPartitions = 1024
	defaultMaxFetch      = 1000

	// Raft timing. With these defaults a heartbeat goes out every tick and a
	// follower waits 1-2 seconds before it starts an election.
	defaultTickInterval   = 100 * time.Millisecond
	defaultHeartbeatTicks = 1
	defaultElectionTicks  = 10

	// defaultMaxMessageBytes caps one node-to-node message. A snapshot travels
	// in a single message, so this is also the largest state that can be sent
	// to a follower that fell behind the log.
	defaultMaxMessageBytes = 64 << 20
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

	// NodeID is this node's ID in a Raft cluster. 0 means standalone: no Raft.
	NodeID uint64
	// Peers maps every cluster member, including this node, to the address
	// (host:port) where its gRPC server can be reached. Required with NodeID.
	Peers map[uint64]string
	// ElectionTicks and HeartbeatTicks are the Raft timeouts in ticks; see
	// raft.Config. TickInterval is the length of one tick.
	ElectionTicks  int
	HeartbeatTicks int
	TickInterval   time.Duration
	// MaxMessageBytes is the largest node-to-node message (see raft.WithMaxMessageBytes).
	MaxMessageBytes int
}

// ParsePeers parses a peer list of the form "1=host:port,2=host:port".
func ParsePeers(s string) (map[uint64]string, error) {
	peers := make(map[uint64]string)
	if strings.TrimSpace(s) == "" {
		return peers, nil
	}
	for _, item := range strings.Split(s, ",") {
		idStr, addr, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			return nil, fmt.Errorf("peer %q: expected id=host:port", item)
		}
		id, err := strconv.ParseUint(strings.TrimSpace(idStr), 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("peer %q: id must be a positive integer", item)
		}
		addr = strings.TrimSpace(addr)
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("peer %q: %w", item, err)
		}
		if _, dup := peers[id]; dup {
			return nil, fmt.Errorf("peer id %d listed twice", id)
		}
		peers[id] = addr
	}
	return peers, nil
}

// FormatPeers is the inverse of ParsePeers, with ids in ascending order.
func FormatPeers(peers map[uint64]string) string {
	ids := make([]uint64, 0, len(peers))
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("%d=%s", id, peers[id])
	}
	return strings.Join(parts, ",")
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
		ElectionTicks:    defaultElectionTicks,
		HeartbeatTicks:   defaultHeartbeatTicks,
		TickInterval:     defaultTickInterval,
		MaxMessageBytes:  defaultMaxMessageBytes,
	}
	var peersFlag string

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
	if v := getenv(EnvNodeID); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvNodeID, err))
		}
		cfg.NodeID = n
	}
	peersFlag = getenv(EnvPeers)
	if v := getenv(EnvElectionTicks); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvElectionTicks, err))
		}
		cfg.ElectionTicks = n
	}
	if v := getenv(EnvHeartbeatTicks); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvHeartbeatTicks, err))
		}
		cfg.HeartbeatTicks = n
	}
	if v := getenv(EnvMaxMessageBytes); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvMaxMessageBytes, err))
		}
		cfg.MaxMessageBytes = n
	}
	if v := getenv(EnvTickInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvTickInterval, err))
		}
		cfg.TickInterval = d
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
	fs.Uint64Var(&cfg.NodeID, "node-id", cfg.NodeID, "Raft node ID, 0 = standalone without Raft ["+EnvNodeID+"]")
	fs.StringVar(&peersFlag, "peers", peersFlag, "cluster members as id=host:port,id=host:port, including this node ["+EnvPeers+"]")
	fs.IntVar(&cfg.ElectionTicks, "election-ticks", cfg.ElectionTicks, "base election timeout in ticks ["+EnvElectionTicks+"]")
	fs.IntVar(&cfg.HeartbeatTicks, "heartbeat-ticks", cfg.HeartbeatTicks, "heartbeat period in ticks ["+EnvHeartbeatTicks+"]")
	fs.DurationVar(&cfg.TickInterval, "tick-interval", cfg.TickInterval, "length of one Raft tick ["+EnvTickInterval+"]")
	fs.IntVar(&cfg.MaxMessageBytes, "max-message-bytes", cfg.MaxMessageBytes, "largest node-to-node message, which bounds the size of a snapshot ["+EnvMaxMessageBytes+"]")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	peers, err := ParsePeers(peersFlag)
	if err != nil {
		return Config{}, fmt.Errorf("peers: %w", err)
	}
	cfg.Peers = peers

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
	if c.NodeID == 0 {
		if len(c.Peers) > 0 {
			errs = append(errs, errors.New("peers are set but node ID is 0: set a node ID or drop the peers"))
		}
	} else {
		if _, ok := c.Peers[c.NodeID]; !ok {
			errs = append(errs, fmt.Errorf("peers must include this node (id %d)", c.NodeID))
		}
		if c.MaxMessageBytes < 1 {
			errs = append(errs, errors.New("max message bytes must be at least 1"))
		}
		if c.TickInterval <= 0 {
			errs = append(errs, errors.New("tick interval must be positive"))
		}
		if c.HeartbeatTicks < 1 {
			errs = append(errs, errors.New("heartbeat ticks must be at least 1"))
		}
		if c.ElectionTicks <= c.HeartbeatTicks {
			errs = append(errs, errors.New("election ticks must be greater than heartbeat ticks"))
		}
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
