package chaos

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/orderservice"
)

// This file is the checker of the kitchen scenarios. The clients are people at the till, in the
// kitchen and in the app: each takes an order through its life, one command at a time, and sends
// a command again, with the same ID, when it gets no answer. What they were told is the history;
// what the service and the log hold afterwards is the final state. The checker says which
// promise was broken.
//
// The promises, for orders that were placed by the clients and nothing else:
//
//   - an order whose placing was acknowledged exists, with the customer, the lines and the total
//     it should have, and no order exists that nobody placed;
//   - every acknowledged step is in the order: a payment is there and is for the whole total, a
//     started ticket is started, a handed-over order is completed, an approved cancellation is
//     cancelled; nothing a client was refused outright happened;
//   - order numbers are not given twice;
//   - the stock is what was received, minus what the orders that were paid and not cancelled
//     before work began took, computed from the menu and the clients' own record of what they
//     ordered, and no ingredient is below zero;
//   - the service's view of the state, the state that a new service builds from the log, and the
//     state that this test builds from the raw log, are all the same, and no record of the log
//     was skipped.

// Command kinds.
const (
	CmdPlace         = "place"
	CmdPay           = "pay"
	CmdStart         = "start"
	CmdFinish        = "finish"
	CmdHandOver      = "handover"
	CmdCancel        = "cancel" // an order that was not paid
	CmdCancelRequest = "cancel-request"
	CmdCancelApprove = "cancel-approve"
	CmdCancelReject  = "cancel-reject"
)

// PlanItem is one line the client ordered.
type PlanItem struct {
	Item string
	Size string // the option of the "size" group, if the item has one
	Qty  int
}

// CmdRec is one command, with what the client knows about it. A command sent several times with
// the same ID is one record: Acked if any attempt got a success answer, Rejected if one got a
// definite refusal, Unknown if none got an answer.
type CmdRec struct {
	ID      string
	Kind    string
	Station string // for start and finish
	Outcome Outcome
	Status  int    // the HTTP status of the refusal, for Rejected
	Code    string // the code of the refusal, such as "out_of_stock"
	// Amount is the cash handed over, for a payment in cash, and the refund the manager set, for an
	// approved cancellation. 0 when the command has none.
	Amount int64
}

// OrderRun is one order as one client took it through its life.
type OrderRun struct {
	Worker   int
	Customer string
	Items    []PlanItem
	Place    CmdRec
	Steps    []CmdRec
}

// OrderID is the ID of the order that the placing command makes.
func (r OrderRun) OrderID() string { return "ord-" + r.Place.ID }

// OrdersFinal is what the service and the log hold after the faults.
type OrdersFinal struct {
	// Live is what the service that ran during the faults says; Fresh what a new service that
	// read the log afterwards says; Rebuilt what this test built from the raw log.
	Live, Fresh, Rebuilt                []orderservice.OrderView
	LiveStock, FreshStock, RebuiltStock map[string]int64
	// Received is the goods whose receipt was acknowledged.
	Received map[string]int64
	// Skipped is how many records of the log the live service skipped (healthz).
	Skipped, FreshSkipped int
	// RebuiltSkipped is how many records the test's own rebuild skipped.
	RebuiltSkipped int
}

// Violation kinds of the kitchen scenarios.
const (
	KindOrderLost        = "acknowledged-order-missing"
	KindPhantomOrder     = "order-nobody-placed"
	KindRefusedOrder     = "refused-order-exists"
	KindWrongCustomer    = "order-of-the-wrong-customer"
	KindWrongLines       = "order-with-other-lines-than-asked"
	KindWrongTotal       = "order-total-is-not-the-price-of-the-lines"
	KindNumberReused     = "order-number-given-twice"
	KindPaymentLost      = "acknowledged-payment-missing"
	KindPaymentAmount    = "payment-is-not-the-total"
	KindChangeWrong      = "cash-change-is-not-what-was-handed-over-minus-the-total"
	KindRefundWrong      = "refund-is-not-what-the-manager-set"
	KindStepLost         = "acknowledged-step-missing"
	KindUnexpectedRefuse = "command-refused-that-should-have-worked"
	KindStockWrong       = "stock-is-not-what-the-orders-imply"
	KindStockNegative    = "stock-below-zero"
	KindLiveDiffers      = "live-state-differs-from-the-log"
	KindFreshDiffers     = "fresh-service-differs-from-the-live-one"
	KindRecordsSkipped   = "records-of-the-log-were-skipped"
)

