package orderservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// The menu, the promotions and the people below are invented for these tests.

var (
	customer    = rbac.Actor{ID: "cus-1", Roles: []rbac.Role{rbac.Customer}}
	otherCust   = rbac.Actor{ID: "cus-2", Roles: []rbac.Role{rbac.Customer}}
	cashier     = rbac.Actor{ID: "cash-1", Roles: []rbac.Role{rbac.Cashier}}
	drinksCook  = rbac.Actor{ID: "k-drinks", Roles: []rbac.Role{rbac.Kitchen}, Station: "drinks"}
	hotCook     = rbac.Actor{ID: "k-hot", Roles: []rbac.Role{rbac.Kitchen}, Station: "hot"}
	manager     = rbac.Actor{ID: "mgr-1", Roles: []rbac.Role{rbac.Manager}}
	storekeeper = rbac.Actor{ID: "store-1", Roles: []rbac.Role{rbac.Storekeeper}}
	runner      = rbac.Actor{ID: "run-1", Roles: []rbac.Role{rbac.Runner}}
)

func testMenu(t testing.TB) *menu.Menu {
	t.Helper()
	m, err := menu.New([]menu.Item{
		{ID: "coffee", Name: "Coffee", Category: "drinks", Station: "drinks", Price: 35000, Active: true,
			Recipe: menu.Recipe{"beans": 20, "milk": 150},
			Groups: []menu.Group{{ID: "size", Name: "Size", Min: 1, Max: 1, Options: []menu.Option{
				{ID: "s", Name: "Small"},
				{ID: "l", Name: "Large", Delta: 10000, Recipe: menu.Recipe{"beans": 10}},
			}}}},
		{ID: "toastie", Name: "Toastie", Category: "food", Station: "hot", Price: 45000, Active: true,
			Recipe: menu.Recipe{"bread": 2, "cheese": 40}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func testPromos(t testing.TB) *promo.Set {
	t.Helper()
	s, err := promo.NewSet([]promo.Promotion{{ID: "save10", Name: "Ten percent", Kind: promo.PercentOff, Percent: 1000, Code: "SAVE10", SingleUse: true}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

type rig struct {
	t   *testing.T
	svc *Service
	log *MemoryLog
	clk *clock
	ctx context.Context
	n   int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, log: &MemoryLog{}, clk: &clock{t: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}, ctx: context.Background()}
	r.svc = r.open(r.log)
	return r
}

func (r *rig) open(l Log) *Service {
	r.t.Helper()
	svc, err := Open(r.ctx, l, Options{Menu: testMenu(r.t), Promos: testPromos(r.t), Location: time.UTC, Now: r.clk.now,
		LowAt: map[stock.Ingredient]int64{"beans": 100}})
	if err != nil {
		r.t.Fatal(err)
	}
	return svc
}

// cmd returns a command ID that has not been used.
func (r *rig) cmd() string {
	r.n++
	return fmt.Sprintf("cmd-%06d", r.n)
}

func (r *rig) receive(ing string, qty int64) {
	r.t.Helper()
	if _, err := r.svc.MoveStock(r.ctx, storekeeper, r.cmd(), stock.Receive, ing, qty, ""); err != nil {
		r.t.Fatalf("receive %s: %v", ing, err)
	}
}

func (r *rig) stockAll() {
	for ing, q := range map[string]int64{"beans": 1000, "milk": 5000, "bread": 100, "cheese": 2000} {
		r.receive(ing, q)
	}
}

func coffee(qty int, size string) menu.Selection {
	return menu.Selection{Item: "coffee", Quantity: qty, Options: menu.Choice{"size": {menu.OptionID(size)}}}
}

func toastie(qty int) menu.Selection { return menu.Selection{Item: "toastie", Quantity: qty} }

func (r *rig) place(a rbac.Actor, sels ...menu.Selection) OrderView {
	r.t.Helper()
	p, err := r.svc.Place(r.ctx, a, PlaceRequest{Command: r.cmd(), Selections: sels})
	if err != nil {
		r.t.Fatalf("place: %v", err)
	}
	return p.Order
}

func (r *rig) pay(id string) OrderView {
	r.t.Helper()
	v, err := r.svc.Pay(r.ctx, cashier, order.ID(id), r.cmd(), order.PayInput{Method: order.Card, Reference: "slip-1"})
	if err != nil {
		r.t.Fatalf("pay: %v", err)
	}
	return v
}

func (r *rig) qty(ing string) int64 {
	r.t.Helper()
	v, err := r.svc.Stock(manager)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, l := range v.Levels {
		if l.Ingredient == ing {
			return l.Qty
		}
	}
	r.t.Fatalf("no ingredient %s in %+v", ing, v)
	return 0
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAnOrderGoesFromTheMenuToTheCustomerAndTakesItsIngredients(t *testing.T) {
	r := newRig(t)
	r.stockAll()

	v := r.place(customer, coffee(2, "l"), toastie(1))
	if v.Number != 1 || v.Status != "placed" || v.Total != 2*45000+45000 || v.Customer != "cus-1" || v.Channel != "app" {
		t.Fatalf("placed order = %+v", v)
	}
	if got := r.qty("beans"); got != 1000 {
		t.Fatalf("beans before payment = %d: the stock must be taken at payment, not before", got)
	}

	v = r.pay(v.ID)
	if v.Status != "paid" || v.Payment == nil || v.Payment.Method != "card" {
		t.Fatalf("paid order = %+v", v)
	}
	// 2 large coffees: 2*(20+10) beans and 2*150 milk; 1 toastie: 2 bread, 40 cheese.
	for ing, want := range map[string]int64{"beans": 940, "milk": 4700, "bread": 98, "cheese": 1960} {
		if got := r.qty(ing); got != want {
			t.Errorf("%s = %d, want %d", ing, got, want)
		}
	}

	id := order.ID(v.ID)
	if _, err := r.svc.StartTicket(r.ctx, drinksCook, id, r.cmd(), "drinks"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.StartTicket(r.ctx, hotCook, id, r.cmd(), "hot"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.FinishTicket(r.ctx, drinksCook, id, r.cmd(), "drinks"); err != nil {
		t.Fatal(err)
	}
	v, err := r.svc.FinishTicket(r.ctx, hotCook, id, r.cmd(), "hot")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "ready" {
		t.Fatalf("after every station finished: %s", v.Status)
	}
	v, err = r.svc.HandOver(r.ctx, runner, id, r.cmd())
	if err != nil || v.Status != "completed" {
		t.Fatalf("hand over: %v %+v", err, v)
	}
	if v.Events != 7 {
		t.Fatalf("%d events, want 7 (placed, paid, 2 started, 2 ready, handed over); the stock receipts are not events of the order", v.Events)
	}
}

func TestPlacingTheSameCommandAgainReturnsTheSameOrderAndWritesNothing(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	req := PlaceRequest{Command: "place-cmd-0001", Selections: []menu.Selection{coffee(1, "s")}}
	first, err := r.svc.Place(r.ctx, customer, req)
	if err != nil {
		t.Fatal(err)
	}
	n := r.log.Len()
	again, err := r.svc.Place(r.ctx, customer, req)
	if err != nil {
		t.Fatal(err)
	}
	if again.Order.ID != first.Order.ID || again.Order.Number != first.Order.Number || r.log.Len() != n {
		t.Fatalf("the repeat made another order or wrote to the log: %+v, log %d -> %d", again.Order, n, r.log.Len())
	}
	// Somebody else sending the same command ID is not told about the order.
	if _, err := r.svc.Place(r.ctx, otherCust, req); !errors.Is(err, order.ErrNotYours) {
		t.Fatalf("another customer repeating the command: %v", err)
	}
}

func TestACommandIDMustBeSafeToKeep(t *testing.T) {
	r := newRig(t)
	for _, bad := range []string{"", "short", "has space in it", "slash/slash/slash", string(make([]byte, 80))} {
		if _, err := r.svc.Place(r.ctx, customer, PlaceRequest{Command: bad, Selections: []menu.Selection{coffee(1, "s")}}); !errors.Is(err, ErrBadCommandID) {
			t.Errorf("command %q: %v", bad, err)
		}
	}
}

func TestAnItemThatCannotBeMadeIsSoldOutAndAPaymentThatTheStockCannotCoverIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	r := newRig(t)
	// Nothing received yet: the coffee cannot be made, so it cannot be ordered.
	_, err := r.svc.Place(r.ctx, customer, PlaceRequest{Command: r.cmd(), Selections: []menu.Selection{coffee(1, "s")}})
	var ce *CartError
	if !errors.As(err, &ce) || !errors.Is(ce.Lines[0].Err, menu.ErrSoldOut) {
		t.Fatalf("ordering with an empty stock: %v", err)
	}

	// Stock for one coffee and one toastie; two customers order a coffee each, and the
	// second one's payment finds the beans gone.
	r.receive("beans", 20)
	r.receive("milk", 150)
	a := r.place(customer, coffee(1, "s"))
	b := r.place(otherCust, coffee(1, "s"))
	r.pay(a.ID)
	n := r.log.Len()
	_, err = r.svc.Pay(r.ctx, cashier, order.ID(b.ID), r.cmd(), order.PayInput{Method: order.Cash, Tendered: 100000})
	var oos *OutOfStockError
	if !errors.As(err, &oos) || len(oos.Shortages) != 2 {
		t.Fatalf("paying with no stock left: %v", err)
	}
	if r.log.Len() != n {
		t.Fatal("a payment that was refused was written to the log")
	}
	if v, _ := r.svc.Order(cashier, order.ID(b.ID)); v.Status != "placed" {
		t.Fatalf("the refused payment changed the order to %s", v.Status)
	}

	// The goods arrive and the same payment goes through.
	r.receive("beans", 20)
	r.receive("milk", 150)
	if v, err := r.svc.Pay(r.ctx, cashier, order.ID(b.ID), r.cmd(), order.PayInput{Method: order.Cash, Tendered: 100000}); err != nil || v.Status != "paid" || v.Payment.Change != 65000 {
		t.Fatalf("paying after the goods arrived: %v %+v", err, v)
	}
}

func TestRebuildingFromTheLogGivesTheSameOrdersTheSameStockAndTheNextNumber(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, coffee(1, "l"))
	b := r.place(otherCust, toastie(2))
	r.pay(a.ID)
	if _, err := r.svc.StartTicket(r.ctx, drinksCook, order.ID(a.ID), r.cmd(), "drinks"); err != nil {
		t.Fatal(err)
	}
	r.pay(b.ID)
	if _, err := r.svc.RequestCancel(r.ctx, otherCust, order.ID(b.ID), r.cmd(), "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(b.ID), r.cmd(), 90000); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.MoveStock(r.ctx, manager, r.cmd(), stock.WriteOff, "milk", 100, "spilled"); err != nil {
		t.Fatal(err)
	}

	again := r.open(r.log)
	if jsonOf(t, again.Orders(manager, OrderFilter{})) != jsonOf(t, r.svc.Orders(manager, OrderFilter{})) {
		t.Fatal("the orders differ after rebuilding")
	}
	s1, _ := r.svc.Stock(manager)
	s2, _ := again.Stock(manager)
	if jsonOf(t, s1) != jsonOf(t, s2) {
		t.Fatalf("the stock differs after rebuilding: %v and %v", s1, s2)
	}
	if again.Health() != r.svc.Health() {
		t.Fatalf("health %+v and %+v", again.Health(), r.svc.Health())
	}
	// The next order gets the next number, not number 1.
	p, err := again.Place(r.ctx, customer, PlaceRequest{Command: "after-restart-1", Selections: []menu.Selection{toastie(1)}})
	if err != nil || p.Order.Number != 3 {
		t.Fatalf("the first order after a restart: %v %+v", err, p.Order)
	}
}

func TestCancellingBeforeTheKitchenStartsGivesTheIngredientsBackAndAfterwardsDoesNot(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	// Not started: the ingredients return.
	a := r.place(customer, toastie(1))
	r.pay(a.ID)
	if r.qty("bread") != 98 {
		t.Fatalf("bread after payment = %d", r.qty("bread"))
	}
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(a.ID), r.cmd(), "too long"); err != nil {
		t.Fatal(err)
	}
	v, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(a.ID), r.cmd(), 45000)
	if err != nil || v.Status != "cancelled" || v.Refunded != 45000 {
		t.Fatalf("approve: %v %+v", err, v)
	}
	if r.qty("bread") != 100 || r.qty("cheese") != 2000 {
		t.Fatalf("the ingredients did not come back: bread %d cheese %d", r.qty("bread"), r.qty("cheese"))
	}

	// Started: the ingredients are used.
	b := r.place(customer, toastie(1))
	r.pay(b.ID)
	if _, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(b.ID), r.cmd(), "hot"); err != nil {
		t.Fatal(err)
	}
	// While a request waits, the kitchen cannot go on.
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(b.ID), r.cmd(), "changed mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.FinishTicket(r.ctx, hotCook, order.ID(b.ID), r.cmd(), "hot"); !errors.Is(err, order.ErrCancelPending) {
		t.Fatalf("finishing while a cancellation waits: %v", err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(b.ID), r.cmd(), 0); err != nil {
		t.Fatal(err)
	}
	if r.qty("bread") != 98 {
		t.Fatalf("bread after a cancellation once work began = %d, want 98", r.qty("bread"))
	}
}

func TestWhoMayDoWhat(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, coffee(1, "s"))
	id := order.ID(a.ID)

	if _, err := r.svc.Order(otherCust, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another customer reading the order: %v", err)
	}
	if _, err := r.svc.Order(customer, id); err != nil {
		t.Fatalf("the customer reading their own order: %v", err)
	}
	if _, err := r.svc.Pay(r.ctx, customer, id, r.cmd(), order.PayInput{Method: order.Card, Reference: "x"}); !errors.Is(err, order.ErrNotAllowed) {
		t.Fatalf("a customer paying: %v", err)
	}
	r.pay(a.ID)
	if _, err := r.svc.StartTicket(r.ctx, hotCook, id, r.cmd(), "drinks"); !errors.Is(err, order.ErrWrongStation) {
		t.Fatalf("a cook of another station: %v", err)
	}
	if _, err := r.svc.RequestCancel(r.ctx, customer, id, r.cmd(), "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, cashier, id, r.cmd(), 0); !errors.Is(err, order.ErrNotAllowed) {
		t.Fatalf("a cashier approving a cancellation: %v", err)
	}
	if _, err := r.svc.Events(cashier, id); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a cashier reading the audit trail: %v", err)
	}
	evs, err := r.svc.Events(manager, id)
	if err != nil || len(evs) != 3 || evs[0].Type != "order.placed" || evs[1].By != "cash-1" {
		t.Fatalf("the audit trail: %v %+v", err, evs)
	}
	if _, err := r.svc.Stock(customer); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a customer reading the stock: %v", err)
	}
	if _, err := r.svc.MoveStock(r.ctx, cashier, r.cmd(), stock.Receive, "beans", 5, ""); !errors.Is(err, order.ErrNotAllowed) {
		t.Fatalf("a cashier receiving goods: %v", err)
	}
	if _, err := r.svc.MoveStock(r.ctx, kitchenWorker(), r.cmd(), stock.WriteOff, "beans", 5, "dropped"); !errors.Is(err, order.ErrNotAllowed) {
		t.Fatalf("a cook writing off goods: %v", err)
	}
}

func kitchenWorker() rbac.Actor { return drinksCook }

func TestCustomersSeeOnlyTheirOwnOrdersInTheList(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	r.place(customer, coffee(1, "s"))
	r.place(otherCust, coffee(1, "s"))
	r.place(customer, toastie(1))
	if got := r.svc.Orders(customer, OrderFilter{}); len(got) != 2 || got[0].Number != 3 || got[1].Number != 1 {
		t.Fatalf("the customer's list: %+v", got)
	}
	if got := r.svc.Orders(manager, OrderFilter{}); len(got) != 3 {
		t.Fatalf("the manager's list has %d orders", len(got))
	}
}

func TestAStationSeesTheOrdersWithWorkWaitingForIt(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, coffee(1, "s"), toastie(1))
	r.pay(a.ID)
	b := r.place(customer, toastie(1))
	r.pay(b.ID)
	waiting := func(station string) []int {
		var out []int
		for _, v := range r.svc.Orders(drinksCook, OrderFilter{Station: station}) {
			out = append(out, v.Number)
		}
		return out
	}
	if got := waiting("drinks"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("drinks waiting: %v", got)
	}
	if got := waiting("hot"); !reflect.DeepEqual(got, []int{2, 1}) {
		t.Fatalf("hot waiting: %v", got)
	}
	_, _ = r.svc.StartTicket(r.ctx, drinksCook, order.ID(a.ID), r.cmd(), "drinks")
	_, _ = r.svc.FinishTicket(r.ctx, drinksCook, order.ID(a.ID), r.cmd(), "drinks")
	if got := waiting("drinks"); len(got) != 0 {
		t.Fatalf("drinks waiting after it finished: %v", got)
	}
}

