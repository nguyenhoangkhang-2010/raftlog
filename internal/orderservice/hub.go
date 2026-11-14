package orderservice

import "sync"

// subscriberBuffer is how many changes a subscriber may fall behind before it is closed.
const subscriberBuffer = 256

// Subscription delivers the changes that its filter lets through.
type Subscription struct {
	// C receives the changes. It is closed when the subscription is closed, or when the
	// subscriber fell too far behind: it should then read the state again and subscribe again.
	C <-chan Change

	h      *hub
	c      chan Change
	filter func(Change) (Change, bool)
	done   bool
}

// Close ends the subscription. It is safe to call twice.
func (s *Subscription) Close() { s.h.remove(s) }

type hub struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

func newHub() *hub { return &hub{subs: map[*Subscription]struct{}{}} }

func (h *hub) subscribe(filter func(Change) (Change, bool)) *Subscription {
	c := make(chan Change, subscriberBuffer)
	s := &Subscription{C: c, h: h, c: c, filter: filter}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) remove(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	delete(h.subs, s)
	close(s.c)
}

// publish hands the change to every subscriber whose filter accepts it. A subscriber with a
// full buffer is closed instead of holding the service up.
func (h *hub) publish(ch Change) {
	if !ch.Applied() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		mine, ok := s.filter(ch)
		if !ok {
			continue
		}
		select {
		case s.c <- mine:
		default:
			s.done = true
			delete(h.subs, s)
			close(s.c)
		}
	}
}
