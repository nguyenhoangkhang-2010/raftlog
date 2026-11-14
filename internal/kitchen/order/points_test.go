package order

import (
	"strings"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
)

// placedWithPoints is the order of the other tests (85.000đ), placed by ann from the app with points used.
func placedWithPoints(t *testing.T, w *world, used int64, value int64) (*Order, Event) {
	t.Helper()
	lines := []menu.Line{drinkLine(), foodLine()}
	ev, err := Place(w.meta(ann), PlaceInput{Order: "o1", Number: 7, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: pricing(t, lines),
		Points: &PointsUse{Used: used, Value: money.Money(value)}})
	if err != nil {
		t.Fatal(err)
	}
	o := &Order{}
	if err := o.Apply(ev); err != nil {
		t.Fatal(err)
	}
	return o, ev
}

func TestPointsTakeMoneyOffAsADiscountOfTheirOwn(t *testing.T) {
	w := newWorld(t)
	o, _ := placedWithPoints(t, w, 30, 100) // 30 points of 100đ: 3.000đ off 85.000đ
	if o.Subtotal != 85_000 || o.Discount != 3_000 || o.Total != 82_000 {
		t.Fatalf("order = %+v", o)
	}
	if o.Points == nil || o.Points.Used != 30 || o.Points.Value != 100 {
		t.Fatalf("points = %+v", o.Points)
	}
	if len(o.Discounts) != 1 || o.Discounts[0].ID != PointsDiscountID || o.Discounts[0].Amount != 3_000 {
		t.Fatalf("discounts = %+v", o.Discounts)
	}
	// payment is for what is left, and earns what the service says
	ev, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 90_000, PointsEarned: 8})
	apply(t, o, ev, err)
	if o.Payment.Amount != 82_000 || o.PointsEarned != 8 {
		t.Fatalf("paid %v, earned %d", o.Payment.Amount, o.PointsEarned)
	}
}

func TestPointsComeAfterThePromotions(t *testing.T) {
	w := newWorld(t)
	lines := []menu.Line{drinkLine(), foodLine()}
	res := pricing(t, lines, promo.Promotion{ID: "ten", Name: "10%", Kind: promo.PercentOff, Percent: 1000}) // 8.500đ off
	ev, err := Place(w.meta(ann), PlaceInput{Order: "o1", Number: 1, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: res, Points: &PointsUse{Used: 50, Value: 100}})
	if err != nil {
		t.Fatal(err)
	}
	o := &Order{}
	if err := o.Apply(ev); err != nil {
		t.Fatal(err)
	}
	if o.Discount != 13_500 || o.Total != 71_500 || len(o.Discounts) != 2 || o.Discounts[1].ID != PointsDiscountID {
		t.Fatalf("order = %+v", o)
	}
	// the pricing that was handed in is not changed
	if len(res.Applied) != 1 || res.Total != 76_500 {
		t.Fatalf("the promotions' result was changed: %+v", res)
	}
}

func TestPointsAreRefusedWhereTheyDoNotFit(t *testing.T) {
	w := newWorld(t)
	lines := []menu.Line{drinkLine(), foodLine()}
	for name, p := range map[string]*PointsUse{
		"none used":     {Used: 0, Value: 100},
		"negative":      {Used: -5, Value: 100},
		"worth nothing": {Used: 10, Value: 0},
		"too many":      {Used: MaxPoints + 1, Value: 100},
		"more than due": {Used: 900, Value: 100}, // 90.000đ off an order of 85.000đ
	} {
		if _, err := Place(w.meta(ann), PlaceInput{Order: "o9", Number: 1, Channel: ChannelApp, Customer: "ann", Lines: lines, Pricing: pricing(t, lines), Points: p}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the cashier does not spend a customer's points
	if _, err := Place(w.meta(cashier), PlaceInput{Order: "o9", Number: 1, Channel: ChannelCounter, Customer: "ann", Lines: lines, Pricing: pricing(t, lines), Points: &PointsUse{Used: 10, Value: 100}}); err == nil {
		t.Error("points were used on an order of the counter")
	}
}

func TestAnEventThatDoesNotAddUpAsPointsIsRefused(t *testing.T) {
	w := newWorld(t)
	_, ev := placedWithPoints(t, w, 30, 100)
	// the discount of points is not what the points are worth
	bad := *ev.Placed
	bad.Discounts = []promo.Applied{{ID: PointsDiscountID, Name: "Points", Amount: 2_900}}
	bad.Discount, bad.Total = 2_900, 82_100
	if err := bad.check(); err == nil || !strings.Contains(err.Error(), "points") {
		t.Errorf("a discount that is not what the points are worth: %v", err)
	}
	// a discount of points with no points
	bad = *ev.Placed
	bad.Points = nil
	if err := bad.check(); err == nil {
		t.Error("a discount of points with no points behind it")
	}
	// points with no discount
	bad = *ev.Placed
	bad.Discounts, bad.Discount, bad.Total = nil, 0, 85_000
	if err := bad.check(); err == nil {
		t.Error("points with no discount")
	}
	// points on an order of the counter
	bad = *ev.Placed
	bad.Channel = ChannelCounter
	if err := bad.check(); err == nil {
		t.Error("points on an order of the counter")
	}
	// a promotion may not use the ID of points
	if _, err := promo.NewSet([]promo.Promotion{{ID: promo.PointsID, Name: "x", Kind: promo.PercentOff, Percent: 100}}); err == nil {
		t.Error("a promotion took the ID of points")
	}
}

func TestPointsAreEarnedOnlyByAnOrderWithACustomer(t *testing.T) {
	w := newWorld(t)
	lines := []menu.Line{drinkLine(), foodLine()}
	// a walk-in at the counter
	ev, err := Place(w.meta(cashier), PlaceInput{Order: "o2", Number: 2, Channel: ChannelCounter, Lines: lines, Pricing: pricing(t, lines)})
	if err != nil {
		t.Fatal(err)
	}
	walkIn := &Order{}
	if err := walkIn.Apply(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := walkIn.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 100_000, PointsEarned: 5}); err == nil {
		t.Error("an order with no customer earned points")
	}
	// the same refused when it comes from the log
	pay, err := walkIn.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	pay.Paid.PointsEarned = 5
	if err := walkIn.Apply(*pay); err == nil {
		t.Error("a payment event with points was applied to an order with no customer")
	}
	// and a negative or huge number is not a payment
	o := placed(t, w)
	for _, n := range []int64{-1, MaxPoints + 1} {
		ev, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 100_000, PointsEarned: n})
		if err == nil {
			if err = o.Apply(*ev); err == nil {
				t.Errorf("%d points earned were accepted", n)
			}
		}
	}
}

func TestAnOrderWithPointsIsRebuiltFromItsEvents(t *testing.T) {
	w := newWorld(t)
	o, first := placedWithPoints(t, w, 30, 100)
	pay, err := o.Pay(w.meta(cashier), PayInput{Method: Cash, Tendered: 90_000, PointsEarned: 8})
	apply(t, o, pay, err)
	raw1, err := Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := Encode(*pay)
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	for _, raw := range [][]byte{raw1, raw2} {
		ev, err := Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	again, err := Replay(evs)
	if err != nil {
		t.Fatal(err)
	}
	if again.Points == nil || *again.Points != (PointsUse{Used: 30, Value: 100}) || again.PointsEarned != 8 || again.Total != 82_000 {
		t.Fatalf("replayed = %+v", again)
	}
}