func TestPromotionCodesApplyOnceForACustomerAndSurviveARestart(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	req := func(cmd string) PlaceRequest {
		return PlaceRequest{Command: cmd, Selections: []menu.Selection{toastie(2)}, Codes: []string{"save10"}}
	}
	first, err := r.svc.Place(r.ctx, customer, req("promo-cmd-0001"))
	if err != nil || first.Order.Discount != 9000 || first.Order.Total != 81000 || len(first.Order.Discounts) != 1 {
		t.Fatalf("with the code: %v %+v", err, first.Order)
	}
	second, err := r.svc.Place(r.ctx, customer, req("promo-cmd-0002"))
	if err != nil || second.Order.Discount != 0 || len(second.Rejected) != 1 {
		t.Fatalf("the same customer using the single-use code again: %v %+v rejected %v", err, second.Order, second.Rejected)
	}
	// The record of the redemption is in the log, so a rebuilt service knows it too.
	again := r.open(r.log)
	third, err := again.Place(r.ctx, customer, req("promo-cmd-0003"))
	if err != nil || third.Order.Discount != 0 {
		t.Fatalf("after a restart: %v %+v", err, third.Order)
	}
	other, err := again.Place(r.ctx, otherCust, req("promo-cmd-0004"))
	if err != nil || other.Order.Discount != 9000 {
		t.Fatalf("another customer with the code: %v %+v", err, other.Order)
	}
}

