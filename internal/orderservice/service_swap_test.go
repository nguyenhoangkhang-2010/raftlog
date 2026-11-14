package orderservice

import (
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// A menu like the test menu, but with the toastie renamed and dearer, and a new item.
func changedMenu(t *testing.T) *menu.Menu {
	t.Helper()
	m, err := menu.New([]menu.Item{
		{ID: "coffee", Name: "Coffee", Category: "drinks", Station: "drinks", Price: 35000, Active: true,
			Recipe: menu.Recipe{"beans": 20, "milk": 150},
			Groups: []menu.Group{{ID: "size", Name: "Size", Min: 1, Max: 1, Options: []menu.Option{
				{ID: "s", Name: "Small"},
				{ID: "l", Name: "Large", Delta: 10000, Recipe: menu.Recipe{"beans": 10}},
			}}}},
		{ID: "toastie", Name: "Cheese toastie", Category: "food", Station: "hot", Price: 60000, Active: true,
			Recipe: menu.Recipe{"bread": 4, "cheese": 80}},
		{ID: "tea", Name: "Peach tea", Category: "drinks", Station: "drinks", Price: 30000, Active: true,
			Recipe: menu.Recipe{"leaves": 2}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAnOrderKeepsWhatItWasSoldForWhenTheMenuChanges(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	o := r.place(customer, toastie(2))
	o = r.pay(o.ID)

	if err := r.svc.SetMenu(changedMenu(t), nil); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.Order(customer, order.ID(o.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 90000 || got.Lines[0].Name != "Toastie" || got.Lines[0].Unit != 45000 {
		t.Fatalf("the order after the change: %+v", got)
	}
	// Cancelling gives back what the order took, by the recipe it was made with (2 x 2 bread), not by the new one.
	before := r.qty("bread")
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(o.ID), r.cmd(), "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(o.ID), r.cmd(), 90000); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if after := r.qty("bread"); after-before != 4 {
		t.Fatalf("bread given back: %d, want 4 as the recipe was when the order was taken", after-before)
	}
	// And the same is true after a restart that begins from the new menu.
	r2 := *r
	r2.svc = func() *Service {
		svc, err := Open(r.ctx, r.log, Options{Menu: changedMenu(t), Promos: testPromos(t), Location: time.UTC, Now: r.clk.now})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}()
	again, err := r2.svc.Order(customer, order.ID(o.ID))
	if err != nil || again.Total != 90000 || again.Lines[0].Name != "Toastie" {
		t.Fatalf("after a restart on the new menu: %+v %v", again, err)
	}
	// An order taken now has the new price.
	r2.receive("leaves", 10)
	r2.receive("bread", 50)
	r2.receive("cheese", 500)
	if n := r2.place(customer, toastie(1)); n.Total != 60000 {
		t.Fatalf("a new order: %+v", n)
	}
}

func TestANewMenuMakesItsIngredientsKnownAndForgetsThoseItDropped(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.MoveStock(r.ctx, storekeeper, r.cmd(), stock.Receive, "leaves", 5, ""); err == nil {
		t.Fatal("an ingredient that no recipe uses was received")
	}
	if err := r.svc.SetMenu(changedMenu(t), nil); err != nil {
		t.Fatal(err)
	}
	r.receive("leaves", 5)
	found := false
	for _, ing := range r.svc.Ingredients() {
		found = found || ing == "leaves"
	}
	if !found {
		t.Fatalf("Ingredients does not list the new one: %v", r.svc.Ingredients())
	}
}

func TestThresholdsOfTheOldMenuDoNotStayWhenTheNewOneHasNone(t *testing.T) {
	r := newRig(t) // the rig's menu starts with beans running low at 100
	r.receive("beans", 50)
	low := func() bool {
		v, _ := r.svc.Stock(manager)
		for _, l := range v.Levels {
			if l.Ingredient == "beans" {
				return l.Low
			}
		}
		return false
	}
	if !low() {
		t.Fatal("50 beans with a threshold of 100 is not low")
	}
	if err := r.svc.SetMenu(changedMenu(t), nil); err != nil {
		t.Fatal(err)
	}
	if low() {
		t.Fatal("the threshold of the old menu stayed after a menu with none")
	}
	if err := r.svc.SetMenu(changedMenu(t), map[stock.Ingredient]int64{"beans": 80}); err != nil {
		t.Fatal(err)
	}
	if !low() {
		t.Fatal("a new threshold of 80 was not applied: 50 beans is low")
	}
}

func TestSwappingTheMenuTellsEverySubscriber(t *testing.T) {
	r := newRig(t)
	sub := r.svc.Subscribe(customer)
	defer sub.Close()
	if err := r.svc.SetMenu(changedMenu(t), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ch := <-sub.C:
		if !ch.MenuChanged {
			t.Fatalf("a change without MenuChanged: %+v", ch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the subscriber was not told")
	}
	if err := r.svc.SetMenu(nil, nil); err == nil {
		t.Fatal("a nil menu was accepted")
	}
	if err := r.svc.SetPromotions(nil); err == nil {
		t.Fatal("a nil set of promotions was accepted")
	}
}
