package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestSomethingToShowIsRequired(t *testing.T) {
	if _, err := parseConfig(nil, env(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "nothing to show") {
		t.Fatalf("no -spawn and no -addr: %v", err)
	}
	if _, err := parseConfig([]string{"-spawn", "3", "-addr", "a:1"}, env(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both: %v", err)
	}
}

func TestWatchingAClusterListsItsAddresses(t *testing.T) {
	cfg, err := parseConfig([]string{"-addr", "h1:9092, h2:9092 ,"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.addrs, "|"); got != "h1:9092|h2:9092" || cfg.spawn != 0 || cfg.binary != "" {
		t.Fatalf("addrs %q, spawn %d, binary %q", got, cfg.spawn, cfg.binary)
	}
	if cfg.listen != defaultListen || cfg.poll != defaultPoll || cfg.load.Topic != defaultTopic || cfg.load.Window <= 0 || cfg.load.MaxTracked <= 0 {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestSpawningNodesGivesThemConsecutivePorts(t *testing.T) {
	cfg, err := parseConfig([]string{"-spawn", "3", "-base-port", "20000", "-raftlogd", "/x/raftlogd", "-node-args", "-tick-interval 50ms  -election-ticks 8"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.addrs, "|"); got != "127.0.0.1:20000|127.0.0.1:20001|127.0.0.1:20002" {
		t.Fatalf("addrs = %q", got)
	}
	if cfg.binary != "/x/raftlogd" || len(cfg.nodeArgs) != 4 || cfg.nodeArgs[0] != "-tick-interval" || cfg.nodeArgs[3] != "8" {
		t.Fatalf("binary %q, node args %v", cfg.binary, cfg.nodeArgs)
	}
	// Without -raftlogd the binary is looked for next to this program.
	cfg, err = parseConfig([]string{"-spawn", "1"}, env(nil), io.Discard)
	if err != nil || !strings.HasPrefix(filepath.Base(cfg.binary), "raftlogd") {
		t.Fatalf("default binary %q, %v", cfg.binary, err)
	}
}

func TestEnvironmentThenFlag(t *testing.T) {
	cfg, err := parseConfig([]string{"-listen", "127.0.0.1:9999"}, env(map[string]string{
		envListen: "127.0.0.1:1111", envSpawn: "2", envDataDir: "somewhere", envBinary: "/e/raftlogd",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.listen != "127.0.0.1:9999" || cfg.spawn != 2 || cfg.dataDir != "somewhere" || cfg.binary != "/e/raftlogd" {
		t.Fatalf("config = %+v", cfg)
	}
	cfg, err = parseConfig([]string{"-addr", "a:1"}, env(map[string]string{envAddr: "ignored:1"}), io.Discard)
	if err != nil || cfg.addrs[0] != "a:1" {
		t.Fatalf("flag over environment: %v %v", cfg.addrs, err)
	}
	if cfg, err = parseConfig(nil, env(map[string]string{envAddr: "env:1"}), io.Discard); err != nil || cfg.addrs[0] != "env:1" {
		t.Fatalf("environment alone: %v %v", cfg.addrs, err)
	}
}

func TestBadSettingsAreRefused(t *testing.T) {
	cases := map[string][]string{
		"a negative spawn":      {"-spawn", "-1"},
		"a listen with no port": {"-addr", "a:1", "-listen", "localhost"},
		"ports past the end":    {"-spawn", "5", "-base-port", "65534"},
		"metrics past the end":  {"-spawn", "3", "-metrics-base-port", "65535"},
		"a zero poll":           {"-addr", "a:1", "-poll", "0s"},
		"a zero verify wait":    {"-addr", "a:1", "-verify-wait", "0s"},
		"no workers":            {"-addr", "a:1", "-workers", "0"},
		"no partitions":         {"-addr", "a:1", "-partitions", "0"},
		"a stray argument":      {"-addr", "a:1", "extra"},
		"an unknown flag":       {"-nope"},
	}
	for name, args := range cases {
		if _, err := parseConfig(args, env(nil), io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseConfig(nil, env(map[string]string{envSpawn: "three"}), io.Discard); err == nil || !strings.Contains(err.Error(), envSpawn) {
		t.Errorf("a bad %s: %v", envSpawn, err)
	}
}

// --- the whole console, on real processes ----------------------------------------

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	return filepath.Join(runtime.GOROOT(), "bin", name)
}

// buildDaemon builds raftlogd into a temporary directory.
func buildDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "raftlogd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command(goTool(), "build", "-o", out, "github.com/nguyenhoangkhang-2010/raftlog/cmd/raftlogd")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building raftlogd: %v\n%s", err, b)
	}
	return out
}

// freePorts returns the first of n consecutive ports that are free. They are taken
// from below the range the operating system hands out for outgoing connections:
// in that range, the middle port is often in use as the source port of some
// connection and cannot be bound.
func freePorts(t *testing.T, n int) int {
	t.Helper()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for try := 0; try < 200; try++ {
		base := 20000 + rng.Intn(9000)
		var held []net.Listener
		ok := true
		for i := 0; i < n; i++ {
			h, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			held = append(held, h)
		}
		for _, h := range held {
			_ = h.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free consecutive ports")
	return 0
}

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

type apiState struct {
	Control bool `json:"control"`
	Summary struct {
		Up        int  `json:"up"`
		LeaderID  int  `json:"leader_id"`
		Term      int  `json:"term"`
		CanCommit bool `json:"can_commit"`
	} `json:"summary"`
	Load struct {
		Running bool `json:"running"`
		Acked   int  `json:"acked"`
		Failed  int  `json:"failed"`
	} `json:"load"`
	Topics []struct {
		Name     string `json:"name"`
		Messages int    `json:"messages"`
	} `json:"topics"`
}

func TestTheWholeConsoleOnRealNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts real processes")
	}
	bin := buildDaemon(t)
	base := freePorts(t, 3)
	dataDir := filepath.Join(t.TempDir(), "console")

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-spawn", "3", "-raftlogd", bin, "-data-dir", dataDir, "-base-port", fmt.Sprint(base),
			"-listen", "127.0.0.1:0", "-node-args", "-tick-interval 20ms -election-ticks 10 -heartbeat-ticks 2", "-poll", "100ms",
		}, env(nil), &out, &errOut)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the console did not stop")
		}
	}()

	re := regexp.MustCompile(`listening on (http://\S+),`)
	var url string
	for deadline := time.Now().Add(20 * time.Second); url == ""; {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			url = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("the console did not start; stderr: %s", errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	get := func() apiState {
		resp, err := http.Get(url + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st apiState
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	post := func(path, body string) []byte {
		req, _ := http.NewRequest("POST", url+path, strings.NewReader(body))
		req.Header.Set("X-Requested-With", "console")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, b)
		}
		return b
	}
	until := func(what string, cond func(apiState) bool) apiState {
		for deadline := time.Now().Add(40 * time.Second); ; {
			st := get()
			if cond(st) {
				return st
			}
			if time.Now().After(deadline) {
				for i := 1; i <= 3; i++ {
					log, _ := os.ReadFile(filepath.Join(dataDir, fmt.Sprintf("node%d.log", i)))
					t.Logf("node %d log:\n%s", i, log)
				}
				t.Fatalf("timed out waiting for: %s (state %+v)", what, st)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	st := until("a leader and three nodes", func(s apiState) bool { return s.Summary.CanCommit && s.Summary.Up == 3 })
	if !st.Control {
		t.Fatal("a console that runs its nodes must offer control")
	}

	post("/api/load", `{"rate":100}`)
	st = until("writes to be acknowledged", func(s apiState) bool { return s.Load.Acked >= 100 })
	old := st.Summary.LeaderID

	post(fmt.Sprintf("/api/nodes/%d/kill", old), "")
	st = until("a new leader", func(s apiState) bool {
		return s.Summary.CanCommit && s.Summary.LeaderID != old && s.Summary.LeaderID != 0 && s.Summary.Up == 2
	})
	acked := st.Load.Acked
	st = until("writes to carry on", func(s apiState) bool { return s.Load.Acked >= acked+100 })
	if st.Load.Failed != 0 {
		t.Fatalf("%d writes failed during the failover: the client should have waited out the election", st.Load.Failed)
	}

	post(fmt.Sprintf("/api/nodes/%d/start", old), "")
	until("the killed node to return", func(s apiState) bool { return s.Summary.Up == 3 && s.Summary.CanCommit })

	post("/api/load/stop", "")
	until("the writes to stop", func(s apiState) bool { return !s.Load.Running })
	time.Sleep(500 * time.Millisecond)

	var v struct {
		Acked   int `json:"acked"`
		Missing int `json:"missing"`
		Stored  int `json:"stored"`
	}
	if err := json.Unmarshal(post("/api/verify", ""), &v); err != nil {
		t.Fatal(err)
	}
	if v.Missing != 0 || v.Acked < 200 || v.Stored < v.Acked {
		t.Fatalf("verification = %+v", v)
	}

	// The topics panel sees what was written. It refreshes about once a second, and it
	// shows the furthest-ahead node, so it ends up at or above what was acknowledged.
	st = until("the topics panel to show every acknowledged message", func(s apiState) bool {
		return len(s.Topics) == 1 && s.Topics[0].Messages >= v.Acked
	})
	if st.Topics[0].Name != defaultTopic {
		t.Fatalf("topics = %+v", st.Topics)
	}
}
