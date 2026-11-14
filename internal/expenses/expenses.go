// Package expenses keeps in PostgreSQL what the venue spends besides the goods it sells, so that a
// report can set it against what the venue earned.
//
// An expense covers one day or a period of days, and a report spreads it evenly over them: the rent of a
// month is a thirtieth of itself each day. The spreading is done in whole đồng with the remainder given to
// the first days, so that the days of a period add up to the amount exactly.
package expenses

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationsTable is where the migrator records what it applied.
const MigrationsTable = "expenses_migrations"

// Migrations returns the SQL files of this package, for the migrator.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}

// Errors of the store.
var (
	ErrNotFound = errors.New("expenses: not found")
	// ErrVoided says that the expense was voided already.
	ErrVoided = errors.New("expenses: the expense was voided already")
)

// InvalidError says that a value was refused, and why.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "expenses: " + e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{fmt.Sprintf(format, args...)}
}

// MaxAmount bounds one expense, so that a typing mistake cannot make a venue lose a fortune on paper.
const MaxAmount = 100_000_000_000

// Expense is one thing that was spent.
type Expense struct {
	ID       int64  `json:"id"`
	Category string `json:"category"`
	// Amount is in whole đồng; From and To are the days it covers, both included (the same for an
	// expense of one day), like 2026-11-01.
	Amount    int64      `json:"amount"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	Note      string     `json:"note,omitempty"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	VoidedAt  *time.Time `json:"voided_at,omitempty"`
	VoidedBy  string     `json:"voided_by,omitempty"`
	// VoidReason says why it was taken back.
	VoidReason string `json:"void_reason,omitempty"`
}

// Store is the expenses of a database.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a Store on a database whose migrations (see Migrations) have been applied. now is the
// clock; time.Now if nil.
func New(pool *pgxpool.Pool, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }}
}

var dayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func checkDay(s, what string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil || !dayPattern.MatchString(s) {
		return time.Time{}, invalid("%s is not a date like 2026-11-14", what)
	}
	return t, nil
}

func text(s, field string, lo, hi int) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < lo || n > hi {
		if lo == 0 {
			return "", invalid("%s is at most %d characters", field, hi)
		}
		return "", invalid("%s is %d to %d characters", field, lo, hi)
	}
	return s, nil
}

const columns = `id, category, amount, from_date::text, to_date::text, note, created_by, created_at, voided_at, COALESCE(voided_by, ''), void_reason`

func scan(row pgx.Row) (Expense, error) {
	var e Expense
	err := row.Scan(&e.ID, &e.Category, &e.Amount, &e.From, &e.To, &e.Note, &e.CreatedBy, &e.CreatedAt, &e.VoidedAt, &e.VoidedBy, &e.VoidReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Expense{}, ErrNotFound
	}
	return e, err
}

// Add writes an expense down. To may be empty for an expense of one day.
func (s *Store) Add(ctx context.Context, e Expense, by string) (Expense, error) {
	var err error
	if by == "" {
		return Expense{}, invalid("say who writes")
	}
	if e.Category, err = text(e.Category, "the category", 1, 40); err != nil {
		return Expense{}, err
	}
	if e.Note, err = text(e.Note, "the note", 0, 200); err != nil {
		return Expense{}, err
	}
	if e.Amount < 1 || e.Amount > MaxAmount {
		return Expense{}, invalid("an amount is a whole number of đồng above zero")
	}
	from, err := checkDay(e.From, "the first day")
	if err != nil {
		return Expense{}, err
	}
	if e.To == "" {
		e.To = e.From
	}
	to, err := checkDay(e.To, "the last day")
	if err != nil {
		return Expense{}, err
	}
	if to.Before(from) {
		return Expense{}, invalid("the last day is before the first")
	}
	if to.Sub(from) > 3659*24*time.Hour {
		return Expense{}, invalid("an expense covers at most ten years")
	}
	return scan(s.pool.QueryRow(ctx, `INSERT INTO expenses (category, amount, from_date, to_date, note, created_by, created_at)
		VALUES ($1, $2, $3::text::date, $4::text::date, $5, $6, $7) RETURNING `+columns, e.Category, e.Amount, e.From, e.To, e.Note, by, s.now()))
}

// Void takes an expense back, with the reason. It is done once.
func (s *Store) Void(ctx context.Context, id int64, by, reason string) (Expense, error) {
	if by == "" {
		return Expense{}, invalid("say who voids")
	}
	reason, err := text(reason, "the reason", 0, 200)
	if err != nil {
		return Expense{}, err
	}
	e, err := scan(s.pool.QueryRow(ctx, `UPDATE expenses SET voided_at = $2, voided_by = $3, void_reason = $4 WHERE id = $1 AND voided_at IS NULL RETURNING `+columns,
		id, s.now(), by, reason))
	if errors.Is(err, ErrNotFound) {
		var n int
		if qerr := s.pool.QueryRow(ctx, `SELECT count(*) FROM expenses WHERE id = $1`, id).Scan(&n); qerr == nil && n > 0 {
			return Expense{}, ErrVoided
		}
	}
	return e, err
}

// List returns the expenses that cover any day of a period, newest first; the voided ones only if asked.
func (s *Store) List(ctx context.Context, from, to string, withVoided bool) ([]Expense, error) {
	if _, err := checkDay(from, "the first day"); err != nil {
		return nil, err
	}
	if _, err := checkDay(to, "the last day"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM expenses
		WHERE from_date <= $2::text::date AND to_date >= $1::text::date AND (voided_at IS NULL OR $3) ORDER BY id DESC LIMIT 1000`, from, to, withVoided)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Expense{}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Categories lists the categories that have been used, most used first, to suggest when writing one.
func (s *Store) Categories(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT category FROM expenses GROUP BY category ORDER BY count(*) DESC, category LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DayAmount is what the expenses of one category come to on one day.
type DayAmount struct {
	Day      string `json:"day"`
	Category string `json:"category"`
	Amount   int64  `json:"amount"`
}

// Allocated spreads the expenses that are not voided over the days of a period (both included), and
// returns what falls on each day for each category. A period of an expense is divided evenly in whole
// đồng, the first days taking the remainder, so the days of an expense add up to its amount.
func (s *Store) Allocated(ctx context.Context, from, to string) ([]DayAmount, error) {
	f, err := checkDay(from, "the first day")
	if err != nil {
		return nil, err
	}
	t, err := checkDay(to, "the last day")
	if err != nil {
		return nil, err
	}
	if t.Before(f) {
		return nil, invalid("the period ends before it starts")
	}
	rows, err := s.pool.Query(ctx, `
WITH e AS (SELECT id, category, amount, from_date, to_date, (to_date - from_date + 1) AS n FROM expenses
           WHERE voided_at IS NULL AND from_date <= $2::text::date AND to_date >= $1::text::date),
d AS (SELECT e.id, e.category, e.amount, e.n, g::date AS day, row_number() OVER (PARTITION BY e.id ORDER BY g) AS k
      FROM e, generate_series(e.from_date, e.to_date, interval '1 day') g)
SELECT day::text, category, sum(amount / n + CASE WHEN k <= amount % n THEN 1 ELSE 0 END)::bigint
FROM d WHERE day BETWEEN $1::text::date AND $2::text::date GROUP BY day, category ORDER BY day, category`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayAmount{}
	for rows.Next() {
		var d DayAmount
		if err := rows.Scan(&d.Day, &d.Category, &d.Amount); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
