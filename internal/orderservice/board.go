package orderservice

import (
	"sort"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
)

// boardLimit bounds how many numbers a column of the call board lists.
const boardLimit = 60

// BoardEntry is one order on the call board. It carries a number and a time and nothing else: the
// board may be shown to a whole room, so it never says who ordered or what.
type BoardEntry struct {
	Number int `json:"number"`
	// Since is when the order was paid, for one that is being made, and when its last part was
	// finished, for one that is ready.
	Since time.Time `json:"since"`
}

// BoardView is what a screen for the customers waiting shows.
type BoardView struct {
	// Preparing are the orders that are paid and not ready, oldest first.
	Preparing []BoardEntry `json:"preparing"`
	// Ready are the orders waiting to be handed over, the one that has waited longest first.
	Ready []BoardEntry `json:"ready"`
	At    time.Time    `json:"at"`
}

// Board returns the numbers of the orders being made and of those that are ready.
func (s *Service) Board() BoardView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := BoardView{Preparing: []BoardEntry{}, Ready: []BoardEntry{}, At: s.at()}
	for _, o := range s.st.Orders() {
		switch o.Status {
		case order.Paid, order.Preparing:
			e := BoardEntry{Number: o.Number}
			if o.Payment != nil {
				e.Since = o.Payment.At
			}
			v.Preparing = append(v.Preparing, e)
		case order.Ready:
			e := BoardEntry{Number: o.Number}
			for _, t := range o.Tickets {
				if t.ReadyAt.After(e.Since) {
					e.Since = t.ReadyAt
				}
			}
			v.Ready = append(v.Ready, e)
		}
	}
	byAge := func(l []BoardEntry) {
		sort.Slice(l, func(i, j int) bool {
			if !l[i].Since.Equal(l[j].Since) {
				return l[i].Since.Before(l[j].Since)
			}
			return l[i].Number < l[j].Number
		})
	}
	byAge(v.Preparing)
	byAge(v.Ready)
	if len(v.Preparing) > boardLimit {
		v.Preparing = v.Preparing[:boardLimit]
	}
	if len(v.Ready) > boardLimit {
		v.Ready = v.Ready[:boardLimit]
	}
	return v
}

// SubscribeBoard returns a subscription that is told when an order changes, with nothing but the
// fact: the screen then asks for the board again. It has no person behind it, so it is for the
// public board only.
func (s *Service) SubscribeBoard() *Subscription {
	return s.hub.subscribe(func(ch Change) (Change, bool) {
		if ch.Order == "" {
			return Change{}, false
		}
		return Change{Offset: ch.Offset, Order: ch.Order}, true
	})
}
