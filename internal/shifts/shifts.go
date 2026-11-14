// Package shifts keeps in PostgreSQL the shifts of the cashiers: the count of the drawer when it was
// opened and when it was closed, the money put in and taken out in between, and what the drawer
// should have held.
//
// The cash that orders brought in is in the log of the service, not here: whoever closes a shift
// reads it there and gives it to Close, which works out what the drawer should hold and how far the
// count is from it, and keeps all of it. This package never reads the log.
package shifts

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationsTable is where the migrator records what it applied.
const MigrationsTable = "shifts_migrations"

// Migrations returns the SQL files of this package, for the migrator.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}

// Denominations are the notes and coins of the drawer, largest first, in đồng.
var Denominations = []int64{500_000, 200_000, 100_000, 50_000, 20_000, 10_000, 5_000, 2_000, 1_000, 500, 200}

// maxPieces bounds how many pieces of one kind a count may name, so that a typing mistake cannot
// make a drawer worth more than the money there is.
const maxPieces = 100_000

// Counts is a count of a drawer: how many pieces of each denomination.
type Counts map[int64]int64

// Total is what the count adds up to. It refuses a denomination that is not one, and a number of
// pieces that is negative or absurd.
func (c Counts) Total() (int64, error) {
	var total int64
	for d, n := range c {
		known := false
		for _, k := range Denominations {
			known = known || k == d
		}
		if !known {
			return 0, invalid("%d is not a note or a coin of the drawer", d)
		}
		if n < 0 || n > maxPieces {
			return 0, invalid("a count of %d pieces of %d is not possible", n, d)
		}
		total += d * n
	}
	return total, nil
}

// MarshalJSON writes the denominations as strings, which is what a JSON key is.
func (c Counts) MarshalJSON() ([]byte, error) {
	m := make(map[string]int64, len(c))
	for d, n := range c {
		if n != 0 {
			m[strconv.FormatInt(d, 10)] = n
		}
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads the denominations from strings.
func (c *Counts) UnmarshalJSON(b []byte) error {
	var m map[string]int64
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	out := make(Counts, len(m))
	for k, n := range m {
		d, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return invalid("%q is not a denomination", k)
		}
		out[d] = n
	}
	*c = out
	return nil
}

// Errors of the store.
var (
	ErrNotFound = errors.New("shifts: not found")
	// ErrCashierBusy says that the cashier has a shift open already.
	ErrCashierBusy = errors.New("shifts: this cashier has a shift open")
	// ErrTillBusy says that the drawer is in another shift that is open.
	ErrTillBusy = errors.New("shifts: this drawer is in a shift that is open")
	// ErrNotOpen says that the shift is closed.
	ErrNotOpen = errors.New("shifts: the shift is closed")
	// ErrNotClosed says that the shift is still open.
	ErrNotClosed = errors.New("shifts: the shift is not closed")
	// ErrReviewed says that the shift was reviewed already.
	ErrReviewed = errors.New("shifts: the shift was reviewed already")
)

// InvalidError says that a value was refused, and why.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "shifts: " + e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{fmt.Sprintf(format, args...)}
}

// Status of a shift.
const (
	Open   = "open"
	Closed = "closed"
)

// Movement is money put into the drawer or taken out of it during a shift.
type Movement struct {
	ID     int64     `json:"id"`
	Kind   string    `json:"kind"` // "in" or "out"
	Amount int64     `json:"amount"`
	Reason string    `json:"reason"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// Shift is one shift of a cashier at a drawer.
type Shift struct {
	ID            int64     `json:"id"`
	Till          string    `json:"till"`
	Cashier       string    `json:"cashier"`
	Status        string    `json:"status"`
	OpenedAt      time.Time `json:"opened_at"`
	OpeningCounts Counts    `json:"opening_counts"`
	OpeningTotal  int64     `json:"opening_total"`

	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	ClosedBy      string     `json:"closed_by,omitempty"`
	ClosingCounts Counts     `json:"closing_counts,omitempty"`
	ClosingTotal  *int64     `json:"closing_total,omitempty"`
	// Sales, Refunds, PayIn and PayOut are what the expected cash was made of; Expected is
	// opening + sales - refunds + pay-in - pay-out, and Variance is the count less Expected: negative
	// when the drawer is short.
	Sales     *int64 `json:"sales,omitempty"`
	Refunds   *int64 `json:"refunds,omitempty"`
	PayIn     *int64 `json:"pay_in,omitempty"`
	PayOut    *int64 `json:"pay_out,omitempty"`
	Expected  *int64 `json:"expected,omitempty"`
	Variance  *int64 `json:"variance,omitempty"`
	CloseNote string `json:"close_note,omitempty"`

	ReviewedBy string     `json:"reviewed_by,omitempty"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
	ReviewNote string     `json:"review_note,omitempty"`
	Movements  []Movement `json:"movements,omitempty"`
}

