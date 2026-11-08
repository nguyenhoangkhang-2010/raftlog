package console

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// MultiLister asks every node and shows the most up-to-date answer. A node that was
// just restarted, or is cut off, can be behind the others and report fewer
// messages; what the cluster holds is what the furthest-ahead node shows, because a
// committed message is never taken back.
type MultiLister struct {
	nodes []Lister
}

// NewMultiLister returns a lister over one lister per node.
func NewMultiLister(nodes ...Lister) *MultiLister { return &MultiLister{nodes: nodes} }

// Topics asks all nodes at once and merges the answers: for every topic, the
// highest end offset seen for each partition. It fails only if no node answers.
func (m *MultiLister) Topics(ctx context.Context) ([]client.TopicInfo, error) {
	type result struct {
		topics []client.TopicInfo
		err    error
	}
	results := make([]result, len(m.nodes))
	var wg sync.WaitGroup
	for i, n := range m.nodes {
		wg.Add(1)
		go func(i int, n Lister) {
			defer wg.Done()
			results[i].topics, results[i].err = n.Topics(ctx)
		}(i, n)
	}
	wg.Wait()

	merged := map[string][]uint64{}
	answered := 0
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		answered++
		for _, t := range r.topics {
			cur := merged[t.Name]
			for len(cur) < len(t.Ends) {
				cur = append(cur, 0)
			}
			for p, end := range t.Ends {
				if end > cur[p] {
					cur[p] = end
				}
			}
			merged[t.Name] = cur
		}
	}
	if answered == 0 {
		if firstErr == nil {
			firstErr = errors.New("console: there are no nodes to ask")
		}
		return nil, firstErr
	}
	out := make([]client.TopicInfo, 0, len(merged))
	for name, ends := range merged {
		out = append(out, client.TopicInfo{Name: name, Ends: ends})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Groups asks all nodes at once. For every partition a group has committed in, the
// answer of the node that has the most messages in it wins, and the highest
// committed offset breaks a tie.
func (m *MultiLister) Groups(ctx context.Context) ([]client.GroupInfo, error) {
	type result struct {
		groups []client.GroupInfo
		err    error
	}
	results := make([]result, len(m.nodes))
	var wg sync.WaitGroup
	for i, n := range m.nodes {
		wg.Add(1)
		go func(i int, n Lister) {
			defer wg.Done()
			results[i].groups, results[i].err = n.Groups(ctx)
		}(i, n)
	}
	wg.Wait()

	type key struct {
		group, topic string
		partition    int
	}
	best := map[key]client.GroupPartition{}
	groups := map[string]bool{}
	answered := 0
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		answered++
		for _, g := range r.groups {
			groups[g.Name] = true
			for _, p := range g.Partitions {
				k := key{g.Name, p.Topic, p.Partition}
				if cur, ok := best[k]; !ok || p.End > cur.End || (p.End == cur.End && p.Offset > cur.Offset) {
					best[k] = p
				}
			}
		}
	}
	if answered == 0 {
		if firstErr == nil {
			firstErr = errors.New("console: there are no nodes to ask")
		}
		return nil, firstErr
	}
	byName := map[string]*client.GroupInfo{}
	for k, p := range best {
		g := byName[k.group]
		if g == nil {
			g = &client.GroupInfo{Name: k.group}
			byName[k.group] = g
		}
		g.Partitions = append(g.Partitions, p)
	}
	out := make([]client.GroupInfo, 0, len(byName))
	for _, g := range byName {
		sort.Slice(g.Partitions, func(i, j int) bool {
			if g.Partitions[i].Topic != g.Partitions[j].Topic {
				return g.Partitions[i].Topic < g.Partitions[j].Topic
			}
			return g.Partitions[i].Partition < g.Partitions[j].Partition
		})
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
