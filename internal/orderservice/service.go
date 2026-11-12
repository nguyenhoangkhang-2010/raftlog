package orderservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// Errors of the service, besides the ones of the kitchen packages.
var (
	// ErrNotFound: there is no such order.
	ErrNotFound = errors.New("orderservice: no such order")
	// ErrBadCommandID: a command needs an ID that is safe to keep in the log and to retry with.
	ErrBadCommandID = errors.New("orderservice: a command ID is 8 to 64 letters, digits, dots, dashes or underscores")
	// ErrUnavailable: the log did not answer. The command may or may not have been stored;
	// repeating it with the same command ID is safe.
	ErrUnavailable = errors.New("orderservice: the log is not available")
	// ErrConflict: the log did not accept the command as decided (another writer got in
	// first); nothing changed.
	ErrConflict = errors.New("orderservice: the command no longer fits the state of the log")
	// ErrUnknownIngredient: no item of the menu uses that ingredient.
	ErrUnknownIngredient = errors.New("orderservice: no item of the menu uses that ingredient")
	// ErrNotAllowed: the person may not see or do that.
	ErrNotAllowed = errors.New("orderservice: not allowed")
)

var commandID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// OutOfStockError says that paying for the order would take more of some ingredients than
// there are. Nothing was taken.
type OutOfStockError struct {
	Shortages []stock.Shortage
}

func (e *OutOfStockError) Error() string {
	return fmt.Sprintf("orderservice: out of stock for %d ingredient(s)", len(e.Shortages))
}

// CartError lists the lines of a cart that were refused, with the reason for each.
type CartError struct {
	Lines []menu.LineError
}

func (e *CartError) Error() string {
	if len(e.Lines) == 0 {
		return "orderservice: the cart was refused"
	}
	return fmt.Sprintf("orderservice: %d line(s) of the cart were refused; the first: %v", len(e.Lines), e.Lines[0])
}

// Options configure a Service.
type Options struct {
	Menu   *menu.Menu
	Promos *promo.Set
	// LowAt is, for each ingredient, the quantity at or below which it counts as running low.
	LowAt map[stock.Ingredient]int64
	// Location is the venue's time zone: prices by time of day follow its clock.
	Location *time.Location
	// Now is the clock; time.Now if nil.
	Now    func() time.Time
	Logger *slog.Logger
	// CommandTimeout bounds one command, including the wait for the log; 10 s if zero. A
	// command that runs out of time answers "unavailable" and may be repeated.
	CommandTimeout time.Duration
	// ReadBatch is how many log entries are read at a time; 500 if zero.
	ReadBatch int
}

// Service decides commands and keeps the state of the kitchen, built from the log.
//
// There must be one Service writing to a log: it decides a command on the state it holds
// and writes the event, so two writers could decide on state the other has already changed.
// If the log holds a record that does not fit (written by another writer, say), every
// reader skips it alike and counts it; see Health.
type Service struct {
	mu      sync.RWMutex // commands hold it for writing, reads for reading
	st      *State
	log     Log
	now     func() time.Time
	loc     *time.Location
	logger  *slog.Logger
	batch   int
	timeout time.Duration

	ingredients map[stock.Ingredient]bool
	// stale is set when a write to the log failed with an unknown outcome: before the next
	// command the state is brought up to date from the log.
	stale bool

	hub *hub
}

