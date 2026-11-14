package menu

import "testing"

func TestCombosAreListedByID(t *testing.T) {
	items := []Item{
		{ID: "tea", Name: "Tea", Category: "drinks", Station: "drinks", Price: 20000, Active: true},
		{ID: "bun", Name: "Bun", Category: "food", Station: "hot", Price: 30000, Active: true},
	}
	combos := []Combo{
		{ID: "z-combo", Name: "Z", Price: 40000, Active: true, Slots: []Slot{{Name: "drink", Items: []ItemID{"tea"}}, {Name: "food", Items: []ItemID{"bun"}}}},
		{ID: "a-combo", Name: "A", Price: 45000, Active: true, Slots: []Slot{{Name: "drink", Items: []ItemID{"tea"}}}},
	}
	m, err := New(items, combos)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Combos()
	if len(got) != 2 || got[0].ID != "a-combo" || got[1].ID != "z-combo" {
		t.Fatalf("combos = %+v, want a-combo then z-combo", got)
	}
	got[0].Name = "changed"
	if again := m.Combos(); again[0].Name != "A" {
		t.Fatal("the list shares memory with the menu")
	}
}
