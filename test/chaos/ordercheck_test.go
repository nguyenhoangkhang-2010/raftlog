package chaos

import (
	"strings"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/orderservice"
)

func sampleMenu(t *testing.T) *menu.Menu {
	t.Helper()
	m, _, err := orderservice.LoadMenu("../../examples/kitchen/menu.json")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var received = map[string]int64{"coffee-beans": 2000, "condensed-milk": 2000, "ice": 10000, "baguette": 50, "pate": 500, "cucumber": 500, "egg": 20}

func copyStock(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyOrders(os []orderservice.OrderView) []orderservice.OrderView {
	out := make([]orderservice.OrderView, len(os))
	for i, o := range os {
		o.Tickets = append([]orderservice.TicketView(nil), o.Tickets...)
		o.Lines = append([]orderservice.LineView(nil), o.Lines...)
		if o.Payment != nil {
			p := *o.Payment
			o.Payment = &p
		}
		out[i] = o
	}
	return out
}

// healthy is a small run in which every promise holds: order A is taken through its whole life
// by khach-1, and order B, of khach-2, is cancelled before it is paid.
func healthy() ([]OrderRun, OrdersFinal) {
	placedAt := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC) // a Wednesday morning: no happy hour
	runs := []OrderRun{
		{Worker: 1, Customer: "khach-1", Items: []PlanItem{{Item: "ca-phe-sua", Size: "l", Qty: 2}, {Item: "banh-mi", Qty: 1}},
			Place: CmdRec{ID: "w1-0001-place", Kind: CmdPlace, Outcome: Acked},
			Steps: []CmdRec{
				{ID: "w1-0001-pay", Kind: CmdPay, Outcome: Acked},
				{ID: "w1-0001-s1", Kind: CmdStart, Station: "drinks", Outcome: Acked},
				{ID: "w1-0001-s2", Kind: CmdStart, Station: "hot", Outcome: Acked},
				{ID: "w1-0001-f1", Kind: CmdFinish, Station: "drinks", Outcome: Acked},
				{ID: "w1-0001-f2", Kind: CmdFinish, Station: "hot", Outcome: Acked},
				{ID: "w1-0001-ho", Kind: CmdHandOver, Outcome: Acked},
			}},
		{Worker: 2, Customer: "khach-2", Items: []PlanItem{{Item: "banh-mi-trung", Qty: 1}},
			Place: CmdRec{ID: "w2-0001-place", Kind: CmdPlace, Outcome: Acked},
			Steps: []CmdRec{{ID: "w2-0001-cancel", Kind: CmdCancel, Outcome: Acked}}},
	}
	a := orderservice.OrderView{
		ID: "ord-w1-0001-place", Number: 1, Customer: "khach-1", Status: "completed", Total: 99000, PlacedAt: placedAt,
		Lines:   []orderservice.LineView{{Name: "Cà phê sữa đá", Quantity: 2}, {Name: "Bánh mì thịt", Quantity: 1}},
		Payment: &orderservice.PaymentView{Method: "card", Amount: 99000},
		Tickets: []orderservice.TicketView{{Station: "drinks", Status: "ready"}, {Station: "hot", Status: "ready"}},
	}
	b := orderservice.OrderView{
		ID: "ord-w2-0001-place", Number: 2, Customer: "khach-2", Status: "cancelled", Total: 30000, PlacedAt: placedAt.Add(time.Minute),
		Lines:   []orderservice.LineView{{Name: "Bánh mì trứng", Quantity: 1}},
		Tickets: []orderservice.TicketView{{Station: "hot", Status: "pending"}},
	}
	stock := copyStock(received)
	for ing, q := range map[string]int64{"coffee-beans": 48, "condensed-milk": 80, "ice": 300, "baguette": 1, "pate": 20, "cucumber": 15} {
		stock[ing] -= q
	}
	orders := []orderservice.OrderView{a, b}
	return runs, OrdersFinal{
		Live: orders, Fresh: copyOrders(orders), Rebuilt: copyOrders(orders),
		LiveStock: stock, FreshStock: copyStock(stock), RebuiltStock: copyStock(stock),
		Received: copyStock(received),
	}
}

func kindList(vs []Violation) string {
	var ks []string
	for _, v := range vs {
		ks = append(ks, v.Kind)
	}
	return strings.Join(ks, ",")
}

func TestAHealthyKitchenRunHasNoViolations(t *testing.T) {
	runs, fin := healthy()
	if vs := CheckOrders(runs, fin, sampleMenu(t), nil); len(vs) != 0 {
		t.Fatalf("violations in a healthy run: %v", vs)
	}
}

func TestTheOrderCheckerNoticesEachKindOfBrokenPromise(t *testing.T) {
	cases := []struct {
		name   string
		want   string
		tweak  func(runs []OrderRun, fin *OrdersFinal)
		strict bool // the violation must be the only kind reported
	}{
		{"an acknowledged order is gone", KindOrderLost, func(_ []OrderRun, f *OrdersFinal) { f.Live = f.Live[:1] }, false},
		{"an order that nobody placed", KindPhantomOrder, func(_ []OrderRun, f *OrdersFinal) {
			f.Live = append(f.Live, orderservice.OrderView{ID: "ord-ghost", Number: 3, Status: "placed"})
		}, false},
		{"a refused order exists", KindRefusedOrder, func(r []OrderRun, _ *OrdersFinal) { r[1].Place.Outcome = Rejected; r[1].Place.Status = 422 }, true},
		{"an order of another customer", KindWrongCustomer, func(_ []OrderRun, f *OrdersFinal) { f.Live[0].Customer = "khach-2" }, false},
		{"other lines than asked", KindWrongLines, func(_ []OrderRun, f *OrdersFinal) { f.Live[0].Lines[0].Quantity = 3 }, false},
		{"a total that is not the price", KindWrongTotal, func(_ []OrderRun, f *OrdersFinal) { f.Live[0].Total++; f.Live[0].Payment.Amount++ }, false},
		{"a number given twice", KindNumberReused, func(_ []OrderRun, f *OrdersFinal) { f.Live[1].Number = 1 }, false},
		{"an acknowledged payment missing", KindPaymentLost, func(_ []OrderRun, f *OrdersFinal) { f.Live[0].Payment = nil }, false},
		{"a payment that is not the total", KindPaymentAmount, func(_ []OrderRun, f *OrdersFinal) { f.Live[0].Payment.Amount-- }, false},
		{"a started ticket that is pending", KindStepLost, func(_ []OrderRun, f *OrdersFinal) { f.Live[0].Tickets[0].Status = "pending" }, false},
		{"a handed-over order that is not completed", KindStepLost, func(_ []OrderRun, f *OrdersFinal) { f.Live[0].Status = "ready" }, false},
		{"an acknowledged cancellation that did not happen", KindStepLost, func(_ []OrderRun, f *OrdersFinal) { f.Live[1].Status = "placed" }, false},
		{"a command refused that should have worked", KindUnexpectedRefuse, func(r []OrderRun, _ *OrdersFinal) { r[0].Steps[1].Outcome = Rejected; r[0].Steps[1].Status = 409 }, false},
		{"stock that is one too many", KindStockWrong, func(_ []OrderRun, f *OrdersFinal) { f.LiveStock["baguette"]++ }, false},
		{"stock that is below zero", KindStockNegative, func(_ []OrderRun, f *OrdersFinal) { f.LiveStock["egg"] = -1 }, false},
		{"a live state that differs from the log", KindLiveDiffers, func(_ []OrderRun, f *OrdersFinal) { f.Rebuilt[0].Status = "ready" }, true},
		{"a fresh service that differs", KindFreshDiffers, func(_ []OrderRun, f *OrdersFinal) { f.FreshStock["pate"]++ }, true},
		{"records that were skipped", KindRecordsSkipped, func(_ []OrderRun, f *OrdersFinal) { f.Skipped = 1 }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runs, fin := healthy()
			c.tweak(runs, &fin)
			vs := CheckOrders(runs, fin, sampleMenu(t), nil)
			found := false
			for _, v := range vs {
				found = found || v.Kind == c.want
			}
			if !found {
				t.Fatalf("%s was not reported; got [%s]", c.want, kindList(vs))
			}
			if c.strict && len(vs) != 1 {
				t.Fatalf("expected only %s, got [%s]", c.want, kindList(vs))
			}
		})
	}
}

