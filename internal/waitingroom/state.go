package waitingroom

import (
	"context"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/client"
)

// Ticket states.
const (
	StateWaiting  = "waiting"
	StateAdmitted = "admitted"
	StateUnknown  = "unknown"
)

// rateWindow is how many recent admissions the measured rate looks at.
const rateWindow = 10

// Ticket is one place in line.
type Ticket struct {
	ID       string
	User     string
	Offset   uint64 // offset of the "joined" event in the queue log
	JoinedAt time.Time
}

// Status is what the room can say about one ticket.
type Status struct {
	Ticket string `json:"ticket"`
	// State is "waiting", "admitted" or "unknown" (never joined, or the room has
	// not read the join yet).
	State string `json:"state"`
	User  string `json:"user,omitempty"`
	// Offset is where the ticket's "joined" event sits in the queue log. Tickets
	// are admitted in this order.
	Offset uint64 `json:"offset"`
	// Position is the place in line: 1 means "next". 0 once admitted.
	Position int `json:"position"`
	// Ahead is how many tickets are admitted later than this one is.
	Ahead int `json:"ahead"`
	// Room totals at the time of the answer.
	Total    int `json:"total"`
	Admitted int `json:"admitted"`
	Waiting  int `json:"waiting"`
	// AdmitRate is the admission rate measured over the last few admissions, in
	// tickets per second; absent until two admissions have been seen. ETASeconds
	// is Ahead plus one divided by that rate: an estimate, not a promise.
	AdmitRate  *float64 `json:"admit_rate_per_s,omitempty"`
	ETASeconds *float64 `json:"eta_seconds,omitempty"`
	JoinedAtMs int64    `json:"joined_at_ms,omitempty"`
}

// Stats are the totals of the room.
type Stats struct {
	Total    int `json:"total"`
	Admitted int `json:"admitted"`
	Waiting  int `json:"waiting"`
	// Gate is the queue offset below which every ticket is admitted.
	Gate uint64 `json:"gate"`
	// QueueEnd and GateEnd are how far the room has read each log.
	QueueEnd uint64 `json:"queue_end"`
	GateEnd  uint64 `json:"gate_end"`
	// Skipped counts records that were not valid events.
	Skipped   int      `json:"skipped"`
	AdmitRate *float64 `json:"admit_rate_per_s,omitempty"`
}

type mark struct {
	at    time.Time
	count int
}

// State is the room as seen by one process: the queue and the gate, rebuilt by
// applying the records of the two logs in order. It does no I/O, and is safe for
// concurrent use.
type State struct {
	mu      sync.RWMutex
	tickets []Ticket       // distinct tickets, in queue order
	byID    map[string]int // ticket ID -> index in tickets
	// queueNext and gateNext are the next offsets to read from each log.
	queueNext, gateNext uint64
	gate                uint64 // every ticket joined below this offset is admitted
	admitted            int    // number of tickets with Offset < gate
	marks               []mark // the most recent admissions
	skipped             int
	wake                chan struct{}
}

// NewState returns an empty room.
func NewState() *State {
	return &State{byID: make(map[string]int), wake: make(chan struct{})}
}

// Changed returns a channel that is closed the next time the state changes. Take
// it before reading the state, then wait on it, to miss no change.
func (s *State) Changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wake
}

// notify wakes everyone waiting on Changed. The caller holds s.mu.
func (s *State) notify() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// ApplyQueue applies one record of the queue log. Records must be applied in
// order; one that was applied before is ignored, so re-reading is harmless.
func (s *State) ApplyQueue(rec client.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Offset < s.queueNext {
		return
	}
	s.queueNext = rec.Offset + 1
	ev, ok := decodeJoined(rec.Value)
	switch {
	case !ok:
		s.skipped++
	case !s.knows(ev.Ticket):
		// Joining twice, for instance a retry after a lost answer, leaves two
		// events in the log. The first one counts; later ones change nothing.
		s.byID[ev.Ticket] = len(s.tickets)
		s.tickets = append(s.tickets, Ticket{ID: ev.Ticket, User: ev.User, Offset: rec.Offset, JoinedAt: rec.Timestamp})
	}
	s.advance()
	s.notify()
}

