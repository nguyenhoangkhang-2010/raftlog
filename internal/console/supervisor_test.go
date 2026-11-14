package console

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "raftlogd")
	if err := os.WriteFile(p, []byte("not a program"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestASupervisorNeedsAWorkingConfiguration(t *testing.T) {
	bin, dir := fakeBinary(t), t.TempDir()
	nodes := []NodeSpec{{ID: 1, Addr: "127.0.0.1:1"}, {ID: 2, Addr: "127.0.0.1:2"}}
	cases := map[string]SupervisorConfig{
		"no binary":              {DataDir: dir, Nodes: nodes},
		"no data directory":      {Binary: bin, Nodes: nodes},
		"no nodes":               {Binary: bin, DataDir: dir},
		"a missing binary":       {Binary: filepath.Join(dir, "nowhere"), DataDir: dir, Nodes: nodes},
		"a duplicate ID":         {Binary: bin, DataDir: dir, Nodes: []NodeSpec{{ID: 1, Addr: "a"}, {ID: 1, Addr: "b"}}},
		"a zero ID":              {Binary: bin, DataDir: dir, Nodes: []NodeSpec{{ID: 0, Addr: "a"}}},
		"a node with no address": {Binary: bin, DataDir: dir, Nodes: []NodeSpec{{ID: 1}}},
	}
	for name, cfg := range cases {
		if _, err := NewSupervisor(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewSupervisor(SupervisorConfig{Binary: bin, DataDir: dir, Nodes: nodes}); err != nil {
		t.Fatalf("a good configuration: %v", err)
	}
}

func TestNothingIsRunningUntilItIsStartedAndUnknownNodesAreRefused(t *testing.T) {
	s, err := NewSupervisor(SupervisorConfig{Binary: fakeBinary(t), DataDir: t.TempDir(), Nodes: []NodeSpec{{ID: 1, Addr: "127.0.0.1:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Running(1) || s.Running(7) {
		t.Fatal("a node that was never started is running")
	}
	if err := s.Start(7); err == nil {
		t.Error("started a node that does not exist")
	}
	if err := s.Kill(7); err == nil {
		t.Error("killed a node that does not exist")
	}
	if err := s.Kill(1); err != nil {
		t.Errorf("killing a node that is not running: %v", err)
	}
	s.Close() // closing a supervisor that started nothing is harmless
}
