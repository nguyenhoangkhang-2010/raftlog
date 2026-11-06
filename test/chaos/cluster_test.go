package chaos

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/nguyenhoangkhang-2010/raftlog/api/raftlog/v1"
)

// The daemon is built once for all tests and run as real operating system
// processes, so that a "crash" is a process killed in the middle of whatever it
// was doing, not a polite shutdown.
var (
	buildOnce  sync.Once
	binaryPath string
	binaryErr  error
	binaryDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binaryDir != "" {
		_ = os.RemoveAll(binaryDir)
	}
	os.Exit(code)
}

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

// daemon returns the path of the built raftlogd.
func daemon(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "raftlog-chaos-bin")
		if err != nil {
			binaryErr = err
			return
		}
		binaryDir = dir
		name := "raftlogd"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binaryPath = filepath.Join(dir, name)
		cmd := exec.Command(goTool(), "build", "-o", binaryPath, "../../cmd/raftlogd")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("build raftlogd: %v\n%s", err, out)
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

type node struct {
	id      int
	addr    string // the node's own address, which clients use
	dir     string
	logPath string

	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
}

// cluster is a set of raftlogd processes. Every node reaches every other node
// through a proxy of its own, so the network between any two nodes can be cut
// or slowed in each direction separately.
type cluster struct {
	t     *testing.T
	n     int
	bin   string
	extra []string
	nodes []*node
	links map[[2]int]*link // [from, to]

	mu sync.Mutex
}

// newCluster prepares n nodes, with their directories and proxies, but starts none.
func newCluster(t *testing.T, n int, extra ...string) *cluster {
	t.Helper()
	c := &cluster{t: t, n: n, bin: daemon(t), extra: extra, links: make(map[[2]int]*link)}
	root := t.TempDir()
	for id := 1; id <= n; id++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close() // free again; the daemon binds it when it starts
		c.nodes = append(c.nodes, &node{
			id: id, addr: addr,
			dir:     filepath.Join(root, fmt.Sprintf("node%d", id)),
			logPath: filepath.Join(root, fmt.Sprintf("node%d.log", id)),
		})
	}
	for from := 1; from <= n; from++ {
		for to := 1; to <= n; to++ {
			if from == to {
				continue
			}
			l, err := newLink(c.node(to).addr)
			if err != nil {
				t.Fatal(err)
			}
			c.links[[2]int{from, to}] = l
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			c.dumpLogs()
		}
		c.shutdown()
	})
	return c
}

func (c *cluster) node(id int) *node { return c.nodes[id-1] }

// peersFor is the -peers value of node j: itself by its own address, every other
// node through the proxy that carries j's traffic to it.
func (c *cluster) peersFor(j int) string {
	parts := make([]string, 0, c.n)
	for i := 1; i <= c.n; i++ {
		addr := c.node(i).addr
		if i != j {
			addr = c.links[[2]int{j, i}].addr()
		}
		parts = append(parts, fmt.Sprintf("%d=%s", i, addr))
	}
	return strings.Join(parts, ",")
}

func (c *cluster) running(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.node(id).cmd != nil
}

// mustStart is start for the goroutine of the test.
func (c *cluster) mustStart(id int) {
	c.t.Helper()
	if err := c.start(id); err != nil {
		c.t.Fatal(err)
	}
}

// start launches the process of a node (it keeps its data directory). It is
// safe to call from any goroutine.
func (c *cluster) start(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	nd := c.node(id)
	if nd.cmd != nil {
		return nil
	}
	logFile, err := os.OpenFile(nd.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	args := append([]string{
		"-node-id", fmt.Sprint(id), "-peers", c.peersFor(id),
		"-listen", nd.addr, "-data-dir", nd.dir,
		"-tick-interval", "10ms", "-election-ticks", "10", "-heartbeat-ticks", "2",
	}, c.extra...)
	cmd := exec.Command(c.bin, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start node %d: %w", id, err)
	}
	nd.cmd, nd.log, nd.done = cmd, logFile, make(chan struct{})
	go func(cmd *exec.Cmd, done chan struct{}) {
		_ = cmd.Wait()
		close(done)
	}(cmd, nd.done)
	return nil
}

// kill stops a node's process at once, without any chance to clean up.
func (c *cluster) kill(id int) {
	c.mu.Lock()
	nd := c.node(id)
	cmd, done, logFile := nd.cmd, nd.done, nd.log
	nd.cmd, nd.done, nd.log = nil, nil, nil
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Process.Kill()
	<-done
	_ = logFile.Close()
}

func (c *cluster) shutdown() {
	for id := 1; id <= c.n; id++ {
		c.kill(id)
	}
	for _, l := range c.links {
		l.close()
	}
}

// cutDirected drops everything node from sends to node to.
func (c *cluster) cutDirected(from, to int) { c.links[[2]int{from, to}].setDown(true) }

// cut isolates a pair of nodes from each other.
func (c *cluster) cut(a, b int) {
	c.cutDirected(a, b)
	c.cutDirected(b, a)
}

// isolate cuts a node off from all the others (it keeps running).
func (c *cluster) isolate(id int) {
	for other := 1; other <= c.n; other++ {
		if other != id {
			c.cut(id, other)
		}
	}
}

// heal restores every link and removes every delay.
func (c *cluster) heal() {
	for _, l := range c.links {
		l.setDown(false)
		l.setDelay(0)
	}
}

// setDelay slows the traffic between a pair of nodes, in both directions.
func (c *cluster) setDelay(a, b int, d time.Duration) {
	c.links[[2]int{a, b}].setDelay(d)
	c.links[[2]int{b, a}].setDelay(d)
}

// dumpLogs prints the end of each node's log, to explain a failed test.
func (c *cluster) dumpLogs() {
	for _, nd := range c.nodes {
		data, err := os.ReadFile(nd.logPath)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) > 25 {
			lines = lines[len(lines)-25:]
		}
		c.t.Logf("---- node %d log (last %d lines)\n%s", nd.id, len(lines), strings.Join(lines, "\n"))
	}
}

// --- talking to the nodes -------------------------------------------------------

// dial returns a client for a node's real address. Reconnecting is made quick, or
// a client would keep a restarted node out for seconds.
func dial(t *testing.T, addr string) (pb.BrokerClient, func()) {
	t.Helper()
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 20 * time.Millisecond, Multiplier: 1.3, Jitter: 0.1, MaxDelay: 200 * time.Millisecond},
			MinConnectTimeout: time.Second,
		}))
	if err != nil {
		t.Fatal(err)
	}
	return pb.NewBrokerClient(conn), func() { _ = conn.Close() }
}

func shortCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// nodeStatus asks a node for its Raft status; nil if it does not answer.
func nodeStatus(cl pb.BrokerClient) *pb.NodeStatusResponse {
	ctx, cancel := shortCtx(500 * time.Millisecond)
	defer cancel()
	resp, err := cl.NodeStatus(ctx, &pb.NodeStatusRequest{})
	if err != nil {
		return nil
	}
	return resp
}