// Store is the shifts of a database.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a Store on a database whose migrations (see Migrations) have been applied. now is
// the clock; time.Now if nil.
func New(pool *pgxpool.Pool, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }}
}

// Now is the clock of the store, which the caller uses to choose the moment a shift closes.
func (s *Store) Now() time.Time { return s.now() }

var tillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func note(s, field string, limit int) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > limit {
		return "", invalid("%s is at most %d characters", field, limit)
	}
	return s, nil
}

const shiftColumns = `id, till, cashier, status, opened_at, opening_counts, opening_total, closed_at, COALESCE(closed_by, ''), closing_counts, closing_total,
    sales, refunds, pay_in, pay_out, expected, variance, close_note, COALESCE(reviewed_by, ''), reviewed_at, review_note`

func scanShift(row pgx.Row) (Shift, error) {
	var sh Shift
	var open, closing []byte
	err := row.Scan(&sh.ID, &sh.Till, &sh.Cashier, &sh.Status, &sh.OpenedAt, &open, &sh.OpeningTotal, &sh.ClosedAt, &sh.ClosedBy, &closing, &sh.ClosingTotal,
		&sh.Sales, &sh.Refunds, &sh.PayIn, &sh.PayOut, &sh.Expected, &sh.Variance, &sh.CloseNote, &sh.ReviewedBy, &sh.ReviewedAt, &sh.ReviewNote)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shift{}, ErrNotFound
	}
	if err != nil {
		return Shift{}, err
	}
	if err := json.Unmarshal(open, &sh.OpeningCounts); err != nil {
		return Shift{}, err
	}
	if len(closing) > 0 {
		if err := json.Unmarshal(closing, &sh.ClosingCounts); err != nil {
			return Shift{}, err
		}
	}
	return sh, nil
}

// Open starts a shift of a cashier at a drawer with the count of the drawer.
func (s *Store) Open(ctx context.Context, cashier, till string, counts Counts) (Shift, error) {
	if cashier == "" {
		return Shift{}, invalid("say who opens")
	}
	if !tillName.MatchString(till) {
		return Shift{}, invalid("a drawer is named with 1 to 32 lower-case letters, digits, dashes and underscores")
	}
	total, err := counts.Total()
	if err != nil {
		return Shift{}, err
	}
	raw, _ := json.Marshal(counts)
	sh, err := scanShift(s.pool.QueryRow(ctx, `INSERT INTO shifts (till, cashier, status, opened_at, opening_counts, opening_total)
		VALUES ($1, $2, 'open', $3, $4::jsonb, $5) RETURNING `+shiftColumns, till, cashier, s.now(), string(raw), total))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		if pe.ConstraintName == "one_open_shift_per_till" {
			return Shift{}, ErrTillBusy
		}
		return Shift{}, ErrCashierBusy
	}
	return sh, err
}

