package orderservice

import (
	"sort"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// Money in the views is a whole number of đồng.

// PieceView is one thing a station makes.
type PieceView struct {
	Station string `json:"station"`
	Name    string `json:"name"`
}

// LineView is one line of an order as it was taken.
type LineView struct {
	Name     string      `json:"name"`
	Options  []string    `json:"options,omitempty"`
	Unit     int64       `json:"unit"`
	Quantity int         `json:"quantity"`
	Total    int64       `json:"total"`
	Note     string      `json:"note,omitempty"`
	Pieces   []PieceView `json:"pieces,omitempty"`
}

// DiscountView is a promotion that took effect.
type DiscountView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Amount int64  `json:"amount"`
}

// PaymentView is how the order was paid.
type PaymentView struct {
	Method    string    `json:"method"`
	Amount    int64     `json:"amount"`
	Tendered  int64     `json:"tendered,omitempty"`
	Change    int64     `json:"change,omitempty"`
	Reference string    `json:"reference,omitempty"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

// TicketView is the work of one station.
type TicketView struct {
	Station   string     `json:"station"`
	Status    string     `json:"status"`
	StartedBy string     `json:"started_by,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	ReadyBy   string     `json:"ready_by,omitempty"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`
}

// CancelRequestView is a cancellation that waits for a manager.
type CancelRequestView struct {
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// OrderView is an order for the API.
type OrderView struct {
	ID       string `json:"id"`
	Number   int    `json:"number"`
	Channel  string `json:"channel"`
	Customer string `json:"customer,omitempty"`
	Status   string `json:"status"`

	Lines     []LineView     `json:"lines"`
	Discounts []DiscountView `json:"discounts,omitempty"`
	Subtotal  int64          `json:"subtotal"`
	Discount  int64          `json:"discount"`
	Total     int64          `json:"total"`

	PlacedAt      time.Time          `json:"placed_at"`
	Payment       *PaymentView       `json:"payment,omitempty"`
	Tickets       []TicketView       `json:"tickets,omitempty"`
	CancelRequest *CancelRequestView `json:"cancel_request,omitempty"`
	Refunded      int64              `json:"refunded,omitempty"`
	CancelledBy   string             `json:"cancelled_by,omitempty"`
	CancelReason  string             `json:"cancel_reason,omitempty"`
	CompletedBy   string             `json:"completed_by,omitempty"`
	CompletedAt   *time.Time         `json:"completed_at,omitempty"`
	Events        int                `json:"events"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func viewOf(o *order.Order) OrderView {
	v := OrderView{
		ID: string(o.ID), Number: o.Number, Channel: string(o.Channel), Customer: o.Customer, Status: string(o.Status),
		Subtotal: int64(o.Subtotal), Discount: int64(o.Discount), Total: int64(o.Total),
		PlacedAt: o.PlacedAt, Refunded: int64(o.Refunded), CancelledBy: o.CancelledBy, CancelReason: o.CancelReason,
		CompletedBy: o.CompletedBy, CompletedAt: timePtr(o.CompletedAt), Events: o.Seq(),
		Lines: make([]LineView, 0, len(o.Lines)),
	}
	for _, l := range o.Lines {
		lv := LineView{Name: l.Name, Options: l.Options, Unit: int64(l.Unit), Quantity: l.Quantity, Total: int64(l.Total), Note: l.Note}
		for _, p := range l.Pieces {
			lv.Pieces = append(lv.Pieces, PieceView{Station: string(p.Station), Name: p.Name})
		}
		v.Lines = append(v.Lines, lv)
	}
	for _, d := range o.Discounts {
		v.Discounts = append(v.Discounts, DiscountView{ID: string(d.ID), Name: d.Name, Amount: int64(d.Amount)})
	}
	if p := o.Payment; p != nil {
		v.Payment = &PaymentView{Method: p.Method, Amount: int64(p.Amount), Tendered: int64(p.Tendered), Change: int64(p.Change), Reference: p.Reference, By: p.By, At: p.At}
	}
	for _, st := range o.Stations() {
		t := o.Tickets[st]
		v.Tickets = append(v.Tickets, TicketView{
			Station: string(st), Status: string(t.Status), StartedBy: t.StartedBy, StartedAt: timePtr(t.StartedAt),
			ReadyBy: t.ReadyBy, ReadyAt: timePtr(t.ReadyAt),
		})
	}
	if r := o.Request; r != nil {
		v.CancelRequest = &CancelRequestView{By: r.By, Reason: r.Reason, At: r.At}
	}
	return v
}

// StockLevelView is one ingredient in the stock.
type StockLevelView struct {
	Ingredient string `json:"ingredient"`
	Qty        int64  `json:"qty"`
	Low        bool   `json:"low,omitempty"`
}

// StockView is the stock.
type StockView struct {
	Levels []StockLevelView `json:"levels"`
}

func (s *Service) stockView() StockView {
	snap := s.st.stock.Snapshot()
	low := map[stock.Ingredient]bool{}
	for _, ing := range s.st.stock.Low() {
		low[ing] = true
	}
	// Every ingredient the menu uses is listed, also one that has never been received.
	for ing := range s.ingredients {
		if _, ok := snap[ing]; !ok {
			snap[ing] = 0
		}
	}
	v := StockView{Levels: make([]StockLevelView, 0, len(snap))}
	for ing, q := range snap {
		v.Levels = append(v.Levels, StockLevelView{Ingredient: string(ing), Qty: q, Low: low[ing]})
	}
	sort.Slice(v.Levels, func(i, j int) bool { return v.Levels[i].Ingredient < v.Levels[j].Ingredient })
	return v
}

// OptionView is a choice inside a group of options.
type OptionView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Delta int64  `json:"delta"`
}

// GroupView is a set of options of which a customer picks some.
type GroupView struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Min     int          `json:"min"`
	Max     int          `json:"max"`
	Options []OptionView `json:"options"`
}

// ItemView is an item of the menu, with the price it has now.
type ItemView struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Category string      `json:"category"`
	Station  string      `json:"station"`
	Price    int64       `json:"price"`
	Groups   []GroupView `json:"groups,omitempty"`
	SoldOut  bool        `json:"sold_out,omitempty"`
}

// SlotView is a choice inside a combo.
type SlotView struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

// ComboView is a combo of the menu.
type ComboView struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Price int64      `json:"price"`
	Slots []SlotView `json:"slots"`
}

// MenuView is what a customer may order now.
type MenuView struct {
	Categories []string    `json:"categories"`
	Items      []ItemView  `json:"items"`
	Combos     []ComboView `json:"combos,omitempty"`
}

// Menu returns the items and combos that are for sale, with today's prices, and which are
// sold out.
func (s *Service) Menu() MenuView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	at := s.at()
	soldOut := s.st.stock.SoldOut(s.st.menu)
	v := MenuView{Categories: s.st.menu.Categories()}
	for _, it := range s.st.menu.Items() {
		if !it.Active {
			continue
		}
		iv := ItemView{ID: string(it.ID), Name: it.Name, Category: it.Category, Station: string(it.Station), Price: int64(it.UnitPrice(at)), SoldOut: soldOut(it.ID)}
		for _, g := range it.Groups {
			gv := GroupView{ID: string(g.ID), Name: g.Name, Min: g.Min, Max: g.Max}
			for _, o := range g.Options {
				gv.Options = append(gv.Options, OptionView{ID: string(o.ID), Name: o.Name, Delta: int64(o.Delta)})
			}
			iv.Groups = append(iv.Groups, gv)
		}
		v.Items = append(v.Items, iv)
	}
	for _, c := range s.st.menu.Combos() {
		if !c.Active {
			continue
		}
		cv := ComboView{ID: string(c.ID), Name: c.Name, Price: int64(c.Price)}
		for _, sl := range c.Slots {
			slv := SlotView{Name: sl.Name}
			for _, it := range sl.Items {
				slv.Items = append(slv.Items, string(it))
			}
			cv.Slots = append(cv.Slots, slv)
		}
		v.Combos = append(v.Combos, cv)
	}
	return v
}

// OrderFilter selects orders for a list.
type OrderFilter struct {
	// Statuses limits the list to those statuses; empty means all.
	Statuses []order.Status
	// Station limits the list to orders with work waiting at the station: paid or
	// preparing, with a ticket for it that is not ready yet.
	Station string
	Limit   int
}

// Order returns one order, if the actor may see it.
func (s *Service) Order(actor rbac.Actor, id order.ID) (OrderView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o := s.st.Order(id)
	if o == nil || !mayView(actor, o) {
		return OrderView{}, ErrNotFound // not telling a customer that an order of somebody else exists
	}
	return viewOf(o), nil
}

// Orders lists the orders the actor may see, newest first.
func (s *Service) Orders(actor rbac.Actor, f OrderFilter) []OrderView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	want := map[order.Status]bool{}
	for _, st := range f.Statuses {
		want[st] = true
	}
	out := []OrderView{}
	for _, o := range s.st.Orders() {
		if !mayView(actor, o) {
			continue
		}
		if len(want) > 0 && !want[o.Status] {
			continue
		}
		if f.Station != "" {
			t := o.Tickets[menu.Station(f.Station)]
			if t == nil || t.Status == order.TicketReady || (o.Status != order.Paid && o.Status != order.Preparing) {
				continue
			}
		}
		out = append(out, viewOf(o))
		if len(out) == limit {
			break
		}
	}
	return out
}

// EventView is one event of an order, for the audit trail.
type EventView struct {
	Seq   int       `json:"seq"`
	Type  string    `json:"type"`
	At    time.Time `json:"at"`
	By    string    `json:"by"`
	Roles string    `json:"roles"`
}

// Events returns the events of an order, oldest first, to those who may read the audit trail.
func (s *Service) Events(actor rbac.Actor, id order.ID) ([]EventView, error) {
	if !actor.Can(rbac.ViewAudit) {
		return nil, ErrNotAllowed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.st.Order(id) == nil {
		return nil, ErrNotFound
	}
	var out []EventView
	for _, ev := range s.st.Events(id) {
		roles := make([]string, len(ev.Roles))
		for i, r := range ev.Roles {
			roles[i] = string(r)
		}
		out = append(out, EventView{Seq: ev.Seq, Type: string(ev.Type), At: ev.At, By: ev.By, Roles: strings.Join(roles, ",")})
	}
	return out, nil
}

// Stock returns the stock to those who may see it.
func (s *Service) Stock(actor rbac.Actor) (StockView, error) {
	if !actor.Can(rbac.ViewStock) {
		return StockView{}, ErrNotAllowed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stockView(), nil
}
