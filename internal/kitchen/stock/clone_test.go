package stock

import "testing"

func TestACloneIsIndependentOfTheStockItCameFrom(t *testing.T) {
	s, err := New(map[Ingredient]Level{"beans": {Qty: 100, LowAt: 10}})
	if err != nil {
		t.Fatal(err)
	}
	c := s.Clone()
	if err := c.Apply(Movement{Kind: Consume, Needs: Recipe{"beans": 60}, Ref: "o1"}); err != nil {
		t.Fatal(err)
	}
	if s.Qty("beans") != 100 || c.Qty("beans") != 40 {
		t.Fatalf("original %d, clone %d: the clone shares state with the original", s.Qty("beans"), c.Qty("beans"))
	}
	if err := s.Apply(Movement{Kind: Receive, Ingredient: "beans", Qty: 5}); err != nil {
		t.Fatal(err)
	}
	if c.Qty("beans") != 40 {
		t.Fatal("a change of the original reached the clone")
	}
	// The low-stock threshold travels with the copy.
	if err := c.Apply(Movement{Kind: Consume, Needs: Recipe{"beans": 31}, Ref: "o2"}); err != nil {
		t.Fatal(err)
	}
	if low := c.Low(); len(low) != 1 || low[0] != "beans" {
		t.Fatalf("low = %v, want [beans]", low)
	}
}

func TestAMovementThatFailsOnACloneChangesNothingAnywhere(t *testing.T) {
	s, _ := New(map[Ingredient]Level{"beans": {Qty: 10}})
	c := s.Clone()
	if err := c.Apply(Movement{Kind: WriteOff, Ingredient: "beans", Qty: 11, Reason: "spilled"}); err == nil {
		t.Fatal("writing off more than there is was accepted")
	}
	if c.Qty("beans") != 10 || s.Qty("beans") != 10 {
		t.Fatal("a refused movement changed a stock")
	}
}
