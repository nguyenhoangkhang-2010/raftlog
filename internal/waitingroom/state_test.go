package waitingroom

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

var epoch = time.Unix(1_700_000_000, 0)

func joinedRec(offset uint64, ticket, user string) client.Record {
	v, err := encodeJoined(ticket, user)
	if err != nil {
		panic(err)
	}
	return client.Record{Offset: offset, Timestamp: epoch.Add(time.Duration(offset) * time.Millisecond), Value: v}
}

func admittedRec(offset, upTo uint64, count int, at time.Time) client.Record {
	v, err := encodeAdmitted(upTo, count)
	if err != nil {
		panic(err)
	}
	return client.Record{Offset: offset, Timestamp: at, Value: v}
}

// room returns a state with tickets t0..t(n-1) at queue offsets 0..n-1.
func roomOf(n int) *State {
	s := NewState()
	for i := 0; i < n; i++ {
		s.ApplyQueue(joinedRec(uint64(i), fmt.Sprintf("t%d", i), fmt.Sprintf("user %d", i)))
	}
	return s
}

func TestPositionsFollowTheQueueOrder(t *testing.T) {
	s := roomOf(5)
	for i := 0; i < 5; i++ {
		st := s.Status(fmt.Sprintf("t%d", i))
		if st.State != StateWaiting || st.Position != i+1 || st.Ahead != i || st.Offset != uint64(i) {
			t.Fatalf("t%d: %+v, want waiting at position %d", i, st, i+1)
		}
		if st.Total != 5 || st.Waiting != 5 || st.Admitted != 0 {
			t.Fatalf("t%d: totals %+v", i, st)
		}
	}
	if st := s.Status("nobody"); st.State != StateUnknown || st.Position != 0 {
		t.Fatalf("unknown ticket: %+v", st)
	}
}

func TestTheGateAdmitsAPrefixAndTheRestMoveUp(t *testing.T) {
	s := roomOf(5)
	s.ApplyGate(admittedRec(0, 2, 2, epoch)) // tickets at offsets 0 and 1

	for _, id := range []string{"t0", "t1"} {
		if st := s.Status(id); st.State != StateAdmitted || st.Position != 0 {
			t.Fatalf("%s: %+v, want admitted", id, st)
		}
	}
	if st := s.Status("t2"); st.State != StateWaiting || st.Position != 1 {
		t.Fatalf("t2: %+v, want position 1", st)
	}
	if st := s.Status("t4"); st.Position != 3 || st.Admitted != 2 || st.Waiting != 3 {
		t.Fatalf("t4: %+v, want position 3 with 2 admitted", st)
	}
}

func TestAJoinThatIsRepeatedKeepsTheFirstPlace(t *testing.T) {
	s := NewState()
	s.ApplyQueue(joinedRec(0, "a", "first label"))
	s.ApplyQueue(joinedRec(1, "b", ""))
	s.ApplyQueue(joinedRec(2, "a", "second label")) // a retry of a's join

	if st := s.Status("a"); st.Position != 1 || st.Offset != 0 || st.User != "first label" {
		t.Fatalf("a: %+v, want the first join to count", st)
	}
	if st := s.Status("b"); st.Position != 2 {
		t.Fatalf("b: %+v, want position 2: the repeat of a must not take a place", st)
	}
	if st := s.Stats(); st.Total != 2 {
		t.Fatalf("Total = %d, want 2", st.Total)
	}
}

func TestTheGateNeverMovesBack(t *testing.T) {
	s := roomOf(6)
	s.ApplyGate(admittedRec(0, 4, 4, epoch))
	s.ApplyGate(admittedRec(1, 2, 2, epoch)) // late or repeated: must change nothing
	if st := s.Stats(); st.Admitted != 4 || st.Gate != 4 {
		t.Fatalf("Stats = %+v, want 4 admitted behind gate 4", st)
	}
	s.ApplyGate(admittedRec(2, 4, 0, epoch)) // same gate again
	if st := s.Stats(); st.Admitted != 4 {
		t.Fatalf("Admitted = %d after the gate was repeated", st.Admitted)
	}
}

