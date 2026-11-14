package shifts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

var ctx = context.Background()

func newStore(t *testing.T) (*Store, func(d time.Duration)) {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctx, pool, Migrations(), pgmigrate.WithTable(MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Minute)
		return clock
	}
	return New(pool, now), func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
}

func TestACountAddsUpAndRefusesWhatIsNotMoney(t *testing.T) {
	total, err := Counts{500_000: 2, 100_000: 3, 10_000: 5, 1_000: 7, 500: 1}.Total()
	if err != nil || total != 1_000_000+300_000+50_000+7_000+500 {
		t.Fatalf("total %d %v", total, err)
	}
	if total, err := (Counts{}).Total(); err != nil || total != 0 {
		t.Fatalf("an empty count: %d %v", total, err)
	}
	for name, c := range map[string]Counts{
		"a note that is not one": {30_000: 1},
		"a negative count":       {10_000: -1},
		"too many pieces":        {10_000: maxPieces + 1},
		"zero as a denomination": {0: 5},
	} {
		var inv *InvalidError
		if _, err := c.Total(); !errors.As(err, &inv) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestACountIsWrittenAndReadAsJSONWithTheDenominationsAsKeys(t *testing.T) {
	raw, err := json.Marshal(Counts{50_000: 2, 20_000: 0, 1_000: 4})
	if err != nil || string(raw) != `{"1000":4,"50000":2}` {
		t.Fatalf("%s %v", raw, err)
	}
	var c Counts
	if err := json.Unmarshal([]byte(`{"500000":1,"2000":3}`), &c); err != nil || c[500_000] != 1 || c[2_000] != 3 {
		t.Fatalf("%v %v", c, err)
	}
	if err := json.Unmarshal([]byte(`{"ten":1}`), &c); err == nil {
		t.Fatal("a denomination that is not a number")
	}
}

func TestAShiftIsOpenedWithACountAndClosedWithAnotherAndKeepsWhatItWasMadeOf(t *testing.T) {
	s, _ := newStore(t)
	sh, err := s.Open(ctx, "cash-1", "main", Counts{100_000: 5, 50_000: 4}) // 700,000
	if err != nil || sh.Status != Open || sh.OpeningTotal != 700_000 || sh.Cashier != "cash-1" || sh.Till != "main" {
		t.Fatalf("open: %+v %v", sh, err)
	}
	if _, err := s.AddMovement(ctx, sh.ID, "out", 50_000, "ice for the drinks", "cash-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMovement(ctx, sh.ID, "in", 200_000, "change from the bank", "cash-1"); err != nil {
		t.Fatal(err)
	}
	cur, err := s.OpenOf(ctx, "cash-1")
	if err != nil || cur.ID != sh.ID || len(cur.Movements) != 2 {
		t.Fatalf("open of: %+v %v", cur, err)
	}
	// expected: 700,000 + 1,000,000 sales - 120,000 refunds + 200,000 in - 50,000 out = 1,730,000
	closed, err := s.Close(ctx, sh.ID, "cash-1", s.Now(), Counts{500_000: 3, 200_000: 1, 20_000: 1, 10_000: 1}, Figures{Sales: 1_000_000, Refunds: 120_000}, " one note was torn ")
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != Closed || *closed.ClosingTotal != 1_730_000 || *closed.Expected != 1_730_000 || *closed.Variance != 0 {
		t.Fatalf("closed: %+v", closed)
	}
	if *closed.Sales != 1_000_000 || *closed.Refunds != 120_000 || *closed.PayIn != 200_000 || *closed.PayOut != 50_000 || closed.CloseNote != "one note was torn" || closed.ClosedBy != "cash-1" {
		t.Fatalf("what it was made of: %+v", closed)
	}
	if closed.ClosingCounts[500_000] != 3 || closed.OpeningCounts[100_000] != 5 {
		t.Fatalf("counts: %+v / %+v", closed.OpeningCounts, closed.ClosingCounts)
	}
	if _, err := s.OpenOf(ctx, "cash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a closed shift is not open: %v", err)
	}
}

func TestAShortDrawerAndAnOverOneGiveAVarianceWithTheSign(t *testing.T) {
	s, _ := newStore(t)
	a, _ := s.Open(ctx, "cash-1", "till-1", Counts{100_000: 1})
	short, err := s.Close(ctx, a.ID, "cash-1", s.Now(), Counts{100_000: 1, 50_000: 3}, Figures{Sales: 200_000}, "")
	if err != nil || *short.Expected != 300_000 || *short.ClosingTotal != 250_000 || *short.Variance != -50_000 {
		t.Fatalf("short: %+v %v", short, err)
	}
	b, _ := s.Open(ctx, "cash-1", "till-1", Counts{})
	over, err := s.Close(ctx, b.ID, "cash-1", s.Now(), Counts{10_000: 3}, Figures{}, "")
	if err != nil || *over.Variance != 30_000 {
		t.Fatalf("over: %+v %v", over, err)
	}
}

func TestACashierHasOneShiftOpenAndADrawerIsInOneShift(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Open(ctx, "cash-1", "till-1", Counts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, "cash-1", "till-2", Counts{}); !errors.Is(err, ErrCashierBusy) {
		t.Fatalf("a second shift of the same cashier: %v", err)
	}
	if _, err := s.Open(ctx, "cash-2", "till-1", Counts{}); !errors.Is(err, ErrTillBusy) {
		t.Fatalf("the same drawer in two shifts: %v", err)
	}
	if _, err := s.Open(ctx, "cash-2", "till-2", Counts{}); err != nil {
		t.Fatalf("another cashier at another drawer: %v", err)
	}
	if ok, _ := s.HasOpen(ctx, "cash-1"); !ok {
		t.Fatal("HasOpen")
	}
	if ok, _ := s.HasOpen(ctx, "cash-3"); ok {
		t.Fatal("HasOpen for a cashier with none")
	}
}

func TestOnlyOneShiftOpensWhenTwoStartAtOnce(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	res := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Open(ctx, "cash-1", "till-"+string(rune('a'+i)), Counts{})
			res <- err
		}()
	}
	wg.Wait()
	close(res)
	opened, busy := 0, 0
	for err := range res {
		switch {
		case err == nil:
			opened++
		case errors.Is(err, ErrCashierBusy):
			busy++
		default:
			t.Fatal(err)
		}
	}
	if opened != 1 || busy != 7 {
		t.Fatalf("%d opened and %d refused, want 1 and 7", opened, busy)
	}
}

