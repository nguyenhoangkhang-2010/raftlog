package orderservice

import (
	"errors"
	"fmt"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// Loyalty is the points program of the venue: what earns a point, and what a point takes off. The program is
// off unless both are set. The settings decide what is written from now on; what was written keeps the value it
// had (an order says what its points were worth, a payment how many it earned), so changing them never
// changes a balance that the log already makes (see ADR 22).
type Loyalty struct {
	// EarnPer is the money paid that earns one point: a payment of 25.000đ with 10.000đ per point earns two.
	EarnPer money.Money
	// Value is the money that one point takes off an order.
	Value money.Money
	// MaxSharePercent is the part of an order, after its promotions, that points may pay for; 50 if zero.
	MaxSharePercent int
}

// On says whether points are earned and used.
func (l Loyalty) On() bool { return l.EarnPer > 0 && l.Value > 0 }

// Validate checks the settings.
func (l Loyalty) Validate() error {
	switch {
	case l.EarnPer < 0 || l.Value < 0:
		return errors.New("orderservice: the settings of the points cannot be negative")
	case l.EarnPer > money.MaxPrice || l.Value > money.MaxPrice:
		return errors.New("orderservice: a point earns or takes off at most 100.000.000đ")
	case l.MaxSharePercent < 0 || l.MaxSharePercent > 100:
		return errors.New("orderservice: the share of an order that points may pay is 1 to 100 percent")
	case (l.EarnPer > 0) != (l.Value > 0):
		return errors.New("orderservice: points need both what earns one and what one is worth, or neither")
	}
	return nil
}

func (l Loyalty) share() int64 {
	if l.MaxSharePercent == 0 {
		return 50
	}
	return int64(l.MaxSharePercent)
}

// Earned is how many points a payment of that much earns, whole points only.
func (l Loyalty) Earned(paid money.Money) int64 {
	if !l.On() || paid <= 0 {
		return 0
	}
	n := int64(paid / l.EarnPer)
	if n > order.MaxPoints {
		n = order.MaxPoints
	}
	return n
}

// MaxUsable is the most points an order of that total, after its promotions, may use when the customer has
// that many available.
func (l Loyalty) MaxUsable(total money.Money, available int64) int64 {
	if !l.On() || total <= 0 || available <= 0 {
		return 0
	}
	n := int64(total) * l.share() / 100 / int64(l.Value)
	if n > available {
		n = available
	}
	if n > order.MaxPoints {
		n = order.MaxPoints
	}
	return n
}

// PointsLimitError says that an order asked for more points than it may use.
type PointsLimitError struct{ Max int64 }

func (e *PointsLimitError) Error() string {
	return fmt.Sprintf("orderservice: an order may use at most %d points", e.Max)
}

// ErrNoPoints is returned when points are asked for and the venue has no points program, or when a person
// who may not use points asks for them.
var ErrNoPoints = errors.New("orderservice: points cannot be used here")

// ---- what the state knows -----------------------------------------------------------------------

// The kinds of an entry of the history of points.
const (
	PointsEarned     = "earned"
	PointsRedeemed   = "redeemed"
	PointsReturned   = "returned"
	PointsClawedBack = "taken_back"
)

// PointsEntry is one thing that happened to the points of a customer: an order's payment earned them, or spent
// them, or a cancellation or a refund gave them back or took them back. Points is signed.
type PointsEntry struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Points int64     `json:"points"`
	Order  string    `json:"order"`
	Number int       `json:"number"`
}

// maxPointsHistory is how many entries of history the state keeps for each customer.
const maxPointsHistory = 200

type pointsAccount struct {
	// balance is what the customer owns, including what is held; held is what orders that are placed and not
	// paid have set aside. Both may be read as they are: balance less held is what is available.
	balance, held int64
	// earned and spent are the sums of every entry of those kinds, whatever has been dropped from the history.
	earned, spent int64
	history       []PointsEntry
	dropped       int
}

// PointsView is the points of a customer, as the API gives them.
type PointsView struct {
	Customer  string `json:"customer"`
	Balance   int64  `json:"balance"`
	Held      int64  `json:"held"`
	Available int64  `json:"available"`
	// Earned and Spent are what was earned and what was spent in all, less what was taken back and what was given back.
	Earned  int64         `json:"earned"`
	Spent   int64         `json:"spent"`
	History []PointsEntry `json:"history"`
	// Dropped says how many older entries of the history are no longer kept.
	Dropped int `json:"dropped,omitempty"`
}

func (s *State) account(customer string) *pointsAccount {
	if s.points == nil {
		s.points = map[string]*pointsAccount{}
	}
	a := s.points[customer]
	if a == nil {
		a = &pointsAccount{}
		s.points[customer] = a
	}
	return a
}

func (a *pointsAccount) note(e PointsEntry) {
	a.history = append(a.history, e)
	if len(a.history) > maxPointsHistory {
		a.dropped += len(a.history) - maxPointsHistory
		a.history = append([]PointsEntry(nil), a.history[len(a.history)-maxPointsHistory:]...)
	}
}