// Open builds the state from the whole log and returns the service.
func Open(ctx context.Context, l Log, o Options) (*Service, error) {
	if o.Menu == nil || o.Promos == nil {
		return nil, errors.New("orderservice: a menu and a set of promotions are needed")
	}
	st, err := NewState(o.Menu, o.Promos, o.LowAt)
	if err != nil {
		return nil, err
	}
	s := &Service{st: st, log: l, now: o.Now, loc: o.Location, logger: o.Logger, batch: o.ReadBatch, timeout: o.CommandTimeout, hub: newHub()}
	if s.now == nil {
		s.now = time.Now
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	if s.logger == nil {
		s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.timeout <= 0 {
		s.timeout = 10 * time.Second
	}
	if s.batch <= 0 {
		s.batch = 500
	}
	s.ingredients = map[stock.Ingredient]bool{}
	for _, it := range o.Menu.Items() {
		for ing := range it.Recipe {
			s.ingredients[ing] = true
		}
		for _, g := range it.Groups {
			for _, op := range g.Options {
				for ing := range op.Recipe {
					s.ingredients[ing] = true
				}
			}
		}
	}
	if err := s.catchUp(ctx, 0); err != nil {
		return nil, fmt.Errorf("reading the log: %w", err)
	}
	return s, nil
}

// at is the current time in the venue's time zone.
func (s *Service) at() time.Time { return s.now().In(s.loc) }

// catchUp reads the log from where the state is up to its end, or, if upTo is not zero,
// until the record at upTo has been applied. It returns the change that record made.
// The caller holds the write lock (or is Open).
func (s *Service) catchUp(ctx context.Context, upTo uint64) error {
	for {
		ents, err := s.log.Read(ctx, s.st.Next(), s.batch, 0)
		if err != nil {
			return err
		}
		if len(ents) == 0 {
			if upTo != 0 && s.st.Next() <= upTo {
				return fmt.Errorf("the log ends at %d, before the record at %d that was stored", s.st.Next(), upTo)
			}
			return nil
		}
		for _, e := range ents {
			if e.Offset != s.st.Next() {
				return fmt.Errorf("the log returned offset %d where %d was expected", e.Offset, s.st.Next())
			}
			s.applyEntry(e)
		}
		if upTo != 0 && s.st.Next() > upTo {
			return nil
		}
	}
}

func (s *Service) applyEntry(e Entry) Change {
	rec, err := DecodeRecord(e.Value)
	var ch Change
	if err != nil {
		ch = Change{Offset: e.Offset, Skipped: err}
		s.st.skip(e.Offset)
	} else {
		ch, _ = s.st.ApplyRecord(e.Offset, rec)
	}
	if ch.Skipped != nil {
		s.logger.Error("a record of the log does not fit and was skipped", "offset", e.Offset, "err", ch.Skipped)
	}
	s.hub.publish(ch)
	return ch
}

// ensureFresh brings the state up to date after a write whose outcome was not known.
func (s *Service) ensureFresh(ctx context.Context) error {
	if !s.stale {
		return nil
	}
	if err := s.catchUp(ctx, 0); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.stale = false
	return nil
}

// appendRecord writes the record and applies it. The caller holds the write lock.
func (s *Service) appendRecord(ctx context.Context, rec Record) error {
	data, err := rec.Encode()
	if err != nil {
		return err
	}
	off, err := s.log.Append(ctx, data)
	if err != nil {
		s.stale = true // it may have been stored; the next command reads the log first
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if off == s.st.Next() {
		ch := s.applyEntry(Entry{Offset: off, Value: data})
		if ch.Skipped != nil {
			return fmt.Errorf("%w: %v", ErrConflict, ch.Skipped)
		}
		return nil
	}
	// Something else came before ours (or this node was behind): read up to ours.
	if err := s.catchUp(ctx, off); err != nil {
		s.stale = true
		return fmt.Errorf("%w: stored at offset %d but not confirmed: %v", ErrUnavailable, off, err)
	}
	return nil
}

func validCommand(id string) error {
	if !commandID.MatchString(id) {
		return ErrBadCommandID
	}
	return nil
}

// staff reports whether the actor takes orders as staff (at the counter), not as a customer.
func staff(a rbac.Actor) bool {
	for _, r := range a.Roles {
		if r != rbac.Customer && rbac.Allowed(r, rbac.PlaceOrder) {
			return true
		}
	}
	return false
}

// mayView says whether the actor may see the order: staff who see all orders, and
// customers their own.
func mayView(a rbac.Actor, o *order.Order) bool {
	for _, r := range a.Roles {
		if r != rbac.Customer && rbac.Allowed(r, rbac.ViewAllOrders) {
			return true
		}
	}
	return a.Can(rbac.ViewOwnOrders) && o.Customer != "" && o.Customer == a.ID
}

// PlaceRequest is an order as the customer or the cashier made it.
type PlaceRequest struct {
	// Command identifies the request. Sending it again, after a lost answer, returns the
	// order that the first one made and does not make another.
	Command    string
	Selections []menu.Selection
	// Codes are promotion codes the customer entered.
	Codes []string
	// Customer is, for an order taken at the counter, the customer's ID if they have one.
	// For an order from the app it is ignored: the customer is the person asking.
	Customer string
}

// Placed is an order that was taken, and the codes that were not used.
type Placed struct {
	Order    OrderView
	Rejected []string
}

// Place prices the cart, applies the promotions and records the order.
func (s *Service) Place(ctx context.Context, actor rbac.Actor, req PlaceRequest) (Placed, error) {
	if err := validCommand(req.Command); err != nil {
		return Placed{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFresh(ctx); err != nil {
		return Placed{}, err
	}

	id := order.ID("ord-" + req.Command)
	if o := s.st.Order(id); o != nil {
		if !mayView(actor, o) {
			return Placed{}, order.ErrNotYours
		}
		return Placed{Order: viewOf(o)}, nil // a repeat of a command that worked
	}

	channel, customer := order.ChannelApp, actor.ID
	if staff(actor) {
		channel, customer = order.ChannelCounter, req.Customer
	}
	at := s.at()
	lines, errs := s.st.menu.PriceCart(req.Selections, menu.PriceOptions{At: at, SoldOut: s.st.stock.SoldOut(s.st.menu)})
	if len(errs) > 0 {
		return Placed{}, &CartError{Lines: errs}
	}
	pricing := s.st.promos.Evaluate(lines, promo.Context{At: at, Codes: req.Codes, Customer: customer, Redeemed: s.st.Redeemed})

	ev, err := order.Place(order.Meta{Cmd: req.Command, Actor: actor, At: at}, order.PlaceInput{
		Order: id, Number: s.st.NextNumber(), Channel: channel, Customer: customer, Lines: lines, Pricing: pricing,
	})
	if err != nil {
		return Placed{}, err
	}
	rec, err := OrderRecord(ev)
	if err != nil {
		return Placed{}, err
	}
	if err := s.appendRecord(ctx, rec); err != nil {
		return Placed{}, err
	}
	res := Placed{Order: viewOf(s.st.Order(id))}
	for _, r := range pricing.Rejected {
		res.Rejected = append(res.Rejected, fmt.Sprintf("%s: %v", r.Code, r.Err))
	}
	return res, nil
}

// command decides one command on an order and records the event it gives. The function f
// is the command method of the order; it returns nil, nil for a command applied before.
func (s *Service) command(ctx context.Context, actor rbac.Actor, id order.ID, cmd string,
	f func(o *order.Order, m order.Meta) (*order.Event, error)) (OrderView, error) {
	if err := validCommand(cmd); err != nil {
		return OrderView{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFresh(ctx); err != nil {
		return OrderView{}, err
	}
	o := s.st.Order(id)
	if o == nil {
		return OrderView{}, ErrNotFound
	}
	ev, err := f(o, order.Meta{Cmd: cmd, Actor: actor, At: s.at()})
	if err != nil {
		return OrderView{}, err
	}
	if ev == nil {
		return viewOf(o), nil
	}
	rec, err := OrderRecord(*ev)
	if err != nil {
		return OrderView{}, err
	}
	if err := s.appendRecord(ctx, rec); err != nil {
		return OrderView{}, err
	}
	return viewOf(s.st.Order(id)), nil
}

// Pay records the payment of the whole total. The ingredients the order needs are taken
// from the stock when the payment is applied, so the payment is refused, before anything
// is written, if the stock cannot cover them.
func (s *Service) Pay(ctx context.Context, actor rbac.Actor, id order.ID, cmd string, in order.PayInput) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) {
		ev, err := o.Pay(m, in)
		if err != nil || ev == nil {
			return ev, err
		}
		if ok, short := s.st.stock.CanMake(o.Needs()); !ok {
			return nil, &OutOfStockError{Shortages: short}
		}
		return ev, nil
	})
}

// StartTicket says that the station has started its part of the order.
func (s *Service) StartTicket(ctx context.Context, actor rbac.Actor, id order.ID, cmd string, st menu.Station) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) { return o.StartTicket(m, st) })
}

// FinishTicket says that the station has finished its part of the order.
func (s *Service) FinishTicket(ctx context.Context, actor rbac.Actor, id order.ID, cmd string, st menu.Station) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) { return o.FinishTicket(m, st) })
}

