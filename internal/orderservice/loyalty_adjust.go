package orderservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// MaxAdjust is the most points that one change by a manager may give or take. The log would hold more
// (order.MaxPoints); the service keeps a single change small, so that a mistake or a misuse is small too.
const MaxAdjust int64 = 10_000

// PointsShortError says that a manager asked to take more points off a customer than they have available.
// Points that orders not paid yet have set aside cannot be taken, so the answer is what can be.
type PointsShortError struct{ Available int64 }

func (e *PointsShortError) Error() string {
	return fmt.Sprintf("orderservice: only %d points can be taken off this customer", e.Available)
}

// applyPointsMove applies a change of points that a manager made. A change that takes more than the customer
// has available at this place in the log, or that would take a balance past the limit, is refused by every
// reader alike; the service checks first, so that this happens only if something else wrote to the log.
func (s *State) applyPointsMove(ch Change, m PointsMove) (Change, error) {
	key := "points:" + m.ID
	if s.moveSeen[key] {
		return Change{Offset: ch.Offset}, nil // a repeat of a command that was applied
	}
	var balance, held int64
	if a := s.points[m.Customer]; a != nil {
		balance, held = a.balance, a.held
	}
	switch {
	case m.Points < 0 && -m.Points > balance-held:
		return ch, fmt.Errorf("points change %s takes %d points off %s, who has %d available", m.ID, -m.Points, m.Customer, balance-held)
	case m.Points > 0 && balance+m.Points > order.MaxPoints:
		return ch, fmt.Errorf("points change %s would take %s past %d points", m.ID, m.Customer, order.MaxPoints)
	}
	a := s.account(m.Customer)
	a.balance += m.Points
	a.adjusted += m.Points
	a.note(PointsEntry{At: m.At, Kind: PointsAdjusted, Points: m.Points, By: m.By, Reason: m.Reason})
	s.moveSeen[key] = true
	return ch, nil
}

// AdjustPoints gives a customer points (a positive number), or takes points off them (a negative one), and
// writes why. It is the one way to change a balance other than by an order. cmd is the ID of the command, so
// sending it again after a lost answer changes nothing the second time.
//
// A manager cannot change their own points. The program must be on. Whether the customer exists is for the
// caller to check (it needs the accounts).
func (s *Service) AdjustPoints(ctx context.Context, actor rbac.Actor, cmd, customer string, points int64, reason string) (PointsAnswer, error) {
	if err := validCommand(cmd); err != nil {
		return PointsAnswer{}, err
	}
	if err := actor.Validate(); err != nil {
		return PointsAnswer{}, err
	}
	if !actor.Can(rbac.AdjustPoints) {
		return PointsAnswer{}, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, rbac.AdjustPoints)
	}
	if !s.loyalty.On() {
		return PointsAnswer{}, ErrNoPoints
	}
	customer = strings.ToLower(strings.TrimSpace(customer))
	switch {
	case customer == "":
		return PointsAnswer{}, fmt.Errorf("%w: say whose points", order.ErrInvalid)
	case customer == actor.ID:
		return PointsAnswer{}, fmt.Errorf("%w: you cannot change your own points", order.ErrNotAllowed)
	case points == 0 || points > MaxAdjust || points < -MaxAdjust:
		return PointsAnswer{}, fmt.Errorf("%w: a change is 1 to %d points", order.ErrInvalid, MaxAdjust)
	case !validPointsReason(reason):
		return PointsAnswer{}, fmt.Errorf("%w: a change of points", order.ErrReasonRequired)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFresh(ctx); err != nil {
		return PointsAnswer{}, err
	}
	if s.st.moveSeen["points:"+cmd] {
		return s.pointsAnswer(customer), nil // a repeat of a command that worked
	}
	if avail := s.st.AvailablePoints(customer); points < 0 && -points > avail {
		if avail < 0 {
			avail = 0
		}
		return PointsAnswer{}, &PointsShortError{Available: avail}
	}
	mv := PointsMove{ID: cmd, By: actor.ID, Roles: append([]rbac.Role(nil), actor.Roles...), At: s.at(),
		Customer: customer, Points: points, Reason: strings.TrimSpace(reason)}
	if err := s.appendRecord(ctx, Record{Points: &mv}); err != nil {
		return PointsAnswer{}, err
	}
	return s.pointsAnswer(customer), nil
}
