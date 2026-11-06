package config

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParsePeers(t *testing.T) {
	got, err := ParsePeers(" 1=10.0.0.1:9092 , 2=node2:9092,3=[::1]:9092 ")
	if err != nil {
		t.Fatalf("ParsePeers: %v", err)
	}
	want := map[uint64]string{1: "10.0.0.1:9092", 2: "node2:9092", 3: "[::1]:9092"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if empty, err := ParsePeers("  "); err != nil || len(empty) != 0 {
		t.Fatalf("empty list: %v, %v", empty, err)
	}
}

func TestParsePeersRejectsBadInput(t *testing.T) {
	for _, bad := range []string{
		"1",              // no address
		"=host:1",        // no id
		"x=host:1",       // id not a number
		"0=host:1",       // id zero is reserved
		"1=host",         // no port
		"1=a:1,1=b:2",    // duplicate id
		"1=a:1,,2=b:2",   // empty item
		"-3=host:1",      // negative id
		"1=a:1;2=b:2",    // wrong separator (parsed as one bad address)
		"1=http://a:1/x", // url instead of host:port
	} {
		if _, err := ParsePeers(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestFormatPeersRoundTrip(t *testing.T) {
	in := map[uint64]string{3: "c:3", 1: "a:1", 2: "b:2"}
	s := FormatPeers(in)
	if s != "1=a:1,2=b:2,3=c:3" {
		t.Fatalf("FormatPeers = %q", s)
	}
	out, err := ParsePeers(s)
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: %v, %v", out, err)
	}
}

func TestStandaloneIsTheDefault(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeID != 0 || len(cfg.Peers) != 0 {
		t.Fatalf("raft must be off by default: %+v", cfg)
	}
}

func TestRaftSettingsFromEnvAndFlags(t *testing.T) {
	cfg, err := Load(
		[]string{"-election-ticks", "30"},
		env(map[string]string{
			EnvNodeID:         "2",
			EnvPeers:          "1=a:9092,2=b:9092,3=c:9092",
			EnvHeartbeatTicks: "2",
			EnvTickInterval:   "50ms",
			EnvElectionTicks:  "20", // overridden by the flag
		}),
		io.Discard,
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeID != 2 || len(cfg.Peers) != 3 || cfg.Peers[3] != "c:9092" ||
		cfg.HeartbeatTicks != 2 || cfg.ElectionTicks != 30 || cfg.TickInterval != 50*time.Millisecond {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	// Flags work on their own too.
	cfg, err = Load([]string{"-node-id", "1", "-peers", "1=a:9092", "-tick-interval", "1s"}, env(nil), io.Discard)
	if err != nil || cfg.NodeID != 1 || cfg.TickInterval != time.Second {
		t.Fatalf("flags: %+v, %v", cfg, err)
	}
}

func TestSnapshotSettings(t *testing.T) {
	cfg, err := Load(
		[]string{"-snapshot-trailing", "7"},
		env(map[string]string{EnvSnapshotEvery: "250", EnvSnapshotTrailing: "99"}),
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SnapshotEvery != 250 || cfg.SnapshotTrailing != 7 {
		t.Fatalf("every %d, trailing %d; want 250 from the environment and 7 from the flag", cfg.SnapshotEvery, cfg.SnapshotTrailing)
	}
	// Zero means "never", and is allowed.
	cfg, err = Load([]string{"-snapshot-every", "0"}, env(nil), io.Discard)
	if err != nil || cfg.SnapshotEvery != 0 {
		t.Fatalf("snapshot-every 0: %+v, %v", cfg, err)
	}
	for _, bad := range []map[string]string{{EnvSnapshotEvery: "-1"}, {EnvSnapshotTrailing: "many"}} {
		if _, err := Load(nil, env(bad), io.Discard); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

func TestInvalidRaftSettings(t *testing.T) {
	good := []string{"-node-id", "1", "-peers", "1=a:9092,2=b:9092"}
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"peers without node id", []string{"-peers", "1=a:9092"}, nil, "node ID is 0"},
		{"node not among peers", []string{"-node-id", "9", "-peers", "1=a:9092"}, nil, "include this node"},
		{"node id without peers", []string{"-node-id", "1"}, nil, "include this node"},
		{"bad peers", []string{"-node-id", "1", "-peers", "1=nohost"}, nil, "peers"},
		{"election not above heartbeat", append(append([]string{}, good...), "-election-ticks", "1", "-heartbeat-ticks", "1"), nil, "election ticks"},
		{"zero heartbeat", append(append([]string{}, good...), "-heartbeat-ticks", "0"), nil, "heartbeat ticks"},
		{"zero tick interval", append(append([]string{}, good...), "-tick-interval", "0s"), nil, "tick interval"},
		{"zero max message bytes", append(append([]string{}, good...), "-max-message-bytes", "0"), nil, "max message bytes"},
		{"bad max message bytes env", nil, map[string]string{EnvMaxMessageBytes: "big"}, EnvMaxMessageBytes},
		{"bad node id env", nil, map[string]string{EnvNodeID: "abc"}, EnvNodeID},
		{"bad tick env", nil, map[string]string{EnvTickInterval: "soon"}, EnvTickInterval},
		{"bad ticks env", nil, map[string]string{EnvElectionTicks: "x", EnvHeartbeatTicks: "y"}, EnvElectionTicks},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(c.args, env(c.env), io.Discard)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}
