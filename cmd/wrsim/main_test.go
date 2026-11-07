package main

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/waitingroom"
)

func env(map[string]string) func(string) string { return func(string) string { return "" } }

// startRoom runs a real waiting room (broker, room, controller, HTTP) and returns
// its URL.
func startRoom(t *testing.T, interval time.Duration, batch int) string {
	t.Helper()
	b, err := broker.Open(filepath.Join(t.TempDir(), "data"), broker.Options{MaxPartitions: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	srv, err := server.New(server.Local(b), server.Options{MaxFetchMessages: 1000, MaxFetchWait: time.Minute})
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
	t.Cleanup(func() { srv.Shutdown(); gs.Stop() })

	c, err := client.New([]string{lis.Addr().String()}, client.WithRetryPause(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	room, err := waitingroom.New(c, waitingroom.Config{
		QueueTopic: "queue", GateTopic: "gate", PollWait: 5 * time.Second, RetryPause: 10 * time.Millisecond,
		FetchLimit: 100, RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := room.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { _ = room.Run(ctx) }()
	go func() { _ = room.RunController(ctx, interval, batch) }()
	h, err := waitingroom.NewHandler(room, waitingroom.HandlerOptions{Keepalive: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })
	return ts.URL
}

func TestEveryoneIsAdmittedAndNothingLooksWrong(t *testing.T) {
	url := startRoom(t, 30*time.Millisecond, 4)
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-url", url, "-users", "40", "-arrival-rate", "400", "-seed", "5", "-timeout", "30s"}, env(nil), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"seed 5", "people            40", "admitted          40", "(must be 0)", "OK"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPeopleWhoAreNotAdmittedInTimeAreReported(t *testing.T) {
	// A room that admits one person every ten seconds cannot serve 10 people in a second.
	url := startRoom(t, 10*time.Second, 1)
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-url", url, "-users", "10", "-arrival-rate", "1000", "-timeout", "1s"}, env(nil), &out, &errOut)
	if code != 1 || !strings.Contains(out.String(), "PROBLEMS") {
		t.Fatalf("exit code %d, want 1 and PROBLEMS:\n%s", code, out.String())
	}
}

func TestTheSameSeedGivesTheSameArrivals(t *testing.T) {
	// The seed decides the gaps; the number of people and the rate decide the
	// rest. Two runs with one seed draw the same gaps.
	draw := func(seed int64) []time.Duration {
		a := newArrivals(seed, 100)
		gaps := make([]time.Duration, 5)
		for i := range gaps {
			gaps[i] = a.next()
		}
		return gaps
	}
	a, b, c := draw(1), draw(1), draw(2)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("gap %d differs for one seed: %v vs %v", i, a[i], b[i])
		}
	}
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
		}
	}
	if same {
		t.Fatal("two seeds drew the same gaps")
	}
}

func TestOvertakesAreSeen(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	mk := func(offset uint64, admitAfter time.Duration) *person {
		return &person{offset: offset, admitted: true, admitAt: base.Add(admitAfter)}
	}
	fair := &report{people: []*person{mk(0, 0), mk(1, time.Second), mk(2, 2*time.Second)}}
	if n := fair.overtakes(time.Second / 2); n != 0 {
		t.Fatalf("a fair order shows %d overtakes", n)
	}
	// Offset 0 was seen admitted 5 s after offset 1: offset 1 got in first.
	unfair := &report{people: []*person{mk(0, 5*time.Second), mk(1, 0), mk(2, time.Second)}}
	if n := unfair.overtakes(time.Second); n == 0 {
		t.Fatal("an overtake went unseen")
	}
	// Within the grace it is only delivery jitter.
	jitter := &report{people: []*person{mk(0, 100*time.Millisecond), mk(1, 0)}}
	if n := jitter.overtakes(time.Second); n != 0 {
		t.Fatalf("jitter within the grace counted as %d overtakes", n)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"-users", "0"}, {"-arrival-rate", "0"}, {"-timeout", "0s"}, {"-grace", "-1s"}, {"extra"}, {"-nope"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, env(nil), &out, &errOut); code != 2 {
			t.Errorf("args %v: exit code %d, want 2", args, code)
		}
	}
}
