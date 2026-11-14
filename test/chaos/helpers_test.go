package chaos

import (
	"context"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// leaderOf returns the id of the node that currently says it is the leader, or 0.
func (c *cluster) leaderOf() int {
	for id := 1; id <= c.n; id++ {
		if !c.running(id) {
			continue
		}
		cl, closeConn := dial(c.t, c.node(id).addr)
		st := nodeStatus(cl)
		closeConn()
		if st != nil && st.Role == "leader" {
			return id
		}
	}
	return 0
}

// readAll reads a whole one-partition topic through the cluster client.
func readAll(t *testing.T, cl *client.Client, topic string) []client.Record {
	t.Helper()
	var out []client.Record
	for offset := uint64(0); ; {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		recs, err := cl.Fetch(ctx, topic, 0, offset, 500)
		cancel()
		if err != nil {
			t.Fatalf("read %s from %d: %v", topic, offset, err)
		}
		if len(recs) == 0 {
			return out
		}
		out = append(out, recs...)
		offset = recs[len(recs)-1].Offset + 1
	}
}
