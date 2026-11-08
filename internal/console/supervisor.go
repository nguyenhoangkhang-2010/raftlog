// Package console is the cluster console: a small web application that shows the
// state of a raftlog cluster, can stop and start its nodes, writes to it under load
// and checks afterwards that nothing it was told was stored has gone missing.
//
// It exists to make failover visible. Everything it shows is read from the nodes
// themselves, through the same gRPC API a client uses; the only thing it owns is
// the node processes, when it was asked to run them.
package console

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// NodeSpec names a node of the cluster and where clients reach it.
type NodeSpec struct {
	ID   int
	Addr string
	// MetricsAddr is where the node serves /metrics; empty if it does not.
	MetricsAddr string
}

// SupervisorConfig describes the processes to run.
type SupervisorConfig struct {
	// Binary is the raftlogd executable.
	Binary string
	// DataDir holds one directory and one log file for each node.
	DataDir string
	Nodes   []NodeSpec
	// ExtraArgs are passed to every node, for instance Raft timings.
	ExtraArgs []string
}

// Supervisor runs the nodes of a cluster as child processes. Stopping one is a
// kill, with no chance to clean up, which is what a crash looks like to the others.
type Supervisor struct {
	cfg   SupervisorConfig
	peers string

	mu    sync.Mutex
	procs map[int]*proc
}

type proc struct {
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
}

// NewSupervisor checks the configuration. It starts nothing.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.Binary == "" || cfg.DataDir == "" || len(cfg.Nodes) == 0 {
		return nil, errors.New("console: a supervisor needs a binary, a data directory and nodes")
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, fmt.Errorf("console: the raftlogd binary: %w", err)
	}
	seen := map[int]bool{}
	var peers []string
	for _, n := range cfg.Nodes {
		if n.ID < 1 || n.Addr == "" || seen[n.ID] {
			return nil, fmt.Errorf("console: node %d needs a unique positive ID and an address", n.ID)
		}
		seen[n.ID] = true
		peers = append(peers, fmt.Sprintf("%d=%s", n.ID, n.Addr))
	}
	return &Supervisor{cfg: cfg, peers: strings.Join(peers, ","), procs: map[int]*proc{}}, nil
}

func (s *Supervisor) spec(id int) (NodeSpec, bool) {
	for _, n := range s.cfg.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return NodeSpec{}, false
}

// Running reports whether the node's process is alive.
func (s *Supervisor) Running(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[id]
	if !ok {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Start launches the node's process. It keeps its data directory, so a node that
// was killed comes back with what it had. Starting a node that runs does nothing.
func (s *Supervisor) Start(id int) error {
	n, ok := s.spec(id)
	if !ok {
		return fmt.Errorf("console: there is no node %d", id)
	}
	if s.Running(id) {
		return nil
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(s.cfg.DataDir, fmt.Sprintf("node%d.log", id)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	args := []string{
		"-node-id", fmt.Sprint(id), "-peers", s.peers, "-listen", n.Addr,
		"-data-dir", filepath.Join(s.cfg.DataDir, fmt.Sprintf("node%d", id)),
	}
	if n.MetricsAddr != "" {
		args = append(args, "-metrics-listen", n.MetricsAddr)
	}
	args = append(args, s.cfg.ExtraArgs...)
	cmd := exec.Command(s.cfg.Binary, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("console: start node %d: %w", id, err)
	}
	p := &proc{cmd: cmd, log: logFile, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
		close(p.done)
	}()
	s.mu.Lock()
	s.procs[id] = p
	s.mu.Unlock()
	return nil
}

// Kill stops the node's process at once and waits for it to be gone. Killing a node
// that is not running does nothing.
func (s *Supervisor) Kill(id int) error {
	if _, ok := s.spec(id); !ok {
		return fmt.Errorf("console: there is no node %d", id)
	}
	s.mu.Lock()
	p := s.procs[id]
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.done
	return nil
}

// StartAll starts every node.
func (s *Supervisor) StartAll() error {
	for _, n := range s.cfg.Nodes {
		if err := s.Start(n.ID); err != nil {
			return err
		}
	}
	return nil
}

// Close kills every node.
func (s *Supervisor) Close() {
	for _, n := range s.cfg.Nodes {
		_ = s.Kill(n.ID)
	}
}

// LogPath is where the node's output goes.
func (s *Supervisor) LogPath(id int) string {
	return filepath.Join(s.cfg.DataDir, fmt.Sprintf("node%d.log", id))
}