// HandOver gives the finished order to the customer.
func (s *Service) HandOver(ctx context.Context, actor rbac.Actor, id order.ID, cmd string) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) { return o.HandOver(m) })
}

// CancelUnpaid cancels an order that has not been paid.
func (s *Service) CancelUnpaid(ctx context.Context, actor rbac.Actor, id order.ID, cmd, reason string) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) { return o.CancelUnpaid(m, reason) })
}

// RequestCancel asks for a paid order to be cancelled; a manager decides.
func (s *Service) RequestCancel(ctx context.Context, actor rbac.Actor, id order.ID, cmd, reason string) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) { return o.RequestCancel(m, reason) })
}

// ApproveCancel cancels the order that has a request, and sets the refund.
func (s *Service) ApproveCancel(ctx context.Context, actor rbac.Actor, id order.ID, cmd string, refund money.Money) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) { return o.ApproveCancel(m, refund) })
}

// RejectCancel refuses a request to cancel; the order carries on.
func (s *Service) RejectCancel(ctx context.Context, actor rbac.Actor, id order.ID, cmd, reason string) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) { return o.RejectCancel(m, reason) })
}

// IssueRefund gives money back for a completed order.
func (s *Service) IssueRefund(ctx context.Context, actor rbac.Actor, id order.ID, cmd string, amount money.Money, reason string) (OrderView, error) {
	return s.command(ctx, actor, id, cmd, func(o *order.Order, m order.Meta) (*order.Event, error) {
		return o.IssueRefund(m, amount, reason)
	})
}

