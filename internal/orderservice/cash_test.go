package orderservice

import (
	"reflect"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

var cashier2 = rbac.Actor{ID: "cash-2", Roles: []rbac.Role{rbac.Cashier}}

var (
	farPast   = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// payCash takes the payment of an order in cash, as the given cashier, and returns the order.
func (r *rig) payCash(a rbac.Actor, id string) OrderView {
	r.t.Helper()
	o, err := r.svc.Order(a, order.ID(id))
	if err != nil {
		r.t.Fatal(err)
	}
	v, err := r.svc.Pay(r.ctx, a, order.ID(id), r.cmd(), order.PayInput{Method: order.Cash, Tendered: money.Money(o.Total + 10_000)})
	if err != nil {
		r.t.Fatalf("pay in cash: %v", err)
	}
	return v
}

func TestCashThatAnOrderBroughtInIsCountedForTheCashierWhoTookItAndCardIsNot(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.payCash(cashier, r.place(customer, toastie(1)).ID)  // 45,000 in cash, by cash-1
	b := r.payCash(cashier2, r.place(customer, toastie(2)).ID) // 90,000 in cash, by cash-2
	r.pay(r.place(customer, toastie(1)).ID)                    // by card, by cash-1: not in a drawer
	one := r.svc.Cash(cashier.ID, farPast, farFuture)
	two := r.svc.Cash(cashier2.ID, farPast, farFuture)
	if one.Sales != a.Total || one.Refunds != 0 || len(one.Entries) != 1 || one.Entries[0].Number != a.Number || one.Entries[0].Kind != CashSale {
		t.Fatalf("cash-1: %+v", one)
	}
	if two.Sales != b.Total || two.Net() != b.Total {
		t.Fatalf("cash-2: %+v", two)
	}
	if none := r.svc.Cash("nobody", farPast, farFuture); none.Sales != 0 || len(none.Entries) != 0 || none.Entries == nil {
		t.Fatalf("a cashier who took nothing: %+v", none)
	}
}

func TestACashRefundTakesMoneyOutOfTheDrawerThatTookItAndACardRefundDoesNot(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	// a cancellation of an order paid in cash, with a full refund
	a := r.payCash(cashier, r.place(customer, toastie(1)).ID)
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(a.ID), r.cmd(), "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(a.ID), r.cmd(), money.Money(a.Total)); err != nil {
		t.Fatal(err)
	}
	// a refund of a completed order paid in cash, by another cashier's drawer
	b := r.payCash(cashier2, r.place(customer, toastie(2)).ID)
	for _, step := range []func() error{
		func() error { _, err := r.svc.StartTicket(r.ctx, hotCook, order.ID(b.ID), r.cmd(), "hot"); return err },
		func() error { _, err := r.svc.FinishTicket(r.ctx, hotCook, order.ID(b.ID), r.cmd(), "hot"); return err },
		func() error { _, err := r.svc.HandOver(r.ctx, cashier2, order.ID(b.ID), r.cmd()); return err },
		func() error {
			_, err := r.svc.IssueRefund(r.ctx, manager, order.ID(b.ID), r.cmd(), 20_000, "one cup was wrong")
			return err
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	// a refund of an order paid by card is not cash
	c := r.pay(r.place(customer, toastie(1)).ID)
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(c.ID), r.cmd(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(c.ID), r.cmd(), money.Money(c.Total)); err != nil {
		t.Fatal(err)
	}
	one, two := r.svc.Cash(cashier.ID, farPast, farFuture), r.svc.Cash(cashier2.ID, farPast, farFuture)
	if one.Sales != a.Total || one.Refunds != a.Total || one.Net() != 0 {
		t.Fatalf("cash-1 took %d and gave back %d", one.Sales, one.Refunds)
	}
	if two.Sales != b.Total || two.Refunds != 20_000 || two.Net() != b.Total-20_000 {
		t.Fatalf("cash-2: %+v", two)
	}
	// the refund goes to the cashier who took the cash, not to the manager who approved it
	if m := r.svc.Cash(manager.ID, farPast, farFuture); len(m.Entries) != 0 {
		t.Fatalf("the manager's drawer: %+v", m)
	}
}

func TestOnlyTheCashOfThePeriodIsCountedAndTheEndIsNotIncluded(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	first := r.payCash(cashier, r.place(customer, toastie(1)).ID)
	cut := r.clk.now() // a moment after the first payment
	second := r.payCash(cashier, r.place(customer, toastie(2)).ID)
	third := r.payCash(cashier, r.place(customer, toastie(3)).ID)
	if v := r.svc.Cash(cashier.ID, farPast, cut); v.Sales != first.Total {
		t.Fatalf("before the cut: %d", v.Sales)
	}
	if v := r.svc.Cash(cashier.ID, cut, farFuture); v.Sales != second.Total+third.Total || len(v.Entries) != 2 || v.Entries[0].Number != second.Number {
		t.Fatalf("after the cut: %+v", v)
	}
	// the period is half open: a payment at the very end belongs to the next period
	at := third.Payment.At
	if v := r.svc.Cash(cashier.ID, cut, at); v.Sales != second.Total {
		t.Fatalf("up to the moment of the third payment: %d", v.Sales)
	}
	if v := r.svc.Cash(cashier.ID, at, farFuture); v.Sales != third.Total {
		t.Fatalf("from the moment of the third payment: %d", v.Sales)
	}
}

func TestTheCashIsTheSameAfterTheServiceIsRebuiltFromTheLog(t *testing.T) {
	r := newRig(t)
	r.stockAll()
	a := r.payCash(cashier, r.place(customer, toastie(1)).ID)
	r.payCash(cashier2, r.place(customer, toastie(2)).ID)
	if _, err := r.svc.RequestCancel(r.ctx, customer, order.ID(a.ID), r.cmd(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ApproveCancel(r.ctx, manager, order.ID(a.ID), r.cmd(), money.Money(a.Total)); err != nil {
		t.Fatal(err)
	}
	again := r.open(r.log)
	for _, c := range []string{cashier.ID, cashier2.ID} {
		if x, y := r.svc.Cash(c, farPast, farFuture), again.Cash(c, farPast, farFuture); !reflect.DeepEqual(x, y) {
			t.Fatalf("%s: %+v after a rebuild, %+v before", c, y, x)
		}
	}
}