func TestStockMovesAreCheckedBeforeTheyAreWrittenAndRepeatsAreHarmless(t *testing.T) {
	r := newRig(t)
	r.receive("beans", 100)
	n := r.log.Len()

	if _, err := r.svc.MoveStock(r.ctx, storekeeper, r.cmd(), stock.WriteOff, "beans", 101, "spilled"); err == nil {
		t.Fatal("writing off more than there is was accepted")
	}
	if _, err := r.svc.MoveStock(r.ctx, storekeeper, r.cmd(), stock.WriteOff, "beans", 5, ""); err == nil {
		t.Fatal("a write-off without a reason was accepted")
	}
	if _, err := r.svc.MoveStock(r.ctx, storekeeper, r.cmd(), stock.Receive, "gold-dust", 5, ""); !errors.Is(err, ErrUnknownIngredient) {
		t.Fatalf("an ingredient that no item uses: %v", err)
	}
	if r.log.Len() != n {
		t.Fatalf("a refused movement was written to the log (%d -> %d entries)", n, r.log.Len())
	}

	cmd := r.cmd()
	if _, err := r.svc.MoveStock(r.ctx, storekeeper, cmd, stock.Receive, "beans", 50, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.MoveStock(r.ctx, storekeeper, cmd, stock.Receive, "beans", 50, ""); err != nil {
		t.Fatal(err)
	}
	if got := r.qty("beans"); got != 150 {
		t.Fatalf("a movement repeated with the same command ID counted twice: %d", got)
	}
	if v, err := r.svc.MoveStock(r.ctx, manager, r.cmd(), stock.Count, "beans", 140, "stocktake"); err != nil || r.qty("beans") != 140 || len(v.Levels) == 0 {
		t.Fatalf("a stocktake: %v", err)
	}
}

func TestAnAnswerThatWasLostDoesNotMakeAPaymentCountTwice(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, toastie(1))
	id := order.ID(a.ID)

	cmd := r.cmd()
	r.log.LoseAnswerNext = errors.New("connection reset")
	_, err := r.svc.Pay(r.ctx, cashier, id, cmd, order.PayInput{Method: order.Card, Reference: "slip"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("the lost answer: %v", err)
	}
	if !r.svc.Health().Stale {
		t.Fatal("the service does not know that its state may be behind the log")
	}
	// The cashier presses the button again, with the same command ID.
	v, err := r.svc.Pay(r.ctx, cashier, id, cmd, order.PayInput{Method: order.Card, Reference: "slip"})
	if err != nil || v.Status != "paid" {
		t.Fatalf("the retry: %v %+v", err, v)
	}
	if got := r.qty("bread"); got != 98 {
		t.Fatalf("bread = %d: the payment was counted twice (or not at all)", got)
	}
	if r.svc.Health().Stale {
		t.Fatal("the service is still stale after reading the log")
	}
}

func TestAReadFromANodeThatIsBehindDoesNotMakeAPaymentCountTwice(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, toastie(1))
	id := order.ID(a.ID)
	stored := r.log.Len()

	cmd := r.cmd()
	r.log.LoseAnswerNext = errors.New("timeout")
	if _, err := r.svc.Pay(r.ctx, cashier, id, cmd, order.PayInput{Method: order.Card, Reference: "slip"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	// The payment is in the log, but the node that answers reads has not got it yet.
	r.log.HideAfter, r.log.HideReads = stored, 1
	v, err := r.svc.Pay(r.ctx, cashier, id, cmd, order.PayInput{Method: order.Card, Reference: "slip"})
	if err != nil || v.Status != "paid" {
		t.Fatalf("the retry: %v %+v", err, v)
	}
	// The log now holds the payment twice. Applying it is the same as applying it once.
	again := r.open(r.log)
	if got, _ := again.Stock(manager); jsonOf(t, got) != jsonOf(t, mustStock(t, r.svc)) {
		t.Fatalf("the rebuilt stock differs")
	}
	if h := again.Health(); h.Skipped != 0 {
		t.Fatalf("the duplicate was counted as a record that did not fit: %+v", h)
	}
	for _, l := range mustStock(t, again).Levels {
		if l.Ingredient == "bread" && l.Qty != 98 {
			t.Fatalf("bread = %d after the duplicate, want 98", l.Qty)
		}
	}
	if v, _ := again.Order(cashier, id); v.Status != "paid" || v.Events != 2 {
		t.Fatalf("the order after the duplicate: %+v", v)
	}
}

func mustStock(t *testing.T, s *Service) StockView {
	t.Helper()
	v, err := s.Stock(manager)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAFailedAppendLeavesNothingBehind(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, toastie(1))
	n := r.log.Len()
	r.log.FailNext = errors.New("no leader")
	if _, err := r.svc.Pay(r.ctx, cashier, order.ID(a.ID), r.cmd(), order.PayInput{Method: order.Card, Reference: "slip"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if r.log.Len() != n {
		t.Fatal("something was written")
	}
	if v, _ := r.svc.Order(cashier, order.ID(a.ID)); v.Status != "placed" || r.qty("bread") != 100 {
		t.Fatalf("the failed payment changed state: %+v bread %d", v, r.qty("bread"))
	}
	// And the next attempt works.
	if v := r.pay(a.ID); v.Status != "paid" {
		t.Fatal("the retry did not pay")
	}
}

func TestRecordsThatDoNotFitAreSkippedAndCountedTheSameWayByEveryReader(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.place(customer, toastie(1))
	r.pay(a.ID)

	// Something else wrote to the log: garbage, an event for an order that does not exist,
	// and a record of a version this code does not know.
	ghost, err := order.Place(order.Meta{Cmd: "ghost-cmd-001", Actor: cashier, At: r.clk.now()}, order.PlaceInput{})
	_ = ghost
	if err == nil {
		t.Fatal("an order without an ID was made")
	}
	_, _ = r.log.Append(r.ctx, []byte("not json"))
	_, _ = r.log.Append(r.ctx, []byte(`{"order":{"id":"x","order":"ord-nobody","seq":2,"type":"order.paid"}}`))
	_, _ = r.log.Append(r.ctx, []byte(`{"future":true}`))

	one, two := r.open(r.log), r.open(r.log)
	if one.Health().Skipped != 3 || two.Health().Skipped != 3 {
		t.Fatalf("skipped %d and %d, want 3", one.Health().Skipped, two.Health().Skipped)
	}
	if jsonOf(t, one.Orders(manager, OrderFilter{})) != jsonOf(t, two.Orders(manager, OrderFilter{})) {
		t.Fatal("two readers of the same log disagree")
	}
	if v, err := one.Order(manager, order.ID(a.ID)); err != nil || v.Status != "paid" {
		t.Fatalf("the real order after the garbage: %v %+v", err, v)
	}
	// And the service goes on.
	if _, err := one.Place(r.ctx, customer, PlaceRequest{Command: "after-garbage-1", Selections: []menu.Selection{toastie(1)}}); err != nil {
		t.Fatal(err)
	}
}

func TestOrdersAreNumberedInTheOrderOfTheLogUnderConcurrentLoad(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	const workers, each = 8, 6
	var wg sync.WaitGroup
	numbers := make(chan int, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				cmd := fmt.Sprintf("load-%02d-%02d-cmd", w, i)
				p, err := r.svc.Place(r.ctx, customer, PlaceRequest{Command: cmd, Selections: []menu.Selection{toastie(1)}})
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := r.svc.Pay(r.ctx, cashier, order.ID(p.Order.ID), cmd+"-pay", order.PayInput{Method: order.Card, Reference: "r"}); err != nil {
					t.Error(err)
					return
				}
				numbers <- p.Order.Number
			}
		}(w)
	}
	wg.Wait()
	close(numbers)
	seen := map[int]bool{}
	for n := range numbers {
		if seen[n] {
			t.Fatalf("number %d given twice", n)
		}
		seen[n] = true
	}
	if len(seen) != workers*each {
		t.Fatalf("%d numbers for %d orders", len(seen), workers*each)
	}
	if got := r.qty("bread"); got != 100-2*workers*each {
		t.Fatalf("bread = %d, want %d", got, 100-2*workers*each)
	}
	again := r.open(r.log)
	if jsonOf(t, again.Orders(manager, OrderFilter{})) != jsonOf(t, r.svc.Orders(manager, OrderFilter{})) {
		t.Fatal("rebuilding after the load gives different orders")
	}
}

func TestSubscribersSeeOnlyWhatTheyMaySee(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	mine, theirs, kitchen, store := r.svc.Subscribe(customer), r.svc.Subscribe(otherCust), r.svc.Subscribe(drinksCook), r.svc.Subscribe(storekeeper)
	defer mine.Close()
	defer theirs.Close()
	defer kitchen.Close()
	defer store.Close()

	a := r.place(customer, toastie(1))
	r.pay(a.ID)
	r.receive("bread", 10)

	drain := func(s *Subscription) (orders, stockChanges int) {
		for {
			select {
			case ch := <-s.C:
				if ch.Order != "" {
					orders++
				}
				if ch.StockChanged {
					stockChanges++
				}
			default:
				return
			}
		}
	}
	if o, s := drain(mine); o != 2 || s != 0 {
		t.Errorf("the customer saw %d order changes and %d stock changes, want 2 and 0", o, s)
	}
	if o, s := drain(theirs); o != 0 || s != 0 {
		t.Errorf("another customer saw %d and %d, want 0 and 0", o, s)
	}
	if o, s := drain(kitchen); o != 2 || s != 2 {
		t.Errorf("the kitchen saw %d order changes and %d stock changes, want 2 and 2 (the payment and the receipt)", o, s)
	}
	if o, s := drain(store); o != 0 || s != 2 {
		t.Errorf("the storekeeper saw %d and %d, want 0 and 2", o, s)
	}
}

func TestASubscriberThatDoesNotKeepUpIsClosedInsteadOfHoldingUpTheService(t *testing.T) {
	r := newRig(t)
	r.receive("bread", 100)
	r.receive("cheese", 1000)
	slow := r.svc.Subscribe(manager)
	for i := 0; i < subscriberBuffer+10; i++ {
		r.place(customer, toastie(1))
	}
	n := 0
	for range slow.C { // closed by the hub, so this ends
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("the slow subscriber got %d changes before it was closed, want %d", n, subscriberBuffer)
	}
}

func TestAnOrderTakenAtTheCounterHasNoCustomerUnlessOneIsGiven(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	p, err := r.svc.Place(r.ctx, cashier, PlaceRequest{Command: "counter-cmd-01", Selections: []menu.Selection{toastie(1)}})
	if err != nil || p.Order.Channel != "counter" || p.Order.Customer != "" {
		t.Fatalf("a walk-in: %v %+v", err, p.Order)
	}
	p, err = r.svc.Place(r.ctx, cashier, PlaceRequest{Command: "counter-cmd-02", Selections: []menu.Selection{toastie(1)}, Customer: "cus-1"})
	if err != nil || p.Order.Customer != "cus-1" {
		t.Fatalf("a customer at the counter: %v %+v", err, p.Order)
	}
	// A customer cannot order for someone else.
	p, err = r.svc.Place(r.ctx, customer, PlaceRequest{Command: "counter-cmd-03", Selections: []menu.Selection{toastie(1)}, Customer: "cus-2"})
	if err != nil || p.Order.Customer != "cus-1" {
		t.Fatalf("a customer naming another customer: %v %+v", err, p.Order)
	}
}

func TestTheMenuShowsPricesAndWhatIsSoldOut(t *testing.T) {
	r := newRig(t)
	r.receive("bread", 10)
	r.receive("cheese", 100)
	v := r.svc.Menu()
	if len(v.Items) != 2 || v.Categories[0] != "drinks" {
		t.Fatalf("menu = %+v", v)
	}
	for _, it := range v.Items {
		switch it.ID {
		case "coffee":
			if !it.SoldOut || it.Price != 35000 || len(it.Groups) != 1 || it.Groups[0].Options[1].Delta != 10000 {
				t.Errorf("coffee = %+v: sold out because no beans were received", it)
			}
		case "toastie":
			if it.SoldOut {
				t.Errorf("toastie is marked sold out with bread and cheese in stock")
			}
		}
	}
}

func TestAnUnknownOrderIsNotFound(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.Pay(r.ctx, cashier, "ord-nothing", r.cmd(), order.PayInput{Method: order.Card, Reference: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("paying a missing order: %v", err)
	}
	if _, err := r.svc.Order(manager, "ord-nothing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading a missing order: %v", err)
	}
}

func TestMoneyInTheViewsIsWholeDong(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	v := r.place(customer, coffee(3, "l"))
	if v.Subtotal != int64(money.Money(45000).Times(3)) || v.Lines[0].Unit != 45000 || v.Lines[0].Pieces[0].Station != "drinks" {
		t.Fatalf("view = %+v", v)
	}
}