func TestRecordsThatWereAlreadyAppliedAreIgnored(t *testing.T) {
	s := NewState()
	s.ApplyQueue(joinedRec(0, "a", ""))
	s.ApplyQueue(joinedRec(1, "b", ""))
	s.ApplyQueue(joinedRec(0, "a", "")) // read again after a reconnect
	s.ApplyQueue(joinedRec(1, "b", ""))
	if st := s.Stats(); st.Total != 2 || st.QueueEnd != 2 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestTheGateMayBeReadBeforeTheQueueCatchesUp(t *testing.T) {
	// The two logs are read independently. When the gate is ahead, tickets that
	// show up afterwards below the gate are admitted at once.
	s := NewState()
	s.ApplyGate(admittedRec(0, 3, 3, epoch))
	s.ApplyQueue(joinedRec(0, "a", ""))
	s.ApplyQueue(joinedRec(1, "b", ""))
	if st := s.Status("b"); st.State != StateAdmitted {
		t.Fatalf("b: %+v, want admitted: the gate had passed its offset", st)
	}
	s.ApplyQueue(joinedRec(2, "c", ""))
	s.ApplyQueue(joinedRec(3, "d", ""))
	if st := s.Status("c"); st.State != StateAdmitted {
		t.Fatalf("c: %+v, want admitted", st)
	}
	if st := s.Status("d"); st.State != StateWaiting || st.Position != 1 {
		t.Fatalf("d: %+v, want waiting at position 1", st)
	}
}

func TestInvalidRecordsAreSkippedAndCounted(t *testing.T) {
	s := NewState()
	s.ApplyQueue(client.Record{Offset: 0, Value: []byte("not json")})
	s.ApplyQueue(client.Record{Offset: 1, Value: []byte(`{"type":"joined","ticket":"bad ticket!"}`)})
	s.ApplyQueue(client.Record{Offset: 2, Value: []byte(`{"type":"other","ticket":"x"}`)})
	s.ApplyQueue(joinedRec(3, "ok", ""))
	s.ApplyGate(client.Record{Offset: 0, Value: []byte("{")})
	st := s.Stats()
	if st.Total != 1 || st.Skipped != 4 || st.QueueEnd != 4 || st.GateEnd != 1 {
		t.Fatalf("Stats = %+v, want 1 ticket and 4 skipped records", st)
	}
	if got := s.Status("ok"); got.Position != 1 {
		t.Fatalf("ok: %+v", got)
	}
}

func TestNextBoundaryAdmitsTheNextBatchInOrder(t *testing.T) {
	s := roomOf(5)
	if upTo, n := s.NextBoundary(2); upTo != 2 || n != 2 {
		t.Fatalf("NextBoundary(2) = %d, %d; want 2, 2", upTo, n)
	}
	s.ApplyGate(admittedRec(0, 2, 2, epoch))
	if upTo, n := s.NextBoundary(10); upTo != 5 || n != 3 {
		t.Fatalf("NextBoundary(10) = %d, %d; want 5, 3 (only 3 are waiting)", upTo, n)
	}
	s.ApplyGate(admittedRec(1, 5, 3, epoch))
	if _, n := s.NextBoundary(1); n != 0 {
		t.Fatalf("nobody is waiting but NextBoundary admits %d", n)
	}
	if _, n := s.NextBoundary(0); n != 0 {
		t.Fatal("a batch of 0 admits someone")
	}
}

func TestTheBoundarySkipsTheOffsetsOfRepeatedJoins(t *testing.T) {
	s := NewState()
	s.ApplyQueue(joinedRec(0, "a", ""))
	s.ApplyQueue(joinedRec(1, "a", "")) // repeat
	s.ApplyQueue(joinedRec(2, "b", ""))
	upTo, n := s.NextBoundary(2)
	if n != 2 || upTo != 3 {
		t.Fatalf("NextBoundary(2) = %d, %d; want 3, 2", upTo, n)
	}
	s.ApplyGate(admittedRec(0, upTo, n, epoch))
	if st := s.Stats(); st.Admitted != 2 || st.Waiting != 0 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestTheRateIsMeasuredNotAssumed(t *testing.T) {
	s := roomOf(40)
	if st := s.Status("t39"); st.AdmitRate != nil || st.ETASeconds != nil {
		t.Fatalf("a rate %v / ETA %v before any admission was measured", st.AdmitRate, st.ETASeconds)
	}
	s.ApplyGate(admittedRec(0, 5, 5, epoch))
	if st := s.Stats(); st.AdmitRate != nil {
		t.Fatalf("one admission cannot give a rate, got %v", *st.AdmitRate)
	}
	// Two more batches of 5, each 2 seconds apart: 10 tickets in 4 seconds.
	s.ApplyGate(admittedRec(1, 10, 5, epoch.Add(2*time.Second)))
	s.ApplyGate(admittedRec(2, 15, 5, epoch.Add(4*time.Second)))
	st := s.Status("t39")
	if st.AdmitRate == nil || *st.AdmitRate != 2.5 {
		t.Fatalf("rate = %v, want 2.5 per second (10 tickets in 4 s)", st.AdmitRate)
	}
	// t39 is at position 25 (40 tickets, 15 admitted): 25 / 2.5 = 10 s.
	if st.Position != 25 || st.ETASeconds == nil || *st.ETASeconds != 10 {
		t.Fatalf("status = %+v, want position 25 and ETA 10 s", st)
	}
}

func TestWaitingForARecordReturnsWhenItIsApplied(t *testing.T) {
	s := NewState()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- s.WaitQueue(ctx, 1)
	}()
	time.Sleep(30 * time.Millisecond)
	s.ApplyQueue(joinedRec(0, "a", ""))
	select {
	case err := <-done:
		t.Fatalf("returned after offset 0 although it waited for offset 1: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.ApplyQueue(joinedRec(1, "b", ""))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitQueue did not return")
	}
}

func TestWaitingStopsWhenTheContextEnds(t *testing.T) {
	s := NewState()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.WaitGate(ctx, 3); err == nil {
		t.Fatal("WaitGate returned without a record")
	}
}

// The room is a function of the two logs: whatever order the records of the two
// logs are read in relative to each other, the same logs give the same room.
func TestTheSameLogsGiveTheSameRoomInAnyReadingOrder(t *testing.T) {
	const tickets = 60
	rng := rand.New(rand.NewSource(7))
	var queue, gate []client.Record
	for i := 0; i < tickets; i++ {
		id := fmt.Sprintf("t%d", rng.Intn(45)) // some tickets join more than once
		queue = append(queue, joinedRec(uint64(i), id, ""))
	}
	var upTo uint64
	for g := 0; upTo < tickets; g++ {
		upTo += uint64(1 + rng.Intn(9))
		if upTo > tickets {
			upTo = tickets
		}
		gate = append(gate, admittedRec(uint64(g), upTo, 1, epoch.Add(time.Duration(g)*time.Second)))
	}

	ref := NewState()
	for _, r := range queue {
		ref.ApplyQueue(r)
	}
	for _, r := range gate {
		ref.ApplyGate(r)
	}
	want := ref.Tickets()

	for trial := 0; trial < 50; trial++ {
		s := NewState()
		qi, gi := 0, 0
		for qi < len(queue) || gi < len(gate) {
			if gi >= len(gate) || (qi < len(queue) && rng.Intn(2) == 0) {
				s.ApplyQueue(queue[qi])
				qi++
			} else {
				s.ApplyGate(gate[gi])
				gi++
			}
		}
		got := s.Tickets()
		if len(got) != len(want) || s.Stats().Admitted != ref.Stats().Admitted {
			t.Fatalf("trial %d: %d tickets, %d admitted; want %d, %d", trial, len(got), s.Stats().Admitted, len(want), ref.Stats().Admitted)
		}
		for i := range want {
			if got[i].ID != want[i].ID || got[i].Offset != want[i].Offset {
				t.Fatalf("trial %d: ticket %d is %+v, want %+v", trial, i, got[i], want[i])
			}
			if s.Status(got[i].ID).State != ref.Status(got[i].ID).State || s.Status(got[i].ID).Position != ref.Status(got[i].ID).Position {
				t.Fatalf("trial %d: ticket %s has status %+v, want %+v", trial, got[i].ID, s.Status(got[i].ID), ref.Status(got[i].ID))
			}
		}
	}
}

func TestEventsAreValidated(t *testing.T) {
	for _, id := range []string{"a", "A-b_9", "x" + string(make([]byte, 0)) + "123456789012345678901234567890123456789012345678901234567890123"} {
		if !validTicket(id) {
			t.Errorf("%q should be a valid ticket", id)
		}
	}
	for _, id := range []string{"", "has space", "slash/", "ünicode", "123456789012345678901234567890123456789012345678901234567890123456"} {
		if validTicket(id) {
			t.Errorf("%q should not be a valid ticket", id)
		}
	}
	if _, err := cleanUser("Nguyễn Văn A  "); err != nil {
		t.Errorf("a name with accents: %v", err)
	}
	if u, _ := cleanUser("  padded "); u != "padded" {
		t.Errorf("cleanUser did not trim: %q", u)
	}
	for _, bad := range []string{"line\nbreak", "tab\there", string(make([]rune, 0)) + string([]byte{0xff, 0xfe})} {
		if _, err := cleanUser(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	long := make([]rune, maxUserRunes+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := cleanUser(string(long)); err == nil {
		t.Error("a label over the limit was accepted")
	}
}
