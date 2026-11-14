package expenses

import (
	"context"
	"errors"
	"strings"
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

func sumOf(days []DayAmount) (total int64, byDay map[string]int64) {
	byDay = map[string]int64{}
	for _, d := range days {
		total += d.Amount
		byDay[d.Day] += d.Amount
	}
	return
}

func TestAnExpenseOfOneDayLandsOnThatDay(t *testing.T) {
	s := newStore(t)
	e, err := s.Add(ctx, Expense{Category: " Electricity ", Amount: 450_000, From: "2026-11-10", Note: " the bill for October "}, "mgr-1")
	if err != nil || e.To != "2026-11-10" || e.Category != "Electricity" || e.Note != "the bill for October" || e.CreatedBy != "mgr-1" {
		t.Fatalf("add: %+v %v", e, err)
	}
	days, err := s.Allocated(ctx, "2026-11-08", "2026-11-12")
	if err != nil || len(days) != 1 || days[0].Day != "2026-11-10" || days[0].Amount != 450_000 || days[0].Category != "Electricity" {
		t.Fatalf("allocated: %+v %v", days, err)
	}
	if none, _ := s.Allocated(ctx, "2026-11-11", "2026-11-20"); len(none) != 0 {
		t.Fatalf("a period that does not have it: %+v", none)
	}
}

func TestAPeriodIsSpreadEvenlyAndTheDaysAddUpToTheAmountExactly(t *testing.T) {
	s := newStore(t)
	// 10,000,000 over the 30 days of November: 333,333 a day and 10 left over for the first ten days
	if _, err := s.Add(ctx, Expense{Category: "Rent", Amount: 10_000_000, From: "2026-11-01", To: "2026-11-30"}, "mgr-1"); err != nil {
		t.Fatal(err)
	}
	days, _ := s.Allocated(ctx, "2026-11-01", "2026-11-30")
	total, by := sumOf(days)
	if total != 10_000_000 || len(by) != 30 {
		t.Fatalf("total %d over %d days", total, len(by))
	}
	if by["2026-11-01"] != 333_334 || by["2026-11-10"] != 333_334 || by["2026-11-11"] != 333_333 || by["2026-11-30"] != 333_333 {
		t.Fatalf("the remainder goes to the first days: %d %d %d %d", by["2026-11-01"], by["2026-11-10"], by["2026-11-11"], by["2026-11-30"])
	}
	// a part of the period: only what falls in it
	part, _ := s.Allocated(ctx, "2026-11-25", "2026-12-05")
	if total, by := sumOf(part); len(by) != 6 || total != 6*333_333 {
		t.Fatalf("the last six days: %d over %d days", total, len(by))
	}
	// a period that is longer than the amount in đồng still adds up
	if _, err := s.Add(ctx, Expense{Category: "Tiny", Amount: 5, From: "2026-12-01", To: "2026-12-10"}, "mgr-1"); err != nil {
		t.Fatal(err)
	}
	tiny, _ := s.Allocated(ctx, "2026-12-01", "2026-12-10")
	if total, _ := sumOf(tiny); total != 5 {
		t.Fatalf("5 dong over 10 days: %d", total)
	}
}

func TestSeveralCategoriesOnTheSameDayAreKeptApart(t *testing.T) {
	s := newStore(t)
	must(t, s, Expense{Category: "Rent", Amount: 300_000, From: "2026-11-01", To: "2026-11-03"}, "m")
	must(t, s, Expense{Category: "Wages", Amount: 200_000, From: "2026-11-02"}, "m")
	days, _ := s.Allocated(ctx, "2026-11-02", "2026-11-02")
	if len(days) != 2 || days[0].Category != "Rent" || days[0].Amount != 100_000 || days[1].Category != "Wages" || days[1].Amount != 200_000 {
		t.Fatalf("%+v", days)
	}
}

func TestAVoidedExpenseCountsForNothingAndStaysInTheBooks(t *testing.T) {
	s := newStore(t)
	e, _ := s.Add(ctx, Expense{Category: "Repairs", Amount: 900_000, From: "2026-11-05"}, "mgr-1")
	v, err := s.Void(ctx, e.ID, "mgr-2", " written twice ")
	if err != nil || v.VoidedAt == nil || v.VoidedBy != "mgr-2" || v.VoidReason != "written twice" {
		t.Fatalf("void: %+v %v", v, err)
	}
	if days, _ := s.Allocated(ctx, "2026-11-01", "2026-11-30"); len(days) != 0 {
		t.Fatalf("a voided expense is counted: %+v", days)
	}
	if list, _ := s.List(ctx, "2026-11-01", "2026-11-30", false); len(list) != 0 {
		t.Fatalf("listed: %+v", list)
	}
	if list, _ := s.List(ctx, "2026-11-01", "2026-11-30", true); len(list) != 1 || list[0].VoidedAt == nil {
		t.Fatalf("with the voided: %+v", list)
	}
	if _, err := s.Void(ctx, e.ID, "mgr-2", "again"); !errors.Is(err, ErrVoided) {
		t.Fatalf("voiding twice: %v", err)
	}
	if _, err := s.Void(ctx, 9999, "mgr-2", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expense that is not there: %v", err)
	}
}

func TestAnExpenseCannotBeRewrittenOrDeleted(t *testing.T) {
	s := newStore(t)
	e, _ := s.Add(ctx, Expense{Category: "Rent", Amount: 1000, From: "2026-11-01"}, "mgr-1")
	for _, q := range []string{
		`UPDATE expenses SET amount = 1 WHERE id = $1`, `UPDATE expenses SET category = 'x' WHERE id = $1`, `UPDATE expenses SET from_date = '2020-01-01', to_date = '2020-01-01' WHERE id = $1`,
		`DELETE FROM expenses WHERE id = $1`,
	} {
		if _, err := s.pool.Exec(ctx, q, e.ID); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	if _, err := s.pool.Exec(ctx, `TRUNCATE expenses`); err == nil {
		t.Error("a truncate was accepted")
	}
	if _, err := s.Void(ctx, e.ID, "m", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE expenses SET void_reason = 'other' WHERE id = $1`, e.ID); err == nil {
		t.Error("a voided expense was changed")
	}
}

func TestAnExpenseThatMakesNoSenseIsRefused(t *testing.T) {
	s := newStore(t)
	for name, e := range map[string]Expense{
		"no category":        {Category: "  ", Amount: 1, From: "2026-11-01"},
		"a long category":    {Category: strings.Repeat("c", 41), Amount: 1, From: "2026-11-01"},
		"no amount":          {Category: "x", Amount: 0, From: "2026-11-01"},
		"a negative amount":  {Category: "x", Amount: -5, From: "2026-11-01"},
		"an absurd amount":   {Category: "x", Amount: MaxAmount + 1, From: "2026-11-01"},
		"no day":             {Category: "x", Amount: 1},
		"a day that is not":  {Category: "x", Amount: 1, From: "2026-02-30"},
		"a last day too far": {Category: "x", Amount: 1, From: "2026-11-01", To: "2026-10-01"},
		"ten years and more": {Category: "x", Amount: 1, From: "2016-01-01", To: "2026-12-31"},
		"a long note":        {Category: "x", Amount: 1, From: "2026-11-01", Note: strings.Repeat("n", 201)},
		"a day with a time":  {Category: "x", Amount: 1, From: "2026-11-01T10:00"},
	} {
		var inv *InvalidError
		if _, err := s.Add(ctx, e, "m"); !errors.As(err, &inv) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.Add(ctx, Expense{Category: "x", Amount: 1, From: "2026-11-01"}, ""); err == nil {
		t.Error("no one to say who writes")
	}
}

func TestCategoriesAreOfferedMostUsedFirst(t *testing.T) {
	s := newStore(t)
	for _, c := range []string{"Rent", "Wages", "Wages", "Power", "Wages", "Rent"} {
		must(t, s, Expense{Category: c, Amount: 1, From: "2026-11-01"}, "m")
	}
	got, err := s.Categories(ctx)
	if err != nil || strings.Join(got, ",") != "Wages,Rent,Power" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestAPeriodThatIsNotOneIsRefusedWhenAsking(t *testing.T) {
	s := newStore(t)
	if _, err := s.Allocated(ctx, "yesterday", "2026-11-10"); err == nil {
		t.Error("a bad first day")
	}
	if _, err := s.Allocated(ctx, "2026-11-10", "2026-11-01"); err == nil {
		t.Error("ending before it starts")
	}
	if _, err := s.List(ctx, "2026-11-01", "tomorrow", false); err == nil {
		t.Error("a bad last day in a list")
	}
}

func must(t *testing.T, s *Store, e Expense, by string) {
	t.Helper()
	if _, err := s.Add(ctx, e, by); err != nil {
		t.Fatal(err)
	}
}
