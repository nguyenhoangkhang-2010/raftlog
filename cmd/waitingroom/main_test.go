package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/broker"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/server"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := parseConfig(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.addrs) != 1 || cfg.addrs[0] != defaultAddr || cfg.listen != defaultListen ||
		cfg.admitInterval != defaultAdmitInterval || cfg.admitBatch != defaultAdmitBatch || !cfg.controller || !cfg.setup {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.room.QueueTopic != "queue" || cfg.room.GateTopic != "gate" {
		t.Fatalf("topics = %q, %q", cfg.room.QueueTopic, cfg.room.GateTopic)
	}
}

func TestEnvironmentThenFlag(t *testing.T) {
	cfg, err := parseConfig(
		[]string{"-admit-batch", "7", "-controller=false"},
		env(map[string]string{
			envAddr: "a:1, b:2 ,", envListen: "127.0.0.1:9000", envAdmitInterval: "3s", envAdmitBatch: "99",
			envQueueTopic: "q", envGateTopic: "g", envController: "true",
		}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.addrs, "|"); got != "a:1|b:2" {
		t.Errorf("addrs = %q (blank entries must be dropped)", got)
	}
	if cfg.listen != "127.0.0.1:9000" || cfg.admitInterval != 3*time.Second || cfg.room.QueueTopic != "q" || cfg.room.GateTopic != "g" {
		t.Errorf("environment ignored: %+v", cfg)
	}
	if cfg.admitBatch != 7 || cfg.controller {
		t.Errorf("flags did not win: batch %d, controller %t", cfg.admitBatch, cfg.controller)
	}
}

func TestBadSettingsAreRefused(t *testing.T) {
	cases := map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"bad interval env": {nil, map[string]string{envAdmitInterval: "soon"}, envAdmitInterval},
		"bad batch env":    {nil, map[string]string{envAdmitBatch: "12abc"}, envAdmitBatch},
		"bad controller":   {nil, map[string]string{envController: "maybe"}, envController},
		"no addresses":     {[]string{"-addr", " , "}, nil, "no raftlog address"},
		"listen no port":   {[]string{"-listen", "localhost"}, nil, "listen address"},
		"zero batch":       {[]string{"-admit-batch", "0"}, nil, "batch"},
		"zero interval":    {[]string{"-admit-interval", "0s"}, nil, "interval"},
		"zero keepalive":   {[]string{"-keepalive", "0s"}, nil, "keepalive"},
		"stray argument":   {[]string{"extra"}, nil, "extra"},
		"unknown flag":     {[]string{"-nope"}, nil, "nope"},
	}
	for name, c := range cases {
		_, err := parseConfig(c.args, env(c.env), io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
	// A process that does not admit anyone does not need an interval.
	if _, err := parseConfig([]string{"-controller=false", "-admit-batch", "0"}, env(nil), io.Discard); err != nil {
		t.Errorf("a web-only process was refused: %v", err)
	}
}

func startBroker(t *testing.T) string {
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
	t.Cleanup(func() {
		srv.Shutdown()
		gs.Stop()
	})
	return lis.Addr().String()
}

// syncBuffer is a bytes.Buffer that can be read while a goroutine writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTheWholeRoomOverHTTP(t *testing.T) {
	raftlog := startBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", raftlog, "-listen", "127.0.0.1:0", "-admit-interval", "30ms", "-admit-batch", "2", "-poll-wait", "5s"},
			env(nil), &out, &errOut)
	}()

	re := regexp.MustCompile(`listening on (\S+),`)
	var base string
	for deadline := time.Now().Add(10 * time.Second); base == ""; {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			base = "http://" + m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("the server did not start; stderr: %s", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	const people = 9
	for i := 0; i < people; i++ {
		body := fmt.Sprintf(`{"ticket":"p%d","user":"person %d"}`, i, i)
		resp, err := http.Post(base+"/api/join", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("join %d: %d", i, resp.StatusCode)
		}
	}

	// The controller lets everybody in, two at a time.
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(base + "/api/stats")
		if err != nil {
			t.Fatal(err)
		}
		var st struct{ Total, Admitted, Waiting int }
		_ = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if st.Admitted == people {
			if st.Total != people || st.Waiting != 0 {
				t.Fatalf("stats = %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d were admitted; stderr: %s", st.Admitted, people, errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
	if !strings.Contains(out.String(), "shutting down") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAWebOnlyProcessDoesNotAdmit(t *testing.T) {
	raftlog := startBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", raftlog, "-listen", "127.0.0.1:0", "-controller=false", "-admit-interval", "10ms"}, env(nil), &out, &errOut)
	}()
	re := regexp.MustCompile(`listening on (\S+),`)
	var base string
	for deadline := time.Now().Add(10 * time.Second); base == ""; {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			base = "http://" + m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("no start; stderr: %s", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Post(base+"/api/join", "application/json", strings.NewReader(`{"ticket":"solo"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(200 * time.Millisecond)
	r2, err := http.Get(base + "/api/status?ticket=solo")
	if err != nil {
		t.Fatal(err)
	}
	var st struct{ State string }
	_ = json.NewDecoder(r2.Body).Decode(&st)
	r2.Body.Close()
	if st.State != "waiting" {
		t.Fatalf("state = %q: a process without -controller admitted someone", st.State)
	}
	cancel()
	<-done
}
