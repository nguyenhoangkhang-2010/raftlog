package chaos

import (
	"strings"
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func acked(client, partition int, value string, offset uint64, invoke, ret int) Write {
	return Write{Client: client, Partition: partition, Value: value, Invoke: ms(invoke), Return: ms(ret), Outcome: Acked, Offset: offset}
}

func other(outcome Outcome, partition int, value string, invoke, ret int) Write {
	return Write{Partition: partition, Value: value, Invoke: ms(invoke), Return: ms(ret), Outcome: outcome}
}

func kinds(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Kind)
	}
	return out
}

// A history in which every promise holds.
func goodHistory() (History, Final) {
	h := History{
		Writes: []Write{
			acked(1, 0, "a", 0, 0, 10),
			acked(2, 0, "b", 1, 5, 20),  // overlaps a: any order is allowed
			acked(1, 0, "c", 2, 30, 40), // after both
			acked(1, 1, "x", 0, 0, 10),  // another partition has its own offsets
			other(Rejected, 0, "r", 12, 13),
			other(Unknown, 0, "u-stored", 15, 100),
			other(Unknown, 0, "u-lost", 16, 100),
		},
		Reads: []Read{
			{Node: 2, Partition: 0, Offset: 0, Values: []string{"a", "b"}},
			{Node: 3, Partition: 0, Offset: 1, Values: []string{"b", "c"}},
			{Node: 1, Partition: 1, Offset: 0, Values: nil},
		},
	}
	// "u-stored" (unknown outcome) did make it into the log, "u-lost" did not.
	final := Final{0: {"a", "b", "c", "u-stored"}, 1: {"x"}}
	return h, final
}

func TestAHealthyHistoryHasNoViolations(t *testing.T) {
	h, final := goodHistory()
	if vs := Check(h, final); len(vs) != 0 {
		t.Fatalf("violations in a healthy history: %v", vs)
	}
}

func TestTheCheckerNoticesEachKindOfBrokenPromise(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(h *History, f Final)
		want   string
	}{
		{"an acknowledged write disappeared", func(_ *History, f Final) {
			f[0] = []string{"a", "c", "u-stored"} // "b" is gone (and "c" moved)
		}, KindLostAck},
		{"an acknowledged write ended at another offset", func(_ *History, f Final) {
			f[0] = []string{"b", "a", "c", "u-stored"}
		}, KindMoved},
		{"a message is stored twice", func(_ *History, f Final) {
			f[0] = []string{"a", "b", "c", "u-stored", "u-stored"}
		}, KindDuplicate},
		{"a message nobody sent appears", func(_ *History, f Final) {
			f[0] = []string{"a", "b", "c", "u-stored", "ghost"}
		}, KindPhantom},
		{"a refused write was stored", func(_ *History, f Final) {
			f[0] = []string{"a", "b", "c", "u-stored", "r"}
		}, KindRejectedStored},
		{"a message landed in another partition", func(_ *History, f Final) {
			f[1] = []string{"x", "u-lost"}
		}, KindWrongPartition},
		{"an append finished before another started but got a higher offset", func(h *History, f Final) {
			// c was sent after a and b had finished, yet sits before them.
			f[0] = []string{"c", "a", "b", "u-stored"}
			h.Writes[0].Offset, h.Writes[1].Offset, h.Writes[2].Offset = 1, 2, 0
		}, KindRealTime},
		{"a read showed data that is not in the log", func(h *History, _ Final) {
			h.Reads = append(h.Reads, Read{Node: 3, Partition: 0, Offset: 3, Values: []string{"rolled-back"}})
		}, KindDirtyRead},
		{"a read showed a value at the wrong offset", func(h *History, _ Final) {
			h.Reads = append(h.Reads, Read{Node: 2, Partition: 0, Offset: 0, Values: []string{"b"}})
		}, KindDirtyRead},
		{"a read went past the end of the log", func(h *History, _ Final) {
			h.Reads = append(h.Reads, Read{Node: 2, Partition: 1, Offset: 0, Values: []string{"x", "extra"}})
		}, KindDirtyRead},
		{"two writes used the same value", func(h *History, _ Final) {
			h.Writes = append(h.Writes, other(Unknown, 0, "a", 50, 60))
		}, KindBadHistory},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, final := goodHistory()
			c.mutate(&h, final)
			vs := Check(h, final)
			for _, v := range vs {
				if v.Kind == c.want {
					return
				}
			}
			t.Fatalf("no %q violation reported; got %v", c.want, kinds(vs))
		})
	}
}

func TestWritesThatOverlapInTimeMayCommitInEitherOrder(t *testing.T) {
	// Neither finished before the other started, so both orders are fine.
	for _, offsets := range [][2]uint64{{0, 1}, {1, 0}} {
		h := History{Writes: []Write{
			acked(1, 0, "a", offsets[0], 0, 50),
			acked(2, 0, "b", offsets[1], 10, 60),
		}}
		final := Final{0: make([]string, 2)}
		final[0][offsets[0]], final[0][offsets[1]] = "a", "b"
		if vs := Check(h, final); len(vs) != 0 {
			t.Errorf("offsets %v: %v", offsets, vs)
		}
	}
}

func TestOnlyAckedWritesTakePartInTheRealTimeOrder(t *testing.T) {
	// A write whose outcome is unknown has no reliable "finished" time, so it
	// cannot contradict anything even if it sits at a lower offset.
	h := History{Writes: []Write{
		acked(1, 0, "a", 1, 0, 10),
		other(Unknown, 0, "u", 20, 30),
	}}
	final := Final{0: {"u", "a"}}
	if vs := Check(h, final); len(vs) != 0 {
		t.Fatalf("violations: %v", vs)
	}
}

func TestViolationsAreReportedInAStableOrderAndMentionTheValue(t *testing.T) {
	h, final := goodHistory()
	final[0] = []string{"a", "c", "u-stored", "z-ghost", "y-ghost"}
	first := Check(h, final)
	second := Check(h, final)
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("violations: %v / %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("the order is not stable: %v vs %v", first[i], second[i])
		}
	}
	var text []string
	for _, v := range first {
		text = append(text, v.String())
	}
	if !strings.Contains(strings.Join(text, "\n"), `"y-ghost"`) {
		t.Fatalf("the report does not name the offending value:\n%s", strings.Join(text, "\n"))
	}
}

func TestAnEmptyRunIsNotAViolation(t *testing.T) {
	if vs := Check(History{}, Final{}); len(vs) != 0 {
		t.Fatalf("violations: %v", vs)
	}
}
