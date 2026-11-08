package broker

import "sort"

// TopicDescription says how far each partition of a topic has got.
type TopicDescription struct {
	Name string
	// Ends[i] is the offset the next message of partition i will get, which is
	// also the number of messages the partition holds.
	Ends []uint64
}

// GroupPartition is the position of a consumer group in one partition.
type GroupPartition struct {
	Topic     string
	Partition int
	// Offset is the committed offset: the next one the group will read.
	Offset uint64
	// End is the offset the next message will get.
	End uint64
}

// Lag is how many messages the group has not read in this partition.
func (g GroupPartition) Lag() uint64 {
	return OffsetInfo{Offset: g.Offset, End: g.End}.Lag()
}

// GroupDescription is every partition one consumer group has committed an offset in.
type GroupDescription struct {
	Group      string
	Partitions []GroupPartition
}

// Describe lists the topics with the end offset of each partition, sorted by name.
func (b *Broker) Describe() []TopicDescription {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]TopicDescription, 0, len(b.topics))
	for _, t := range b.topics {
		d := TopicDescription{Name: t.name, Ends: make([]uint64, len(t.parts))}
		for i, p := range t.parts {
			d.Ends[i] = p.log.NextOffset()
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Groups lists the consumer groups that have committed an offset, with their
// position in every partition they have committed in, sorted by group, topic and
// partition. An offset left behind by a topic that no longer exists is not listed.
func (b *Broker) Groups() []GroupDescription {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.offsets == nil {
		return nil
	}
	byGroup := map[string][]GroupPartition{}
	for k, off := range b.offsets.all() {
		t, ok := b.topics[k.topic]
		if !ok || k.partition < 0 || k.partition >= len(t.parts) {
			continue
		}
		byGroup[k.group] = append(byGroup[k.group], GroupPartition{
			Topic: k.topic, Partition: k.partition, Offset: off, End: t.parts[k.partition].log.NextOffset(),
		})
	}
	out := make([]GroupDescription, 0, len(byGroup))
	for g, ps := range byGroup {
		sort.Slice(ps, func(i, j int) bool {
			if ps[i].Topic != ps[j].Topic {
				return ps[i].Topic < ps[j].Topic
			}
			return ps[i].Partition < ps[j].Partition
		})
		out = append(out, GroupDescription{Group: g, Partitions: ps})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}
