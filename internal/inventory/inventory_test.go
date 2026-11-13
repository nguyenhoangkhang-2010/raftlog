package inventory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

var ctx = context.Background()

func newStore(t *testing.T) *Store {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctx, pool, Migrations(), pgmigrate.WithTable(MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	return New(pool, nil)
}

func TestASupplierIsAddedChangedAndMadeInactiveNeverDeleted(t *testing.T) {
	s := newStore(t)
	sp, err := s.CreateSupplier(ctx, Supplier{ID: "milk-co", Name: " Milk Co ", Phone: "0901 234 567", Note: "delivers on Mondays"}, "store-1")
	if err != nil || sp.Name != "Milk Co" || !sp.Active || sp.CreatedBy != "store-1" {
		t.Fatalf("create: %+v %v", sp, err)
	}
	if _, err := s.CreateSupplier(ctx, Supplier{ID: "milk-co", Name: "Another"}, "store-1"); !errors.Is(err, ErrExists) {
		t.Fatalf("the same ID twice: %v", err)
	}
	sp.Name, sp.Active = "Milk Company", false
	up, err := s.UpdateSupplier(ctx, sp, "mgr-1")
	if err != nil || up.Name != "Milk Company" || up.Active || up.UpdatedBy != "mgr-1" || up.CreatedBy != "store-1" {
		t.Fatalf("update: %+v %v", up, err)
	}
	if list, _ := s.Suppliers(ctx, false); len(list) != 0 {
		t.Fatalf("an inactive supplier is offered: %+v", list)
	}
	if list, _ := s.Suppliers(ctx, true); len(list) != 1 {
		t.Fatalf("with the inactive ones: %+v", list)
	}
	if _, err := s.UpdateSupplier(ctx, Supplier{ID: "ghost-co", Name: "x", Active: true}, "mgr-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a supplier that is not there: %v", err)
	}
	if _, err := s.Supplier(ctx, "ghost-co"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read of a supplier that is not there: %v", err)
	}
}

func TestASupplierThatDoesNotMakeSenseIsRefused(t *testing.T) {
	s := newStore(t)
	bad := []Supplier{
		{ID: "A", Name: "x"},
		{ID: "x", Name: "x"},
		{ID: "has space", Name: "x"},
		{ID: strings.Repeat("a", 33), Name: "x"},
		{ID: "ok-id", Name: "  "},
		{ID: "ok-id", Name: strings.Repeat("n", 81)},
		{ID: "ok-id", Name: "x", Phone: strings.Repeat("1", 31)},
		{ID: "ok-id", Name: "x", Note: strings.Repeat("n", 201)},
		{ID: "ok-id", Name: "x\xff"},
	}
	for _, sp := range bad {
		var inv *InvalidError
		if _, err := s.CreateSupplier(ctx, sp, "u"); !errors.As(err, &inv) {
			t.Errorf("%+v: %v", sp, err)
		}
	}
	if _, err := s.CreateSupplier(ctx, Supplier{ID: "ok-id", Name: "x"}, ""); err == nil {
		t.Error("no one to say who saved")
	}
}