func TestACancelledOrderThatWasPaidGivesTheStockBackOnlyIfWorkHadNotStarted(t *testing.T) {
	m := sampleMenu(t)
	for _, started := range []bool{false, true} {
		runs, fin := healthy()
		// Order B was paid, then cancelled by an approved request.
		runs[1].Steps = []CmdRec{{ID: "w2-0001-pay", Kind: CmdPay, Outcome: Acked}, {ID: "w2-0001-approve", Kind: CmdCancelApprove, Outcome: Acked}}
		fin.Live[1].Payment = &orderservice.PaymentView{Method: "card", Amount: 30000}
		if started {
			fin.Live[1].Tickets[0].Status = "started"
			fin.LiveStock["baguette"]--
			fin.LiveStock["egg"]--
		}
		fin.Fresh, fin.Rebuilt = copyOrders(fin.Live), copyOrders(fin.Live)
		fin.FreshStock, fin.RebuiltStock = copyStock(fin.LiveStock), copyStock(fin.LiveStock)
		if vs := CheckOrders(runs, fin, m, nil); len(vs) != 0 {
			t.Fatalf("work started = %v: %v", started, vs)
		}
		// And the same stock is wrong in the other case.
		wrong := copyStock(fin.LiveStock)
		if started {
			wrong["egg"]++
		} else {
			wrong["egg"]--
		}
		fin.LiveStock = wrong
		if vs := CheckOrders(runs, fin, m, nil); !strings.Contains(kindList(vs), KindStockWrong) {
			t.Fatalf("work started = %v: a wrong stock was not noticed", started)
		}
	}
}

