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
	srv, err := server.New(server.Local(b), server.Options{MaxFetchMessages: 100, MaxFetchWait: time.Minute})
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

func TestFetchAndConsumeCanWaitForAMessage(t *testing.T) {
	addr := startBroker(t)

	type outcome struct {
		code     int
		out, err string
	}
	// Each command gets its own empty topic to wait on.
	cases := map[string][]string{
		"fetch":   {"fetch", "-wait", "8s", "fetch"},
		"consume": {"consume", "-group", "g", "-wait", "8s", "consume"},
	}
	for topic, args := range cases {
		if code, _, e := ctl(t, addr, "", "create-topic", topic); code != 0 {
			t.Fatalf("create-topic: %q", e)
		}
		done := make(chan outcome, 1)
		go func() {
			code, out, e := ctl(t, addr, "", args...)
			done <- outcome{code, out, e}
		}()
		select {
		case o := <-done:
			t.Fatalf("%v returned before there was a message: %+v", args, o)
		case <-time.After(200 * time.Millisecond):
		}
		if code, _, e := ctl(t, addr, "", "produce", "-value", "late-"+topic, topic); code != 0 {
			t.Fatalf("produce: %q", e)
		}
		select {
		case o := <-done:
			if o.code != 0 || !strings.Contains(o.out, `"late-`+topic+`"`) {
				t.Fatalf("%v: %+v", args, o)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%v kept waiting although a message arrived", args)
		}
	}

	// Nothing arrives: the command returns quietly when the wait is over.
	code, out, e := ctl(t, addr, "", "fetch", "-offset", "1", "-wait", "200ms", "fetch")
	if code != 0 || out != "" {
		t.Fatalf("fetch at the end: code %d, stdout %q, stderr %q", code, out, e)
	}
}

func TestAWaitThatDoesNotFitInTheTimeoutIsRefused(t *testing.T) {
	addr := startBroker(t)
	if code, _, e := ctl(t, addr, "", "create-topic", "events"); code != 0 {
		t.Fatalf("create-topic: %q", e)
	}
	code, _, e := ctl(t, addr, "", "-timeout", "2s", "fetch", "-wait", "5s", "events")
	if code != 2 || !strings.Contains(e, "-timeout") {
		t.Fatalf("code %d, stderr %q; want a usage error that names -timeout", code, e)
	}
	if code, _, _ := ctl(t, addr, "", "fetch", "-wait", "-1s", "events"); code != 2 {
		t.Fatalf("a negative wait: code %d, want 2", code)
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

func TestTopicsAndGroupsCommands(t *testing.T) {
	addr := startBroker(t)

	// Nothing yet: no output and no error.
	for _, cmd := range []string{"topics", "groups"} {
		if code, out, e := ctl(t, addr, "", cmd); code != 0 || out != "" {
			t.Fatalf("%s on an empty broker: code %d, stdout %q, stderr %q", cmd, code, out, e)
		}
	}

	if code, _, e := ctl(t, addr, "", "create-topic", "-partitions", "2", "events"); code != 0 {
		t.Fatalf("create-topic: %q", e)
	}
	if code, _, e := ctl(t, addr, "", "produce", "-value", "a", "-partition", "1", "events"); code != 0 {
		t.Fatalf("produce: %q", e)
	}
	if code, _, e := ctl(t, addr, "", "produce", "-value", "b", "-partition", "1", "events"); code != 0 {
		t.Fatalf("produce: %q", e)
	}
	code, out, e := ctl(t, addr, "", "topics")
	if code != 0 || out != "topic=events partitions=2 messages=2 ends=0,2\n" {
		t.Fatalf("topics: code %d, stdout %q, stderr %q", code, out, e)
	}

	if code, _, e := ctl(t, addr, "", "commit", "-group", "billing", "-partition", "1", "-offset", "1", "events"); code != 0 {
		t.Fatalf("commit: %q", e)
	}
	code, out, e = ctl(t, addr, "", "groups")
	if code != 0 || out != "group=billing topic=events partition=1 offset=1 end=2 lag=1\n" {
		t.Fatalf("groups: code %d, stdout %q, stderr %q", code, out, e)
	}

	// Both take no arguments.
	for _, cmd := range []string{"topics", "groups"} {
		if code, _, _ := ctl(t, addr, "", cmd, "extra"); code != 2 {
			t.Errorf("%s with an argument: exit code %d, want 2", cmd, code)
		}
	}
}