// Points returns the points of a customer.
func (s *State) Points(customer string) PointsView {
	v := PointsView{Customer: customer, History: []PointsEntry{}}
	if a := s.points[customer]; a != nil {
		v.Balance, v.Held, v.Available, v.Earned, v.Spent, v.Dropped = a.balance, a.held, a.balance-a.held, a.earned, a.spent, a.dropped
		v.History = append(v.History, a.history...)
	}
	return v
}

// AvailablePoints is what a customer may use now: what they own less what is set aside for orders that are
// placed and not paid.
func (s *State) AvailablePoints(customer string) int64 {
	if a := s.points[customer]; a != nil {
		return a.balance - a.held
	}
	return 0
}

// pointsPlaced sets the points of a new order aside. It refuses an order that uses more than the customer has
// available at this place in the log, so that every reader decides alike.
func (s *State) pointsPlaced(o *order.Order) error {
	if o.Points == nil {
		return nil
	}
	if o.Customer == "" {
		return fmt.Errorf("order %s uses points and has no customer", o.ID)
	}
	a := s.account(o.Customer)
	if o.Points.Used > a.balance-a.held {
		return fmt.Errorf("order %s uses %d points and %s has %d available", o.ID, o.Points.Used, o.Customer, a.balance-a.held)
	}
	a.held += o.Points.Used
	return nil
}

// pointsPaid spends the points an order set aside and gives the customer what the payment earned.
func (s *State) pointsPaid(o *order.Order, at time.Time) {
	if o.Customer == "" {
		return
	}
	if o.Points == nil && o.PointsEarned == 0 {
		return
	}
	a := s.account(o.Customer)
	if p := o.Points; p != nil {
		a.held -= p.Used
		a.balance -= p.Used
		a.spent += p.Used
		a.note(PointsEntry{At: at, Kind: PointsRedeemed, Points: -p.Used, Order: string(o.ID), Number: o.Number})
	}
	if n := o.PointsEarned; n > 0 {
		a.balance += n
		a.earned += n
		a.note(PointsEntry{At: at, Kind: PointsEarned, Points: n, Order: string(o.ID), Number: o.Number})
	}
}

// share is the part of n that is in proportion refunded/paid, rounded down.
func share(n int64, refunded, paid money.Money) int64 {
	if n <= 0 || paid <= 0 || refunded <= 0 {
		return 0
	}
	if refunded >= paid {
		return n
	}
	return n * int64(refunded) / int64(paid)
}

// pointsCancelled ends an order: an order that was never paid lets go of its points; one that was paid, and is
// given back in whole or in part, gives back the points it used and takes back the points it earned, in
// proportion to the money that was given back.
func (s *State) pointsCancelled(o *order.Order, at time.Time, wasPaid bool) {
	if o.Customer == "" || (o.Points == nil && o.PointsEarned == 0) {
		return
	}
	a := s.account(o.Customer)
	if !wasPaid {
		if o.Points != nil {
			a.held -= o.Points.Used
		}
		return
	}
	paid := o.Payment.Amount
	if p := o.Points; p != nil {
		back := p.Used
		if paid > 0 {
			back = share(p.Used, o.Refunded, paid)
		}
		if back > 0 {
			a.balance += back
			a.spent -= back
			a.note(PointsEntry{At: at, Kind: PointsReturned, Points: back, Order: string(o.ID), Number: o.Number})
		}
	}
	if n := share(o.PointsEarned, o.Refunded, paid); n > 0 {
		a.balance -= n
		a.earned -= n
		a.note(PointsEntry{At: at, Kind: PointsClawedBack, Points: -n, Order: string(o.ID), Number: o.Number})
	}
}

// pointsRefunded takes back, from what the order earned, the share that this refund adds to what was already
// given back. Points that were used are not given back for a refund after the order was handed over: the
// discount was had, and the money came back.
func (s *State) pointsRefunded(o *order.Order, at time.Time, refundedBefore money.Money) {
	if o.Customer == "" || o.PointsEarned == 0 || o.Payment == nil {
		return
	}
	paid := o.Payment.Amount
	n := share(o.PointsEarned, o.Refunded, paid) - share(o.PointsEarned, refundedBefore, paid)
	if n <= 0 {
		return
	}
	a := s.account(o.Customer)
	a.balance -= n
	a.earned -= n
	a.note(PointsEntry{At: at, Kind: PointsClawedBack, Points: -n, Order: string(o.ID), Number: o.Number})
}

// seesAll says whether the actor may see every order, and so the points of any customer.
func (s *Service) seesAll(a rbac.Actor) bool {
	for _, r := range a.Roles {
		if r != rbac.Customer && rbac.Allowed(r, rbac.ViewAllOrders) {
			return true
		}
	}
	return false
}

// PointsOf returns the points of a customer, with the rules of the program.
func (s *Service) PointsOf(customer string) PointsAnswer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ans := PointsAnswer{Enabled: s.loyalty.On(), PointsView: s.st.Points(customer)}
	if ans.Enabled {
		ans.EarnPer, ans.Value, ans.MaxShare = int64(s.loyalty.EarnPer), int64(s.loyalty.Value), s.loyalty.share()
	}
	return ans
}

// Loyalty returns the settings of the points program.
func (s *Service) Loyalty() Loyalty { return s.loyalty }