// ApplyGate applies one record of the gate log. The gate only moves forward, so
// repeated or reordered admissions cannot let anyone in twice or take anyone back.
func (s *State) ApplyGate(rec client.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Offset < s.gateNext {
		return
	}
	s.gateNext = rec.Offset + 1
	ev, ok := decodeAdmitted(rec.Value)
	switch {
	case !ok:
		s.skipped++
	case ev.UpTo > s.gate:
		s.gate = ev.UpTo
		if ev.Count > 0 {
			s.marks = append(s.marks, mark{at: rec.Timestamp, count: ev.Count})
			if len(s.marks) > rateWindow {
				s.marks = s.marks[len(s.marks)-rateWindow:]
			}
		}
	}
	s.advance()
	s.notify()
}

func (s *State) knows(id string) bool {
	_, ok := s.byID[id]
	return ok
}

// advance counts the tickets the gate has passed. The queue log and the gate log
// are read independently, so the gate can get ahead of the queue; tickets that
// show up later are then admitted at once.
func (s *State) advance() {
	for s.admitted < len(s.tickets) && s.tickets[s.admitted].Offset < s.gate {
		s.admitted++
	}
}

// rate is the measured admission rate in tickets per second. The first mark only
// starts the clock: its count was collected over time that was not measured.
func (s *State) rate() (float64, bool) {
	if len(s.marks) < 2 {
		return 0, false
	}
	span := s.marks[len(s.marks)-1].at.Sub(s.marks[0].at)
	if span <= 0 {
		return 0, false
	}
	n := 0
	for _, m := range s.marks[1:] {
		n += m.count
	}
	return float64(n) / span.Seconds(), true
}

// Stats returns the totals.
func (s *State) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats()
}

func (s *State) stats() Stats {
	st := Stats{
		Total: len(s.tickets), Admitted: s.admitted, Waiting: len(s.tickets) - s.admitted,
		Gate: s.gate, QueueEnd: s.queueNext, GateEnd: s.gateNext, Skipped: s.skipped,
	}
	if r, ok := s.rate(); ok {
		st.AdmitRate = &r
	}
	return st
}

// Status returns what is known about a ticket.
func (s *State) Status(id string) Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.stats()
	out := Status{
		Ticket: id, State: StateUnknown,
		Total: st.Total, Admitted: st.Admitted, Waiting: st.Waiting, AdmitRate: st.AdmitRate,
	}
	i, ok := s.byID[id]
	if !ok {
		return out
	}
	t := s.tickets[i]
	out.User, out.Offset, out.JoinedAtMs = t.User, t.Offset, t.JoinedAt.UnixMilli()
	if i < s.admitted {
		out.State = StateAdmitted
		return out
	}
	out.State = StateWaiting
	out.Position = i - s.admitted + 1
	out.Ahead = out.Position - 1
	if st.AdmitRate != nil && *st.AdmitRate > 0 {
		eta := float64(out.Position) / *st.AdmitRate
		out.ETASeconds = &eta
	}
	return out
}

// NextBoundary says where the gate should move to let in the next n waiting
// tickets: every ticket joined below upTo, and count of them are new. It returns
// count 0 when nobody is waiting.
func (s *State) NextBoundary(n int) (upTo uint64, count int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	waiting := len(s.tickets) - s.admitted
	if n > waiting {
		n = waiting
	}
	if n <= 0 {
		return 0, 0
	}
	return s.tickets[s.admitted+n-1].Offset + 1, n
}

// Admitted returns the admitted tickets in admission order, for checks.
func (s *State) Admitted() []Ticket {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Ticket(nil), s.tickets[:s.admitted]...)
}

// Tickets returns every ticket in queue order, for checks.
func (s *State) Tickets() []Ticket {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Ticket(nil), s.tickets...)
}

// waitFor blocks until ok reports true for the state, or ctx ends.
func (s *State) waitFor(ctx context.Context, ok func() bool) error {
	for {
		changed := s.Changed()
		s.mu.RLock()
		done := ok()
		s.mu.RUnlock()
		if done {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WaitQueue blocks until the record at queue offset has been applied.
func (s *State) WaitQueue(ctx context.Context, offset uint64) error {
	return s.waitFor(ctx, func() bool { return s.queueNext > offset })
}

// WaitGate blocks until the record at gate offset has been applied.
func (s *State) WaitGate(ctx context.Context, offset uint64) error {
	return s.waitFor(ctx, func() bool { return s.gateNext > offset })
}

// WaitRead blocks until the room has read at least queueEnd records of the queue
// and gateEnd records of the gate.
func (s *State) WaitRead(ctx context.Context, queueEnd, gateEnd uint64) error {
	return s.waitFor(ctx, func() bool { return s.queueNext >= queueEnd && s.gateNext >= gateEnd })
}