// CheckOrders reports every broken promise. tolerate lists the refusal codes that a scenario
// expects on a payment (a scarce stock makes "out_of_stock" a legitimate answer).
func CheckOrders(runs []OrderRun, fin OrdersFinal, m *menu.Menu, tolerate map[string]bool) []Violation {
	var out []Violation
	add := func(kind, format string, args ...any) {
		out = append(out, Violation{Kind: kind, Detail: fmt.Sprintf(format, args...)})
	}

	live := map[string]orderservice.OrderView{}
	for _, o := range fin.Live {
		live[o.ID] = o
	}
	asked := map[string]bool{}
	for _, r := range runs {
		asked[r.OrderID()] = true
	}

	// The orders that exist were all placed by somebody.
	for _, o := range fin.Live {
		if !asked[o.ID] {
			add(KindPhantomOrder, "%s (number %d) is in the state, but no client sent that command", o.ID, o.Number)
		}
	}

	// Numbers.
	byNumber := map[int][]string{}
	for _, o := range fin.Live {
		byNumber[o.Number] = append(byNumber[o.Number], o.ID)
	}
	for n, ids := range byNumber {
		if len(ids) > 1 {
			sort.Strings(ids)
			add(KindNumberReused, "number %d is on %d orders: %v", n, len(ids), ids)
		}
	}

	for _, r := range runs {
		o, exists := live[r.OrderID()]
		switch {
		case r.Place.Outcome == Acked && !exists:
			add(KindOrderLost, "%s: placing %s was acknowledged but there is no such order", r.Customer, r.Place.ID)
			continue
		case r.Place.Outcome == Rejected && exists:
			add(KindRefusedOrder, "%s: placing was refused (HTTP %d %s) but the order exists", r.Place.ID, r.Place.Status, r.Place.Code)
			continue
		case !exists:
			continue // an order that never reached the log, and was never promised
		}
		checkOrder(r, o, m, tolerate, add)
	}

	// Stock.
	expect := map[string]int64{}
	for ing, q := range fin.Received {
		expect[ing] = q
	}
	for _, r := range runs {
		o, ok := live[r.OrderID()]
		if !ok || o.Payment == nil {
			continue
		}
		restocked := o.Status == "cancelled" && !startedWork(o)
		if restocked {
			continue
		}
		for ing, q := range needs(m, r.Items) {
			expect[ing] -= q
		}
	}
	for ing, want := range expect {
		if got := fin.LiveStock[ing]; got != want {
			add(KindStockWrong, "%s: the service says %d, the orders imply %d (received %d)", ing, got, want, fin.Received[ing])
		}
	}
	for ing, got := range fin.LiveStock {
		if got < 0 {
			add(KindStockNegative, "%s is %d", ing, got)
		}
		if _, ok := expect[ing]; !ok && got != 0 {
			add(KindStockWrong, "%s: the service says %d, and nothing of it was received", ing, got)
		}
	}

	// The three views of the state.
	if d := differ(fin.Live, fin.LiveStock, fin.Rebuilt, fin.RebuiltStock); d != "" {
		add(KindLiveDiffers, "the service and the state rebuilt from the raw log: %s", d)
	}
	if d := differ(fin.Live, fin.LiveStock, fin.Fresh, fin.FreshStock); d != "" {
		add(KindFreshDiffers, "%s", d)
	}
	if fin.Skipped != 0 || fin.FreshSkipped != 0 || fin.RebuiltSkipped != 0 {
		add(KindRecordsSkipped, "%d (live), %d (fresh), %d (rebuilt) records did not fit and were skipped", fin.Skipped, fin.FreshSkipped, fin.RebuiltSkipped)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func startedWork(o orderservice.OrderView) bool {
	for _, t := range o.Tickets {
		if t.Status != "pending" {
			return true
		}
	}
	return false
}

func checkOrder(r OrderRun, o orderservice.OrderView, m *menu.Menu, tolerate map[string]bool, add func(kind, format string, args ...any)) {
	if o.Customer != r.Customer {
		add(KindWrongCustomer, "%s belongs to %q, the client was %q", o.ID, o.Customer, r.Customer)
	}
	if want, ok := expectedLines(r.Items); ok {
		got := map[string]int{}
		for _, l := range o.Lines {
			got[l.Name] += l.Quantity
		}
		if !sameQuantities(got, want, m) {
			add(KindWrongLines, "%s: asked for %v, the order has %v", o.ID, r.Items, o.Lines)
		}
	}
	if want, ok := expectedTotal(m, r.Items, o.PlacedAt); ok && o.Total != want {
		add(KindWrongTotal, "%s: total %d, the lines cost %d", o.ID, o.Total, want)
	}
	if o.Payment != nil && o.Payment.Amount != o.Total {
		add(KindPaymentAmount, "%s: paid %d for a total of %d", o.ID, o.Payment.Amount, o.Total)
	}

	ticket := func(station string) string {
		for _, t := range o.Tickets {
			if t.Station == station {
				return t.Status
			}
		}
		return ""
	}
	for _, s := range r.Steps {
		switch s.Outcome {
		case Rejected:
			if s.Kind == CmdPay && tolerate[s.Code] {
				continue
			}
			add(KindUnexpectedRefuse, "%s: %s %s was refused (HTTP %d %s)", o.ID, s.Kind, s.Station, s.Status, s.Code)
		case Acked:
			switch s.Kind {
			case CmdPay:
				if o.Payment == nil {
					add(KindPaymentLost, "%s: the payment %s was acknowledged and the order has none (status %s)", o.ID, s.ID, o.Status)
				} else if o.Payment.Method == "cash" && s.Amount != 0 && (o.Payment.Tendered != s.Amount || o.Payment.Change != s.Amount-o.Total) {
					add(KindChangeWrong, "%s: %d handed over for %d, the order says %d tendered and %d change", o.ID, s.Amount, o.Total, o.Payment.Tendered, o.Payment.Change)
				}
			case CmdStart:
				if st := ticket(s.Station); st == "pending" || st == "" {
					add(KindStepLost, "%s: starting %s was acknowledged and the ticket is %q", o.ID, s.Station, st)
				}
			case CmdFinish:
				if st := ticket(s.Station); st != "ready" {
					add(KindStepLost, "%s: finishing %s was acknowledged and the ticket is %q", o.ID, s.Station, st)
				}
			case CmdHandOver:
				if o.Status != "completed" {
					add(KindStepLost, "%s: the hand-over was acknowledged and the order is %s", o.ID, o.Status)
				}
			case CmdCancel, CmdCancelApprove:
				if o.Status != "cancelled" {
					add(KindStepLost, "%s: the %s was acknowledged and the order is %s", o.ID, s.Kind, o.Status)
				} else if s.Kind == CmdCancelApprove && o.Refunded != s.Amount {
					add(KindRefundWrong, "%s: the manager set a refund of %d, the order gave back %d", o.ID, s.Amount, o.Refunded)
				}
			case CmdCancelReject:
				if o.Status == "cancelled" {
					add(KindStepLost, "%s: the cancellation was rejected (acknowledged) and the order is cancelled", o.ID)
				}
			}
		}
	}
}

// expectedLines is what the client asked for, as quantity by item ID.
func expectedLines(items []PlanItem) (map[string]int, bool) {
	out := map[string]int{}
	for _, it := range items {
		out[it.Item] += it.Qty
	}
	return out, len(out) > 0
}

// sameQuantities compares the quantities of the order's lines, by name, with what was asked for.
func sameQuantities(got map[string]int, want map[string]int, m *menu.Menu) bool {
	names := map[string]int{}
	for id, q := range want {
		it, ok := m.Item(menu.ItemID(id))
		if !ok {
			return false
		}
		names[it.Name] += q
	}
	if len(names) != len(got) {
		return false
	}
	for n, q := range names {
		if got[n] != q {
			return false
		}
	}
	return true
}

// expectedTotal is what the lines cost at the time of the order: the item's price at that
// moment plus the price change of the size, for every unit. No promotion is configured in these
// scenarios, so nothing is taken off.
func expectedTotal(m *menu.Menu, items []PlanItem, at time.Time) (int64, bool) {
	var total int64
	for _, pi := range items {
		it, ok := m.Item(menu.ItemID(pi.Item))
		if !ok {
			return 0, false
		}
		unit := int64(it.UnitPrice(at))
		if pi.Size != "" {
			found := false
			for _, g := range it.Groups {
				for _, op := range g.Options {
					if string(g.ID) == "size" && string(op.ID) == pi.Size {
						unit += int64(op.Delta)
						found = true
					}
				}
			}
			if !found {
				return 0, false
			}
		}
		total += unit * int64(pi.Qty)
	}
	return total, true
}

// needs is the ingredients that the items of an order use.
func needs(m *menu.Menu, items []PlanItem) map[string]int64 {
	out := map[string]int64{}
	for _, pi := range items {
		it, ok := m.Item(menu.ItemID(pi.Item))
		if !ok {
			continue
		}
		for ing, q := range it.Recipe {
			out[string(ing)] += q * int64(pi.Qty)
		}
		if pi.Size == "" {
			continue
		}
		for _, g := range it.Groups {
			for _, op := range g.Options {
				if string(g.ID) == "size" && string(op.ID) == pi.Size {
					for ing, q := range op.Recipe {
						out[string(ing)] += q * int64(pi.Qty)
					}
				}
			}
		}
	}
	return out
}

// differ says how two states differ, or "" if they are the same.
func differ(aOrders []orderservice.OrderView, aStock map[string]int64, bOrders []orderservice.OrderView, bStock map[string]int64) string {
	index := func(os []orderservice.OrderView) map[string]string {
		out := map[string]string{}
		for _, o := range os {
			raw, _ := json.Marshal(o)
			out[o.ID] = string(raw)
		}
		return out
	}
	a, b := index(aOrders), index(bOrders)
	var ids []string
	for id := range a {
		ids = append(ids, id)
	}
	for id := range b {
		if _, ok := a[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		switch {
		case a[id] == "":
			return fmt.Sprintf("%s is only in the second", id)
		case b[id] == "":
			return fmt.Sprintf("%s is only in the first", id)
		case a[id] != b[id]:
			return fmt.Sprintf("%s differs:\n  first:  %s\n  second: %s", id, a[id], b[id])
		}
	}
	for ing, q := range aStock {
		if bStock[ing] != q {
			return fmt.Sprintf("stock of %s: %d against %d", ing, q, bStock[ing])
		}
	}
	for ing, q := range bStock {
		if _, ok := aStock[ing]; !ok && q != 0 {
			return fmt.Sprintf("stock of %s: nothing against %d", ing, q)
		}
	}
	return ""
}
