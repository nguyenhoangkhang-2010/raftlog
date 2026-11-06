// Command raftlogctl is the command-line client for raftlog.
//
// Usage:
//
//	raftlogctl [-addr host:port] [-timeout 10s] <command> [flags] <args>
//
// Commands:
//
//	create-topic [-partitions N] <topic>
//	produce      [-key K] [-value V | -lines] [-partition P] <topic>
//	fetch        [-partition P] [-offset O] [-max N] <topic>
//	commit       -group G [-partition P] -offset O <topic>
//	offsets      -group G [-partition P] <topic>
//	consume      -group G [-partition P] [-max N] <topic>
//
// consume reads from the group's committed offset (or from 0 if none), prints
// the messages, then commits the offset after the last one it printed, so
// running it again continues where it stopped (at-least-once delivery).
//
// produce reads the value from stdin when -value is not given. With -lines,
// every line of stdin is sent as one message.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

// version is set at build time via -ldflags.
var version = "dev"

const (
	envAddr     = "RAFTLOG_ADDR"
	defaultAddr = "localhost:9092"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// run executes one command and returns the process exit code:
// 0 success, 1 failure, 2 usage error.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	addr := defaultAddr
	if v := getenv(envAddr); v != "" {
		addr = v
	}

	fs := flag.NewFlagSet("raftlogctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&addr, "addr", addr, "broker address ["+envAddr+"]")
	timeout := fs.Duration("timeout", 10*time.Second, "request timeout")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "raftlogctl", version)
		return 0
	}
	if fs.NArg() == 0 {
		usage(stderr)
		return 2
	}
	cmd, rest := fs.Arg(0), fs.Args()[1:]

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(stderr, "raftlogctl:", err)
		return 1
	}
	defer conn.Close()
	client := pb.NewBrokerClient(conn)

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var code int
	switch cmd {
	case "create-topic":
		code = cmdCreateTopic(ctx, client, rest, stderr)
	case "produce":
		code = cmdProduce(ctx, client, rest, stdin, stdout, stderr)
	case "fetch":
		code = cmdFetch(ctx, client, rest, stdout, stderr)
	case "commit":
		code = cmdCommit(ctx, client, rest, stderr)
	case "offsets":
		code = cmdOffsets(ctx, client, rest, stdout, stderr)
	case "consume":
		code = cmdConsume(ctx, client, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "raftlogctl: unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}
	return code
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: raftlogctl [-addr host:port] [-timeout 10s] <command> [flags] <args>

commands:
  create-topic [-partitions N] <topic>
  produce      [-key K] [-value V | -lines] [-partition P] <topic>
  fetch        [-partition P] [-offset O] [-max N] <topic>
  commit       -group G [-partition P] -offset O <topic>
  offsets      -group G [-partition P] <topic>
  consume      -group G [-partition P] [-max N] <topic>
`)
}

// newFlags returns a flag set for a subcommand that reports errors to stderr.
func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse parses subcommand arguments and requires exactly one positional argument (the topic).
func parse(fs *flag.FlagSet, args []string, stderr io.Writer) (topic string, code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", 0, false
		}
		return "", 2, false
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "raftlogctl %s: expected exactly one <topic> argument\n", fs.Name())
		return "", 2, false
	}
	return fs.Arg(0), 0, true
}

// fail prints a gRPC error as "<code>: <message>" and returns exit code 1.
func fail(stderr io.Writer, err error) int {
	if st, ok := status.FromError(err); ok {
		fmt.Fprintf(stderr, "raftlogctl: %s: %s\n", st.Code(), st.Message())
	} else {
		fmt.Fprintln(stderr, "raftlogctl:", err)
	}
	return 1
}

func cmdCreateTopic(ctx context.Context, c pb.BrokerClient, args []string, stderr io.Writer) int {
	fs := newFlags("create-topic", stderr)
	partitions := fs.Uint("partitions", 1, "number of partitions")
	topic, code, ok := parse(fs, args, stderr)
	if !ok {
		return code
	}
	if _, err := c.CreateTopic(ctx, &pb.CreateTopicRequest{Topic: topic, Partitions: uint32(*partitions)}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdProduce(ctx context.Context, c pb.BrokerClient, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("produce", stderr)
	key := fs.String("key", "", "message key (decides the partition)")
	value := fs.String("value", "", "message value; read from stdin if not given")
	lines := fs.Bool("lines", false, "send every line of stdin as one message")
	partition := fs.Int("partition", -1, "partition to write to; -1 lets the broker choose")
	topic, code, ok := parse(fs, args, stderr)
	if !ok {
		return code
	}

	valueSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "value" {
			valueSet = true
		}
	})
	if valueSet && *lines {
		fmt.Fprintln(stderr, "raftlogctl produce: -value and -lines cannot be used together")
		return 2
	}

	var msgs []*pb.Message
	switch {
	case valueSet:
		msgs = append(msgs, &pb.Message{Key: []byte(*key), Value: []byte(*value)})
	case *lines:
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			msgs = append(msgs, &pb.Message{Key: []byte(*key), Value: append([]byte(nil), sc.Bytes()...)})
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(stderr, "raftlogctl: read stdin:", err)
			return 1
		}
	default:
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "raftlogctl: read stdin:", err)
			return 1
		}
		msgs = append(msgs, &pb.Message{Key: []byte(*key), Value: data})
	}
	if len(msgs) == 0 {
		fmt.Fprintln(stderr, "raftlogctl produce: nothing to send")
		return 1
	}

	req := &pb.ProduceRequest{Topic: topic, Messages: msgs}
	if *partition >= 0 {
		p := uint32(*partition)
		req.Partition = &p
	}
	resp, err := c.Produce(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "partition=%d base_offset=%d count=%d\n", resp.Partition, resp.BaseOffset, len(msgs))
	return 0
}

// requireGroup checks that -group was given.
func requireGroup(name, group string, stderr io.Writer) bool {
	if group == "" {
		fmt.Fprintf(stderr, "raftlogctl %s: -group is required\n", name)
		return false
	}
	return true
}

func cmdCommit(ctx context.Context, c pb.BrokerClient, args []string, stderr io.Writer) int {
	fs := newFlags("commit", stderr)
	group := fs.String("group", "", "consumer group name (required)")
	partition := fs.Uint("partition", 0, "partition")
	offset := fs.Uint64("offset", 0, "next offset the group will read (required)")
	topic, code, ok := parse(fs, args, stderr)
	if !ok {
		return code
	}
	offsetSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "offset" {
			offsetSet = true
		}
	})
	if !requireGroup("commit", *group, stderr) {
		return 2
	}
	if !offsetSet {
		fmt.Fprintln(stderr, "raftlogctl commit: -offset is required")
		return 2
	}
	if _, err := c.CommitOffset(ctx, &pb.CommitOffsetRequest{
		Group: *group, Topic: topic, Partition: uint32(*partition), Offset: *offset,
	}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdOffsets(ctx context.Context, c pb.BrokerClient, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("offsets", stderr)
	group := fs.String("group", "", "consumer group name (required)")
	partition := fs.Uint("partition", 0, "partition")
	topic, code, ok := parse(fs, args, stderr)
	if !ok {
		return code
	}
	if !requireGroup("offsets", *group, stderr) {
		return 2
	}
	resp, err := c.FetchOffset(ctx, &pb.FetchOffsetRequest{Group: *group, Topic: topic, Partition: uint32(*partition)})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "group=%s topic=%s partition=%d committed=%t offset=%d end=%d lag=%d\n",
		*group, topic, *partition, resp.Committed, resp.Offset, resp.EndOffset, resp.Lag)
	return 0
}

func cmdConsume(ctx context.Context, c pb.BrokerClient, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("consume", stderr)
	group := fs.String("group", "", "consumer group name (required)")
	partition := fs.Uint("partition", 0, "partition to read")
	limit := fs.Uint("max", 10, "maximum number of messages")
	topic, code, ok := parse(fs, args, stderr)
	if !ok {
		return code
	}
	if !requireGroup("consume", *group, stderr) {
		return 2
	}

	pos, err := c.FetchOffset(ctx, &pb.FetchOffsetRequest{Group: *group, Topic: topic, Partition: uint32(*partition)})
	if err != nil {
		return fail(stderr, err)
	}
	resp, err := c.Fetch(ctx, &pb.FetchRequest{
		Topic:       topic,
		Partition:   uint32(*partition),
		Offset:      pos.Offset,
		MaxMessages: uint32(*limit),
	})
	if err != nil {
		return fail(stderr, err)
	}
	if len(resp.Messages) == 0 {
		return 0
	}
	for _, m := range resp.Messages {
		fmt.Fprintf(stdout, "%d\t%d\t%s\t%s\n", m.Offset, m.TimestampMs, strconv.Quote(string(m.Key)), strconv.Quote(string(m.Value)))
	}

	// Commit only after the messages were printed: a crash in between means the
	// next run prints them again (at-least-once), never that they are skipped.
	next := resp.Messages[len(resp.Messages)-1].Offset + 1
	if _, err := c.CommitOffset(ctx, &pb.CommitOffsetRequest{
		Group: *group, Topic: topic, Partition: uint32(*partition), Offset: next,
	}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdFetch(ctx context.Context, c pb.BrokerClient, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("fetch", stderr)
	partition := fs.Uint("partition", 0, "partition to read")
	offset := fs.Uint64("offset", 0, "first offset to read")
	limit := fs.Uint("max", 10, "maximum number of messages")
	topic, code, ok := parse(fs, args, stderr)
	if !ok {
		return code
	}

	resp, err := c.Fetch(ctx, &pb.FetchRequest{
		Topic:       topic,
		Partition:   uint32(*partition),
		Offset:      *offset,
		MaxMessages: uint32(*limit),
	})
	if err != nil {
		return fail(stderr, err)
	}
	for _, m := range resp.Messages {
		fmt.Fprintf(stdout, "%d\t%d\t%s\t%s\n", m.Offset, m.TimestampMs, strconv.Quote(string(m.Key)), strconv.Quote(string(m.Value)))
	}
	return 0
}