func TestMoneyMovesOnlyInAnOpenShiftAndOnlyWithAReason(t *testing.T) {
	s, _ := newStore(t)
	sh, _ := s.Open(ctx, "cash-1", "main", Counts{})
	for name, m := range map[string]struct {
		kind   string
		amount int64
		reason string
	}{
		"no reason":       {"out", 1000, "  "},
		"a zero amount":   {"out", 0, "x"},
		"a negative one":  {"in", -5, "x"},
		"neither in/out":  {"away", 5, "x"},
		"a long reason":   {"out", 5, strings.Repeat("r", 201)},
		"an absurd value": {"in", 2_000_000_000, "x"},
	} {
		var inv *InvalidError
		if _, err := s.AddMovement(ctx, sh.ID, m.kind, m.amount, m.reason, "cash-1"); !errors.As(err, &inv) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.Close(ctx, sh.ID, "cash-1", s.Now(), Counts{}, Figures{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMovement(ctx, sh.ID, "out", 5000, "x", "cash-1"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a movement in a closed shift: %v", err)
	}
	if _, err := s.AddMovement(ctx, 9999, "out", 5000, "x", "cash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a movement in a shift that is not there: %v", err)
	}
}

func TestAClosedShiftCannotBeClosedAgainOrChanged(t *testing.T) {
	s, _ := newStore(t)
	sh, _ := s.Open(ctx, "cash-1", "main", Counts{10_000: 1})
	if _, err := s.Close(ctx, sh.ID, "cash-1", s.Now(), Counts{10_000: 1}, Figures{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(ctx, sh.ID, "cash-1", s.Now(), Counts{10_000: 9}, Figures{}, ""); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("closing twice: %v", err)
	}
	// even the owner of the database cannot rewrite what was counted without taking the guard away
	for _, q := range []string{
		`UPDATE shifts SET closing_total = 999999 WHERE id = $1`,
		`UPDATE shifts SET variance = 0, expected = 0 WHERE id = $1`,
		`UPDATE shifts SET cashier = 'someone-else' WHERE id = $1`,
		`UPDATE shifts SET status = 'open' WHERE id = $1`,
		`DELETE FROM shifts WHERE id = $1`,
	} {
		if _, err := s.pool.Exec(ctx, q, sh.ID); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	if _, err := s.pool.Exec(ctx, `TRUNCATE shifts CASCADE`); err == nil {
		t.Error("a truncate was accepted")
	}
	got, _ := s.Get(ctx, sh.ID)
	if *got.ClosingTotal != 10_000 {
		t.Fatalf("the count changed: %+v", got)
	}
}

func TestAMovementIsKeptAndCannotBeChanged(t *testing.T) {
	s, _ := newStore(t)
	sh, _ := s.Open(ctx, "cash-1", "main", Counts{})
	m, err := s.AddMovement(ctx, sh.ID, "out", 5000, "ice", "cash-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE shift_movements SET amount = 1 WHERE id = $1`, `DELETE FROM shift_movements WHERE id = $1`} {
		if _, err := s.pool.Exec(ctx, q, m.ID); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

func TestAManagerReviewsAClosedShiftOnce(t *testing.T) {
	s, _ := newStore(t)
	sh, _ := s.Open(ctx, "cash-1", "main", Counts{})
	if _, err := s.Review(ctx, sh.ID, "mgr-1", "x"); !errors.Is(err, ErrNotClosed) {
		t.Fatalf("reviewing an open shift: %v", err)
	}
	if _, err := s.Close(ctx, sh.ID, "cash-1", s.Now(), Counts{5_000: 2}, Figures{}, "found a note on the floor"); err != nil {
		t.Fatal(err)
	}
	rv, err := s.Review(ctx, sh.ID, "mgr-1", " seen: the note was from the tip jar ")
	if err != nil || rv.ReviewedBy != "mgr-1" || rv.ReviewedAt == nil || rv.ReviewNote != "seen: the note was from the tip jar" {
		t.Fatalf("review: %+v %v", rv, err)
	}
	if _, err := s.Review(ctx, sh.ID, "mgr-2", "again"); !errors.Is(err, ErrReviewed) {
		t.Fatalf("reviewing twice: %v", err)
	}
	if _, err := s.Review(ctx, 9999, "mgr-1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a shift that is not there: %v", err)
	}
	if got, _ := s.Get(ctx, sh.ID); got.ReviewedBy != "mgr-1" || *got.Variance != 10_000 {
		t.Fatalf("the review changed the figures or was not kept: %+v", got)
	}
}

func TestShiftsAreListedNewestFirstForACashierOrForAll(t *testing.T) {
	s, _ := newStore(t)
	for _, c := range []string{"cash-1", "cash-2", "cash-1"} {
		sh, err := s.Open(ctx, c, "main", Counts{})
		if err != nil {
			t.Fatal(err)
		}
		if c != "cash-1" || sh.ID < 3 {
			if _, err := s.Close(ctx, sh.ID, c, s.Now(), Counts{}, Figures{}, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	all, _ := s.List(ctx, Filter{})
	if len(all) != 3 || all[0].ID < all[1].ID || all[1].ID < all[2].ID {
		t.Fatalf("all: %+v", all)
	}
	if mine, _ := s.List(ctx, Filter{Cashier: "cash-1"}); len(mine) != 2 {
		t.Fatalf("of cash-1: %d", len(mine))
	}
	if open, _ := s.List(ctx, Filter{Status: Open}); len(open) != 1 || open[0].Cashier != "cash-1" {
		t.Fatalf("open: %+v", open)
	}
	if _, err := s.Get(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a shift that is not there: %v", err)
	}
}

func TestAShiftCannotCloseBeforeItOpened(t *testing.T) {
	s, _ := newStore(t)
	sh, _ := s.Open(ctx, "cash-1", "main", Counts{})
	got, err := s.Close(ctx, sh.ID, "cash-1", sh.OpenedAt.Add(-time.Hour), Counts{}, Figures{}, "")
	if err != nil || got.ClosedAt.Before(got.OpenedAt) {
		t.Fatalf("closed at %v, opened at %v: %v", got.ClosedAt, got.OpenedAt, err)
	}
}

func TestADrawerNameThatIsNotOneIsRefused(t *testing.T) {
	s, _ := newStore(t)
	for _, till := range []string{"", "Main", "has space", strings.Repeat("a", 33), "-x"} {
		var inv *InvalidError
		if _, err := s.Open(ctx, "cash-1", till, Counts{}); !errors.As(err, &inv) {
			t.Errorf("%q: %v", till, err)
		}
	}
	if _, err := s.Open(ctx, "", "main", Counts{}); err == nil {
		t.Error("no cashier")
	}
	if _, err := s.Open(ctx, "cash-1", "main", Counts{3: 1}); err == nil {
		t.Error("a denomination that is not one")
	}
}