func TestARefusalForLackOfStockIsAllowedOnlyWhereTheScenarioExpectsIt(t *testing.T) {
	runs, fin := healthy()
	runs[0].Steps[0] = CmdRec{ID: "w1-0001-pay", Kind: CmdPay, Outcome: Rejected, Status: 409, Code: "out_of_stock"}
	// The order was not paid: nothing of the later steps happened either.
	fin.Live[0].Payment, fin.Live[0].Status = nil, "placed"
	for i := range fin.Live[0].Tickets {
		fin.Live[0].Tickets[i].Status = "pending"
	}
	runs[0].Steps = runs[0].Steps[:1]
	fin.LiveStock = copyStock(received)
	fin.Fresh, fin.Rebuilt = copyOrders(fin.Live), copyOrders(fin.Live)
	fin.FreshStock, fin.RebuiltStock = copyStock(fin.LiveStock), copyStock(fin.LiveStock)

	m := sampleMenu(t)
	if vs := CheckOrders(runs, fin, m, map[string]bool{"out_of_stock": true}); len(vs) != 0 {
		t.Fatalf("an expected refusal was reported: %v", vs)
	}
	if vs := CheckOrders(runs, fin, m, nil); !strings.Contains(kindList(vs), KindUnexpectedRefuse) {
		t.Fatalf("an unexpected refusal was not reported: %v", vs)
	}
}

func TestAnOrderThatNeverReachedTheLogIsNotAPromise(t *testing.T) {
	runs, fin := healthy()
	runs = append(runs, OrderRun{Worker: 3, Customer: "khach-1", Items: []PlanItem{{Item: "banh-mi", Qty: 1}},
		Place: CmdRec{ID: "w3-0001-place", Kind: CmdPlace, Outcome: Unknown}})
	if vs := CheckOrders(runs, fin, sampleMenu(t), nil); len(vs) != 0 {
		t.Fatalf("an order with an unknown outcome that is not in the log was reported: %v", vs)
	}
}

func TestAnOrderWithAnUnknownOutcomeThatIsInTheLogIsCheckedLikeAnyOther(t *testing.T) {
	runs, fin := healthy()
	runs[1].Place.Outcome = Unknown
	if vs := CheckOrders(runs, fin, sampleMenu(t), nil); len(vs) != 0 {
		t.Fatalf("an order that was stored although its answer was lost: %v", vs)
	}
	fin.Live[1].Customer = "khach-1"
	if vs := CheckOrders(runs, fin, sampleMenu(t), nil); !strings.Contains(kindList(vs), KindWrongCustomer) {
		t.Fatal("an unknown-outcome order with the wrong customer was not noticed")
	}
}

func TestTheCheckerComparesTheChangeAndTheRefundWithWhatTheClientsSent(t *testing.T) {
	m := sampleMenu(t)

	// Order A was paid in cash: 100,000 handed over for 99,000.
	runs, fin := healthy()
	runs[0].Steps[0].Amount = 100000
	fin.Live[0].Payment.Method, fin.Live[0].Payment.Tendered, fin.Live[0].Payment.Change = "cash", 100000, 1000
	fin.Fresh, fin.Rebuilt = copyOrders(fin.Live), copyOrders(fin.Live)
	if vs := CheckOrders(runs, fin, m, nil); len(vs) != 0 {
		t.Fatalf("a correct cash payment: %v", vs)
	}
	fin.Live[0].Payment.Change = 1001
	fin.Fresh, fin.Rebuilt = copyOrders(fin.Live), copyOrders(fin.Live)
	if vs := CheckOrders(runs, fin, m, nil); !strings.Contains(kindList(vs), KindChangeWrong) {
		t.Fatalf("a wrong change was not noticed: %v", vs)
	}

	// Order B was cancelled after payment, with a refund of 15,000 set by the manager.
	runs, fin = healthy()
	runs[1].Steps = []CmdRec{{ID: "w2-0001-pay", Kind: CmdPay, Outcome: Acked}, {ID: "w2-0001-ok", Kind: CmdCancelApprove, Outcome: Acked, Amount: 15000}}
	fin.Live[1].Payment = &orderservice.PaymentView{Method: "card", Amount: 30000}
	fin.Live[1].Tickets[0].Status = "started"
	fin.Live[1].Refunded = 15000
	fin.LiveStock["baguette"]--
	fin.LiveStock["egg"]--
	fin.Fresh, fin.Rebuilt = copyOrders(fin.Live), copyOrders(fin.Live)
	fin.FreshStock, fin.RebuiltStock = copyStock(fin.LiveStock), copyStock(fin.LiveStock)
	if vs := CheckOrders(runs, fin, m, nil); len(vs) != 0 {
		t.Fatalf("a correct refund: %v", vs)
	}
	fin.Live[1].Refunded = 30000
	fin.Fresh, fin.Rebuilt = copyOrders(fin.Live), copyOrders(fin.Live)
	if vs := CheckOrders(runs, fin, m, nil); !strings.Contains(kindList(vs), KindRefundWrong) {
		t.Fatalf("a wrong refund was not noticed: %v", vs)
	}
}
