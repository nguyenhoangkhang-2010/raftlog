// Package chaos checks the behaviour of a raftlog cluster under injected faults.
//
// The tests in this directory run real raftlogd processes, kill and restart
// them, cut and slow the network between them while clients keep writing, and
// then compare what the clients were told with what the cluster holds. This
// file is the second half: given the history of the clients and the final state
// of the cluster, it reports every promise that was broken.
//
// Why not a general linearizability checker such as Porcupine? The cluster does
// not promise linearizable reads: a follower may lag, and a leader that was just
// deposed may still answer. What it does promise is checked here exactly:
//
//   - an acknowledged write is in the final log, at the offset it was told;
//   - no message is stored twice, and none appears that was never sent;
//   - a write that was refused outright is not in the log;
//   - appends are linearizable: if one append finished before another started,
//     it has the lower offset;
//   - a read never shows data that is not part of the final log (no dirty reads).
package chaos

import (
	"fmt"
	"sort"
	"time"
)

// Outcome is what a client knows about a write.
type Outcome int

const (
	// Acked: the cluster answered success and told the offset.
	Acked Outcome = iota
	// Rejected: the cluster refused the write before storing it (for example a
	// node that is not the leader). It must not be in the log.
	Rejected
	// Unknown: the client could not tell (a timeout, a connection that broke
	// after the request was sent). The message may be in the log, once, or not.
	Unknown
)

func (o Outcome) String() string {
	return [...]string{"acked", "rejected", "unknown"}[o]
}

// Write is one append attempt. Every attempt carries a value no other attempt
// has, so a value found in the log identifies the attempt that put it there.
type Write struct {
	Client    int
	Partition int
	Value     string
	Invoke    time.Duration // when the client sent the request, since the start of the run
	Return    time.Duration // when the client got the answer
	Outcome   Outcome
	Offset    uint64 // meaningful for Acked
}

// Read is what one Fetch returned: the values at Offset, Offset+1, ...
type Read struct {
	Node      int
	Partition int
	Offset    uint64
	Values    []string
}

// History is everything the clients saw.
type History struct {
	Writes []Write
	Reads  []Read
}

// Final is the content of the cluster once it has settled: for each partition,
// the values by offset.
type Final map[int][]string

// Violation is one broken promise.
type Violation struct {
	Kind   string
	Detail string
}

func (v Violation) String() string { return v.Kind + ": " + v.Detail }

// Violation kinds.
const (
	KindLostAck        = "lost-acknowledged-write"
	KindMoved          = "acknowledged-write-at-another-offset"
	KindDuplicate      = "duplicate-message"
	KindPhantom        = "message-nobody-sent"
	KindRejectedStored = "refused-write-was-stored"
	KindWrongPartition = "message-in-another-partition"
	KindRealTime       = "append-order-violates-real-time"
	KindDirtyRead      = "read-of-data-not-in-the-final-log"
	KindBadHistory     = "bad-history"
)

type position struct {
	partition int
	offset    uint64
}

