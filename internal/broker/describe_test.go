package broker

import (
	"reflect"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/storage"
)

func TestDescribeListsTopicsWithTheEndOfEachPartition(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if got := b.Describe(); len(got) != 0 {
		t.Fatalf("an empty broker describes %v", got)
	}
	if err := b.CreateTopic("zeta", 1); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("alpha", 3); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := b.Produce("alpha", 2, []storage.Message{msg("k", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := b.Produce("alpha", 0, []storage.Message{msg("k", "v"), msg("k", "w")}); err != nil {
		t.Fatal(err)
	}

	want := []TopicDescription{{Name: "alpha", Ends: []uint64{2, 0, 5}}, {Name: "zeta", Ends: []uint64{0}}}
	if got := b.Describe(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Describe = %v, want %v", got, want)
	}
}

func TestGroupsListsEveryCommittedPositionAndItsLag(t *testing.T) {
	b := openBroker(t, t.TempDir())
	if err := b.CreateTopic("events", 2); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateTopic("audit", 1); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{0, 0, 0, 1} {
		if _, _, err := b.Produce("events", p, []storage.Message{msg("k", "v")}); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.Groups(); len(got) != 0 {
		t.Fatalf("no group has committed, Groups = %v", got)
	}

	for _, c := range []struct {
		group, topic string
		partition    int
		offset       uint64
	}{{"billing", "events", 1, 1}, {"billing", "events", 0, 1}, {"analytics", "events", 0, 3}, {"billing", "audit", 0, 0}} {
		if err := b.CommitOffset(c.group, c.topic, c.partition, c.offset); err != nil {
			t.Fatal(err)
		}
	}
	got := b.Groups()
	want := []GroupDescription{
		{Group: "analytics", Partitions: []GroupPartition{{Topic: "events", Partition: 0, Offset: 3, End: 3}}},
		{Group: "billing", Partitions: []GroupPartition{
			{Topic: "audit", Partition: 0, Offset: 0, End: 0},
			{Topic: "events", Partition: 0, Offset: 1, End: 3},
			{Topic: "events", Partition: 1, Offset: 1, End: 1},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Groups =\n%v\nwant\n%v", got, want)
	}
	if lag := got[1].Partitions[1].Lag(); lag != 2 {
		t.Fatalf("lag of billing in events/0 = %d, want 2", lag)
	}
	if lag := got[0].Partitions[0].Lag(); lag != 0 {
		t.Fatalf("lag of analytics = %d, want 0", lag)
	}
}
