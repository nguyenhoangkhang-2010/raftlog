package config

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// env returns a getenv function backed by the given variables.
func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != defaultListenAddr || cfg.DataDir != defaultDataDir ||
		cfg.MaxPartitions != defaultMaxPartitions || cfg.MaxFetchMessages != defaultMaxFetch || cfg.MaxFetchWait != defaultMaxFetchWait || cfg.SyncOnAppend ||
		cfg.SegmentMaxBytes != 0 || cfg.IndexIntervalBytes != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestEnvOverridesDefaults(t *testing.T) {
	cfg, err := Load(nil, env(map[string]string{
		EnvListenAddr:         "127.0.0.1:7000",
		EnvDataDir:            "somewhere",
		EnvSegmentMaxBytes:    "1048576",
		EnvIndexIntervalBytes: "2048",
		EnvSyncOnAppend:       "true",
		EnvMaxPartitions:      "16",
		EnvMaxFetchMessages:   "50",
		EnvMaxFetchWait:       "2s",
	}), io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		ListenAddr:         "127.0.0.1:7000",
		DataDir:            "somewhere",
		SegmentMaxBytes:    1048576,
		IndexIntervalBytes: 2048,
		SyncOnAppend:       true,
		MaxPartitions:      16,
		MaxFetchMessages:   50,
		MaxFetchWait:       2 * time.Second,
	}
	// The Raft settings were not given, so they keep their defaults.
	want.ElectionTicks, want.HeartbeatTicks, want.TickInterval = defaultElectionTicks, defaultHeartbeatTicks, defaultTickInterval
	want.MaxMessageBytes = defaultMaxMessageBytes
	want.SnapshotEvery, want.SnapshotTrailing = defaultSnapshotEvery, defaultSnapshotTrailing
	want.Peers = map[uint64]string{}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	cfg, err := Load(
		[]string{"-listen", "127.0.0.1:8000", "-data-dir=flagdir", "-sync-on-append", "-max-partitions", "4"},
		env(map[string]string{EnvListenAddr: "127.0.0.1:7000", EnvDataDir: "envdir", EnvMaxPartitions: "16"}),
		io.Discard,
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:8000" || cfg.DataDir != "flagdir" || !cfg.SyncOnAppend || cfg.MaxPartitions != 4 {
		t.Fatalf("flags did not win: %+v", cfg)
	}
}

func TestInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string // substring of the error
	}{
		{"bad env int", nil, map[string]string{EnvSegmentMaxBytes: "lots"}, EnvSegmentMaxBytes},
		{"bad env bool", nil, map[string]string{EnvSyncOnAppend: "maybe"}, EnvSyncOnAppend},
		{"bad flag value", []string{"-max-partitions", "x"}, nil, "max-partitions"},
		{"unknown flag", []string{"-nope"}, nil, "nope"},
		{"stray argument", []string{"extra"}, nil, "extra"},
		{"listen without port", []string{"-listen", "localhost"}, nil, "listen address"},
		{"empty data dir", []string{"-data-dir", ""}, nil, "data dir"},
		{"negative segment size", []string{"-segment-max-bytes", "-1"}, nil, "segment max bytes"},
		{"negative index interval", []string{"-index-interval-bytes", "-1"}, nil, "index interval"},
		{"zero partitions", []string{"-max-partitions", "0"}, nil, "max partitions"},
		{"zero max fetch", []string{"-max-fetch-messages", "0"}, nil, "max fetch messages"},
		{"negative fetch wait", []string{"-max-fetch-wait=-1s"}, nil, "max fetch wait"},
		{"bad env fetch wait", nil, map[string]string{EnvMaxFetchWait: "soon"}, EnvMaxFetchWait},
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

func TestFetchWaitCanBeSwitchedOff(t *testing.T) {
	cfg, err := Load([]string{"-max-fetch-wait", "0"}, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxFetchWait != 0 {
		t.Fatalf("MaxFetchWait = %v, want 0 (a flag of 0 must not fall back to the default)", cfg.MaxFetchWait)
	}
}

func TestStorageOptions(t *testing.T) {
	cfg := Config{SegmentMaxBytes: 10, IndexIntervalBytes: 20, SyncOnAppend: true}
	o := cfg.StorageOptions()
	if o.SegmentMaxBytes != 10 || o.IndexIntervalBytes != 20 || !o.SyncOnAppend {
		t.Fatalf("StorageOptions = %+v", o)
	}
}