// MoveStock records goods received, something written off, or a stocktake. ID is the ID of
// the command. The movement is tried on a copy of the stock first, so that one the stock
// would refuse is never written to the log.
func (s *Service) MoveStock(ctx context.Context, actor rbac.Actor, cmd string, kind stock.Kind, ingredient string, qty int64, reason string) (StockView, error) {
	if err := validCommand(cmd); err != nil {
		return StockView{}, err
	}
	need := rbac.AdjustStock
	if kind == stock.Receive {
		need = rbac.ReceiveStock
	}
	if err := actor.Validate(); err != nil {
		return StockView{}, err
	}
	if !actor.Can(need) {
		return StockView{}, fmt.Errorf("%w: needs %s", order.ErrNotAllowed, need)
	}
	if kind != stock.Receive && kind != stock.WriteOff && kind != stock.Count {
		return StockView{}, fmt.Errorf("orderservice: %q is not a movement a person can make", kind)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureFresh(ctx); err != nil {
		return StockView{}, err
	}
	if s.st.moveSeen[cmd] {
		return s.stockView(), nil // a repeat of a command that worked
	}
	if !s.ingredients[stock.Ingredient(ingredient)] {
		return StockView{}, fmt.Errorf("%w: %q", ErrUnknownIngredient, ingredient)
	}
	mv := StockMove{ID: cmd, By: actor.ID, Roles: append([]rbac.Role(nil), actor.Roles...), At: s.at(), Kind: kind, Ingredient: ingredient, Qty: qty, Reason: reason}
	if err := s.st.stock.Clone().Apply(mv.Movement()); err != nil {
		return StockView{}, err
	}
	if err := s.appendRecord(ctx, Record{Stock: &mv}); err != nil {
		return StockView{}, err
	}
	return s.stockView(), nil
}

// Health says how the service is doing.
type Health struct {
	Orders  int    `json:"orders"`
	Next    uint64 `json:"next_offset"`
	Skipped int    `json:"skipped_records"`
	Stale   bool   `json:"stale"`
}

// Health reports the state of the service.
func (s *Service) Health() Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Health{Orders: len(s.st.orders), Next: s.st.Next(), Skipped: s.st.Skipped(), Stale: s.stale}
}

// Subscribe returns the changes that the actor may see, from now on. Close the subscription
// when done. A subscriber that does not keep up is closed; it reads the state again and
// subscribes again.
//
// What a subscriber receives is masked to what the person may see: the order fields are
// empty for an order that is not theirs to read, and StockChanged is false for somebody who
// may not read the stock. A change with nothing left is not delivered.
func (s *Service) Subscribe(actor rbac.Actor) *Subscription {
	viewAll := false
	for _, r := range actor.Roles {
		if r != rbac.Customer && rbac.Allowed(r, rbac.ViewAllOrders) {
			viewAll = true
		}
	}
	return s.hub.subscribe(func(ch Change) (Change, bool) {
		out := Change{Offset: ch.Offset}
		if ch.Order != "" && (viewAll || (actor.Can(rbac.ViewOwnOrders) && ch.Customer != "" && ch.Customer == actor.ID)) {
			out.Order, out.Number, out.Type, out.Status, out.Customer = ch.Order, ch.Number, ch.Type, ch.Status, ch.Customer
		}
		if ch.StockChanged && actor.Can(rbac.ViewStock) {
			out.StockChanged = true
		}
		return out, out.Applied()
	})
}
