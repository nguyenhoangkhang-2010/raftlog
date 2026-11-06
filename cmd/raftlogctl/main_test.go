package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

// startBroker runs a real server on a free local port and returns its address.
func startBroker(t *testing.T) string {
	t.Helper()
	b, err := broker.Open(t.TempDir(), broker.Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(b, server.Options{MaxFetchMessages: 100})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	server.Register(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(func() {
		gs.Stop()
		_ = b.Close()
	})
	return lis.Addr().String()
}

// ctl runs raftlogctl against addr and returns exit code, stdout and stderr.
func ctl(t *testing.T, addr, stdin string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	full := append([]string{"-addr", addr}, args...)
	code := run(ctx, full, strings.NewReader(stdin), &out, &errOut, func(string) string { return "" })
	return code, out.String(), errOut.String()
}

func TestCreateProduceFetch(t *testing.T) {
	addr := startBroker(t)

	if code, _, e := ctl(t, addr, "", "create-topic", "-partitions", "2", "events"); code != 0 {
		t.Fatalf("create-topic: code %d, stderr %q", code, e)
	}

	code, out, e := ctl(t, addr, "", "produce", "-key", "u1", "-value", "hello", "-partition", "1", "events")
	if code != 0 || !strings.Contains(out, "partition=1 base_offset=0 count=1") {
		t.Fatalf("produce: code %d, stdout %q, stderr %q", code, out, e)
	}

	code, out, e = ctl(t, addr, "", "fetch", "-partition", "1", "events")
	if code != 0 {
		t.Fatalf("fetch: code %d, stderr %q", code, e)
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 4 || fields[0] != "0" || fields[2] != `"u1"` || fields[3] != `"hello"` {
		t.Fatalf("fetch output = %q", out)
	}
}

func TestProduceFromStdin(t *testing.T) {
	addr := startBroker(t)
	if code, _, e := ctl(t, addr, "", "create-topic", "events"); code != 0 {
		t.Fatalf("create-topic: %q", e)
	}

	// The whole of stdin is one message (it may contain newlines).
	if code, _, e := ctl(t, addr, "line one\nline two\n", "produce", "events"); code != 0 {
		t.Fatalf("produce: %q", e)
	}
	// With -lines, each line is its own message.
	code, out, e := ctl(t, addr, "a\nb\nc\n", "produce", "-lines", "events")
	if code != 0 || !strings.Contains(out, "base_offset=1 count=3") {
		t.Fatalf("produce -lines: code %d, stdout %q, stderr %q", code, out, e)
	}

	_, out, _ = ctl(t, addr, "", "fetch", "events")
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4:\n%s", len(rows), out)
	}
	if !strings.HasSuffix(rows[0], `"line one\nline two\n"`) {
		t.Fatalf("first row = %q", rows[0])
	}
	if !strings.HasSuffix(rows[3], `"c"`) {
		t.Fatalf("last row = %q", rows[3])
	}
}

func TestFailuresHaveClearMessagesAndExitCodes(t *testing.T) {
	addr := startBroker(t)

	code, _, e := ctl(t, addr, "", "fetch", "nope")
	if code != 1 || !strings.Contains(e, "NotFound") {
		t.Fatalf("fetch unknown topic: code %d, stderr %q", code, e)
	}

	if code, _, e := ctl(t, addr, "", "create-topic", "events"); code != 0 {
		t.Fatalf("create-topic: %q", e)
	}
	code, _, e = ctl(t, addr, "", "create-topic", "events")
	if code != 1 || !strings.Contains(e, "AlreadyExists") {
		t.Fatalf("duplicate topic: code %d, stderr %q", code, e)
	}
}

func TestUsageErrors(t *testing.T) {
	addr := startBroker(t)
	cases := [][]string{
		{},                  // no command
		{"bogus"},           // unknown command
		{"fetch"},           // missing topic
		{"fetch", "a", "b"}, // too many topics
		{"produce", "-value", "v", "-lines", "t"}, // conflicting flags
		{"fetch", "-nope", "t"},                   // unknown flag
	}
	for _, args := range cases {
		if code, _, _ := ctl(t, addr, "", args...); code != 2 {
			t.Errorf("args %v: exit code %d, want 2", args, code)
		}
	}
}

func TestAddressFromEnvironment(t *testing.T) {
	addr := startBroker(t)
	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == envAddr {
			return addr
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := run(ctx, []string{"create-topic", "events"}, strings.NewReader(""), &out, &errOut, getenv)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
}