// Check compares a history with the final state and returns every violation,
// in a stable order. An empty result means every checked promise held.
func Check(h History, final Final) []Violation {
	var out []Violation
	add := func(kind, format string, args ...any) {
		out = append(out, Violation{Kind: kind, Detail: fmt.Sprintf(format, args...)})
	}

	byValue := make(map[string]*Write, len(h.Writes))
	for i := range h.Writes {
		w := &h.Writes[i]
		if _, dup := byValue[w.Value]; dup {
			add(KindBadHistory, "value %q was used by two writes, so the check cannot tell them apart", w.Value)
			continue
		}
		byValue[w.Value] = w
	}

	// Where each value sits in the final log.
	where := make(map[string][]position)
	partitions := make([]int, 0, len(final))
	for p := range final {
		partitions = append(partitions, p)
	}
	sort.Ints(partitions)
	for _, p := range partitions {
		for off, v := range final[p] {
			where[v] = append(where[v], position{p, uint64(off)})
		}
	}

	values := make([]string, 0, len(where))
	for v := range where {
		values = append(values, v)
	}
	sort.Strings(values)
	for _, v := range values {
		pos := where[v]
		if len(pos) > 1 {
			add(KindDuplicate, "%q is stored %d times: %v", v, len(pos), pos)
		}
		w, known := byValue[v]
		switch {
		case !known:
			add(KindPhantom, "%q is in the log at %v but no client sent it", v, pos[0])
		case w.Outcome == Rejected:
			add(KindRejectedStored, "%q was refused to client %d (it never got an offset) but is stored at %v", v, w.Client, pos[0])
		case pos[0].partition != w.Partition:
			add(KindWrongPartition, "%q was sent to partition %d but is stored in partition %d", v, w.Partition, pos[0].partition)
		}
	}

	// Every acknowledged write is where it was promised.
	var acked []Write
	for _, w := range h.Writes {
		if w.Outcome != Acked {
			continue
		}
		acked = append(acked, w)
		pos, found := where[w.Value]
		switch {
		case !found:
			add(KindLostAck, "%q was acknowledged at offset %d of partition %d but is not in the final log", w.Value, w.Offset, w.Partition)
		case !contains(pos, position{w.Partition, w.Offset}):
			add(KindMoved, "%q was acknowledged at offset %d of partition %d but is stored at %v", w.Value, w.Offset, w.Partition, pos)
		}
	}

	out = append(out, checkRealTimeOrder(acked)...)

	for _, r := range h.Reads {
		fin := final[r.Partition]
		for i, v := range r.Values {
			off := r.Offset + uint64(i)
			if off >= uint64(len(fin)) {
				add(KindDirtyRead, "node %d returned %q at offset %d of partition %d, past the end of the final log (%d messages)", r.Node, v, off, r.Partition, len(fin))
				break
			}
			if fin[off] != v {
				add(KindDirtyRead, "node %d returned %q at offset %d of partition %d, but the final log has %q there", r.Node, v, off, r.Partition, fin[off])
				break
			}
		}
	}
	return out
}

func contains(ps []position, p position) bool {
	for _, q := range ps {
		if q == p {
			return true
		}
	}
	return false
}

// checkRealTimeOrder verifies that appends are linearizable: if append A got its
// answer before append B was sent, A's offset is lower than B's. Offsets are the
// order in which the cluster committed the appends, so a violation would mean it
// committed them in an order that contradicts what the clients observed.
func checkRealTimeOrder(acked []Write) []Violation {
	byPartition := make(map[int][]Write)
	for _, w := range acked {
		byPartition[w.Partition] = append(byPartition[w.Partition], w)
	}
	partitions := make([]int, 0, len(byPartition))
	for p := range byPartition {
		partitions = append(partitions, p)
	}
	sort.Ints(partitions)

	var out []Violation
	for _, p := range partitions {
		ws := byPartition[p]
		byInvoke := append([]Write(nil), ws...)
		byReturn := append([]Write(nil), ws...)
		sort.Slice(byInvoke, func(i, j int) bool { return byInvoke[i].Invoke < byInvoke[j].Invoke })
		sort.Slice(byReturn, func(i, j int) bool { return byReturn[i].Return < byReturn[j].Return })

		// Sweep through the writes in the order they were sent, keeping the write
		// with the highest offset among those that had already finished.
		j := 0
		var latest Write
		have := false
		for _, b := range byInvoke {
			for j < len(byReturn) && byReturn[j].Return < b.Invoke {
				if !have || byReturn[j].Offset > latest.Offset {
					latest, have = byReturn[j], true
				}
				j++
			}
			if have && latest.Offset >= b.Offset {
				out = append(out, Violation{Kind: KindRealTime, Detail: fmt.Sprintf(
					"partition %d: %q (offset %d) finished at %v, before %q (offset %d) was sent at %v",
					p, latest.Value, latest.Offset, latest.Return, b.Value, b.Offset, b.Invoke)})
			}
		}
	}
	return out
}
