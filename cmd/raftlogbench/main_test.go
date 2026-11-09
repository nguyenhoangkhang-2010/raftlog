package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestAClusterIsStartedByDefault(t *testing.T) {
	cfg, err := parseProduce(nil, env(map[string]string{envBinary: "/x/raftlogd"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.spawn != defaultSpawn || len(cfg.addrs) != defaultSpawn || cfg.addrs[0] != fmt.Sprintf("127.0.0.1:%d", defaultBasePort) {
		t.Fatalf("spawn %d addrs %v", cfg.spawn, cfg.addrs)
	}
	if cfg.produce.Topic == "" || !strings.HasPrefix(cfg.produce.Topic, "bench-") {
		t.Fatalf("topic %q: a run needs a topic of its own", cfg.produce.Topic)
	}
}

func TestMeasuringARunningClusterNeedsAddressesAndStartsNothing(t *testing.T) {
	cfg, err := parseProduce([]string{"-addr", "a:1, b:2 ,c:3"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.spawn != 0 || len(cfg.addrs) != 3 || cfg.addrs[1] != "b:2" {
		t.Fatalf("spawn %d addrs %v", cfg.spawn, cfg.addrs)
	}
	if _, err := parseProduce([]string{"-addr", "a:1", "-spawn", "3"}, env(nil), io.Discard); err == nil {
		t.Fatal("-addr and -spawn together were accepted")
	}
	if _, err := parseFailover([]string{"-addr", "a:1,b:2,c:3"}, env(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "needs -spawn") {
		t.Fatalf("a failover of nodes that cannot be killed: %v", err)
	}
}

func TestSettingsComeFromTheEnvironmentAndFlagsWin(t *testing.T) {
	cfg, err := parseProduce([]string{"-raftlogd", "/flag/raftlogd"}, env(map[string]string{envBinary: "/env/raftlogd", envDataDir: "/env/data"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.binary != "/flag/raftlogd" || cfg.dataDir != "/env/data" {
		t.Fatalf("binary %q data dir %q", cfg.binary, cfg.dataDir)
	}
}

func TestSyncOnAppendIsPassedToTheNodes(t *testing.T) {
	cfg, err := parseProduce([]string{"-sync-on-append", "-node-args", "-tick-interval 50ms"}, env(map[string]string{envBinary: "x"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.nodeArgs, " "); got != "-tick-interval 50ms -sync-on-append" {
		t.Fatalf("node arguments %q", got)
	}
}

func TestBadSettingsAreRefused(t *testing.T) {
	bad := [][]string{
		{"-producers", "0"}, {"-message-size", "0"}, {"-batch", "0"}, {"-duration", "0s"},
		{"-spawn", "-1"}, {"-fetch-limit", "0"}, {"unexpected"},
	}
	for _, args := range bad {
		if _, err := parseProduce(args, env(map[string]string{envBinary: "x"}), io.Discard); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if _, err := parseFailover([]string{"-rounds", "0"}, env(map[string]string{envBinary: "x"}), io.Discard); err == nil {
		t.Error("zero rounds accepted")
	}
	if err := run(context.Background(), []string{"nonsense"}, env(nil), io.Discard, io.Discard); err == nil {
		t.Error("an unknown command was accepted")
	}
}

// --- on real processes ------------------------------------------------------------

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

func buildDaemon(t *testing.T) string {
	t.Helper()
	name := "raftlogd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command(goTool(), "build", "-o", out, "github.com/nguyenhoangkhang-2010/raftlog/cmd/raftlogd")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building raftlogd: %v\n%s", err, b)
	}
	return out
}

// freePorts returns the first of n consecutive free ports, taken from below the range
// the operating system hands out to outgoing connections.
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

const fastRaft = "-tick-interval 50ms -election-ticks 20 -heartbeat-ticks 2"

func TestProduceMeasuresARealClusterAndReadsEverythingBack(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts real processes")
	}
	bin := buildDaemon(t)
	base := freePorts(t, 3)
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{
		"produce", "-raftlogd", bin, "-base-port", fmt.Sprint(base), "-node-args", fastRaft,
		"-producers", "4", "-partitions", "2", "-batch", "5", "-message-size", "64",
		"-warmup", "500ms", "-duration", "2s", "-json",
	}, env(nil), &out, &errOut)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	var rep produceReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("output is not the JSON report: %v\n%s", err, out.String())
	}
	if rep.Produce.Messages == 0 || rep.Produce.Errors != 0 || rep.Produce.MessagesPerSec <= 0 {
		t.Fatalf("write result %+v", rep.Produce)
	}
	if rep.Fetch == nil || rep.Fetch.Messages != rep.Stored || rep.Stored < rep.Produce.Messages {
		t.Fatalf("read back %+v of %d stored, %d acknowledged in the window", rep.Fetch, rep.Stored, rep.Produce.Messages)
	}
	if len(rep.Notes) != 0 {
		t.Fatalf("notes: %v", rep.Notes)
	}
	if rep.Environment.Nodes != 3 || !rep.Environment.Started {
		t.Fatalf("environment %+v", rep.Environment)
	}
}

func TestFailoverKillsTheLeaderOfARealClusterAndLosesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts real processes")
	}
	bin := buildDaemon(t)
	base := freePorts(t, 3)
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err := run(ctx, []string{
		"failover", "-raftlogd", bin, "-base-port", fmt.Sprint(base), "-node-args", fastRaft,
		"-rounds", "2", "-steady", "500ms",
	}, env(nil), &out, &errOut)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "acknowledged writes missing from the log: 0") {
		t.Fatalf("the report does not say that nothing was lost:\n%s", text)
	}
	if strings.Count(text, "\n    1 ")+strings.Count(text, "\n    2 ") != 2 {
		t.Fatalf("expected two rounds in the table:\n%s", text)
	}
}

func TestADataDirectoryWithOldDataIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{"produce", "-raftlogd", os.Args[0], "-data-dir", dir, "-base-port", fmt.Sprint(freePorts(t, 3))}, env(nil), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("old data in the directory: %v", err)
	}
}
