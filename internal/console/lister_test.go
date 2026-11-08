package console

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

func listerWith(topics []client.TopicInfo, groups []client.GroupInfo, err error) *fakeLister {
	l := &fakeLister{}
	l.set(topics, groups, err)
	return l
}

func TestTheFurthestAheadNodeDecidesWhatTheClusterHolds(t *testing.T) {
	ahead := listerWith([]client.TopicInfo{{Name: "orders", Ends: []uint64{10, 7}}}, nil, nil)
	behind := listerWith([]client.TopicInfo{{Name: "orders", Ends: []uint64{4, 7}}, {Name: "audit", Ends: []uint64{3}}}, nil, nil)
	m := NewMultiLister(behind, ahead)
	got, err := m.Topics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []client.TopicInfo{{Name: "audit", Ends: []uint64{3}}, {Name: "orders", Ends: []uint64{10, 7}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Topics = %+v, want %+v: partition by partition, the highest end", got, want)
	}
}

func TestANodeThatDoesNotAnswerIsSkippedAndAllSilentIsAnError(t *testing.T) {
	up := listerWith([]client.TopicInfo{{Name: "orders", Ends: []uint64{2}}}, nil, nil)
	down := listerWith(nil, nil, errors.New("connection refused"))
	if got, err := NewMultiLister(down, up).Topics(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("one node down: %v, %v", got, err)
	}
	if _, err := NewMultiLister(down, down).Topics(context.Background()); err == nil || err.Error() != "connection refused" {
		t.Fatalf("every node down: %v", err)
	}
	if _, err := NewMultiLister().Topics(context.Background()); err == nil {
		t.Fatal("no nodes at all gave an answer")
	}
	if _, err := NewMultiLister(down).Groups(context.Background()); err == nil {
		t.Fatal("groups from a dead node gave an answer")
	}
}

func TestGroupsTakeTheAnswerOfTheNodeThatHasTheMostMessages(t *testing.T) {
	a := listerWith(nil, []client.GroupInfo{{Name: "billing", Partitions: []client.GroupPartition{
		{Topic: "orders", Partition: 0, Offset: 5, End: 8, Lag: 3},
		{Topic: "orders", Partition: 1, Offset: 2, End: 2, Lag: 0},
	}}}, nil)
	b := listerWith(nil, []client.GroupInfo{
		{Name: "billing", Partitions: []client.GroupPartition{{Topic: "orders", Partition: 0, Offset: 4, End: 10, Lag: 6}}},
		{Name: "analytics", Partitions: []client.GroupPartition{{Topic: "orders", Partition: 0, Offset: 1, End: 10, Lag: 9}}},
	}, nil)
	got, err := NewMultiLister(a, b).Groups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []client.GroupInfo{
		{Name: "analytics", Partitions: []client.GroupPartition{{Topic: "orders", Partition: 0, Offset: 1, End: 10, Lag: 9}}},
		{Name: "billing", Partitions: []client.GroupPartition{
			{Topic: "orders", Partition: 0, Offset: 4, End: 10, Lag: 6},
			{Topic: "orders", Partition: 1, Offset: 2, End: 2, Lag: 0},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Groups =\n%+v\nwant\n%+v", got, want)
	}

	// Equal ends: the higher committed offset is the newer commit.
	x := listerWith(nil, []client.GroupInfo{{Name: "g", Partitions: []client.GroupPartition{{Topic: "t", Offset: 3, End: 9, Lag: 6}}}}, nil)
	y := listerWith(nil, []client.GroupInfo{{Name: "g", Partitions: []client.GroupPartition{{Topic: "t", Offset: 5, End: 9, Lag: 4}}}}, nil)
	got, _ = NewMultiLister(x, y).Groups(context.Background())
	if got[0].Partitions[0].Offset != 5 {
		t.Fatalf("tie: %+v", got)
	}
}