func (s *Store) movements(ctx context.Context, id int64) ([]Movement, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, amount, reason, by, at FROM shift_movements WHERE shift_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Movement{}
	for rows.Next() {
		var m Movement
		if err := rows.Scan(&m.ID, &m.Kind, &m.Amount, &m.Reason, &m.By, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns a shift with its movements.
func (s *Store) Get(ctx context.Context, id int64) (Shift, error) {
	sh, err := scanShift(s.pool.QueryRow(ctx, `SELECT `+shiftColumns+` FROM shifts WHERE id = $1`, id))
	if err != nil {
		return Shift{}, err
	}
	if sh.Movements, err = s.movements(ctx, id); err != nil {
		return Shift{}, err
	}
	return sh, nil
}

// OpenOf returns the shift that a cashier has open, with its movements; ErrNotFound if none.
func (s *Store) OpenOf(ctx context.Context, cashier string) (Shift, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM shifts WHERE cashier = $1 AND status = 'open'`, cashier).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shift{}, ErrNotFound
	}
	if err != nil {
		return Shift{}, err
	}
	return s.Get(ctx, id)
}

// HasOpen says whether a cashier has a shift open.
func (s *Store) HasOpen(ctx context.Context, cashier string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM shifts WHERE cashier = $1 AND status = 'open'`, cashier).Scan(&n)
	return n > 0, err
}

// Filter chooses which shifts List returns: of one cashier (empty for all), of one status (empty for
// both).
type Filter struct {
	Cashier string
	Status  string
	Limit   int
}

// List returns shifts, newest first, without their movements.
func (s *Store) List(ctx context.Context, f Filter) ([]Shift, error) {
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+shiftColumns+` FROM shifts
		WHERE ($1 = '' OR cashier = $1) AND ($2 = '' OR status = $2) ORDER BY id DESC LIMIT $3`, f.Cashier, f.Status, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Shift
	for rows.Next() {
		sh, err := scanShift(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// AddMovement records money put into the drawer ("in") or taken out of it ("out") during an open
// shift.
func (s *Store) AddMovement(ctx context.Context, shiftID int64, kind string, amount int64, reason, by string) (Movement, error) {
	if kind != "in" && kind != "out" {
		return Movement{}, invalid("money goes in or out")
	}
	if amount < 1 || amount > 1_000_000_000 {
		return Movement{}, invalid("an amount is a whole number of đồng above zero")
	}
	reason, err := note(reason, "the reason", 200)
	if err != nil {
		return Movement{}, err
	}
	if reason == "" {
		return Movement{}, invalid("say why")
	}
	if by == "" {
		return Movement{}, invalid("say who records")
	}
	m := Movement{Kind: kind, Amount: amount, Reason: reason, By: by, At: s.now()}
	err = s.pool.QueryRow(ctx, `INSERT INTO shift_movements (shift_id, kind, amount, reason, by, at)
		SELECT $1, $2, $3, $4, $5, $6 WHERE EXISTS (SELECT 1 FROM shifts WHERE id = $1 AND status = 'open') RETURNING id`,
		shiftID, kind, amount, reason, by, m.At).Scan(&m.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Movement{}, s.whyNotOpen(ctx, shiftID)
	}
	return m, err
}

func (s *Store) whyNotOpen(ctx context.Context, id int64) error {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT status FROM shifts WHERE id = $1`, id).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return ErrNotOpen
}

// Figures are what the log says about the cash of a shift, for Close.
type Figures struct {
	Sales   int64
	Refunds int64
}

// Close ends a shift. at is the moment it ends, and the one the figures were read up to. The
// drawer should hold the opening count, plus the sales, less the refunds, plus what was put in and
// less what was taken out; the variance is the count less that. The shift is closed in one
// transaction with its movements locked, so a movement cannot slip in between.
func (s *Store) Close(ctx context.Context, id int64, by string, at time.Time, counts Counts, f Figures, closeNote string) (Shift, error) {
	if by == "" {
		return Shift{}, invalid("say who closes")
	}
	total, err := counts.Total()
	if err != nil {
		return Shift{}, err
	}
	if f.Sales < 0 || f.Refunds < 0 {
		return Shift{}, invalid("the figures are not negative")
	}
	closeNote, err = note(closeNote, "the note", 300)
	if err != nil {
		return Shift{}, err
	}
	raw, _ := json.Marshal(counts)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Shift{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	var opening int64
	var openedAt time.Time
	err = tx.QueryRow(ctx, `SELECT status, opening_total, opened_at FROM shifts WHERE id = $1 FOR UPDATE`, id).Scan(&status, &opening, &openedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shift{}, ErrNotFound
	}
	if err != nil {
		return Shift{}, err
	}
	if status != Open {
		return Shift{}, ErrNotOpen
	}
	if at.Before(openedAt) {
		at = openedAt
	}
	var in, out int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(sum(amount) FILTER (WHERE kind = 'in'), 0), COALESCE(sum(amount) FILTER (WHERE kind = 'out'), 0)
		FROM shift_movements WHERE shift_id = $1`, id).Scan(&in, &out)
	if err != nil {
		return Shift{}, err
	}
	expected := opening + f.Sales - f.Refunds + in - out
	_, err = tx.Exec(ctx, `UPDATE shifts SET status = 'closed', closed_at = $2, closed_by = $3, closing_counts = $4::jsonb, closing_total = $5,
		sales = $6, refunds = $7, pay_in = $8, pay_out = $9, expected = $10, variance = $11, close_note = $12 WHERE id = $1`,
		id, at, by, string(raw), total, f.Sales, f.Refunds, in, out, expected, total-expected, closeNote)
	if err != nil {
		return Shift{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Shift{}, err
	}
	return s.Get(ctx, id)
}

// Review records that a manager has looked at a closed shift, with a note. It is done once.
func (s *Store) Review(ctx context.Context, id int64, by, reviewNote string) (Shift, error) {
	if by == "" {
		return Shift{}, invalid("say who reviews")
	}
	reviewNote, err := note(reviewNote, "the note", 300)
	if err != nil {
		return Shift{}, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE shifts SET reviewed_by = $2, reviewed_at = $3, review_note = $4 WHERE id = $1 AND status = 'closed' AND reviewed_at IS NULL`,
		id, by, s.now(), reviewNote)
	if err != nil {
		return Shift{}, err
	}
	if tag.RowsAffected() == 0 {
		var st string
		var reviewed *time.Time
		err := s.pool.QueryRow(ctx, `SELECT status, reviewed_at FROM shifts WHERE id = $1`, id).Scan(&st, &reviewed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return Shift{}, ErrNotFound
		case err != nil:
			return Shift{}, err
		case st != Closed:
			return Shift{}, ErrNotClosed
		}
		return Shift{}, ErrReviewed
	}
	return s.Get(ctx, id)
}
