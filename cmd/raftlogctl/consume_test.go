package main

import (
	"fmt"
	"strings"
	"testing"
)

// rows splits command output into non-empty lines.
func rows(out string) []string {
	var r []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			r = append(r, l)
		}
	}
	return r
}

func TestConsumeContinuesWhereItStopped(t *testing.T) {
	addr := startBroker(t)
	if code, _, e := ctl(t, addr, "", "create-topic", "events"); code != 0 {
		t.Fatalf("create-topic: %q", e)
	}
	var in strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&in, "message-%d\n", i)
	}
	if code, _, e := ctl(t, addr, in.String(), "produce", "-lines", "events"); code != 0 {
		t.Fatalf("produce: %q", e)
	}

	// Before consuming anything the whole log is lag.
	_, out, _ := ctl(t, addr, "", "offsets", "-group", "g1", "events")
	if !strings.Contains(out, "committed=false offset=0 end=5 lag=5") {
		t.Fatalf("offsets before consume: %q", out)
	}

	// First batch: offsets 0..2.
	code, out, e := ctl(t, addr, "", "consume", "-group", "g1", "-max", "3", "events")
	if code != 0 || len(rows(out)) != 3 || !strings.Contains(rows(out)[0], `"message-0"`) {
		t.Fatalf("first consume: code %d, out %q, stderr %q", code, out, e)
	}
	_, out, _ = ctl(t, addr, "", "offsets", "-group", "g1", "events")
	if !strings.Contains(out, "committed=true offset=3 end=5 lag=2") {
		t.Fatalf("offsets after first consume: %q", out)
	}

	// Second run continues with offsets 3..4.
	code, out, _ = ctl(t, addr, "", "consume", "-group", "g1", "-max", "3", "events")
	r := rows(out)
	if code != 0 || len(r) != 2 || !strings.Contains(r[0], `"message-3"`) || !strings.Contains(r[1], `"message-4"`) {
		t.Fatalf("second consume: code %d, out %q", code, out)
	}

	// Nothing left.
	code, out, _ = ctl(t, addr, "", "consume", "-group", "g1", "events")
	if code != 0 || len(rows(out)) != 0 {
		t.Fatalf("third consume: code %d, out %q", code, out)
	}

	// Another group starts from the beginning.
	_, out, _ = ctl(t, addr, "", "consume", "-group", "g2", "-max", "1", "events")
	if len(rows(out)) != 1 || !strings.Contains(out, `"message-0"`) {
		t.Fatalf("other group: %q", out)
	}
}

func TestCommitCommandCanRewind(t *testing.T) {
	addr := startBroker(t)
	ctl(t, addr, "", "create-topic", "events")
	ctl(t, addr, "a\nb\nc\n", "produce", "-lines", "events")

	if code, _, e := ctl(t, addr, "", "commit", "-group", "g1", "-offset", "3", "events"); code != 0 {
		t.Fatalf("commit: %q", e)
	}
	// Rewind to 1 and consume again from there.
	if code, _, e := ctl(t, addr, "", "commit", "-group", "g1", "-offset", "1", "events"); code != 0 {
		t.Fatalf("rewind: %q", e)
	}
	_, out, _ := ctl(t, addr, "", "consume", "-group", "g1", "events")
	r := rows(out)
	if len(r) != 2 || !strings.Contains(r[0], `"b"`) {
		t.Fatalf("consume after rewind: %q", out)
	}

	// Committing beyond the end is rejected with a clear message.
	code, _, e := ctl(t, addr, "", "commit", "-group", "g1", "-offset", "99", "events")
	if code != 1 || !strings.Contains(e, "OutOfRange") {
		t.Fatalf("commit beyond end: code %d, stderr %q", code, e)
	}
}

func TestGroupCommandsRequireTheirFlags(t *testing.T) {
	addr := startBroker(t)
	cases := [][]string{
		{"commit", "-offset", "1", "t"},                // no -group
		{"commit", "-group", "g", "t"},                 // no -offset
		{"offsets", "t"},                               // no -group
		{"consume", "t"},                               // no -group
		{"consume", "-group", "g"},                     // no topic
		{"commit", "-group", "g", "-offset", "x", "t"}, // bad number
	}
	for _, args := range cases {
		if code, _, _ := ctl(t, addr, "", args...); code != 2 {
			t.Errorf("args %v: exit code %d, want 2", args, code)
		}
	}
}