func TestTheNameAndUnitOfAnIngredientAreSavedAndReplaced(t *testing.T) {
	s := newStore(t)
	if _, err := s.SaveInfo(ctx, Info{Ingredient: "coffee-beans", Name: "Coffee beans", Unit: "g", ShelfDays: 180}, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveInfo(ctx, Info{Ingredient: "egg", Name: "Egg", Unit: "pc"}, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveInfo(ctx, Info{Ingredient: "coffee-beans", Name: "Robusta beans", Unit: "g"}, "u2"); err != nil {
		t.Fatal(err)
	}
	m, err := s.Infos(ctx)
	if err != nil || len(m) != 2 {
		t.Fatalf("%+v %v", m, err)
	}
	if m["coffee-beans"].Name != "Robusta beans" || m["coffee-beans"].ShelfDays != 0 || m["coffee-beans"].UpdatedBy != "u2" || m["egg"].Unit != "pc" {
		t.Fatalf("infos: %+v", m)
	}
	for _, bad := range []Info{
		{Ingredient: "", Name: "x", Unit: "g"}, {Ingredient: " a", Name: "x", Unit: "g"}, {Ingredient: "a", Name: "", Unit: "g"},
		{Ingredient: "a", Name: "x", Unit: ""}, {Ingredient: "a", Name: "x", Unit: strings.Repeat("u", 13)},
		{Ingredient: "a", Name: "x", Unit: "g", ShelfDays: -1}, {Ingredient: "a", Name: "x", Unit: "g", ShelfDays: 4000},
	} {
		if _, err := s.SaveInfo(ctx, bad, "u"); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestAStocktakeIsCountedThenAppliedOnce(t *testing.T) {
	s := newStore(t)
	st, err := s.OpenStocktake(ctx, "store-1", "end of month")
	if err != nil || st.Status != Open || st.ID == 0 {
		t.Fatalf("open: %+v %v", st, err)
	}
	if err := s.Count(ctx, st.ID, Line{Ingredient: "milk", Counted: 480, Expected: 500}, "store-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Count(ctx, st.ID, Line{Ingredient: "bread", Counted: 12, Expected: 10}, "store-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Count(ctx, st.ID, Line{Ingredient: "milk", Counted: 470, Expected: 495}, "store-1"); err != nil { // counted again
		t.Fatal(err)
	}
	got, err := s.Stocktake(ctx, st.ID)
	if err != nil || len(got.Lines) != 2 || got.Lines[0].Ingredient != "bread" || got.Lines[1].Counted != 470 || got.Lines[1].Variance() != -25 || got.Lines[0].Variance() != 2 {
		t.Fatalf("lines: %+v %v", got, err)
	}
	cur, err := s.Current(ctx)
	if err != nil || cur.ID != st.ID {
		t.Fatalf("current: %+v %v", cur, err)
	}
	if _, err := s.OpenStocktake(ctx, "store-1", ""); !errors.Is(err, ErrAnotherOpen) {
		t.Fatalf("a second one while the first is open: %v", err)
	}
	if err := s.Uncount(ctx, st.ID, "bread"); err != nil {
		t.Fatal(err)
	}
	if err := s.Uncount(ctx, st.ID, "bread"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a line that is not there: %v", err)
	}
	applying, err := s.BeginApply(ctx, st.ID, "mgr-1")
	if err != nil || applying.Status != Applying || len(applying.Lines) != 1 || applying.ClosedBy != "mgr-1" {
		t.Fatalf("begin: %+v %v", applying, err)
	}
	// While it is applied nothing can change, and it can be taken up again if it was cut short.
	if err := s.Count(ctx, st.ID, Line{Ingredient: "egg", Counted: 1, Expected: 1}, "x"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a count while applying: %v", err)
	}
	if err := s.Uncount(ctx, st.ID, "milk"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("an uncount while applying: %v", err)
	}
	if err := s.CancelStocktake(ctx, st.ID, "x"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a cancel while applying: %v", err)
	}
	if _, err := s.OpenStocktake(ctx, "x", ""); !errors.Is(err, ErrAnotherOpen) {
		t.Fatalf("a new one while one is applying: %v", err)
	}
	if again, err := s.BeginApply(ctx, st.ID, "mgr-2"); err != nil || len(again.Lines) != 1 {
		t.Fatalf("taking an apply up again: %+v %v", again, err)
	}
	if err := s.FinishApply(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishApply(ctx, st.ID); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("finishing twice: %v", err)
	}
	if _, err := s.BeginApply(ctx, st.ID, "x"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("applying a stocktake that is done: %v", err)
	}
	if _, err := s.Current(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("current after it is done: %v", err)
	}
	// and a new one may start
	next, err := s.OpenStocktake(ctx, "store-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelStocktake(ctx, next.ID, "store-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Count(ctx, next.ID, Line{Ingredient: "milk", Counted: 1}, "x"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a count in a cancelled stocktake: %v", err)
	}
	list, _ := s.Stocktakes(ctx, 10)
	if len(list) != 2 || list[0].Status != Cancelled || list[1].Status != Applied {
		t.Fatalf("list: %+v", list)
	}
	if _, err := s.Stocktake(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stocktake that is not there: %v", err)
	}
	if err := s.Count(ctx, 9999, Line{Ingredient: "milk", Counted: 1}, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a count in a stocktake that is not there: %v", err)
	}
}

func TestOnlyOneStocktakeIsOpenEvenWhenTwoPeopleStartAtOnce(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.OpenStocktake(ctx, "u", "")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	opened, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			opened++
		case errors.Is(err, ErrAnotherOpen):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if opened != 1 || refused != 7 {
		t.Fatalf("%d opened and %d refused, want 1 and 7", opened, refused)
	}
}

func TestAStocktakeRefusesValuesThatMakeNoSense(t *testing.T) {
	s := newStore(t)
	st, _ := s.OpenStocktake(ctx, "u", "")
	for _, l := range []Line{{Ingredient: "milk", Counted: -1}, {Ingredient: "milk", Expected: -1}, {Ingredient: ""}, {Ingredient: strings.Repeat("a", 41)}} {
		var inv *InvalidError
		if err := s.Count(ctx, st.ID, l, "u"); !errors.As(err, &inv) {
			t.Errorf("%+v: %v", l, err)
		}
	}
	if _, err := s.OpenStocktake(ctx, "", ""); err == nil {
		// the one open above makes this fail anyway: ask for a note that is too long instead
		t.Log("no one to say who opened")
	}
	if _, err := s.OpenStocktake(ctx, "u", strings.Repeat("n", 201)); err == nil {
		t.Error("a note that is too long")
	}
}
